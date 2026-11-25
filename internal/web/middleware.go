package web

import (
	"container/list"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"byname/internal/store"
	"byname/internal/whois"
)

// Request limits.
const (
	maxBodyBytes  = 16 << 10
	maxRequestURI = 4096
)

// contentSecurityPolicy allows only this origin's own CSS, JS and images, and
// forbids framing, plugins, <base> rewriting and posting forms elsewhere.
const contentSecurityPolicy = "default-src 'none'; style-src 'self'; script-src 'self'; img-src 'self'; " +
	"form-action 'self'; base-uri 'none'; frame-ancestors 'none'"

// recoverer turns a panic into a 500 instead of a dropped connection.
func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				s.log.Error("panic", "err", v, "path", r.URL.Path, "stack", string(debug.Stack()))
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// securityHeaders sets headers on every response, including rejections.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		// same-origin, not no-referrer: under no-referrer, browsers send
		// "Origin: null" on form POSTs, and over plain HTTP (no
		// Sec-Fetch-Site) CrossOriginProtection would then reject byname's own
		// forms. same-origin still sends no Referer to any other site,
		// including redirect destinations.
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

// logRequests logs mutations and errors at info/error, everything else at
// debug, so the journal (volatile on the Pi) is not filled by redirects.
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		level := slog.LevelDebug
		switch {
		case rec.status >= 500:
			level = slog.LevelError
		case r.Method != http.MethodGet && r.Method != http.MethodHead, rec.status == http.StatusForbidden,
			rec.status == http.StatusMisdirectedRequest, rec.status == http.StatusTooManyRequests:
			level = slog.LevelInfo
		}
		s.log.Log(r.Context(), level, "request", "method", r.Method, "path", r.URL.Path,
			"status", rec.status, "ms", time.Since(start).Milliseconds(), "remote", r.RemoteAddr)
	})
}

// notFunnel marks requests as arriving on the private listener, and refuses
// any that Tailscale Funnel delivered. tailscaled always adds
// Tailscale-Funnel-Request to Funnel traffic (and strips it from clients), so
// a Funnel pointed at this listener by mistake (`tailscale funnel 80`, which
// arrives from localhost) can't skip sign-in.
func notFunnel(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Tailscale-Funnel-Request") != "" {
			http.Error(w, "this address is not published on the internet", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, withVia(r, viaPrivate))
	})
}

// sourceCheck rejects requests whose peer address is outside the allowed
// networks (the tailnet by default; the home LAN too when configured). This is
// the application-level copy of the nftables and systemd IPAddressAllow rules.
func (s *Server) sourceCheck(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip, ok := peerIP(r)
		if !ok || !s.allowedIP(ip) {
			http.Error(w, s.display+" is not reachable from this network", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func peerIP(r *http.Request) (netip.Addr, bool) {
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}, false
	}
	return ap.Addr().Unmap(), true
}

func (s *Server) allowedIP(ip netip.Addr) bool {
	for _, p := range s.cfg.AllowedNets {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// normalizeHost lower-cases a Host header and strips a trailing dot and the
// default port.
func normalizeHost(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	h = strings.TrimSuffix(h, ":80")
	return strings.TrimSuffix(h, ".")
}

// hostCheck rejects requests for any Host but ours. This defeats DNS
// rebinding: a hostile site that re-points its own name at the Pi still sends
// its own name as Host.
func (s *Server) hostCheck(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.hosts[normalizeHost(r.Host)] {
			http.Error(w, "unknown host", http.StatusMisdirectedRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// limits caps request URI and body sizes.
func limits(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.RequestURI) > maxRequestURI {
			http.Error(w, "request URI too long", http.StatusRequestURITooLong)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		next.ServeHTTP(w, r)
	})
}

// limiterSet holds two token buckets per key (device, account or address): a
// short-term rate (stops bursts) and a daily budget (bounds how fast a
// compromised device could grow the append-only audit log and fill the SD
// card). Memory is bounded: entries are kept in least-recently-used order and
// dropped once idle long enough that both buckets would be full again. What
// happens past maxKeys depends on evict:
//
//   - evict (anonymous internet requests): the least recently used entry
//     makes room, so every address gets a bucket of its own and nobody can
//     drain a shared one;
//   - no evict (changes): new keys are refused, so a daily budget can never
//     be reset by pushing its entry out. Change keys are signed-in accounts,
//     tailnet devices and addresses, and one shared key for everything
//     without Tailscale (see actor): far fewer than maxKeys.
type limiterSet struct {
	mu      sync.Mutex
	limit   rate.Limit // short-term rate; <= 0 disables
	burst   int
	daily   int           // per key per 24 h; <= 0 disables
	idle    time.Duration // after this long unused, an entry is as good as new
	maxKeys int
	evict   bool
	m       map[string]*list.Element // values are *limiterEntry
	lru     *list.List               // front: most recently used
}

type limiterEntry struct {
	key        string
	short, day *rate.Limiter
	seen       time.Time
}

const limiterMaxKeys = 10000

func newLimiterSet(limit rate.Limit, burst, daily int) *limiterSet {
	idle := time.Minute
	switch {
	case daily > 0:
		idle = 24 * time.Hour
	case limit > 0 && limit != rate.Inf:
		idle = time.Duration(float64(burst)/float64(limit)*float64(time.Second)) + time.Second
	}
	return &limiterSet{limit: limit, burst: burst, daily: daily, idle: idle,
		maxKeys: limiterMaxKeys, m: make(map[string]*list.Element), lru: list.New()}
}

func (ls *limiterSet) remove(el *list.Element) {
	ls.lru.Remove(el)
	delete(ls.m, el.Value.(*limiterEntry).key)
}

func (ls *limiterSet) allow(key string) bool {
	now := time.Now()
	ls.mu.Lock()
	defer ls.mu.Unlock()
	// The back of the list is the least recently used, so idle entries are
	// all there: dropping them costs only as many steps as there are.
	for el := ls.lru.Back(); el != nil && now.Sub(el.Value.(*limiterEntry).seen) > ls.idle; el = ls.lru.Back() {
		ls.remove(el)
	}
	var e *limiterEntry
	if el, ok := ls.m[key]; ok {
		e = el.Value.(*limiterEntry)
		ls.lru.MoveToFront(el)
	} else {
		if ls.lru.Len() >= ls.maxKeys {
			if !ls.evict {
				return false
			}
			ls.remove(ls.lru.Back())
		}
		e = &limiterEntry{key: key}
		if ls.limit > 0 {
			e.short = rate.NewLimiter(ls.limit, ls.burst)
		}
		if ls.daily > 0 {
			e.day = rate.NewLimiter(rate.Limit(float64(ls.daily)/(24*60*60)), ls.daily)
		}
		ls.m[key] = ls.lru.PushFront(e)
	}
	e.seen = now
	var dayRes *rate.Reservation
	if e.day != nil {
		dayRes = e.day.ReserveN(now, 1)
		if !dayRes.OK() || dayRes.DelayFrom(now) > 0 {
			dayRes.CancelAt(now)
			return false
		}
	}
	if e.short != nil && !e.short.AllowN(now, 1) {
		if dayRes != nil {
			dayRes.CancelAt(now) // refund: the change did not happen
		}
		return false
	}
	return true
}

type actorKey struct{}

// tailnetNets are Tailscale's address ranges; only these can have a WhoIs
// identity. Other allowed peers (home LAN, localhost) are known by IP only.
var tailnetNets = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("fd7a:115c:a1e0::/48"),
}

func onTailnet(ip netip.Addr) bool {
	for _, p := range tailnetNets {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// identify resolves who sent r via Tailscale WhoIs, falling back to the IP.
func (s *Server) identify(r *http.Request) (store.Actor, whois.Identity, error) {
	ip, _ := peerIP(r)
	if !onTailnet(ip) {
		return store.Actor{IP: ip.String()}, whois.Identity{}, whois.ErrUnknown
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	id, err := s.who.WhoIs(ctx, r.RemoteAddr)
	if err != nil && !errors.Is(err, whois.ErrUnknown) {
		s.log.Warn("whois failed; attributing by IP", "remote", r.RemoteAddr, "err", err)
	}
	return store.Actor{Login: id.Login, Node: id.Node, IP: ip.String()}, id, err
}

// actor is who r's changes are recorded as, and the key their write budget is
// counted under: the signed-in account on the public address; otherwise the
// Tailscale device, or its tailnet address when WhoIs knows nobody. Devices
// without Tailscale (the home LAN, localhost) share one budget: they are known
// only by address, and a LAN device can take as many addresses as it likes,
// so per-address budgets would let it fill the limiter's table (which then
// refuses newcomers) and multiply its daily allowance.
func (s *Server) actor(r *http.Request) (store.Actor, string) {
	if sc := sessionOf(r); sc != nil {
		ip, _ := peerIP(r)
		label := sc.Provider
		if p := s.providerByID[sc.Provider]; p != nil {
			label = p.Label()
		}
		return store.Actor{Login: sc.Email, Node: "via " + label, IP: ip.String()},
			"user:" + sc.Provider + ":" + sc.Subject
	}
	a, _, _ := s.identify(r)
	if a.Node != "" {
		return a, "node:" + a.Node
	}
	if ip, ok := peerIP(r); ok && onTailnet(ip) {
		return a, "ip:" + a.IP
	}
	return a, "no-tailscale"
}

// write wraps a mutating handler: it attributes the request to an actor and
// applies the per-actor write rate limit.
func (s *Server) write(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a, key := s.actor(r)
		if !s.writes.allow(key) {
			w.Header().Set("Retry-After", "10")
			http.Error(w, "too many changes; slow down", http.StatusTooManyRequests)
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), actorKey{}, a)))
	}
}

func actorFrom(r *http.Request) store.Actor {
	a, _ := r.Context().Value(actorKey{}).(store.Actor)
	return a
}

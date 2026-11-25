package web

import (
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

	"otterly-go/internal/store"
	"otterly-go/internal/whois"
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
		// Sec-Fetch-Site) CrossOriginProtection would then reject ot's own
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

// limiterSet holds two token buckets per device: a short-term rate (stops
// bursts) and a daily budget (bounds how fast a compromised device could grow
// the append-only audit log and fill the SD card).
type limiterSet struct {
	mu    sync.Mutex
	limit rate.Limit // short-term rate; <= 0 disables
	burst int
	daily int // changes per device per 24 h; <= 0 disables
	m     map[string]*limiterEntry
}

type limiterEntry struct {
	short, day *rate.Limiter
	seen       time.Time
}

func newLimiterSet(limit rate.Limit, burst, daily int) *limiterSet {
	return &limiterSet{limit: limit, burst: burst, daily: daily, m: make(map[string]*limiterEntry)}
}

func (ls *limiterSet) allow(key string) bool {
	now := time.Now()
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if len(ls.m) > 1000 {
		for k, e := range ls.m {
			if now.Sub(e.seen) > 24*time.Hour { // a full day idle: both buckets are full again
				delete(ls.m, k)
			}
		}
	}
	e, ok := ls.m[key]
	if !ok {
		e = &limiterEntry{}
		if ls.limit > 0 {
			e.short = rate.NewLimiter(ls.limit, ls.burst)
		}
		if ls.daily > 0 {
			e.day = rate.NewLimiter(rate.Limit(float64(ls.daily)/(24*60*60)), ls.daily)
		}
		ls.m[key] = e
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

// write wraps a mutating handler: it attributes the request to an actor and
// applies the per-actor write rate limit.
func (s *Server) write(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a, _, _ := s.identify(r)
		key := "ip:" + a.IP
		if a.Node != "" {
			key = "node:" + a.Node
		}
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

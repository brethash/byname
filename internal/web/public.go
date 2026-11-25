package web

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"otterly-go/internal/auth"
	"otterly-go/internal/store"
)

// The public address.
//
// Tailscale Funnel publishes https://<name>.<tailnet>.ts.net on the internet
// and Tailscale Serve answers the same name for tailnet devices. tailscaled
// terminates TLS and proxies both to ot over a unix socket that only it
// (root) and ot can open. ot trusts what tailscaled adds to those requests,
// and only on that socket:
//
//   - X-Forwarded-For is the real client address (tailscaled sets it, never
//     appends to a client's value);
//   - X-Forwarded-Host is the name the browser asked for;
//   - Tailscale-Funnel-Request is present on requests from the internet
//     (tailscaled strips any copy a client sends).
//
// Requests from tailnet devices skip sign-in, exactly as on the private
// listener; the plain-HTTP listener refuses anything Funnel marks, so a
// Funnel pointed at it by mistake can't bypass sign-in. Everything from the internet needs a Google or Slack session,
// except the sign-in pages themselves and static assets.

const (
	sessionCookie = "__Host-ot_session"
	signinCookie  = "__Host-ot_signin"
	signinTTL     = 10 * time.Minute
)

// via is where a request came from. The zero value is the internet, so a
// request that was never classified gets the least trust.
type via int

const (
	viaInternet via = iota // public listener: Tailscale Funnel, or headers that can't be read
	viaServe               // public listener: a tailnet device through Tailscale Serve
	viaPrivate             // the private listener (tailnet, LAN, localhost)
)

type viaKey struct{}
type sessionKey struct{}

func viaOf(r *http.Request) via {
	v, _ := r.Context().Value(viaKey{}).(via)
	return v
}

func withVia(r *http.Request, v via) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), viaKey{}, v))
}

// public reports whether r arrived on the public listener.
func public(r *http.Request) bool { return viaOf(r) != viaPrivate }

// fromInternet reports whether r came from outside the tailnet.
func fromInternet(r *http.Request) bool { return viaOf(r) == viaInternet }

type session struct {
	store.Session
	hash []byte
}

func sessionOf(r *http.Request) *session {
	sc, _ := r.Context().Value(sessionKey{}).(*session)
	return sc
}

// Public returns the handler for the public listener, or nil when no public
// address is configured.
func (s *Server) Public() http.Handler { return s.public }

func (s *Server) publicRoutes() http.Handler {
	var h http.Handler = s.crossOrigin().Handler(s.mux(true))
	h = limits(h)
	h = s.publicGate(h)
	h = s.logRequests(h)
	h = s.fromServe(h)
	h = securityHeaders(h)
	h = hsts(h)
	h = s.recoverer(h)
	return h
}

func hsts(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Strict-Transport-Security", "max-age=31536000")
		next.ServeHTTP(w, r)
	})
}

// fromServe reads tailscaled's forwarding headers (see the comment at the top
// of this file) and classifies the request. A request whose headers can't be
// read is treated as coming from the internet.
func (s *Server) fromServe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := strings.TrimSuffix(normalizeHost(r.Header.Get("X-Forwarded-Host")), ":443")
		if host != s.publicHost {
			http.Error(w, "unknown host", http.StatusMisdirectedRequest)
			return
		}
		r.Host = host // what CrossOriginProtection compares Origin with
		v := viaInternet
		r.RemoteAddr = ""
		if ip, err := netip.ParseAddr(strings.TrimSpace(r.Header.Get("X-Forwarded-For"))); err == nil {
			ip = ip.Unmap()
			r.RemoteAddr = netip.AddrPortFrom(ip, 0).String()
			// A tailnet device: no Funnel marker, a tailnet address, and one
			// the private listener would accept too (--allow-cidrs).
			if r.Header.Get("Tailscale-Funnel-Request") == "" && onTailnet(ip) && s.allowedIP(ip) {
				v = viaServe
			}
		}
		next.ServeHTTP(w, withVia(r, v))
	})
}

// publicGate lets tailnet devices and signed-in people through, and sends
// everyone else to the sign-in page.
func (s *Server) publicGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !fromInternet(r) {
			next.ServeHTTP(w, r)
			return
		}
		if sc := s.lookupSession(w, r); sc != nil {
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), sessionKey{}, sc)))
			return
		}
		ip, _ := peerIP(r)
		if !s.anon.allow(anonKey(ip)) {
			w.Header().Set("Retry-After", "30")
			http.Error(w, "too many requests; slow down", http.StatusTooManyRequests)
			return
		}
		if openPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		if (r.Method == http.MethodGet || r.Method == http.MethodHead) && !strings.HasPrefix(r.URL.Path, "/_/api/") {
			http.Redirect(w, r, "/_/signin?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
			return
		}
		http.Error(w, "sign in first", http.StatusUnauthorized)
	})
}

// anonKey groups internet clients for rate limiting: by address, or by /64
// for IPv6, since one host usually owns a whole /64.
func anonKey(ip netip.Addr) string {
	if ip.Is6() {
		p, _ := ip.Prefix(64)
		return "net:" + p.String()
	}
	return "ip:" + ip.String()
}

// openPath lists what the internet may reach without signing in.
func openPath(p string) bool {
	switch p {
	case "/_/signin", "/_/signout", "/favicon.ico", "/_/favicon.svg", "/robots.txt":
		return true
	}
	return strings.HasPrefix(p, "/_/static/") || strings.HasPrefix(p, "/_/auth/")
}

// lookupSession returns the live session for r's cookie, if any. A session
// whose person is no longer allowed (removed from the allow list, or their
// provider turned off) is deleted.
func (s *Server) lookupSession(w http.ResponseWriter, r *http.Request) *session {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil
	}
	if len(c.Value) != 43 { // auth.Token's length
		clearCookie(w, sessionCookie)
		return nil
	}
	sum := sha256.Sum256([]byte(c.Value))
	x, err := s.svc.Store.SessionByHash(r.Context(), sum[:])
	if err != nil {
		if !errors.Is(err, store.ErrNoSession) {
			s.log.Error("session lookup failed", "err", err)
		}
		clearCookie(w, sessionCookie)
		return nil
	}
	p := s.providerByID[x.Provider]
	if p == nil || !p.Allowed(auth.Claims{Provider: x.Provider, Subject: x.Subject, Email: x.Email, Org: x.Org}) {
		s.log.Warn("session ended: no longer allowed", "provider", x.Provider, "email", x.Email, "remote", r.RemoteAddr)
		if err := s.svc.Store.DeleteSession(r.Context(), sum[:]); err != nil {
			s.log.Error("session delete failed", "err", err)
		}
		clearCookie(w, sessionCookie)
		return nil
	}
	return &session{Session: x, hash: sum[:]}
}

func setCookie(w http.ResponseWriter, name, value string, maxAge time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: value, Path: "/", MaxAge: int(maxAge.Seconds()),
		Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
}

func clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: "", Path: "/", MaxAge: -1,
		Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
}

// safeNext keeps a post-sign-in destination on this site: a local path, not
// "//host" or "/\host" (which browsers treat as another host), and not the
// sign-in pages themselves.
func safeNext(next string) string {
	if next == "" || len(next) > 2048 || next[0] != '/' || strings.HasPrefix(next, "//") ||
		strings.HasPrefix(next, "/_/signin") || strings.HasPrefix(next, "/_/signout") || strings.HasPrefix(next, "/_/auth/") {
		return "/"
	}
	for i := 0; i < len(next); i++ {
		if c := next[i]; c < 0x21 || c == 0x7f || c == '\\' {
			return "/"
		}
	}
	return next
}

func (s *Server) redirectURI(p *auth.Provider) string {
	return s.publicURL + "/_/auth/" + p.ID() + "/callback"
}

type providerView struct{ ID, Label string }

func (s *Server) renderSignin(w http.ResponseWriter, status int, next, msg, errMsg string) {
	v := view{Title: "Sign in", Bare: true, Next: safeNext(next), Message: msg, Error: errMsg}
	for _, p := range s.providers {
		v.Providers = append(v.Providers, providerView{ID: p.ID(), Label: p.Label()})
	}
	s.render(w, status, "signin", v)
}

func (s *Server) signin(w http.ResponseWriter, r *http.Request) {
	next := r.URL.Query().Get("next")
	if !fromInternet(r) || sessionOf(r) != nil {
		http.Redirect(w, r, safeNext(next), http.StatusSeeOther)
		return
	}
	msg := ""
	if r.URL.Query().Has("out") {
		msg = "You're signed out."
	}
	s.renderSignin(w, http.StatusOK, next, msg, "")
}

func (s *Server) authStart(w http.ResponseWriter, r *http.Request) {
	p := s.providerByID[r.PathValue("provider")]
	if p == nil {
		http.NotFound(w, r)
		return
	}
	next := safeNext(r.URL.Query().Get("next"))
	if !fromInternet(r) || sessionOf(r) != nil {
		http.Redirect(w, r, next, http.StatusSeeOther)
		return
	}
	a, cookie, err := s.attempts.Start(p.ID(), next)
	if err != nil {
		s.log.Error("sign-in start failed", "err", err)
		s.renderSignin(w, http.StatusInternalServerError, next, "", "Couldn't start signing in. Try again.")
		return
	}
	setCookie(w, signinCookie, cookie, signinTTL)
	http.Redirect(w, r, p.AuthURL(s.redirectURI(p), a.State, a.Nonce, a.Verifier), http.StatusSeeOther)
}

func (s *Server) authCallback(w http.ResponseWriter, r *http.Request) {
	p := s.providerByID[r.PathValue("provider")]
	if p == nil {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	var cookie string
	if c, err := r.Cookie(signinCookie); err == nil {
		cookie = c.Value
	}
	a, ok := s.attempts.Finish(q.Get("state"), cookie)
	if !ok || a.Provider != p.ID() {
		// The browser's sign-in cookie is left alone, so a stray or forged
		// callback can't cancel a real sign-in in progress.
		s.log.Info("sign-in callback without a matching sign-in", "provider", p.ID(), "remote", r.RemoteAddr)
		s.renderSignin(w, http.StatusBadRequest, "/", "", "That sign-in expired or was started in another tab. Try again.")
		return
	}
	clearCookie(w, signinCookie) // used once
	if e := q.Get("error"); e != "" {
		s.log.Info("sign-in cancelled", "provider", p.ID(), "error", trunc(e, 40), "remote", r.RemoteAddr)
		s.renderSignin(w, http.StatusOK, a.Next, "", "Sign-in with "+p.Label()+" was cancelled.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	claims, err := p.Exchange(ctx, q.Get("code"), s.redirectURI(p), a.Verifier, a.Nonce)
	if err != nil {
		s.log.Warn("sign-in failed", "provider", p.ID(), "err", err, "remote", r.RemoteAddr)
		s.renderSignin(w, http.StatusBadGateway, a.Next, "", "Couldn't finish signing in with "+p.Label()+". Try again.")
		return
	}
	if !p.Allowed(claims) {
		s.log.Warn("sign-in refused: not on the allow list", "provider", p.ID(), "email", claims.Email,
			"org", claims.Org, "remote", r.RemoteAddr)
		s.renderSignin(w, http.StatusForbidden, a.Next, "",
			claims.Email+" isn't allowed to use "+s.display+"/. Ask whoever runs it to add you, or sign in with another account.")
		return
	}
	if old, err := r.Cookie(sessionCookie); err == nil { // never keep a session across sign-ins
		sum := sha256.Sum256([]byte(old.Value))
		s.svc.Store.DeleteSession(r.Context(), sum[:])
	}
	token := auth.Token()
	sum := sha256.Sum256([]byte(token))
	now := time.Now()
	ip, _ := peerIP(r)
	if err := s.svc.Store.CreateSession(r.Context(), sum[:], store.Session{
		Provider: claims.Provider, Subject: claims.Subject, Email: claims.Email, Name: claims.Name,
		Org: claims.Org, IP: ip.String(), Created: now, Expires: now.Add(s.sessionTTL),
	}); err != nil {
		s.log.Error("session create failed", "err", err)
		s.renderSignin(w, http.StatusInternalServerError, a.Next, "", "Couldn't finish signing in. Try again.")
		return
	}
	setCookie(w, sessionCookie, token, s.sessionTTL)
	s.log.Info("signed in", "provider", p.ID(), "email", claims.Email, "org", claims.Org, "remote", r.RemoteAddr)
	http.Redirect(w, r, a.Next, http.StatusSeeOther)
}

func (s *Server) signout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		sum := sha256.Sum256([]byte(c.Value))
		if err := s.svc.Store.DeleteSession(r.Context(), sum[:]); err != nil {
			s.log.Error("session delete failed", "err", err)
		}
		if sc := sessionOf(r); sc != nil {
			s.log.Info("signed out", "provider", sc.Provider, "email", sc.Email, "remote", r.RemoteAddr)
		}
	}
	clearCookie(w, sessionCookie)
	http.Redirect(w, r, "/_/signin?out=1", http.StatusSeeOther)
}

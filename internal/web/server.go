// Package web is byname's HTTP layer: redirects, the server-rendered UI and the
// JSON API, behind a fixed security middleware chain.
package web

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"html"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/time/rate"

	"byname/internal/app"
	"byname/internal/auth"
	"byname/internal/link"
	"byname/internal/store"
	"byname/internal/whois"
)

// Config configures the server.
type Config struct {
	// Hosts are the accepted Host header values; the first is the display
	// name used in the UI ("nym" → "nym/wiki").
	Hosts []string
	// AllowedNets are the peer networks allowed to connect (the tailnet).
	AllowedNets []netip.Prefix
	// WriteRate and WriteBurst bound changes per device; WriteDaily caps a
	// device's changes per 24 h (0 = no daily cap).
	WriteRate  rate.Limit
	WriteBurst int
	WriteDaily int
	// DataDir is where the database lives; health reports its free space.
	DataDir string
	// ThrottledPath is the Raspberry Pi firmware's get_throttled file; "" or
	// missing disables reporting.
	ThrottledPath string
	Version       string
	Logger        *slog.Logger

	// PublicURL is the server's public https:// address. Empty disables the
	// public handler. People reaching it from the internet must sign in with
	// one of Providers.
	//
	// With PublicBehindTailscale, the address is the tailnet name
	// (https://nym.<tailnet>.ts.net) published by Tailscale Funnel, which
	// proxies to byname over a private socket; byname trusts tailscaled's forwarding
	// headers there, and tailnet devices skip sign-in. Without it, byname serves
	// its own domain (https://hg.example.com) with TLS directly: no header is
	// trusted and every request counts as coming from the internet.
	PublicURL             string
	PublicBehindTailscale bool
	Providers             []*auth.Provider
	// SessionTTL is how long a sign-in lasts (default 30 days).
	SessionTTL time.Duration
}

// Server serves byname over HTTP.
type Server struct {
	cfg     Config
	svc     *app.Service
	who     whois.Resolver
	log     *slog.Logger
	hosts   map[string]bool
	display string
	favicon []byte
	pages   map[string]*template.Template
	// internetPages render for people reaching the public address from the
	// internet: they type the full https name, not the tailnet's short one.
	internetPages map[string]*template.Template
	static        fs.FS
	writes        *limiterSet
	started       time.Time
	handler       http.Handler

	// Public address (zero when not configured).
	public       http.Handler
	publicURL    string // https://host, no trailing slash
	publicHost   string
	providers    []*auth.Provider
	tailnet      bool // the private listener accepts tailnet peers
	providerByID map[string]*auth.Provider
	attempts     *auth.Attempts
	anon         *limiterSet // requests from the internet without a session, per IP or IPv6 /64
	sessionTTL   time.Duration
}

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

var pageNames = []string{"home", "list", "form", "detail", "notfound", "activity", "error", "whoami", "signin"}

// New builds a server.
func New(cfg Config, svc *app.Service, who whois.Resolver) (*Server, error) {
	if len(cfg.Hosts) == 0 {
		return nil, errors.New("at least one host name is required")
	}
	if len(cfg.AllowedNets) == 0 {
		return nil, errors.New("at least one allowed network is required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if who == nil {
		who = whois.Nop{}
	}
	s := &Server{
		cfg:     cfg,
		svc:     svc,
		who:     who,
		log:     cfg.Logger,
		hosts:   make(map[string]bool),
		display: normalizeHost(cfg.Hosts[0]),
		writes:  newLimiterSet(cfg.WriteRate, cfg.WriteBurst, cfg.WriteDaily),
		started: time.Now(),
	}
	s.favicon = faviconSVG(s.display)
	for _, p := range cfg.AllowedNets {
		for _, t := range tailnetNets {
			if p.Overlaps(t) {
				s.tailnet = true
			}
		}
	}
	for _, h := range cfg.Hosts {
		s.hosts[normalizeHost(h)] = true
	}
	pages, err := s.parsePages(s.display, false)
	if err != nil {
		return nil, err
	}
	s.pages = pages
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		return nil, err
	}
	s.static = sub
	s.handler = s.routes()
	if err := s.setupPublic(); err != nil {
		return nil, err
	}
	return s, nil
}

// setupPublic validates the public-address settings and builds its handler.
func (s *Server) setupPublic() error {
	cfg := s.cfg
	if cfg.PublicURL == "" {
		if len(cfg.Providers) > 0 {
			return errors.New("sign-in providers need a public URL")
		}
		return nil
	}
	u, err := url.Parse(cfg.PublicURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("public URL %q: want https://host, e.g. https://hg.example.com or https://nym.<tailnet>.ts.net", cfg.PublicURL)
	}
	if len(cfg.Providers) == 0 {
		return errors.New("a public URL needs at least one sign-in provider (Google or Slack)")
	}
	s.publicHost = strings.TrimSuffix(normalizeHost(u.Host), ":443")
	s.publicURL = "https://" + s.publicHost
	s.providerByID = make(map[string]*auth.Provider)
	for _, p := range cfg.Providers {
		if s.providerByID[p.ID()] != nil {
			return fmt.Errorf("sign-in provider %s configured twice", p.ID())
		}
		s.providerByID[p.ID()] = p
		s.providers = append(s.providers, p)
	}
	s.sessionTTL = cfg.SessionTTL
	if s.sessionTTL <= 0 {
		s.sessionTTL = 30 * 24 * time.Hour
	}
	s.attempts = auth.NewAttempts(signinTTL)
	pages, err := s.parsePages(s.publicHost, true)
	if err != nil {
		return err
	}
	s.internetPages = pages
	s.anon = newLimiterSet(rate.Limit(1), 30, 0) // 60/min, bursts of 30
	s.anon.evict = true
	s.public = s.publicRoutes()
	return nil
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.handler.ServeHTTP(w, r) }

func (s *Server) routes() http.Handler {
	var h http.Handler = s.crossOrigin().Handler(s.mux(false))
	h = limits(h)
	h = s.hostCheck(h)
	h = s.sourceCheck(h)
	h = notFunnel(h)
	h = s.logRequests(h)
	h = securityHeaders(h)
	h = s.recoverer(h)
	return h
}

// mux routes requests; the public listener adds the sign-in pages.
func (s *Server) mux(public bool) *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", s.home)
	mux.HandleFunc("GET /{path...}", s.follow)
	mux.HandleFunc("GET /favicon.ico", s.serveFavicon)
	mux.HandleFunc("GET /_/favicon.svg", s.serveFavicon)
	mux.HandleFunc("GET /robots.txt", robots)

	mux.Handle("GET /_/static/", http.StripPrefix("/_/static/", s.staticHandler()))
	mux.HandleFunc("GET /_/healthz", s.health)
	mux.HandleFunc("GET /_/whoami", s.whoami)
	mux.HandleFunc("GET /_/go", s.keyword)
	mux.HandleFunc("GET /_/opensearch.xml", s.openSearch)
	mux.HandleFunc("GET /_/export", s.export)
	mux.HandleFunc("GET /_/activity", s.activity)

	mux.HandleFunc("GET /_/links", s.list)
	mux.HandleFunc("GET /_/new", s.newForm)
	mux.HandleFunc("POST /_/links", s.write(s.create))
	mux.HandleFunc("GET /_/links/{id}", s.detailByID)
	mux.HandleFunc("GET /_/links/{id}/edit", s.editForm)
	mux.HandleFunc("POST /_/links/{id}", s.write(s.update))
	mux.HandleFunc("POST /_/links/{id}/delete", s.write(s.delete))
	mux.HandleFunc("POST /_/links/{id}/restore", s.write(s.restore))
	mux.HandleFunc("POST /_/events/{id}/revert", s.write(s.revert))

	mux.HandleFunc("GET /_/api/links", s.apiList)
	mux.HandleFunc("POST /_/api/links", s.write(s.apiCreate))
	mux.HandleFunc("GET /_/api/links/{name}", s.apiGet)
	mux.HandleFunc("PUT /_/api/links/{name}", s.write(s.apiUpdate))
	mux.HandleFunc("DELETE /_/api/links/{name}", s.write(s.apiDelete))

	if public {
		mux.HandleFunc("GET /_/signin", s.signin)
		mux.HandleFunc("POST /_/signout", s.signout)
		mux.HandleFunc("GET /_/auth/{provider}/start", s.authStart)
		mux.HandleFunc("GET /_/auth/{provider}/callback", s.authCallback)
	}
	return mux
}

// crossOrigin rejects cross-origin non-GET/HEAD/OPTIONS requests. Over plain
// HTTP browsers do not send Sec-Fetch-Site, so it compares the Origin header
// with Host; hostCheck and fromServe (outside) guarantee Host is ours.
func (s *Server) crossOrigin() *http.CrossOriginProtection {
	cop := http.NewCrossOriginProtection()
	cop.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.log.Warn("blocked cross-origin request", "method", r.Method, "path", r.URL.Path,
			"origin", r.Header.Get("Origin"), "remote", r.RemoteAddr)
		http.Error(w, "cross-origin request blocked", http.StatusForbidden)
	}))
	return cop
}

func (s *Server) staticHandler() http.Handler {
	files := http.FileServerFS(s.static)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "" || strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r) // no directory listings
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=86400")
		files.ServeHTTP(w, r)
	})
}

func (s *Server) serveFavicon(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Write(s.favicon)
}

// faviconSVG draws the service name as the browser-tab icon: the whole name
// when it is short ("nym/", "go/"), otherwise its first letter ("l/" for
// "links"). Only the first DNS label is used, so "nym.lan" draws "nym/".
func faviconSVG(display string) []byte {
	name, _, _ := strings.Cut(display, ".")
	if utf8.RuneCountInString(name) > 3 {
		r, _ := utf8.DecodeRuneInString(name)
		name = string(r)
	}
	label := name + "/"
	size := 26
	switch utf8.RuneCountInString(label) {
	case 1, 2:
		size = 34
	case 4:
		size = 20
	}
	return fmt.Appendf(nil, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64">`+
		`<rect width="64" height="64" rx="14" fill="#0f6e6e"/>`+
		`<text x="32" y="%d" font-family="Menlo,Consolas,monospace" font-size="%d" font-weight="700" text-anchor="middle" fill="#fff">%s</text></svg>`,
		32+size*35/100, size, html.EscapeString(label))
}

func robots(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte("User-agent: *\nDisallow: /\n"))
}

// view is the data every page template receives.
type view struct {
	Host    string // display host, e.g. "nym"
	Version string
	Query   string // search box contents
	Title   string

	Link        *store.Link
	Links       []store.Link
	Popular     []store.Link
	Newest      []store.Link
	Events      []store.Event
	Suggestions []store.Link
	Count       int
	Sort        string
	ShowDeleted bool

	Form    formData
	Missing string // not-found name
	Error   string
	Message string

	Identity  whois.Identity
	PeerIP    string
	OnTailnet bool
	WhoIsErr  string

	// Public address.
	Bare      bool // sign-in page: no search box, navigation or footer links
	Next      string
	Providers []providerView
	Session   *store.Session
	Provider  string // label of Session's provider
	Public    bool   // the page was served on the public address
}

type formData struct {
	ID          int64
	Rev         int64
	Name        string
	Target      string
	Description string
	Editing     bool
	Errors      map[string]string // field → message
}

// parsePages parses every page with host as what people type before
// "/name" ("nym", or the public https name for people on the internet).
func (s *Server) parsePages(host string, internet bool) (map[string]*template.Template, error) {
	pages := make(map[string]*template.Template)
	for _, name := range pageNames {
		t, err := template.New(name).Funcs(s.funcs(host, internet)).ParseFS(templateFS,
			"templates/layout.html", "templates/partials.html", "templates/"+name+".html")
		if err != nil {
			return nil, fmt.Errorf("template %s: %w", name, err)
		}
		pages[name] = t
	}
	return pages, nil
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, page string, v view) {
	v.Host = s.display
	v.Version = s.cfg.Version
	pages := s.pages
	if fromInternet(r) && s.internetPages != nil {
		pages = s.internetPages
	}
	var buf bytes.Buffer
	if err := pages[page].ExecuteTemplate(&buf, "layout", v); err != nil {
		s.log.Error("render failed", "page", page, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	w.Write(buf.Bytes())
}

func (s *Server) renderError(w http.ResponseWriter, r *http.Request, status int, title, msg string) {
	s.render(w, r, status, "error", view{Title: title, Error: msg})
}

// fail maps a service/store error to a status and an error page. Internal
// errors are logged, not shown.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	status := errStatus(err)
	if status == http.StatusInternalServerError {
		s.log.Error("request failed", "path", r.URL.Path, "err", err)
		s.renderError(w, r, status, "Something went wrong", "The server hit an error. Try again; if it keeps happening, check the logs on the Pi.")
		return
	}
	s.renderError(w, r, status, http.StatusText(status), err.Error())
}

func errStatus(err error) int {
	var fe *app.FieldError
	switch {
	case errors.As(err, &fe):
		return http.StatusUnprocessableEntity
	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrEventNotFound):
		return http.StatusNotFound
	case errors.Is(err, store.ErrExists), errors.Is(err, store.ErrConflict),
		errors.Is(err, store.ErrLimit), errors.Is(err, store.ErrNotRevertible):
		return http.StatusConflict
	}
	return http.StatusInternalServerError
}

// funcs are the template helpers. host is what people type before "/name";
// brand is the service's name, the same for everyone; internet is true on
// pages for people outside the tailnet.
func (s *Server) funcs(host string, internet bool) template.FuncMap {
	return template.FuncMap{
		"host":       func() string { return host },
		"brand":      func() string { return s.display },
		"internet":   func() bool { return internet },
		"tailnet":    func() bool { return s.tailnet },
		"ago":        ago,
		"when":       func(t time.Time) string { return t.Local().Format("2006-01-02 15:04") },
		"trunc":      trunc,
		"isTemplate": link.HasPlaceholders,
		"sameFields": func(a *store.Fields, l *store.Link) bool {
			return a != nil && l != nil && !l.Deleted() && *a == l.Fields()
		},
	}
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
	return t.Local().Format("2006-01-02")
}

// trunc shortens s to at most n runes.
func trunc(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}

// Package web is ot's HTTP layer: redirects, the server-rendered UI and the
// JSON API, behind a fixed security middleware chain.
package web

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/time/rate"

	"otterly-go/internal/app"
	"otterly-go/internal/link"
	"otterly-go/internal/store"
	"otterly-go/internal/whois"
)

// Config configures the server.
type Config struct {
	// Hosts are the accepted Host header values; the first is the display
	// name used in the UI ("ot" → "ot/wiki").
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
}

// Server serves ot over HTTP.
type Server struct {
	cfg     Config
	svc     *app.Service
	who     whois.Resolver
	log     *slog.Logger
	hosts   map[string]bool
	display string
	pages   map[string]*template.Template
	static  fs.FS
	writes  *limiterSet
	started time.Time
	handler http.Handler
}

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

var pageNames = []string{"home", "list", "form", "detail", "notfound", "activity", "error", "whoami"}

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
		pages:   make(map[string]*template.Template),
		writes:  newLimiterSet(cfg.WriteRate, cfg.WriteBurst, cfg.WriteDaily),
		started: time.Now(),
	}
	for _, h := range cfg.Hosts {
		s.hosts[normalizeHost(h)] = true
	}
	for _, name := range pageNames {
		t, err := template.New(name).Funcs(s.funcs()).ParseFS(templateFS,
			"templates/layout.html", "templates/partials.html", "templates/"+name+".html")
		if err != nil {
			return nil, fmt.Errorf("template %s: %w", name, err)
		}
		s.pages[name] = t
	}
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		return nil, err
	}
	s.static = sub
	s.handler = s.routes()
	return s, nil
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.handler.ServeHTTP(w, r) }

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", s.home)
	mux.HandleFunc("GET /{path...}", s.follow)
	mux.HandleFunc("GET /favicon.ico", s.favicon)
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

	// Cross-origin protection for every non-GET/HEAD/OPTIONS request. Over
	// plain HTTP browsers do not send Sec-Fetch-Site, so this compares the
	// Origin header with Host; hostCheck (outside) guarantees Host is ours.
	cop := http.NewCrossOriginProtection()
	cop.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.log.Warn("blocked cross-origin request", "method", r.Method, "path", r.URL.Path,
			"origin", r.Header.Get("Origin"), "remote", r.RemoteAddr)
		http.Error(w, "cross-origin request blocked", http.StatusForbidden)
	}))

	var h http.Handler = cop.Handler(mux)
	h = limits(h)
	h = s.hostCheck(h)
	h = s.sourceCheck(h)
	h = s.logRequests(h)
	h = securityHeaders(h)
	h = s.recoverer(h)
	return h
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

func (s *Server) favicon(w http.ResponseWriter, r *http.Request) {
	b, err := fs.ReadFile(s.static, "favicon.svg")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Write(b)
}

func robots(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte("User-agent: *\nDisallow: /\n"))
}

// view is the data every page template receives.
type view struct {
	Host    string // display host, e.g. "ot"
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

	Identity whois.Identity
	PeerIP   string
	WhoIsErr string
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

func (s *Server) render(w http.ResponseWriter, status int, page string, v view) {
	v.Host = s.display
	v.Version = s.cfg.Version
	var buf bytes.Buffer
	if err := s.pages[page].ExecuteTemplate(&buf, "layout", v); err != nil {
		s.log.Error("render failed", "page", page, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	w.Write(buf.Bytes())
}

func (s *Server) renderError(w http.ResponseWriter, status int, title, msg string) {
	s.render(w, status, "error", view{Title: title, Error: msg})
}

// fail maps a service/store error to a status and an error page. Internal
// errors are logged, not shown.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	status := errStatus(err)
	if status == http.StatusInternalServerError {
		s.log.Error("request failed", "path", r.URL.Path, "err", err)
		s.renderError(w, status, "Something went wrong", "The server hit an error. Try again; if it keeps happening, check the logs on the Pi.")
		return
	}
	s.renderError(w, status, http.StatusText(status), err.Error())
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

func (s *Server) funcs() template.FuncMap {
	return template.FuncMap{
		"host":       func() string { return s.display },
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

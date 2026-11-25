package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"

	"byname/internal/auth"
)

const directURL = "https://hg.example.com"

// newDirectHarness is a server with no Tailscale at all: the private listener
// only accepts localhost, and the public domain is served directly.
func newDirectHarness(t *testing.T) (*harness, *fakeGoogle) {
	t.Helper()
	g := newFakeGoogle(t)
	h := newHarness(t, func(c *Config) {
		c.Hosts = []string{"hg"}
		c.AllowedNets = []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32"), netip.MustParsePrefix("::1/128")}
		c.PublicURL = directURL
		c.Providers = []*auth.Provider{g.provider(t, "alice@gmail.com")}
	})
	return h, g
}

// direct sends a request to the public TLS listener from an internet peer.
func (h *harness) direct(method, target, body string, opts ...reqOpt) *httptest.ResponseRecorder {
	h.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, directURL+target, rd)
	r.RemoteAddr = "198.51.100.7:40000"
	r.Host = "hg.example.com"
	for _, o := range opts {
		o(r)
	}
	w := httptest.NewRecorder()
	h.srv.Public().ServeHTTP(w, r)
	return w
}

func directOrigin() reqOpt {
	return func(r *http.Request) {
		r.Header.Set("Origin", directURL)
		r.Header.Set("Sec-Fetch-Site", "same-origin")
	}
}

func TestDirectEverythingNeedsSignIn(t *testing.T) {
	h, _ := newDirectHarness(t)
	h.create("wiki", "https://wiki.example.com/")
	for name, opts := range map[string][]reqOpt{
		"plain":                     nil,
		"tailnet peer":              {peer("100.64.0.1:5000")},
		"forged forwarding headers": {header("X-Forwarded-For", "100.64.0.1"), header("Tailscale-User-Login", "alice@example.com")},
		"localhost peer":            {peer("127.0.0.1:5000")},
	} {
		w := h.direct("GET", "/wiki", "", opts...)
		if w.Code != http.StatusSeeOther || !strings.HasPrefix(w.Header().Get("Location"), "/_/signin") {
			t.Errorf("%s: %d → %q, want 303 to sign-in", name, w.Code, w.Header().Get("Location"))
		}
	}
	expectStatus(t, h.direct("GET", "/wiki", "", host("evil.example")), http.StatusMisdirectedRequest)
	expectStatus(t, h.direct("GET", "/wiki", "", host("hg")), http.StatusMisdirectedRequest)
	if w := h.direct("GET", "/_/signin", ""); w.Header().Get("Strict-Transport-Security") == "" {
		t.Error("no HSTS on the directly served domain")
	}
}

func TestDirectSignInFlow(t *testing.T) {
	h, g := newDirectHarness(t)
	h.create("wiki", "https://wiki.example.com/")
	w := h.direct("GET", "/_/signin?next=%2Fwiki", "")
	expectStatus(t, w, http.StatusSeeOther)
	w = h.direct("GET", w.Header().Get("Location"), "")
	expectStatus(t, w, http.StatusSeeOther)
	loc, _ := url.Parse(w.Header().Get("Location"))
	if loc.Query().Get("redirect_uri") != directURL+"/_/auth/google/callback" {
		t.Fatalf("redirect_uri = %q", loc.Query().Get("redirect_uri"))
	}
	g.nonce = loc.Query().Get("nonce")
	w = h.direct("GET", "/_/auth/google/callback?code=good-1&state="+url.QueryEscape(loc.Query().Get("state")), "",
		withCookie(cookieNamed(w, signinCookie)))
	expectStatus(t, w, http.StatusSeeOther)
	sess := cookieNamed(w, sessionCookie)
	if sess == nil || w.Header().Get("Location") != "/wiki" {
		t.Fatalf("callback: session %v, Location %q", sess, w.Header().Get("Location"))
	}
	expectStatus(t, h.direct("GET", "/wiki", "", withCookie(sess)), http.StatusFound)
	w = h.direct("POST", "/_/links", form("name", "docs", "target", "https://docs.example.com/"),
		withCookie(sess), formBody(), directOrigin())
	expectStatus(t, w, http.StatusSeeOther)
	if l, err := h.svc.Store.Get(t.Context(), "docs"); err != nil || l.CreatedBy != "alice@gmail.com (via Google)" {
		t.Fatalf("created by %q (%v)", l.CreatedBy, err)
	}
	home := h.direct("GET", "/", "", withCookie(sess)).Body.String()
	if !strings.Contains(home, "https://hg.example.com/name") || strings.Contains(home, "Tailscale") {
		t.Errorf("home page without Tailscale: %.1200s", home)
	}
}

func TestPrivateOrRedirect(t *testing.T) {
	h, _ := newDirectHarness(t)
	handler := h.srv.PrivateOrRedirect()
	get := func(target, remote, hostHdr string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", target, nil)
		r.RemoteAddr, r.Host = remote, hostHdr
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	for target, want := range map[string]string{
		"/wiki?x=1":                 "https://hg.example.com/wiki?x=1",
		"/":                         "https://hg.example.com/",
		"//evil.example/x":          "https://hg.example.com//evil.example/x",
		"http:evil.example":         "https://hg.example.com/",
		"http://evil.example/a?b=1": "https://hg.example.com/a?b=1",
	} {
		w := get(target, "203.0.113.5:1000", "hg.example.com")
		if w.Code != http.StatusMovedPermanently || w.Header().Get("Location") != want {
			t.Errorf("%s: %d → %q, want 301 → %q", target, w.Code, w.Header().Get("Location"), want)
		}
	}
	if w := get("/wiki", "203.0.113.5:1000", "hg.example.com"); w.Header().Get("Connection") != "close" {
		t.Error("redirect doesn't close the connection")
	}
	// Localhost gets only the health check: a local proxy or tunnel that
	// forwarded internet traffic to :80 must not expose the private handler.
	if w := get("/_/healthz", "127.0.0.1:1000", "hg"); w.Code != http.StatusOK {
		t.Errorf("localhost health check: %d", w.Code)
	}
	for _, target := range []string{"/", "/_/new", "/_/export", "/_/api/links"} {
		if w := get(target, "127.0.0.1:1000", "hg"); w.Code != http.StatusMovedPermanently {
			t.Errorf("localhost %s: %d, want a redirect", target, w.Code)
		}
	}
}

func TestPrivateOrRedirectWithLAN(t *testing.T) {
	g := newFakeGoogle(t)
	h := newHarness(t, func(c *Config) {
		c.Hosts = []string{"hg"}
		c.AllowedNets = append(c.AllowedNets, netip.MustParsePrefix("192.168.1.0/24"))
		c.PublicURL = directURL
		c.Providers = []*auth.Provider{g.provider(t, "alice@gmail.com")}
	})
	handler := h.srv.PrivateOrRedirect()
	get := func(target, remote, hostHdr string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", target, nil)
		r.RemoteAddr, r.Host = remote, hostHdr
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	// Tailnet and LAN peers using the short name get the private handler...
	for _, remote := range []string{"192.168.1.20:5000", alicePeer} {
		if w := get("/", remote, "hg"); w.Code != http.StatusOK {
			t.Errorf("%s: %d, want the private home page", remote, w.Code)
		}
	}
	// ...and are sent to https when they use the public name.
	if w := get("/wiki", "192.168.1.20:5000", "hg.example.com"); w.Code != http.StatusMovedPermanently {
		t.Errorf("LAN peer on the public name: %d, want 301", w.Code)
	}
	if !h.srv.Trusted(netip.MustParseAddr("::ffff:192.168.1.20")) || h.srv.Trusted(netip.MustParseAddr("203.0.113.5")) {
		t.Error("Trusted disagrees with the allow list")
	}
}

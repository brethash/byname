package web

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"byname/internal/app"
	"byname/internal/store"
	"byname/internal/whois"
)

const (
	alicePeer = "100.64.0.1:40000"
	bobPeer   = "100.64.0.2:40000"
)

type fakeWho map[string]whois.Identity

func (f fakeWho) WhoIs(_ context.Context, remoteAddr string) (whois.Identity, error) {
	ap, err := netip.ParseAddrPort(remoteAddr)
	if err != nil {
		return whois.Identity{}, err
	}
	if id, ok := f[ap.Addr().Unmap().String()]; ok {
		return id, nil
	}
	return whois.Identity{}, whois.ErrUnknown
}

type harness struct {
	t   *testing.T
	srv *Server
	svc *app.Service
}

func newHarness(t *testing.T, mod ...func(*Config)) *harness {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "byname.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	hosts := []string{"me", "me.example.ts.net"}
	svc := app.New(st, app.Config{SelfHosts: hosts, Logger: quiet})
	cfg := Config{
		Hosts:       hosts,
		AllowedNets: []netip.Prefix{netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("fd7a:115c:a1e0::/48")},
		WriteRate:   rate.Inf,
		WriteBurst:  1,
		Version:     "test",
		Logger:      quiet,
	}
	for _, m := range mod {
		m(&cfg)
	}
	srv, err := New(cfg, svc, fakeWho{"100.64.0.1": {Login: "alice@example.com", Node: "alice-laptop"}})
	if err != nil {
		t.Fatal(err)
	}
	return &harness{t: t, srv: srv, svc: svc}
}

type reqOpt func(*http.Request)

func peer(addr string) reqOpt   { return func(r *http.Request) { r.RemoteAddr = addr } }
func host(h string) reqOpt      { return func(r *http.Request) { r.Host = h } }
func header(k, v string) reqOpt { return func(r *http.Request) { r.Header.Set(k, v) } }
func formBody() reqOpt          { return header("Content-Type", "application/x-www-form-urlencoded") }
func jsonBody() reqOpt          { return header("Content-Type", "application/json") }
func sameOrigin() reqOpt        { return header("Origin", "http://me") }

func (h *harness) do(method, target string, body string, opts ...reqOpt) *httptest.ResponseRecorder {
	h.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, "http://me"+target, rd)
	r.RemoteAddr = alicePeer
	r.Host = "me"
	for _, o := range opts {
		o(r)
	}
	w := httptest.NewRecorder()
	h.srv.ServeHTTP(w, r)
	return w
}

func (h *harness) create(name, target string) store.Link {
	h.t.Helper()
	l, err := h.svc.Create(context.Background(), app.Input{Name: name, Target: target}, store.Actor{IP: "100.64.0.1"})
	if err != nil {
		h.t.Fatal(err)
	}
	return l
}

func form(kv ...string) string {
	v := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		v.Set(kv[i], kv[i+1])
	}
	return v.Encode()
}

func expectStatus(t *testing.T, w *httptest.ResponseRecorder, want int) {
	t.Helper()
	if w.Code != want {
		t.Fatalf("status = %d, want %d; body: %.300s", w.Code, want, w.Body.String())
	}
}

func TestRedirects(t *testing.T) {
	h := newHarness(t)
	h.create("wiki", "https://wiki.example.com/start")
	h.create("gh", "https://github.com/{1}")
	cases := map[string]string{
		"/wiki":            "https://wiki.example.com/start",
		"/WIKI":            "https://wiki.example.com/start",
		"/w-i-k-i":         "https://wiki.example.com/start",
		"/wiki/":           "https://wiki.example.com/start",
		"/wiki/a/b%20c":    "https://wiki.example.com/start/a/b%20c",
		"/wiki?x=1":        "https://wiki.example.com/start?x=1",
		"/gh/octocat":      "https://github.com/octocat",
		"/gh/octocat/more": "https://github.com/octocat",
		// The extra path is decoded once, never twice.
		"/wiki/100%25":        "https://wiki.example.com/start/100%25",
		"/wiki/50%25off/page": "https://wiki.example.com/start/50%25off/page",
		"/wiki/a%2520b":       "https://wiki.example.com/start/a%2520b",
		"/wiki/a%2Fb":         "https://wiki.example.com/start/a%2Fb",
		"/gh/100%25":          "https://github.com/100%25",
	}
	for path, want := range cases {
		w := h.do("GET", path, "")
		// Always 302 (never a cacheable 301), with no-store.
		expectStatus(t, w, http.StatusFound)
		if got := w.Header().Get("Location"); got != want {
			t.Errorf("%s → %q, want %q", path, got, want)
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s: Cache-Control = %q", path, w.Header().Get("Cache-Control"))
		}
	}
}

func TestClickCounting(t *testing.T) {
	h := newHarness(t)
	l := h.create("wiki", "https://wiki.example.com")
	h.do("GET", "/wiki", "")
	h.do("GET", "/wiki", "")
	h.do("HEAD", "/wiki", "")
	h.svc.FlushClicks(context.Background())
	got, _ := h.svc.Store.GetByID(context.Background(), l.ID)
	if got.Clicks != 2 {
		t.Fatalf("clicks = %d, want 2 (HEAD not counted)", got.Clicks)
	}
}

func TestPreviewAndNotFound(t *testing.T) {
	h := newHarness(t)
	h.create("wiki", "https://wiki.example.com")
	h.create("wikipedia", "https://en.wikipedia.org")

	w := h.do("GET", "/wiki+", "")
	expectStatus(t, w, http.StatusOK)
	if !strings.Contains(w.Body.String(), "https://wiki.example.com") {
		t.Fatal("preview does not show destination")
	}

	w = h.do("GET", "/nope", "")
	expectStatus(t, w, http.StatusNotFound)
	if !strings.Contains(w.Body.String(), `name="name" value="nope"`) {
		t.Fatal("not-found page does not offer to create the link")
	}

	w = h.do("GET", "/wik", "")
	expectStatus(t, w, http.StatusNotFound)
	if !strings.Contains(w.Body.String(), "Did you mean") || !strings.Contains(w.Body.String(), "me/wikipedia") {
		t.Fatal("not-found page has no suggestions")
	}

	w = h.do("GET", "/_secret", "")
	expectStatus(t, w, http.StatusNotFound)
}

func TestOnlyTailnetPeers(t *testing.T) {
	h := newHarness(t)
	for addr, want := range map[string]int{
		"192.168.1.20:5000":          http.StatusForbidden, // LAN
		"203.0.113.9:5000":           http.StatusForbidden, // internet
		"127.0.0.1:5000":             http.StatusForbidden, // not configured
		"100.64.0.1:5000":            http.StatusOK,
		"[fd7a:115c:a1e0::1]:5000":   http.StatusOK,
		"[::ffff:100.100.1.1]:5000":  http.StatusOK, // 4-in-6 mapped tailnet
		"[::ffff:192.168.1.20]:5000": http.StatusForbidden,
		"garbage":                    http.StatusForbidden,
	} {
		w := h.do("GET", "/", "", peer(addr))
		if w.Code != want {
			t.Errorf("peer %s: status %d, want %d", addr, w.Code, want)
		}
		if want == http.StatusForbidden && !strings.Contains(w.Body.String(), "me is not reachable") {
			t.Errorf("peer %s: body %q does not name the service", addr, w.Body.String())
		}
	}
}

// The service name is whatever the first host is; nothing in the UI is
// hard-wired to "me".
func TestCustomServiceName(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Hosts = []string{"go", "go.example.ts.net"} })
	h.create("wiki", "https://wiki.example")
	for path, want := range map[string]string{
		"/":                 "go/",
		"/wiki+":            "go/wiki",
		"/_/opensearch.xml": "http://go/_/go?q={searchTerms}",
		"/_/favicon.svg":    ">go/</text>",
	} {
		w := h.do("GET", path, "", host("go"))
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), want) {
			t.Errorf("GET %s: %d, body lacks %q", path, w.Code, want)
		}
		if path != "/_/favicon.svg" && regexp.MustCompile(`\bme/`).MatchString(w.Body.String()) {
			t.Errorf("GET %s still says me/", path)
		}
	}
	expectStatus(t, h.do("GET", "/", "", host("me")), http.StatusMisdirectedRequest)
	if w := h.do("GET", "/", "", peer("203.0.113.9:5000"), host("go")); !strings.Contains(w.Body.String(), "go is not reachable") {
		t.Errorf("403 body = %q", w.Body.String())
	}
}

func TestFaviconLabel(t *testing.T) {
	for display, want := range map[string]string{
		"me":     ">me/<",
		"go.lan": ">go/<",
		"web":    ">web/<",
		"links":  ">l/<",
		"a&b":    ">a&amp;b/<",
	} {
		if got := string(faviconSVG(display)); !strings.Contains(got, want) {
			t.Errorf("faviconSVG(%q) = %s, want it to contain %s", display, got, want)
		}
	}
}

func TestLANAccessWhenConfigured(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.AllowedNets = append(c.AllowedNets, netip.MustParsePrefix("192.168.1.0/24"))
	})
	lan := peer("192.168.1.20:5000")
	expectStatus(t, h.do("GET", "/", "", lan), http.StatusOK)
	expectStatus(t, h.do("GET", "/", "", peer("10.0.0.5:5000")), http.StatusForbidden)

	// LAN devices can edit (same-origin), attributed by IP; WhoIs isn't asked.
	w := h.do("POST", "/_/links", form("name", "lanlink", "target", "https://lan.example"), formBody(), sameOrigin(), lan)
	expectStatus(t, w, http.StatusSeeOther)
	l, err := h.svc.Store.Get(context.Background(), "lanlink")
	if err != nil || l.CreatedBy != "192.168.1.20" {
		t.Fatalf("LAN create = %+v, %v", l, err)
	}
	// Cross-origin protection still applies on the LAN.
	w = h.do("POST", "/_/links", form("name", "x", "target", "https://x.example"), formBody(), header("Origin", "http://evil.example"), lan)
	expectStatus(t, w, http.StatusForbidden)

	body := h.do("GET", "/_/whoami", "", lan).Body.String()
	if !strings.Contains(body, "home network (no Tailscale)") || strings.Contains(body, "no tailnet peer") {
		t.Fatalf("whoami for LAN device:\n%s", body)
	}
}

func TestHostAllowlist(t *testing.T) {
	h := newHarness(t)
	for hst, want := range map[string]int{
		"me":                 http.StatusOK,
		"ME":                 http.StatusOK,
		"me:80":              http.StatusOK,
		"me.":                http.StatusOK,
		"me.example.ts.net":  http.StatusOK,
		"me.example.ts.net.": http.StatusOK,
		"evil.example":       http.StatusMisdirectedRequest, // DNS rebinding
		"100.64.0.5":         http.StatusMisdirectedRequest,
		"me:8080":            http.StatusMisdirectedRequest,
		"":                   http.StatusMisdirectedRequest,
	} {
		if w := h.do("GET", "/", "", host(hst)); w.Code != want {
			t.Errorf("Host %q: status %d, want %d", hst, w.Code, want)
		}
	}
}

func TestCrossOriginWritesBlocked(t *testing.T) {
	h := newHarness(t)
	body := form("name", "bank", "target", "https://evil.example/phish")
	attacks := []reqOpt{
		header("Origin", "http://evil.example"),
		header("Origin", "null"),
		header("Origin", "http://me.evil.example"),
		header("Sec-Fetch-Site", "cross-site"),
	}
	for i, a := range attacks {
		w := h.do("POST", "/_/links", body, formBody(), a)
		expectStatus(t, w, http.StatusForbidden)
		if _, err := h.svc.Store.Get(context.Background(), "bank"); err == nil {
			t.Fatalf("attack %d created a link", i)
		}
	}
	// Same-origin form post works...
	w := h.do("POST", "/_/links", body, formBody(), sameOrigin())
	expectStatus(t, w, http.StatusSeeOther)
	// ...and so does a non-browser client (no Origin, no Sec-Fetch-Site).
	w = h.do("POST", "/_/links", form("name", "cli", "target", "https://x.example"), formBody())
	expectStatus(t, w, http.StatusSeeOther)
}

func TestFormCreateEditDeleteRestoreRevert(t *testing.T) {
	h := newHarness(t)
	w := h.do("POST", "/_/links", form("name", "Docs", "target", "https://v1.example.com", "description", "the docs"), formBody(), sameOrigin())
	expectStatus(t, w, http.StatusSeeOther)
	if loc := w.Header().Get("Location"); loc != "/Docs+" {
		t.Fatalf("after create → %q", loc)
	}
	l, err := h.svc.Store.Get(context.Background(), "docs")
	if err != nil {
		t.Fatal(err)
	}
	if l.CreatedBy != "alice@example.com (alice-laptop)" {
		t.Fatalf("created_by = %q (WhoIs attribution)", l.CreatedBy)
	}
	id := strconv.FormatInt(l.ID, 10)

	// Edit form renders with rev.
	w = h.do("GET", "/_/links/"+id+"/edit", "")
	expectStatus(t, w, http.StatusOK)
	if !strings.Contains(w.Body.String(), `name="rev" value="1"`) {
		t.Fatal("edit form missing rev")
	}

	// Update from an unknown-identity peer: attributed by IP.
	w = h.do("POST", "/_/links/"+id, form("rev", "1", "name", "docs", "target", "https://v2.example.com"), formBody(), sameOrigin(), peer(bobPeer))
	expectStatus(t, w, http.StatusSeeOther)
	l, _ = h.svc.Store.GetByID(context.Background(), l.ID)
	if l.Target != "https://v2.example.com" || l.UpdatedBy != "100.64.0.2" {
		t.Fatalf("after update: %+v", l)
	}

	// Stale rev → 409 and nothing changes.
	w = h.do("POST", "/_/links/"+id, form("rev", "1", "name", "docs", "target", "https://evil.example"), formBody(), sameOrigin())
	expectStatus(t, w, http.StatusConflict)

	// Delete, then the link 404s, then restore.
	expectStatus(t, h.do("POST", "/_/links/"+id+"/delete", "", sameOrigin()), http.StatusSeeOther)
	expectStatus(t, h.do("GET", "/docs", ""), http.StatusNotFound)
	expectStatus(t, h.do("POST", "/_/links/"+id+"/restore", "", sameOrigin()), http.StatusSeeOther)
	expectStatus(t, h.do("GET", "/docs", ""), http.StatusFound)

	// Revert to the first version.
	hist, _ := h.svc.Store.History(context.Background(), l.ID)
	first := hist[len(hist)-1]
	w = h.do("POST", "/_/events/"+strconv.FormatInt(first.ID, 10)+"/revert", "", sameOrigin())
	expectStatus(t, w, http.StatusSeeOther)
	if loc := h.do("GET", "/docs", "").Header().Get("Location"); loc != "https://v1.example.com" {
		t.Fatalf("after revert → %q", loc)
	}

	// The history page shows who did what.
	body := h.do("GET", "/_/links/"+id, "").Body.String()
	for _, want := range []string{"alice@example.com", "100.64.0.2", "revert", "restore", "delete"} {
		if !strings.Contains(body, want) {
			t.Errorf("history page missing %q", want)
		}
	}
}

func TestFormValidationErrors(t *testing.T) {
	h := newHarness(t)
	for _, c := range []struct{ name, target, want string }{
		{"x", "javascript:alert(1)", "must start with http"},
		{"x", "http://me/x", "points back"},
		{"bad name", "https://x.example", "must start with a letter"},
		{"x", "https://bank.example@evil.example", "user name or password"},
	} {
		w := h.do("POST", "/_/links", form("name", c.name, "target", c.target), formBody(), sameOrigin())
		expectStatus(t, w, http.StatusUnprocessableEntity)
		if !strings.Contains(w.Body.String(), c.want) {
			t.Errorf("%q/%q: body lacks %q", c.name, c.target, c.want)
		}
	}
	h.create("taken", "https://x.example")
	w := h.do("POST", "/_/links", form("name", "TAKEN", "target", "https://y.example"), formBody(), sameOrigin())
	expectStatus(t, w, http.StatusConflict)
}

func TestGETNeverMutates(t *testing.T) {
	h := newHarness(t)
	l := h.create("keep", "https://keep.example")
	id := strconv.FormatInt(l.ID, 10)
	for _, p := range []string{"/_/links/" + id + "/delete", "/_/links/" + id + "/restore", "/_/events/1/revert"} {
		w := h.do("GET", p, "")
		if w.Code == http.StatusSeeOther || w.Code == http.StatusOK {
			t.Errorf("GET %s → %d", p, w.Code)
		}
	}
	if _, err := h.svc.Store.Get(context.Background(), "keep"); err != nil {
		t.Fatal("GET deleted the link")
	}
}

func TestStoredXSSIsEscaped(t *testing.T) {
	h := newHarness(t)
	_, err := h.svc.Create(context.Background(), app.Input{
		Name: "x", Target: "https://x.example/?q=\"><script>alert(1)</script>",
		Description: `<img src=x onerror=alert(1)>`,
	}, store.Actor{IP: "100.64.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/x+", "/_/links", "/"} {
		body := h.do("GET", p, "").Body.String()
		if strings.Contains(body, "<script>alert") || strings.Contains(body, "<img src=x") {
			t.Fatalf("%s renders unescaped markup", p)
		}
	}
}

func TestSecurityHeadersEverywhere(t *testing.T) {
	h := newHarness(t)
	for _, w := range []*httptest.ResponseRecorder{
		h.do("GET", "/", ""),
		h.do("GET", "/missing", ""),
		h.do("GET", "/", "", host("evil.example")),
		h.do("GET", "/", "", peer("192.168.1.2:1")),
		h.do("POST", "/_/links", "", header("Origin", "http://evil.example")),
	} {
		if !strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") ||
			w.Header().Get("X-Content-Type-Options") != "nosniff" ||
			w.Header().Get("Referrer-Policy") != "same-origin" {
			t.Fatalf("status %d response missing security headers: %v", w.Code, w.Header())
		}
	}
}

func TestLimits(t *testing.T) {
	h := newHarness(t)
	w := h.do("GET", "/"+strings.Repeat("a", maxRequestURI), "")
	expectStatus(t, w, http.StatusRequestURITooLong)
	w = h.do("POST", "/_/links", form("name", "big", "target", "https://x.example/"+strings.Repeat("a", maxBodyBytes)), formBody(), sameOrigin())
	expectStatus(t, w, http.StatusRequestEntityTooLarge)
}

func TestWriteRateLimit(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.WriteRate = rate.Every(time.Hour); c.WriteBurst = 2 })
	codes := []int{}
	for i := range 3 {
		w := h.do("POST", "/_/links", form("name", "n"+strconv.Itoa(i), "target", "https://x.example"), formBody(), sameOrigin())
		codes = append(codes, w.Code)
	}
	if codes[0] != http.StatusSeeOther || codes[1] != http.StatusSeeOther || codes[2] != http.StatusTooManyRequests {
		t.Fatalf("codes = %v", codes)
	}
	// A different device has its own bucket.
	w := h.do("POST", "/_/links", form("name", "other", "target", "https://x.example"), formBody(), sameOrigin(), peer(bobPeer))
	expectStatus(t, w, http.StatusSeeOther)
}

func TestDevicesWithoutTailscaleShareABudget(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.WriteRate = rate.Inf
		c.WriteDaily = 2
		c.AllowedNets = append(c.AllowedNets, netip.MustParsePrefix("192.168.0.0/16"))
	})
	post := func(name, from string) int {
		return h.do("POST", "/_/links", form("name", name, "target", "https://x.example"),
			formBody(), sameOrigin(), peer(from)).Code
	}
	// A LAN device hopping addresses gets no more than the shared budget...
	if post("a", "192.168.1.20:5000") != http.StatusSeeOther || post("b", "192.168.7.31:5000") != http.StatusSeeOther {
		t.Fatal("first two LAN changes refused")
	}
	if code := post("c", "192.168.200.9:5000"); code != http.StatusTooManyRequests {
		t.Fatalf("third LAN change from a new address: %d, want 429", code)
	}
	// ...and doesn't touch tailnet devices' budgets.
	if post("d", alicePeer) != http.StatusSeeOther {
		t.Fatal("tailnet device refused after LAN budget ran out")
	}
	if n := len(h.srv.writes.m); n != 2 {
		t.Fatalf("%d limiter entries, want 2 (alice's device and the shared LAN key)", n)
	}
}

func TestDailyWriteBudget(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.WriteRate = rate.Inf; c.WriteDaily = 2 })
	codes := []int{}
	for i := range 3 {
		w := h.do("POST", "/_/links", form("name", "d"+strconv.Itoa(i), "target", "https://x.example"), formBody(), sameOrigin())
		codes = append(codes, w.Code)
	}
	if codes[0] != http.StatusSeeOther || codes[1] != http.StatusSeeOther || codes[2] != http.StatusTooManyRequests {
		t.Fatalf("codes = %v", codes)
	}
}

func TestShortLimitRefundsDailyBudget(t *testing.T) {
	ls := newLimiterSet(rate.Every(time.Hour), 1, 2)
	if !ls.allow("k") { // uses 1 short + 1 daily
		t.Fatal("first write refused")
	}
	if ls.allow("k") { // short bucket empty: refused, daily refunded
		t.Fatal("second write allowed despite short limit")
	}
	e := ls.m["k"].Value.(*limiterEntry)
	if got := e.day.Tokens(); got < 0.99 {
		t.Fatalf("daily budget not refunded: %.2f tokens left", got)
	}
}

func TestAPI(t *testing.T) {
	h := newHarness(t)
	// Must be JSON (forces a CORS preflight for browsers).
	w := h.do("POST", "/_/api/links", `{"name":"a","target":"https://a.example"}`, header("Content-Type", "text/plain"))
	expectStatus(t, w, http.StatusUnsupportedMediaType)

	w = h.do("POST", "/_/api/links", `{"name":"a","target":"https://a.example","description":"A"}`, jsonBody())
	expectStatus(t, w, http.StatusCreated)
	var got apiLink
	json.Unmarshal(w.Body.Bytes(), &got)
	if got.Name != "a" || got.Rev != 1 || got.URL != "http://me/a" || got.CreatedBy != "alice@example.com (alice-laptop)" {
		t.Fatalf("created = %+v", got)
	}

	w = h.do("POST", "/_/api/links", `{"name":"b","target":"javascript:x"}`, jsonBody())
	expectStatus(t, w, http.StatusUnprocessableEntity)
	if !strings.Contains(w.Body.String(), `"field": "target"`) {
		t.Fatalf("validation error body = %s", w.Body.String())
	}
	w = h.do("POST", "/_/api/links", `{"name":"b","target":"https://b.example","admin":true}`, jsonBody())
	expectStatus(t, w, http.StatusBadRequest) // unknown fields rejected

	expectStatus(t, h.do("PUT", "/_/api/links/a", `{"target":"https://a2.example"}`, jsonBody()), http.StatusBadRequest)
	expectStatus(t, h.do("PUT", "/_/api/links/a", `{"target":"https://a2.example","rev":1}`, jsonBody()), http.StatusOK)
	expectStatus(t, h.do("PUT", "/_/api/links/a", `{"target":"https://a3.example","rev":1}`, jsonBody()), http.StatusConflict)

	w = h.do("GET", "/_/api/links/A", "")
	expectStatus(t, w, http.StatusOK)
	json.Unmarshal(w.Body.Bytes(), &got)
	// Omitted fields (description) keep their values on update.
	if got.Target != "https://a2.example" || got.Rev != 2 || got.Description != "A" {
		t.Fatalf("get = %+v", got)
	}

	w = h.do("GET", "/_/api/links?q=a2", "")
	var list []apiLink
	json.Unmarshal(w.Body.Bytes(), &list)
	if len(list) != 1 {
		t.Fatalf("list = %+v", list)
	}

	expectStatus(t, h.do("DELETE", "/_/api/links/a", ""), http.StatusNoContent)
	expectStatus(t, h.do("GET", "/_/api/links/a", ""), http.StatusNotFound)

	// Cross-origin API write is blocked even with the right content type.
	w = h.do("POST", "/_/api/links", `{"name":"c","target":"https://c.example"}`, jsonBody(), header("Origin", "http://evil.example"))
	expectStatus(t, w, http.StatusForbidden)
	// And the server never grants CORS.
	if w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("CORS header present")
	}
}

func TestKeywordSearch(t *testing.T) {
	h := newHarness(t)
	h.create("gh", "https://github.com/{1}/{2}")
	w := h.do("GET", "/_/go?q="+url.QueryEscape("gh octocat go"), "")
	expectStatus(t, w, http.StatusFound)
	if loc := w.Header().Get("Location"); loc != "https://github.com/octocat/go" {
		t.Fatalf("keyword → %q", loc)
	}
	w = h.do("GET", "/_/go?q="+url.QueryEscape("me/gh/a b"), "")
	if loc := w.Header().Get("Location"); loc != "https://github.com/a/b" {
		t.Fatalf("keyword with prefix → %q", loc)
	}
	w = h.do("GET", "/_/go?q="+url.QueryEscape("what is this"), "")
	if loc := w.Header().Get("Location"); loc != "/_/links?q=what+is+this" {
		t.Fatalf("keyword fallback → %q", loc)
	}
}

func TestHealthAndPower(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "get_throttled")
	os.WriteFile(path, []byte("10005\n"), 0o644)
	h := newHarness(t, func(c *Config) { c.ThrottledPath = path; c.DataDir = dir })
	h.create("a", "https://a.example")
	w := h.do("GET", "/_/healthz", "")
	expectStatus(t, w, http.StatusOK)
	var rep healthReport
	if err := json.Unmarshal(w.Body.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Status != "ok" || rep.Links != 1 || rep.Schema != 2 || rep.Version != "test" || rep.DiskFreeMB == nil || *rep.DiskFreeMB <= 0 {
		t.Fatalf("health = %+v", rep)
	}
	p := rep.Power
	if p == nil || !p.UnderVoltageNow || !p.ThrottledNow || !p.UnderVoltageSinceBoot || p.ThrottledSinceBoot || p.Raw != "0x10005" {
		t.Fatalf("power = %+v", p)
	}
}

func TestMiscEndpoints(t *testing.T) {
	h := newHarness(t)
	h.create("a", "https://a.example")
	for path, want := range map[string]string{
		"/_/static/style.css":   "--accent",
		"/_/static/app.js":      "data-confirm",
		"/favicon.ico":          ">me/</text>",
		"/_/favicon.svg":        ">me/</text>",
		"/robots.txt":           "Disallow: /",
		"/_/opensearch.xml":     "http://me/_/go?q={searchTerms}",
		"/_/export":             `"format": "byname-links"`,
		"/_/whoami":             "alice@example.com",
		"/_/activity":           "create",
		"/_/links?sort=popular": "me/a",
		"/_/new?name=fresh":     `value="fresh"`,
	} {
		w := h.do("GET", path, "")
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), want) {
			t.Errorf("GET %s: %d, body lacks %q", path, w.Code, want)
		}
	}
	expectStatus(t, h.do("GET", "/_/static/", ""), http.StatusNotFound) // no listings
}

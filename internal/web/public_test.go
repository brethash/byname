package web

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"byname/internal/auth"
)

const publicURL = "https://me.example.ts.net"

// fakeGoogle is a Google token endpoint that returns an ID token for whoever
// the test says signed in, echoing the nonce from the sign-in request.
type fakeGoogle struct {
	*httptest.Server
	email, hd string
	nonce     string // set by the test from the auth URL
	calls     int
	used      map[string]bool
}

func newFakeGoogle(t *testing.T) *fakeGoogle {
	f := &fakeGoogle{email: "alice@gmail.com", used: map[string]bool{}}
	f.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls++
		r.ParseForm()
		code := r.PostForm.Get("code")
		if !strings.HasPrefix(code, "good-") || f.used[code] { // codes work once, as at Google
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
			return
		}
		f.used[code] = true
		now := time.Now().Unix()
		claims := map[string]any{"iss": "https://accounts.google.com", "aud": "gid", "sub": "sub-" + f.email,
			"exp": now + 600, "iat": now, "nonce": f.nonce, "email": f.email, "email_verified": true, "name": "Alice"}
		if f.hd != "" {
			claims["hd"] = f.hd
		}
		b, _ := json.Marshal(claims)
		tok := "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(b) + ".c2ln"
		json.NewEncoder(w).Encode(map[string]string{"id_token": tok})
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeGoogle) provider(t *testing.T, allow ...string) *auth.Provider {
	t.Helper()
	p, err := auth.Google(auth.GoogleConfig{ClientID: "gid", ClientSecret: "gsecret", Allow: allow},
		auth.WithEndpoints("https://accounts.test/auth", f.URL), auth.WithHTTPClient(f.Client()))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func newPublicHarness(t *testing.T, allow ...string) (*harness, *fakeGoogle) {
	t.Helper()
	g := newFakeGoogle(t)
	if len(allow) == 0 {
		allow = []string{"alice@gmail.com"}
	}
	h := newHarness(t, func(c *Config) {
		c.PublicURL = publicURL
		c.PublicBehindTailscale = true
		c.Providers = []*auth.Provider{g.provider(t, allow...)}
	})
	return h, g
}

// Request options for the public listener: what tailscaled sends.
func funnel(ip string) reqOpt {
	return func(r *http.Request) {
		r.Header.Set("X-Forwarded-For", ip)
		r.Header.Set("Tailscale-Funnel-Request", "?1")
	}
}
func serveFrom(ip string) reqOpt { return header("X-Forwarded-For", ip) }
func withCookie(c *http.Cookie) reqOpt {
	return func(r *http.Request) { r.AddCookie(c) }
}
func publicOrigin() reqOpt {
	return func(r *http.Request) {
		r.Header.Set("Origin", publicURL)
		r.Header.Set("Sec-Fetch-Site", "same-origin")
	}
}

func (h *harness) pub(method, target, body string, opts ...reqOpt) *httptest.ResponseRecorder {
	h.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, "http://localhost"+target, rd)
	r.RemoteAddr = "@" // a unix-socket peer
	r.Host = "localhost"
	r.Header.Set("X-Forwarded-Host", "me.example.ts.net")
	r.Header.Set("X-Forwarded-Proto", "https")
	for _, o := range opts {
		o(r)
	}
	w := httptest.NewRecorder()
	h.srv.Public().ServeHTTP(w, r)
	return w
}

func cookieNamed(w *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range w.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// signIn runs the whole Google sign-in from the internet and returns the
// session cookie (nil if none was set) and the callback's response.
func signIn(t *testing.T, h *harness, g *fakeGoogle, ip, next string) (*http.Cookie, *httptest.ResponseRecorder) {
	t.Helper()
	w := h.pub("GET", "/_/auth/google/start?next="+url.QueryEscape(next), "", funnel(ip))
	expectStatus(t, w, http.StatusSeeOther)
	loc, err := url.Parse(w.Header().Get("Location"))
	if err != nil || loc.Host != "accounts.test" {
		t.Fatalf("start redirected to %q", w.Header().Get("Location"))
	}
	q := loc.Query()
	if q.Get("redirect_uri") != publicURL+"/_/auth/google/callback" {
		t.Fatalf("redirect_uri = %q", q.Get("redirect_uri"))
	}
	state := cookieNamed(w, signinCookie)
	if state == nil || state.Value == "" || !state.Secure || !state.HttpOnly || state.SameSite != http.SameSiteLaxMode || state.Path != "/" {
		t.Fatalf("sign-in cookie = %+v", state)
	}
	if strings.Contains(state.Value, q.Get("nonce")) {
		t.Fatal("sign-in cookie isn't sealed")
	}
	g.nonce = q.Get("nonce")
	w = h.pub("GET", "/_/auth/google/callback?code=good-"+auth.Token()+"&state="+url.QueryEscape(q.Get("state")), "",
		funnel(ip), withCookie(state))
	return cookieNamed(w, sessionCookie), w
}

// startSignIn starts a Google sign-in from the internet and returns the
// browser's sign-in cookie and the query sent to Google.
func startSignIn(t *testing.T, h *harness, _ *fakeGoogle) (*http.Cookie, url.Values) {
	t.Helper()
	w := h.pub("GET", "/_/auth/google/start", "", funnel("203.0.113.9"))
	expectStatus(t, w, http.StatusSeeOther)
	loc, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	c := cookieNamed(w, signinCookie)
	if c == nil {
		t.Fatal("no sign-in cookie")
	}
	return c, loc.Query()
}

func TestPublicConfig(t *testing.T) {
	g := newFakeGoogle(t)
	p := g.provider(t, "alice@gmail.com")
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := newHarness(t)
	for name, c := range map[string]Config{
		"providers without URL": {Providers: []*auth.Provider{p}},
		"URL without providers": {PublicURL: publicURL},
		"plain http":            {PublicURL: "http://me.example.ts.net", Providers: []*auth.Provider{p}},
		"with a path":           {PublicURL: publicURL + "/x", Providers: []*auth.Provider{p}},
		"provider twice":        {PublicURL: publicURL, Providers: []*auth.Provider{p, p}},
	} {
		c.Hosts, c.AllowedNets, c.Logger = h.srv.cfg.Hosts, h.srv.cfg.AllowedNets, quiet
		if _, err := New(c, h.svc, nil); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if h.srv.Public() != nil {
		t.Error("public handler exists without a public URL")
	}
}

func TestPublicTailnetSkipsSignIn(t *testing.T) {
	h, _ := newPublicHarness(t)
	h.create("wiki", "https://wiki.example.com/")
	// A tailnet device reaching the https name through Tailscale Serve.
	w := h.pub("GET", "/wiki", "", serveFrom("100.64.0.1"))
	expectStatus(t, w, http.StatusFound)
	if w.Header().Get("Strict-Transport-Security") == "" {
		t.Error("no HSTS header on the public address")
	}
	w = h.pub("POST", "/_/links", form("name", "docs", "target", "https://docs.example.com/"),
		serveFrom("100.64.0.1"), formBody(), publicOrigin())
	expectStatus(t, w, http.StatusSeeOther)
	l, err := h.svc.Store.Get(t.Context(), "docs")
	if err != nil || l.CreatedBy != "alice@example.com (alice-laptop)" {
		t.Fatalf("created by %q (%v), want the Tailscale identity", l.CreatedBy, err)
	}
	w = h.pub("GET", "/_/api/links/docs", "", serveFrom("100.64.0.1"))
	expectStatus(t, w, http.StatusOK)
	if !strings.Contains(w.Body.String(), `"url": "`+publicURL+`/docs"`) {
		t.Errorf("API URL should use the public address: %s", w.Body)
	}
	w = h.pub("GET", "/_/signin?next=/wiki", "", serveFrom("100.64.0.1"))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/wiki" {
		t.Errorf("sign-in page for a tailnet device: %d → %q", w.Code, w.Header().Get("Location"))
	}
}

func TestPublicInternetNeedsSignIn(t *testing.T) {
	h, _ := newPublicHarness(t)
	h.create("wiki", "https://wiki.example.com/")
	cases := []struct {
		name   string
		method string
		path   string
		opts   []reqOpt
		status int
	}{
		{"follow", "GET", "/wiki?x=1", []reqOpt{funnel("203.0.113.9")}, http.StatusSeeOther},
		{"home", "GET", "/", []reqOpt{funnel("203.0.113.9")}, http.StatusSeeOther},
		{"api read", "GET", "/_/api/links", []reqOpt{funnel("203.0.113.9")}, http.StatusUnauthorized},
		{"form post", "POST", "/_/links", []reqOpt{funnel("203.0.113.9"), formBody(), publicOrigin()}, http.StatusUnauthorized},
		{"export", "GET", "/_/export", []reqOpt{funnel("203.0.113.9")}, http.StatusSeeOther},
		{"health", "GET", "/_/healthz", []reqOpt{funnel("203.0.113.9")}, http.StatusSeeOther},
		// Funnel's marker wins even if the address looks like the tailnet.
		{"funnel from 100.x", "GET", "/wiki", []reqOpt{funnel("100.64.0.1")}, http.StatusSeeOther},
		// No forwarding headers: treated as the internet, never as the tailnet.
		{"no X-Forwarded-For", "GET", "/wiki", nil, http.StatusSeeOther},
		{"garbage X-Forwarded-For", "GET", "/wiki", []reqOpt{serveFrom("100.64.0.1, 203.0.113.9")}, http.StatusSeeOther},
		{"sign-in page, one provider", "GET", "/_/signin", []reqOpt{funnel("203.0.113.9")}, http.StatusSeeOther},
		{"stylesheet", "GET", "/_/static/style.css", []reqOpt{funnel("203.0.113.9")}, http.StatusOK},
		{"favicon", "GET", "/favicon.ico", []reqOpt{funnel("203.0.113.9")}, http.StatusOK},
		{"other host", "GET", "/wiki", []reqOpt{funnel("203.0.113.9"), header("X-Forwarded-Host", "evil.example")}, http.StatusMisdirectedRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := h.pub(tc.method, tc.path, "", tc.opts...)
			expectStatus(t, w, tc.status)
		})
	}
	w := h.pub("GET", "/wiki?x=1", "", funnel("203.0.113.9"))
	if loc := w.Header().Get("Location"); loc != "/_/signin?next=%2Fwiki%3Fx%3D1" {
		t.Errorf("Location = %q", loc)
	}
}

func TestSignInOneProviderGoesStraightThere(t *testing.T) {
	h, _ := newPublicHarness(t)
	w := h.pub("GET", "/_/signin?next=%2Fwiki%3Fx%3D1", "", funnel("203.0.113.9"))
	expectStatus(t, w, http.StatusSeeOther)
	if loc := w.Header().Get("Location"); loc != "/_/auth/google/start?next=%2Fwiki%3Fx%3D1" {
		t.Errorf("Location = %q", loc)
	}
	w = h.pub("GET", "/_/signin?next=%2F%2Fevil.example", "", funnel("203.0.113.9"))
	if loc := w.Header().Get("Location"); loc != "/_/auth/google/start?next=%2F" {
		t.Errorf("unsafe next passed along: %q", loc)
	}
	// After signing out, stay on the page: going back to Google would sign
	// the person straight back in.
	w = h.pub("GET", "/_/signin?out=1", "", funnel("203.0.113.9"))
	expectStatus(t, w, http.StatusOK)
	if body := w.Body.String(); !strings.Contains(body, "You&#39;re signed out.") || !strings.Contains(body, "Continue with Google") {
		t.Errorf("signed-out page: %.600s", body)
	}
}

func TestSignInChooser(t *testing.T) {
	g := newFakeGoogle(t)
	slack, err := auth.Slack(auth.SlackConfig{ClientID: "sid", ClientSecret: "s", Teams: []string{"T0123ABCD"}},
		auth.WithEndpoints("https://slack.test/auth", "https://slack.test/token"))
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, func(c *Config) {
		c.PublicURL = publicURL
		c.PublicBehindTailscale = true
		c.Providers = []*auth.Provider{g.provider(t, "alice@gmail.com"), slack}
	})
	w := h.pub("GET", "/_/signin?next=%2Fwiki", "", funnel("203.0.113.9"))
	expectStatus(t, w, http.StatusOK)
	body := w.Body.String()
	for _, want := range []string{
		`href="/_/auth/google/start?next=%2fwiki"`, "Continue with Google",
		`href="/_/auth/slack/start?next=%2fwiki"`, "Continue with Slack",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("chooser lacks %q: %.800s", want, body)
		}
	}
	if strings.Contains(body, `href="/_/new"`) {
		t.Error("sign-in page shows the navigation")
	}
}

func TestInternetPagesShowThePublicAddress(t *testing.T) {
	h, g := newPublicHarness(t)
	h.create("wiki", "https://wiki.example.com/")
	sess, _ := signIn(t, h, g, "203.0.113.9", "/")
	home := h.pub("GET", "/", "", funnel("203.0.113.9"), withCookie(sess)).Body.String()
	if !strings.Contains(home, "https://me.example.ts.net/name") || !strings.Contains(home, `class="brand" href="/">me<span>/</span>`) {
		t.Errorf("internet home page: %.1500s", home)
	}
	if strings.Contains(home, "on any device on the tailnet") {
		t.Error("internet home page tells people to use the tailnet name")
	}
	list := h.pub("GET", "/_/links", "", funnel("203.0.113.9"), withCookie(sess)).Body.String()
	if !strings.Contains(list, ">me.example.ts.net/wiki</a>") {
		t.Errorf("internet link list: %.1500s", list)
	}
	// Tailnet devices, through Serve or directly, keep the short name.
	for name, w := range map[string]*httptest.ResponseRecorder{
		"serve":   h.pub("GET", "/", "", serveFrom("100.64.0.1")),
		"private": h.do("GET", "/", ""),
	} {
		if body := w.Body.String(); !strings.Contains(body, "Type <code>me/name</code>") || strings.Contains(body, "ts.net/name") {
			t.Errorf("%s home page: %.1500s", name, body)
		}
	}
}

func TestPublicSignInFlow(t *testing.T) {
	h, g := newPublicHarness(t)
	h.create("wiki", "https://wiki.example.com/")
	sess, w := signIn(t, h, g, "203.0.113.9", "/wiki")
	expectStatus(t, w, http.StatusSeeOther)
	if w.Header().Get("Location") != "/wiki" {
		t.Fatalf("after sign-in went to %q", w.Header().Get("Location"))
	}
	if sess == nil || !sess.Secure || !sess.HttpOnly || sess.SameSite != http.SameSiteLaxMode || sess.Path != "/" || sess.MaxAge < 86400 {
		t.Fatalf("session cookie = %+v", sess)
	}
	if c := cookieNamed(w, signinCookie); c == nil || c.MaxAge >= 0 {
		t.Error("sign-in cookie not cleared")
	}

	expectStatus(t, h.pub("GET", "/wiki", "", funnel("203.0.113.9"), withCookie(sess)), http.StatusFound)

	w = h.pub("POST", "/_/links", form("name", "docs", "target", "https://docs.example.com/"),
		funnel("198.51.100.4"), withCookie(sess), formBody(), publicOrigin())
	expectStatus(t, w, http.StatusSeeOther)
	l, err := h.svc.Store.Get(t.Context(), "docs")
	if err != nil || l.CreatedBy != "alice@gmail.com (via Google)" {
		t.Fatalf("created by %q (%v)", l.CreatedBy, err)
	}
	events, _ := h.svc.Store.History(t.Context(), l.ID)
	if len(events) != 1 || events[0].Actor.IP != "198.51.100.4" || events[0].Actor.Login != "alice@gmail.com" {
		t.Fatalf("audit actor = %+v", events)
	}

	// Cross-site forms are still refused with a session.
	w = h.pub("POST", "/_/links", form("name", "evil", "target", "https://evil.example/"),
		funnel("203.0.113.9"), withCookie(sess), formBody(),
		header("Origin", "https://evil.example"), header("Sec-Fetch-Site", "cross-site"))
	expectStatus(t, w, http.StatusForbidden)

	w = h.pub("GET", "/_/whoami", "", funnel("203.0.113.9"), withCookie(sess))
	if !strings.Contains(w.Body.String(), "alice@gmail.com") || !strings.Contains(w.Body.String(), "Sign out") {
		t.Errorf("whoami: %.500s", w.Body)
	}

	w = h.pub("POST", "/_/signout", "", funnel("203.0.113.9"), withCookie(sess), publicOrigin())
	expectStatus(t, w, http.StatusSeeOther)
	if c := cookieNamed(w, sessionCookie); c == nil || c.MaxAge >= 0 {
		t.Error("session cookie not cleared on sign-out")
	}
	// The session is gone on the server too, so a copied cookie is useless.
	expectStatus(t, h.pub("GET", "/wiki", "", funnel("203.0.113.9"), withCookie(sess)), http.StatusSeeOther)
}

func TestPublicSignInRejections(t *testing.T) {
	h, g := newPublicHarness(t)

	// Not on the allow list.
	g.email = "mallory@gmail.com"
	sess, w := signIn(t, h, g, "203.0.113.9", "/")
	expectStatus(t, w, http.StatusForbidden)
	if sess != nil {
		t.Fatal("session cookie set for someone not on the allow list")
	}
	if !strings.Contains(w.Body.String(), "mallory@gmail.com isn&#39;t allowed") {
		t.Errorf("refusal page: %.500s", w.Body)
	}
	g.email = "alice@gmail.com"

	// A state that doesn't match the browser's cookie (login CSRF: the
	// attacker's own sign-in finished in the victim's browser), no cookie, a
	// made-up cookie, then the real callback, which the stray ones didn't
	// cancel, and a replay of it, which the provider refuses (codes work once).
	cookie, state := startSignIn(t, h, g)
	otherCookie, _ := startSignIn(t, h, g)
	g.nonce = state.Get("nonce")
	callback := "/_/auth/google/callback?code=good-1&state=" + url.QueryEscape(state.Get("state"))
	w = h.pub("GET", callback, "", funnel("203.0.113.9"), withCookie(otherCookie))
	expectStatus(t, w, http.StatusBadRequest)
	if cookieNamed(w, signinCookie) != nil {
		t.Error("a mismatched callback touched the browser's sign-in cookie")
	}
	expectStatus(t, h.pub("GET", callback, "", funnel("203.0.113.9")), http.StatusBadRequest)
	expectStatus(t, h.pub("GET", callback, "", funnel("203.0.113.9"),
		withCookie(&http.Cookie{Name: signinCookie, Value: "x"})), http.StatusBadRequest)
	w = h.pub("GET", callback, "", funnel("203.0.113.9"), withCookie(cookie))
	expectStatus(t, w, http.StatusSeeOther)
	if c := cookieNamed(w, signinCookie); c == nil || c.MaxAge >= 0 {
		t.Error("sign-in cookie not cleared after use")
	}
	w = h.pub("GET", callback, "", funnel("203.0.113.9"), withCookie(cookie))
	expectStatus(t, w, http.StatusBadGateway)
	if cookieNamed(w, sessionCookie) != nil {
		t.Fatal("a replayed callback created a session")
	}
	expectStatus(t, h.pub("GET", "/_/auth/github/start", "", funnel("203.0.113.9")), http.StatusNotFound)

	// The provider refuses the code; the user cancels.
	calls := g.calls
	cookie, state = startSignIn(t, h, g)
	w = h.pub("GET", "/_/auth/google/callback?code=bad-code&state="+url.QueryEscape(state.Get("state")), "",
		funnel("203.0.113.9"), withCookie(cookie))
	expectStatus(t, w, http.StatusBadGateway)
	if cookieNamed(w, sessionCookie) != nil || g.calls != calls+1 {
		t.Fatal("bad code: session set, or token endpoint not called once")
	}
	cookie, state = startSignIn(t, h, g)
	w = h.pub("GET", "/_/auth/google/callback?error=access_denied&state="+url.QueryEscape(state.Get("state")), "",
		funnel("203.0.113.9"), withCookie(cookie))
	expectStatus(t, w, http.StatusOK)
	if !strings.Contains(w.Body.String(), "cancelled") || g.calls != calls+1 {
		t.Fatal("cancelled sign-in: wrong page, or the token endpoint was called")
	}

	// A made-up session cookie.
	fake := &http.Cookie{Name: sessionCookie, Value: auth.Token()}
	expectStatus(t, h.pub("GET", "/", "", funnel("203.0.113.9"), withCookie(fake)), http.StatusSeeOther)
}

func TestPublicSessionEndsWhenRemovedFromAllowList(t *testing.T) {
	h, g := newPublicHarness(t, "alice@gmail.com", "bob@gmail.com")
	sess, _ := signIn(t, h, g, "203.0.113.9", "/")
	expectStatus(t, h.pub("GET", "/", "", funnel("203.0.113.9"), withCookie(sess)), http.StatusOK)

	// Restart with Alice taken off the list, same database.
	cfg := h.srv.cfg
	cfg.Providers = []*auth.Provider{g.provider(t, "bob@gmail.com")}
	srv, err := New(cfg, h.svc, nil)
	if err != nil {
		t.Fatal(err)
	}
	h.srv = srv
	expectStatus(t, h.pub("GET", "/", "", funnel("203.0.113.9"), withCookie(sess)), http.StatusSeeOther)
	if list, _ := h.svc.Store.Sessions(t.Context()); len(list) != 0 {
		t.Fatalf("session for a removed account kept: %+v", list)
	}
}

func TestPublicWorkspaceDomain(t *testing.T) {
	h, g := newPublicHarness(t, "example.com")
	g.email = "carol@example.com" // a personal Google account using a work address: no hd
	sess, w := signIn(t, h, g, "203.0.113.9", "/")
	expectStatus(t, w, http.StatusForbidden)
	if sess != nil {
		t.Fatal("accepted a domain match without the hd claim")
	}
	g.hd = "example.com"
	sess, w = signIn(t, h, g, "203.0.113.9", "/")
	expectStatus(t, w, http.StatusSeeOther)
	if sess == nil {
		t.Fatal("Workspace member refused")
	}
}

func TestPublicAnonymousRateLimit(t *testing.T) {
	h, _ := newPublicHarness(t)
	limited := false
	for range 40 {
		if h.pub("GET", "/_/signin", "", funnel("203.0.113.9")).Code == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("40 anonymous requests in a row were never limited")
	}
	if code := h.pub("GET", "/_/signin", "", funnel("203.0.113.10")).Code; code == http.StatusTooManyRequests {
		t.Error("another address was limited too")
	}
}

func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{
		"/wiki?x=1":                     "/wiki?x=1",
		"":                              "/",
		"wiki":                          "/",
		"//evil.example/":               "/",
		"/\\evil.example/":              "/",
		"https://evil.example/":         "/",
		"/_/auth/google/start":          "/",
		"/_/signin?next=//evil":         "/",
		"/a\r\nSet-Cookie: x=1":         "/",
		"/" + strings.Repeat("a", 3000): "/",
	} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPrivateListenerHasNoSignIn(t *testing.T) {
	h, _ := newPublicHarness(t)
	w := h.do("GET", "/_/signin", "")
	if w.Code == http.StatusOK && strings.Contains(w.Body.String(), "Continue with") {
		t.Fatal("the private listener serves the sign-in page")
	}
	// Forwarding headers mean nothing on the private listener.
	w = h.do("GET", "/_/whoami", "", header("X-Forwarded-For", "100.64.0.2"))
	if !strings.Contains(w.Body.String(), "alice@example.com") {
		t.Errorf("private listener used forwarding headers: %.400s", w.Body)
	}
	// Funnel pointed at the private listener (`tailscale funnel 80`) arrives
	// from localhost with Tailscale's marker: refused, not served unsigned-in.
	h.create("wiki", "https://wiki.example.com/")
	expectStatus(t, h.do("GET", "/wiki", "", peer("127.0.0.1:5000"), header("Tailscale-Funnel-Request", "?1")), http.StatusForbidden)
	expectStatus(t, h.do("POST", "/_/links", form("name", "x", "target", "https://x.example/"),
		peer("127.0.0.1:5000"), formBody(), sameOrigin(), header("Tailscale-Funnel-Request", "?1")), http.StatusForbidden)
}

func TestAnonKey(t *testing.T) {
	a := anonKey(netip.MustParseAddr("2001:db8:1:2:aaaa::1"))
	b := anonKey(netip.MustParseAddr("2001:db8:1:2:bbbb::9"))
	c := anonKey(netip.MustParseAddr("2001:db8:1:3::1"))
	if a != b || a == c {
		t.Errorf("IPv6 keys: %q %q %q; want one per /64", a, b, c)
	}
	if anonKey(netip.MustParseAddr("203.0.113.9")) == anonKey(netip.MustParseAddr("203.0.113.10")) {
		t.Error("IPv4 addresses share a key")
	}
}

func TestWriteLimiterNeverEvicts(t *testing.T) {
	ls := newLimiterSet(rate.Limit(1), 5, 3)
	ls.maxKeys = 2
	for range 3 {
		ls.allow("alice")
	}
	if ls.allow("alice") {
		t.Fatal("daily budget not enforced")
	}
	ls.allow("bob")
	if ls.allow("mallory") {
		t.Fatal("a new key was admitted to a full table")
	}
	if ls.allow("alice") {
		t.Fatal("alice's spent daily budget was reset")
	}
	if !ls.allow("bob") {
		t.Fatal("an existing key was refused")
	}
}

func TestLimiterMemoryBounded(t *testing.T) {
	ls := newLimiterSet(rate.Limit(1), 3, 0)
	ls.evict = true
	if ls.idle > time.Minute {
		t.Fatalf("anonymous entries kept %v after going idle", ls.idle)
	}
	ls.maxKeys = 50
	// A regular visitor keeps using their bucket while an attacker cycles
	// through far more addresses than fit, draining each one.
	for i := range 500 {
		if i%10 == 0 {
			ls.allow("regular")
		}
		for range 5 {
			ls.allow("attacker:" + strconv.Itoa(i))
		}
	}
	if len(ls.m) > 50 || ls.lru.Len() != len(ls.m) {
		t.Fatalf("%d entries (list %d), want at most 50", len(ls.m), ls.lru.Len())
	}
	if _, ok := ls.m["regular"]; !ok {
		t.Error("a recently used entry was evicted before idle ones")
	}
	// A newcomer while the table is full gets a fresh bucket of its own, not
	// one the attacker has drained.
	if !ls.allow("newcomer") {
		t.Error("a new address was refused because the table was full")
	}

	// Idle entries are dropped.
	for _, el := range ls.m {
		el.Value.(*limiterEntry).seen = time.Now().Add(-time.Hour)
	}
	ls.allow("fresh")
	if len(ls.m) != 1 || ls.lru.Len() != 1 {
		t.Fatalf("%d entries after going idle, want 1", len(ls.m))
	}
}

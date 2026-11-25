package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// FakeIDToken builds an unsigned JWT carrying claims, as a token endpoint
// would return it (the signature part is never checked).
func fakeIDToken(claims map[string]any) string {
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	return enc(map[string]string{"alg": "RS256", "typ": "JWT"}) + "." + enc(claims) + ".c2ln"
}

type tokenServer struct {
	*httptest.Server
	form   url.Values
	status int
	body   any
}

func newTokenServer(t *testing.T) *tokenServer {
	ts := &tokenServer{status: http.StatusOK}
	ts.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		ts.form = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(ts.status)
		json.NewEncoder(w).Encode(ts.body)
	}))
	t.Cleanup(ts.Close)
	return ts
}

func googleClaims(nonce string) map[string]any {
	now := time.Now().Unix()
	return map[string]any{
		"iss": "https://accounts.google.com", "aud": "gid", "sub": "1234",
		"exp": now + 3600, "iat": now, "nonce": nonce,
		"email": "Alice@Gmail.com", "email_verified": true, "name": "Alice",
	}
}

func newGoogle(t *testing.T, ts *tokenServer, allow ...string) *Provider {
	t.Helper()
	p, err := Google(GoogleConfig{ClientID: "gid", ClientSecret: "gsecret", Allow: allow},
		WithEndpoints("https://accounts.example/auth", ts.URL), WithHTTPClient(ts.Client()))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestGoogleExchange(t *testing.T) {
	ts := newTokenServer(t)
	p := newGoogle(t, ts, "alice@gmail.com")
	ts.body = map[string]any{"id_token": fakeIDToken(googleClaims("n1")), "access_token": "x"}

	c, err := p.Exchange(context.Background(), "code1", "https://nym.example.ts.net/_/auth/google/callback", "verifier1", "n1")
	if err != nil {
		t.Fatal(err)
	}
	want := Claims{Provider: "google", Subject: "1234", Email: "alice@gmail.com", Name: "Alice"}
	if c != want {
		t.Fatalf("claims = %+v, want %+v", c, want)
	}
	if !p.Allowed(c) {
		t.Fatal("alice@gmail.com should be allowed")
	}
	for k, v := range map[string]string{
		"grant_type": "authorization_code", "code": "code1", "client_id": "gid",
		"client_secret": "gsecret", "code_verifier": "verifier1",
		"redirect_uri": "https://nym.example.ts.net/_/auth/google/callback",
	} {
		if got := ts.form.Get(k); got != v {
			t.Errorf("token request %s = %q, want %q", k, got, v)
		}
	}
}

func TestGoogleRejectsBadTokens(t *testing.T) {
	ts := newTokenServer(t)
	p := newGoogle(t, ts, "alice@gmail.com")
	cases := map[string]func(map[string]any){
		"wrong issuer":     func(c map[string]any) { c["iss"] = "https://evil.example" },
		"wrong audience":   func(c map[string]any) { c["aud"] = "someone-else" },
		"multi aud no azp": func(c map[string]any) { c["aud"] = []string{"gid", "other"} },
		"expired":          func(c map[string]any) { c["exp"] = time.Now().Add(-time.Hour).Unix() },
		"future iat":       func(c map[string]any) { c["iat"] = time.Now().Add(time.Hour).Unix() },
		"wrong nonce":      func(c map[string]any) { c["nonce"] = "other" },
		"no nonce":         func(c map[string]any) { delete(c, "nonce") },
		"unverified email": func(c map[string]any) { c["email_verified"] = false },
		"no email":         func(c map[string]any) { delete(c, "email") },
		"no subject":       func(c map[string]any) { delete(c, "sub") },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := googleClaims("n1")
			mutate(c)
			ts.body = map[string]any{"id_token": fakeIDToken(c)}
			if _, err := p.Exchange(context.Background(), "code", "https://x/cb", "v", "n1"); err == nil {
				t.Fatal("accepted a bad ID token")
			}
		})
	}
	t.Run("error response", func(t *testing.T) {
		ts.status, ts.body = http.StatusBadRequest, map[string]any{"error": "invalid_grant"}
		defer func() { ts.status = http.StatusOK }()
		if _, err := p.Exchange(context.Background(), "code", "https://x/cb", "v", "n1"); err == nil || !strings.Contains(err.Error(), "invalid_grant") {
			t.Fatalf("err = %v, want invalid_grant", err)
		}
	})
	t.Run("not a JWT", func(t *testing.T) {
		ts.body = map[string]any{"id_token": "abc"}
		if _, err := p.Exchange(context.Background(), "code", "https://x/cb", "v", "n1"); err == nil {
			t.Fatal("accepted a non-JWT")
		}
	})
	t.Run("multi aud with azp", func(t *testing.T) {
		c := googleClaims("n1")
		c["aud"], c["azp"] = []string{"gid", "other"}, "gid"
		ts.body = map[string]any{"id_token": fakeIDToken(c)}
		if _, err := p.Exchange(context.Background(), "code", "https://x/cb", "v", "n1"); err != nil {
			t.Fatal(err)
		}
	})
}

func TestGoogleAllowList(t *testing.T) {
	ts := newTokenServer(t)
	p := newGoogle(t, ts, "alice@gmail.com", "Example.com", "dave@corp.example", "erin@googlemail.com")
	for _, tc := range []struct {
		c    Claims
		want bool
	}{
		{Claims{Provider: "google", Email: "alice@gmail.com"}, true},
		{Claims{Provider: "google", Email: "erin@googlemail.com"}, true},
		{Claims{Provider: "google", Email: "bob@gmail.com"}, false},
		// A listed non-Gmail address counts only when Google vouches for it
		// through that domain's Workspace, not on a personal account.
		{Claims{Provider: "google", Email: "dave@corp.example"}, false},
		{Claims{Provider: "google", Email: "dave@corp.example", Org: "corp.example"}, true},
		{Claims{Provider: "google", Email: "dave@corp.example", Org: "other.example"}, false},
		{Claims{Provider: "google", Email: "carol@example.com", Org: "example.com"}, true},
		// An address at the domain is not enough: a personal Google account
		// can use any address. Only the hd claim proves Workspace membership.
		{Claims{Provider: "google", Email: "mallory@example.com"}, false},
		{Claims{Provider: "google", Email: "x@other.com", Org: "other.com"}, false},
		{Claims{Provider: "slack", Email: "alice@gmail.com"}, false},
	} {
		if got := p.Allowed(tc.c); got != tc.want {
			t.Errorf("Allowed(%+v) = %v, want %v", tc.c, got, tc.want)
		}
	}
}

func TestSlackExchange(t *testing.T) {
	ts := newTokenServer(t)
	p, err := Slack(SlackConfig{ClientID: "sid", ClientSecret: "ssecret", Teams: []string{"T0123ABCD"}},
		WithEndpoints("https://slack.example/auth", ts.URL), WithHTTPClient(ts.Client()))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	claims := map[string]any{
		"iss": "https://slack.com", "aud": "sid", "sub": "U123", "exp": now + 3600, "iat": now,
		"nonce": "n2", "email": "bob@corp.example", "email_verified": true, "name": "Bob",
		"https://slack.com/team_id": "T0123ABCD",
	}
	ts.body = map[string]any{"ok": true, "id_token": fakeIDToken(claims)}
	c, err := p.Exchange(context.Background(), "code2", "https://x/cb", "", "n2")
	if err != nil {
		t.Fatal(err)
	}
	if c.Org != "T0123ABCD" || c.Email != "bob@corp.example" || !p.Allowed(c) {
		t.Fatalf("claims %+v not allowed", c)
	}
	if ts.form.Has("code_verifier") {
		t.Error("sent a PKCE verifier to Slack, which does not support PKCE")
	}
	claims["https://slack.com/team_id"] = "T9999ZZZZ"
	ts.body = map[string]any{"ok": true, "id_token": fakeIDToken(claims)}
	if c, err := p.Exchange(context.Background(), "code3", "https://x/cb", "", "n2"); err != nil || p.Allowed(c) {
		t.Fatalf("another workspace: err=%v allowed=%v, want allowed=false", err, p.Allowed(c))
	}
	ts.body = map[string]any{"ok": false, "error": "invalid_code"}
	if _, err := p.Exchange(context.Background(), "code4", "https://x/cb", "", "n2"); err == nil || !strings.Contains(err.Error(), "invalid_code") {
		t.Fatalf("err = %v, want invalid_code", err)
	}
}

func TestAuthURL(t *testing.T) {
	ts := newTokenServer(t)
	p := newGoogle(t, ts, "example.com")
	u, err := url.Parse(p.AuthURL("https://nym.example.ts.net/_/auth/google/callback", "st", "no", "ver"))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	for k, v := range map[string]string{
		"response_type": "code", "client_id": "gid", "scope": "openid email profile",
		"state": "st", "nonce": "no", "code_challenge": challenge("ver"),
		"code_challenge_method": "S256", "hd": "example.com", "prompt": "select_account",
	} {
		if got := q.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if q.Has("client_secret") {
		t.Error("client secret leaked into the browser URL")
	}
}

func TestConfigValidation(t *testing.T) {
	bad := []func() error{
		func() error { _, err := Google(GoogleConfig{ClientSecret: "s", Allow: []string{"a@b.co"}}); return err },
		func() error { _, err := Google(GoogleConfig{ClientID: "i", Allow: []string{"a@b.co"}}); return err },
		func() error { _, err := Google(GoogleConfig{ClientID: "i", ClientSecret: "s"}); return err },
		func() error {
			_, err := Google(GoogleConfig{ClientID: "i", ClientSecret: "s", Allow: []string{"not an email@"}})
			return err
		},
		func() error {
			_, err := Google(GoogleConfig{ClientID: "i", ClientSecret: "s", Allow: []string{"*"}})
			return err
		},
		func() error { _, err := Slack(SlackConfig{ClientID: "i", ClientSecret: "s"}); return err },
		func() error {
			_, err := Slack(SlackConfig{ClientID: "i", ClientSecret: "s", Teams: []string{"acme"}})
			return err
		},
		func() error {
			_, err := Slack(SlackConfig{ClientID: "i", ClientSecret: "s x", Teams: []string{"T0123"}})
			return err
		},
	}
	for i, f := range bad {
		if f() == nil {
			t.Errorf("case %d: bad config accepted", i)
		}
	}
}

func TestAttempts(t *testing.T) {
	a := NewAttempts(time.Minute)
	now := time.Now()
	a.now = func() time.Time { return now }
	at, cookie, err := a.Start("google", "/wiki")
	if err != nil || at.State == "" || at.Nonce == "" || at.Verifier == "" || at.Nonce == at.State {
		t.Fatalf("Start: %v %+v", err, at)
	}
	if strings.Contains(cookie, at.Nonce) || strings.Contains(cookie, "/wiki") {
		t.Fatal("cookie is not encrypted")
	}
	got, ok := a.Finish(at.State, cookie)
	if !ok || got != at {
		t.Fatalf("Finish = %+v %v, want %+v", got, ok, at)
	}

	other, otherCookie, _ := a.Start("google", "/")
	for name, tc := range map[string][2]string{
		"another sign-in's state": {other.State, cookie},
		"no cookie":               {at.State, ""},
		"no state":                {"", cookie},
		"garbage cookie":          {at.State, "AAAA"},
		"tampered cookie":         {at.State, cookie[:len(cookie)-2] + "AA"},
		"huge cookie":             {at.State, strings.Repeat("A", 5000)},
	} {
		if _, ok := a.Finish(tc[0], tc[1]); ok {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, ok := NewAttempts(time.Minute).Finish(other.State, otherCookie); ok {
		t.Error("accepted a cookie sealed by another process (key)")
	}
	now = now.Add(2 * time.Minute)
	if _, ok := a.Finish(other.State, otherCookie); ok {
		t.Error("accepted an expired sign-in")
	}
}

func TestPlainHTTPTokenEndpointRefused(t *testing.T) {
	p, err := Google(GoogleConfig{ClientID: "gid", ClientSecret: "s", Allow: []string{"a@gmail.com"}},
		WithEndpoints("https://x/auth", "http://127.0.0.1:1/token"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exchange(context.Background(), "code", "https://x/cb", "v", "n"); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("err = %v, want https refusal", err)
	}
}

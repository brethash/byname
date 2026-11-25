// Package auth signs people in with Google or Slack on byname's public address,
// using the OpenID Connect authorization-code flow as a confidential client.
//
// ID tokens come straight from the provider's token endpoint, over TLS, in a
// request authenticated with the client secret, so their signatures are not
// checked: OpenID Connect Core 1.0 §3.1.3.7 (6) allows TLS server validation
// in place of a signature check for tokens received this way, and Google's
// OpenID Connect guide says the same. Issuer, audience, expiry and nonce are
// still checked, and only verified email addresses are accepted.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Claims is who signed in.
type Claims struct {
	Provider string // provider ID: "google" or "slack"
	Subject  string // the provider's stable user ID
	Email    string // verified, lower case
	Name     string
	Org      string // Google Workspace domain (hd) or Slack workspace ID; "" for a personal Google account
}

// Provider is one configured sign-in provider.
type Provider struct {
	id, label  string
	clientID   string
	secret     string
	authURL    string
	tokenURL   string
	issuers    []string
	pkce       bool
	authParams url.Values
	allow      func(Claims) bool
	org        func(idToken) string
	hc         *http.Client
}

// ID is the provider's short name, used in URLs and sessions ("google").
func (p *Provider) ID() string { return p.id }

// Label is the provider's display name ("Google").
func (p *Provider) Label() string { return p.label }

// Allowed reports whether the configured allow list admits c. Callers check it
// at sign-in and again on every request, so removing someone from the list
// takes effect at once.
func (p *Provider) Allowed(c Claims) bool { return c.Provider == p.id && p.allow(c) }

// Option adjusts a provider. Tests use them to point at fake endpoints.
type Option func(*Provider)

// WithEndpoints replaces the provider's authorization and token endpoints.
// For tests. The token endpoint must still be https: ID tokens are trusted
// because they come from it over TLS.
func WithEndpoints(authURL, tokenURL string) Option {
	return func(p *Provider) { p.authURL, p.tokenURL = authURL, tokenURL }
}

// WithHTTPClient replaces the client used for the token request.
func WithHTTPClient(hc *http.Client) Option { return func(p *Provider) { p.hc = hc } }

// defaultClient talks to the providers' token endpoints: short timeouts, no
// proxy from the environment, and no redirects.
var defaultClient = &http.Client{
	Timeout: 15 * time.Second,
	Transport: &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		MaxIdleConns:          4,
		IdleConnTimeout:       60 * time.Second,
		ForceAttemptHTTP2:     true,
	},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// GoogleConfig configures sign-in with Google.
type GoogleConfig struct {
	ClientID     string
	ClientSecret string
	// Allow lists who may sign in: full addresses (alice@gmail.com), and
	// Google Workspace domains (example.com), which are matched against the
	// token's hd claim, never against the end of an address.
	//
	// Google vouches for an address only if it is @gmail.com, or if the
	// account belongs to the address's Workspace domain (hd). Any other
	// address on a personal Google account was verified once, when the
	// account was made, and the mailbox may have changed hands since. So a
	// listed address that isn't Gmail matches only an account in that
	// domain's Workspace.
	Allow []string
}

// googleVouches reports whether Google is authoritative for c's address.
func googleVouches(c Claims) bool {
	_, domain, _ := strings.Cut(c.Email, "@")
	return domain == "gmail.com" || domain == "googlemail.com" || (c.Org != "" && c.Org == domain)
}

var (
	emailRE  = regexp.MustCompile(`^[a-z0-9.!#$%&'*+/=?^_{|}~-]+@[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)
	domainRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)
	teamRE   = regexp.MustCompile(`^[TE][A-Z0-9]{2,31}$`)
)

// Google returns the Google provider.
func Google(c GoogleConfig, opts ...Option) (*Provider, error) {
	if err := checkClient("google", c.ClientID, c.ClientSecret); err != nil {
		return nil, err
	}
	emails, domains := map[string]bool{}, map[string]bool{}
	for _, a := range c.Allow {
		a = strings.ToLower(strings.TrimSpace(a))
		switch {
		case a == "":
		case strings.Contains(a, "@"):
			if !emailRE.MatchString(a) {
				return nil, fmt.Errorf("google: %q is not an email address", a)
			}
			emails[a] = true
		default:
			if !domainRE.MatchString(a) {
				return nil, fmt.Errorf("google: %q is not a domain name", a)
			}
			domains[a] = true
		}
	}
	if len(emails)+len(domains) == 0 {
		return nil, errors.New("google: the allow list is empty; list the addresses or Workspace domains that may sign in")
	}
	params := url.Values{"prompt": {"select_account"}}
	if len(emails) == 0 && len(domains) == 1 {
		for d := range domains {
			params.Set("hd", d) // only narrows Google's account chooser; Allowed still checks
		}
	}
	p := &Provider{
		id: "google", label: "Google",
		clientID: c.ClientID, secret: c.ClientSecret,
		authURL:    "https://accounts.google.com/o/oauth2/v2/auth",
		tokenURL:   "https://oauth2.googleapis.com/token",
		issuers:    []string{"https://accounts.google.com", "accounts.google.com"},
		pkce:       true,
		authParams: params,
		allow: func(c Claims) bool {
			return (emails[c.Email] && googleVouches(c)) || (c.Org != "" && domains[c.Org])
		},
		org: func(t idToken) string { return strings.ToLower(t.HD) },
		hc:  defaultClient,
	}
	for _, o := range opts {
		o(p)
	}
	return p, nil
}

// SlackConfig configures Sign in with Slack.
type SlackConfig struct {
	ClientID     string
	ClientSecret string
	// Teams lists the Slack workspace IDs (T0123ABCD) whose members may sign in.
	Teams []string
}

// Slack returns the Slack provider.
func Slack(c SlackConfig, opts ...Option) (*Provider, error) {
	if err := checkClient("slack", c.ClientID, c.ClientSecret); err != nil {
		return nil, err
	}
	teams := map[string]bool{}
	var first string
	for _, t := range c.Teams {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if !teamRE.MatchString(t) {
			return nil, fmt.Errorf("slack: %q is not a workspace ID (like T0123ABCD)", t)
		}
		teams[t] = true
		if first == "" {
			first = t
		}
	}
	if len(teams) == 0 {
		return nil, errors.New("slack: the workspace list is empty; list the workspace IDs whose members may sign in")
	}
	params := url.Values{}
	if len(teams) == 1 {
		params.Set("team", first) // skips Slack's workspace picker
	}
	p := &Provider{
		id: "slack", label: "Slack",
		clientID: c.ClientID, secret: c.ClientSecret,
		authURL:  "https://slack.com/openid/connect/authorize",
		tokenURL: "https://slack.com/api/openid.connect.token",
		issuers:  []string{"https://slack.com"},
		// Slack's discovery document lists no PKCE support; state, nonce and
		// the client secret protect the flow.
		pkce:       false,
		authParams: params,
		allow:      func(c Claims) bool { return teams[c.Org] },
		org:        func(t idToken) string { return t.SlackTeam },
		hc:         defaultClient,
	}
	for _, o := range opts {
		o(p)
	}
	return p, nil
}

func checkClient(name, id, secret string) error {
	switch {
	case strings.TrimSpace(id) == "":
		return fmt.Errorf("%s: client ID is empty", name)
	case strings.TrimSpace(secret) == "":
		return fmt.Errorf("%s: client secret is empty", name)
	case strings.ContainsAny(id+secret, " \t\r\n"):
		return fmt.Errorf("%s: client ID or secret contains whitespace", name)
	}
	return nil
}

// Token returns 256 random bits, URL-safe base64 encoded (43 characters).
func Token() string {
	b := make([]byte, 32)
	rand.Read(b) // never fails (crypto/rand panics instead)
	return base64.RawURLEncoding.EncodeToString(b)
}

// challenge is the PKCE S256 code challenge for verifier.
func challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// AuthURL is where to send the browser to sign in. verifier is ignored by
// providers without PKCE.
func (p *Provider) AuthURL(redirectURI, state, nonce, verifier string) string {
	q := url.Values{
		"response_type": {"code"},
		"client_id":     {p.clientID},
		"redirect_uri":  {redirectURI},
		"scope":         {"openid email profile"},
		"state":         {state},
		"nonce":         {nonce},
	}
	if p.pkce {
		q.Set("code_challenge", challenge(verifier))
		q.Set("code_challenge_method", "S256")
	}
	for k, v := range p.authParams {
		q[k] = v
	}
	return p.authURL + "?" + q.Encode()
}

// Exchange trades an authorization code for the signed-in person's claims.
// It does not check the allow list; call Allowed.
func (p *Provider) Exchange(ctx context.Context, code, redirectURI, verifier, nonce string) (Claims, error) {
	if code == "" || len(code) > 2048 {
		return Claims{}, errors.New("auth: missing or oversized authorization code")
	}
	if !strings.HasPrefix(p.tokenURL, "https://") {
		return Claims{}, fmt.Errorf("auth: %s token endpoint must be https", p.id)
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {p.clientID},
		"client_secret": {p.secret},
	}
	if p.pkce {
		form.Set("code_verifier", verifier)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return Claims{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := p.hc.Do(req)
	if err != nil {
		return Claims{}, fmt.Errorf("auth: %s token request: %w", p.id, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return Claims{}, fmt.Errorf("auth: %s token response: %w", p.id, err)
	}
	var tr struct {
		OK      *bool  `json:"ok"` // Slack answers errors with 200 and ok=false
		Error   string `json:"error"`
		IDToken string `json:"id_token"`
	}
	jerr := json.Unmarshal(body, &tr)
	if resp.StatusCode != http.StatusOK || (tr.OK != nil && !*tr.OK) || tr.Error != "" {
		return Claims{}, fmt.Errorf("auth: %s token endpoint: status %d, error %q", p.id, resp.StatusCode, clip(tr.Error, 80))
	}
	if jerr != nil {
		return Claims{}, fmt.Errorf("auth: %s token response: %w", p.id, jerr)
	}
	t, err := parseIDToken(tr.IDToken)
	if err != nil {
		return Claims{}, fmt.Errorf("auth: %s: %w", p.id, err)
	}
	if err := p.check(t, nonce, time.Now()); err != nil {
		return Claims{}, fmt.Errorf("auth: %s ID token: %w", p.id, err)
	}
	return Claims{
		Provider: p.id,
		Subject:  t.Sub,
		Email:    strings.ToLower(t.Email),
		Name:     clip(t.Name, 200),
		Org:      p.org(t),
	}, nil
}

// clockSkew tolerates small clock differences with the provider.
const clockSkew = 2 * time.Minute

func (p *Provider) check(t idToken, nonce string, now time.Time) error {
	switch {
	case !slices.Contains(p.issuers, t.Iss):
		return fmt.Errorf("unexpected issuer %q", clip(t.Iss, 80))
	case !slices.Contains(t.Aud, p.clientID):
		return errors.New("issued to another client")
	case len(t.Aud) > 1 && t.Azp != p.clientID: // OIDC Core §3.1.3.7 (4–5)
		return errors.New("several audiences and no matching azp")
	case t.Exp == 0 || now.After(time.Unix(int64(t.Exp), 0).Add(clockSkew)):
		return errors.New("expired")
	case t.Iat != 0 && time.Unix(int64(t.Iat), 0).After(now.Add(clockSkew)):
		return errors.New("issued in the future")
	case nonce == "" || subtle.ConstantTimeCompare([]byte(t.Nonce), []byte(nonce)) != 1:
		return errors.New("nonce mismatch")
	case t.Sub == "":
		return errors.New("no subject")
	case t.Email == "" || !bool(t.EmailVerified):
		return errors.New("no verified email address")
	}
	return nil
}

// idToken is the subset of ID token claims byname reads.
type idToken struct {
	Iss           string      `json:"iss"`
	Sub           string      `json:"sub"`
	Aud           audience    `json:"aud"`
	Azp           string      `json:"azp"`
	Exp           numericDate `json:"exp"`
	Iat           numericDate `json:"iat"`
	Nonce         string      `json:"nonce"`
	Email         string      `json:"email"`
	EmailVerified flexBool    `json:"email_verified"`
	Name          string      `json:"name"`
	HD            string      `json:"hd"`                        // Google Workspace domain
	SlackTeam     string      `json:"https://slack.com/team_id"` // Slack workspace ID
}

// parseIDToken decodes a JWT's claims without checking its signature (see the
// package comment for why that is sound here).
func parseIDToken(raw string) (idToken, error) {
	if raw == "" {
		return idToken{}, errors.New("no ID token in the response")
	}
	if len(raw) > 16<<10 {
		return idToken{}, errors.New("ID token too large")
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return idToken{}, errors.New("ID token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return idToken{}, fmt.Errorf("ID token payload: %w", err)
	}
	var t idToken
	if err := json.Unmarshal(payload, &t); err != nil {
		return idToken{}, fmt.Errorf("ID token payload: %w", err)
	}
	return t, nil
}

// audience is the aud claim: a string or an array of strings.
type audience []string

func (a *audience) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*a = audience{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return errors.New("aud is neither a string nor a list of strings")
	}
	*a = many
	return nil
}

// numericDate is seconds since the epoch, as a JSON number or numeric string.
type numericDate int64

func (d *numericDate) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return fmt.Errorf("bad time %q", clip(s, 20))
	}
	*d = numericDate(f)
	return nil
}

// flexBool accepts true/false as JSON booleans or strings.
type flexBool bool

func (f *flexBool) UnmarshalJSON(b []byte) error {
	switch strings.Trim(string(b), `"`) {
	case "true":
		*f = true
	case "false", "null":
		*f = false
	default:
		return fmt.Errorf("bad boolean %s", clip(string(b), 20))
	}
	return nil
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

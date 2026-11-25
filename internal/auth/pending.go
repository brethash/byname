package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"time"
)

// Attempt is a sign-in that has been sent to a provider and not come back.
type Attempt struct {
	Provider string `json:"p"`
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"` // PKCE code verifier
	Next     string `json:"x"` // local path to return to
	Expires  int64  `json:"e"` // unix seconds
}

// Attempts keeps in-progress sign-ins in the browser, not on the server: each
// one is sealed (AES-256-GCM, with a key made at startup) into the sign-in
// cookie. The server holds nothing per sign-in, so nobody can exhaust it by
// starting sign-ins. A restart only means a sign-in in progress has to start
// again.
type Attempts struct {
	aead cipher.AEAD
	ttl  time.Duration
	now  func() time.Time
}

// maxCookie bounds the sealed cookie, keeping it under browsers' 4 KB limit.
const maxCookie = 3800

var attemptAAD = []byte("ot sign-in v1")

// NewAttempts returns sign-in state that lasts ttl.
func NewAttempts(ttl time.Duration) *Attempts {
	key := make([]byte, 32)
	rand.Read(key)
	block, err := aes.NewCipher(key)
	if err != nil {
		panic(err) // a 32-byte key is always valid
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		panic(err)
	}
	return &Attempts{aead: aead, ttl: ttl, now: time.Now}
}

// Start begins a sign-in with provider that returns to next. It returns the
// attempt (with a fresh state, nonce and PKCE verifier) and the sealed cookie
// value that carries it.
func (a *Attempts) Start(provider, next string) (Attempt, string, error) {
	at := Attempt{
		Provider: provider, State: Token(), Nonce: Token(), Verifier: Token(),
		Next: next, Expires: a.now().Add(a.ttl).Unix(),
	}
	plain, err := json.Marshal(at)
	if err != nil {
		return Attempt{}, "", err
	}
	nonce := make([]byte, a.aead.NonceSize())
	rand.Read(nonce)
	sealed := a.aead.Seal(nonce, nonce, plain, attemptAAD)
	return at, base64.RawURLEncoding.EncodeToString(sealed), nil
}

// Finish opens cookie and returns its attempt if it is genuine (sealed by
// this process), unexpired, and for state. A callback whose state doesn't
// match the browser's own cookie (login CSRF) fails here.
func (a *Attempts) Finish(state, cookie string) (Attempt, bool) {
	if state == "" || cookie == "" || len(cookie) > maxCookie {
		return Attempt{}, false
	}
	sealed, err := base64.RawURLEncoding.DecodeString(cookie)
	if err != nil || len(sealed) < a.aead.NonceSize() {
		return Attempt{}, false
	}
	n := a.aead.NonceSize()
	plain, err := a.aead.Open(nil, sealed[:n], sealed[n:], attemptAAD)
	if err != nil {
		return Attempt{}, false
	}
	var at Attempt
	if json.Unmarshal(plain, &at) != nil {
		return Attempt{}, false
	}
	if a.now().Unix() > at.Expires || subtle.ConstantTimeCompare([]byte(state), []byte(at.State)) != 1 {
		return Attempt{}, false
	}
	return at, true
}

package store

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"testing"
	"time"
)

func hashOf(s string) []byte { h := sha256.Sum256([]byte(s)); return h[:] }

func TestSessions(t *testing.T) {
	s := openTest(t)
	now := s.now()
	alice := Session{Provider: "google", Subject: "1", Email: "alice@gmail.com", Name: "Alice",
		IP: "203.0.113.7", Created: now, Expires: now.Add(time.Hour)}
	if err := s.CreateSession(ctx, hashOf("a"), alice); err != nil {
		t.Fatal(err)
	}
	got, err := s.SessionByHash(ctx, hashOf("a"))
	if err != nil || got.Email != "alice@gmail.com" || got.Provider != "google" || got.IP != "203.0.113.7" {
		t.Fatalf("SessionByHash = %+v, %v", got, err)
	}
	if _, err := s.SessionByHash(ctx, hashOf("b")); !errors.Is(err, ErrNoSession) {
		t.Fatalf("unknown token: err = %v", err)
	}
	if err := s.CreateSession(ctx, []byte("short"), alice); err == nil {
		t.Fatal("accepted a hash that isn't 32 bytes")
	}

	// The token itself is never stored.
	var stored []byte
	s.db.QueryRow(`SELECT token_hash FROM sessions`).Scan(&stored)
	if !bytes.Equal(stored, hashOf("a")) {
		t.Fatal("stored something other than the token's hash")
	}

	// Expired sessions are invisible, and purged by the next sign-in.
	old := alice
	old.Expires = now.Add(-time.Minute)
	s.db.Exec(`INSERT INTO sessions (token_hash, provider, subject, email, created_at, expires_at) VALUES (?, 'google', '1', 'alice@gmail.com', 0, ?)`,
		hashOf("old"), old.Expires.UnixMilli())
	if _, err := s.SessionByHash(ctx, hashOf("old")); !errors.Is(err, ErrNoSession) {
		t.Fatalf("expired session returned: %v", err)
	}
	bob := Session{Provider: "slack", Subject: "U1", Email: "bob@corp.example", Org: "T0123",
		Created: now, Expires: now.Add(time.Hour)}
	if err := s.CreateSession(ctx, hashOf("b"), bob); err != nil {
		t.Fatal(err)
	}
	var n int
	s.db.QueryRow(`SELECT count(*) FROM sessions`).Scan(&n)
	if n != 2 {
		t.Fatalf("%d sessions stored, want 2 (expired one purged)", n)
	}
	list, err := s.Sessions(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("Sessions = %v, %v", list, err)
	}

	if err := s.DeleteSession(ctx, hashOf("a")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionByHash(ctx, hashOf("a")); !errors.Is(err, ErrNoSession) {
		t.Fatal("deleted session still live")
	}
	s.CreateSession(ctx, hashOf("a2"), alice)
	if n, err := s.RevokeSessions(ctx, "alice@gmail.com"); err != nil || n != 1 {
		t.Fatalf("RevokeSessions(alice) = %d, %v", n, err)
	}
	if n, err := s.RevokeSessions(ctx, ""); err != nil || n != 1 {
		t.Fatalf("RevokeSessions(all) = %d, %v", n, err)
	}

	// One account keeps at most maxSessionsPerAccount sessions; the oldest go.
	for i := range maxSessionsPerAccount + 3 {
		x := alice
		x.Created = now.Add(time.Duration(i) * time.Minute)
		if err := s.CreateSession(ctx, hashOf("many"+string(rune('a'+i))), x); err != nil {
			t.Fatal(err)
		}
	}
	s.db.QueryRow(`SELECT count(*) FROM sessions WHERE email = 'alice@gmail.com'`).Scan(&n)
	if n != maxSessionsPerAccount {
		t.Fatalf("alice has %d sessions, want %d", n, maxSessionsPerAccount)
	}
	if _, err := s.SessionByHash(ctx, hashOf("manya")); !errors.Is(err, ErrNoSession) {
		t.Fatal("the oldest session survived the per-account cap")
	}
	if _, err := s.SessionByHash(ctx, hashOf("many"+string(rune('a'+maxSessionsPerAccount+2)))); err != nil {
		t.Fatalf("the newest session was dropped: %v", err)
	}
}

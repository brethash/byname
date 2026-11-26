package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"byname/internal/app"
	"byname/internal/link"
	"byname/internal/store"
	"byname/internal/web"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func openStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "byname.db")
	st, err := store.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st, p
}

func TestParsePublic(t *testing.T) {
	cases := []struct {
		url, listen string
		want        publicAddr
	}{
		{"", "", publicAddr{}},
		{"https://me.example.com", "", publicAddr{host: "me.example.com", listen: ":443"}},
		{"https://ME.Example.com.", "", publicAddr{host: "me.example.com", listen: ":443"}},
		{"https://me.tail1234.ts.net", "", publicAddr{host: "me.tail1234.ts.net", listen: "unix:/run/byname/public.sock", behindTailscale: true}},
		{"https://me.example.com", "unix:/tmp/x.sock", publicAddr{host: "me.example.com", listen: "unix:/tmp/x.sock", behindTailscale: true}},
	}
	for _, c := range cases {
		got, err := parsePublic(c.url, c.listen)
		if err != nil || got != c.want {
			t.Errorf("parsePublic(%q, %q) = %+v, %v; want %+v", c.url, c.listen, got, err, c.want)
		}
	}
	if _, err := parsePublic("https://me.example.com:8443", ""); err == nil {
		t.Error("a directly served address on another port was accepted")
	}
}

// In --public-domain mode the public name is not among --hosts; a link to it
// would redirect to itself forever.
func TestLinksToThePublicDomainAreRejected(t *testing.T) {
	st, _ := openStore(t)
	pub, err := parsePublic("https://me.example.com", "")
	if err != nil {
		t.Fatal(err)
	}
	svc := app.New(st, app.Config{SelfHosts: selfHosts([]string{"me", "me.lan"}, pub.host), Logger: quiet})
	for _, target := range []string{
		"https://me.example.com/wiki",
		"https://ME.example.com./wiki",
		"http://me.example.com/wiki",
		"https://me.example.com:443/{1}",
		"http://me/wiki",
	} {
		if _, err := svc.Validate(app.Input{Name: "loop", Target: target}); !errors.Is(err, link.ErrTargetSelf) {
			t.Errorf("Validate(%q) err = %v, want ErrTargetSelf", target, err)
		}
	}
	if _, err := svc.Validate(app.Input{Name: "ok", Target: "https://example.com/"}); err != nil {
		t.Errorf("an unrelated target was rejected: %v", err)
	}
	if got := selfHosts([]string{"me"}, ""); len(got) != 1 {
		t.Errorf("selfHosts without a public address = %v", got)
	}
}

// A click served while the server is shutting down must still reach the
// database: background work (and its final flush) stops only after the
// servers have finished their last requests.
func TestShutdownFlushesClicksServedDuringShutdown(t *testing.T) {
	st, _ := openStore(t)
	svc := app.New(st, app.Config{ClickFlush: time.Hour, Logger: quiet})
	l, err := svc.Create(context.Background(), app.Input{Name: "wiki", Target: "https://example.com"}, store.Actor{IP: "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, stopRun := context.WithCancel(context.Background())
	defer stopRun()
	done := make(chan struct{})
	go func() { svc.Run(runCtx); close(done) }()

	entered, release := make(chan struct{}), make(chan struct{})
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release // still serving while Shutdown waits
		svc.RecordClick(l.ID)
		w.WriteHeader(http.StatusFound)
	})}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	reqDone := make(chan error, 1)
	go func() {
		c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		resp, err := c.Get("http://" + ln.Addr().String() + "/wiki")
		if err == nil {
			resp.Body.Close()
		}
		reqDone <- err
	}()
	<-entered

	stopped := make(chan struct{})
	go func() { stopServing(quiet, 5*time.Second, []*http.Server{srv}, stopRun, done); close(stopped) }()
	select {
	case <-done:
		t.Fatal("background work stopped while a request was still being served")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	<-stopped
	if err := <-reqDone; err != nil {
		t.Fatalf("request failed: %v", err)
	}
	got, err := st.GetByID(context.Background(), l.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Clicks != 1 {
		t.Fatalf("clicks = %d, want 1: the click served during shutdown was lost", got.Clicks)
	}
}

func TestSessionsRevoke(t *testing.T) {
	st, db := openStore(t)
	ctx := context.Background()
	now := time.Now()
	add := func(token, email string) {
		t.Helper()
		sum := sha256.Sum256([]byte(token))
		if err := st.CreateSession(ctx, sum[:], store.Session{Provider: "google", Subject: token, Email: email,
			Created: now, Expires: now.Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	live := func() int {
		t.Helper()
		list, err := st.Sessions(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return len(list)
	}
	add("a", "alice@gmail.com")
	add("b", "bob@gmail.com")

	// An empty argument never means "everyone".
	for _, empty := range []string{"", " "} {
		if err := sessions([]string{"--db", db, "revoke", empty}); err == nil {
			t.Errorf("revoke %q succeeded", empty)
		}
	}
	if err := sessions([]string{"--db", db, "revoke"}); !errors.Is(err, errUsage) {
		t.Errorf("revoke without an argument: err = %v, want usage", err)
	}
	if n := live(); n != 2 {
		t.Fatalf("%d sessions left, want 2", n)
	}
	if err := sessions([]string{"--db", db, "revoke", "Alice@Gmail.com"}); err != nil {
		t.Fatal(err)
	}
	if n := live(); n != 1 {
		t.Fatalf("%d sessions left after revoking alice, want 1", n)
	}
	if err := sessions([]string{"--db", db, "revoke", "--all"}); err != nil {
		t.Fatal(err)
	}
	if n := live(); n != 0 {
		t.Fatalf("%d sessions left after revoke --all", n)
	}
}

func writeExport(t *testing.T, links ...web.ExportLink) string {
	t.Helper()
	b, err := json.Marshal(web.Export{Format: web.ExportFormat, Version: 1, Links: links})
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "export.json")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func liveLinks(t *testing.T, st *store.Store) int {
	t.Helper()
	n, err := st.CountLive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestImportCommand(t *testing.T) {
	good := []web.ExportLink{
		{Name: "a", Target: "https://a.example.com"},
		{Name: "b", Target: "https://b.example.com"},
		{Name: "c", Target: "https://c.example.com"},
	}
	t.Run("all valid", func(t *testing.T) {
		st, db := openStore(t)
		if err := importLinks([]string{"--db", db, writeExport(t, good...)}); err != nil {
			t.Fatal(err)
		}
		if n := liveLinks(t, st); n != 3 {
			t.Fatalf("%d links, want 3", n)
		}
	})
	t.Run("invalid entries exit non-zero", func(t *testing.T) {
		st, db := openStore(t)
		f := writeExport(t, append(good, web.ExportLink{Name: "bad name", Target: "https://x.example.com"},
			web.ExportLink{Name: "loop", Target: "https://me.example.com/x"})...)
		err := importLinks([]string{"--db", db, "--public-url", "https://me.example.com", f})
		if err == nil || !strings.Contains(err.Error(), "invalid entries left out: 2") {
			t.Fatalf("err = %v", err)
		}
		if n := liveLinks(t, st); n != 3 {
			t.Fatalf("%d links, want the 3 valid ones", n)
		}
	})
	t.Run("link cap", func(t *testing.T) {
		st, db := openStore(t)
		err := importLinks([]string{"--db", db, "--max-links", "2", writeExport(t, good...)})
		if err == nil || !strings.Contains(err.Error(), "nothing was imported") {
			t.Fatalf("err = %v", err)
		}
		if n := liveLinks(t, st); n != 0 {
			t.Fatalf("%d links imported past the cap", n)
		}
	})
	t.Run("link cap from the environment", func(t *testing.T) {
		st, db := openStore(t)
		t.Setenv("BYNAME_MAX_LINKS", "2")
		if err := importLinks([]string{"--db", db, writeExport(t, good...)}); err == nil {
			t.Fatal("BYNAME_MAX_LINKS ignored")
		}
		if n := liveLinks(t, st); n != 0 {
			t.Fatalf("%d links imported past the cap", n)
		}
	})
	t.Run("dry run", func(t *testing.T) {
		st, db := openStore(t)
		if err := importLinks([]string{"--db", db, "--dry-run", writeExport(t, good...)}); err != nil {
			t.Fatal(err)
		}
		if err := importLinks([]string{"--db", db, "--dry-run", "--max-links", "2", writeExport(t, good...)}); err == nil {
			t.Fatal("dry run over the cap succeeded")
		}
		if n := liveLinks(t, st); n != 0 {
			t.Fatalf("dry run imported %d links", n)
		}
	})
}

func TestDevRefusesPublicAddress(t *testing.T) {
	err := serve([]string{"--dev", "--public-url", "https://me.example.com", "--hosts", "me", "--db", filepath.Join(t.TempDir(), "x.db")})
	if err == nil || !strings.Contains(err.Error(), "--dev") {
		t.Fatalf("serve --dev --public-url: err = %v", err)
	}
}

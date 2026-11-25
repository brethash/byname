package whois

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// fakeTailscaled serves a LocalAPI-shaped whois endpoint on a unix socket and
// enforces the same request rules tailscaled does.
func fakeTailscaled(t *testing.T) (socket string, calls *atomic.Int32) {
	t.Helper()
	dir, err := os.MkdirTemp("", "ts") // short path: unix sockets have a ~104 byte limit on macOS
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket = filepath.Join(dir, "s.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	calls = new(atomic.Int32)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Referer() != "" || r.Header.Get("Origin") != "" || (r.Host != "" && r.Host != localAPIHost) {
			http.Error(w, "invalid localapi request", http.StatusForbidden)
			return
		}
		if r.URL.Path != "/localapi/v0/whois" {
			http.NotFound(w, r)
			return
		}
		switch r.FormValue("addr") {
		case "100.64.0.1":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"Node":{"Name":"alice-phone.tail1234.ts.net.","ComputedName":"alice-phone"},
				"UserProfile":{"LoginName":"alice@example.com","DisplayName":"Alice"},"CapMap":null}`))
		case "100.64.0.9":
			w.Write([]byte(`{"Node":{"Name":"build-box.tail1234.ts.net."},"UserProfile":{"LoginName":"tagged-devices"}}`))
		case "100.64.0.7":
			http.Error(w, "boom", http.StatusInternalServerError)
		default:
			http.Error(w, "no match for IP:port", http.StatusNotFound)
		}
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return socket, calls
}

func TestWhoIs(t *testing.T) {
	sock, calls := fakeTailscaled(t)
	c := New(sock)
	ctx := context.Background()

	id, err := c.WhoIs(ctx, "100.64.0.1:51234")
	if err != nil {
		t.Fatal(err)
	}
	if id != (Identity{Login: "alice@example.com", Display: "Alice", Node: "alice-phone"}) {
		t.Fatalf("identity = %+v", id)
	}
	// Cached: same IP, different port, no new call.
	if _, err := c.WhoIs(ctx, "100.64.0.1:9"); err != nil || calls.Load() != 1 {
		t.Fatalf("expected cache hit; calls=%d err=%v", calls.Load(), err)
	}
	// Node name falls back to the FQDN's first label.
	if id, _ := c.WhoIs(ctx, "100.64.0.9:1"); id.Node != "build-box" || id.Login != "tagged-devices" {
		t.Fatalf("tagged identity = %+v", id)
	}
	if _, err := c.WhoIs(ctx, "100.64.0.2:1"); !errors.Is(err, ErrUnknown) {
		t.Fatalf("unknown peer err = %v", err)
	}
	// Server errors are reported, not cached as identities.
	if _, err := c.WhoIs(ctx, "100.64.0.7:1"); err == nil || errors.Is(err, ErrUnknown) {
		t.Fatalf("server error err = %v", err)
	}
	// IPv4-mapped IPv6 is normalised.
	if id, err := c.WhoIs(ctx, "[::ffff:100.64.0.1]:5"); err != nil || id.Login != "alice@example.com" {
		t.Fatalf("mapped addr = %+v, %v", id, err)
	}
	if _, err := c.WhoIs(ctx, "not-an-ip"); err == nil {
		t.Fatal("bad address accepted")
	}
}

func TestWhoIsSocketMissing(t *testing.T) {
	c := New(filepath.Join(t.TempDir(), "missing.sock"))
	if _, err := c.WhoIs(context.Background(), "100.64.0.1:1"); err == nil {
		t.Fatal("expected error for missing socket")
	}
}

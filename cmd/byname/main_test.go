package main

import (
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"byname/internal/app"
	"byname/internal/link"
	"byname/internal/store"
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

package main

import (
	"io"
	"log/slog"
	"net"
	"net/netip"
	"testing"
	"time"
)

// fakeListener hands out pipe connections with chosen remote addresses.
type fakeListener struct{ conns chan net.Conn }

func (f *fakeListener) Accept() (net.Conn, error) {
	c, ok := <-f.conns
	if !ok {
		return nil, net.ErrClosed
	}
	return c, nil
}
func (f *fakeListener) Close() error   { return nil }
func (f *fakeListener) Addr() net.Addr { return &net.TCPAddr{} }

type addrConn struct {
	net.Conn
	remote net.Addr
	closed bool
}

func (c *addrConn) RemoteAddr() net.Addr { return c.remote }
func (c *addrConn) Close() error         { c.closed = true; return nil }

func dial(f *fakeListener, remote string) *addrConn {
	a, _ := net.Pipe()
	c := &addrConn{Conn: a, remote: net.TCPAddrFromAddrPort(netip.MustParseAddrPort(remote))}
	f.conns <- c
	return c
}

func TestGuardListener(t *testing.T) {
	f := &fakeListener{conns: make(chan net.Conn, 100)}
	trusted := func(ip netip.Addr) bool {
		return ip.IsLoopback() || netip.MustParsePrefix("100.64.0.0/10").Contains(ip)
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	l := guard(f, trusted, 4, 2, quiet)

	accept := func() net.Conn {
		t.Helper()
		done := make(chan net.Conn, 1)
		go func() { c, _ := l.Accept(); done <- c }()
		select {
		case c := <-done:
			return c
		case <-time.After(time.Second):
			t.Fatal("Accept blocked")
			return nil
		}
	}

	// Two from one address are fine; the third is closed, not queued.
	a1 := dial(f, "203.0.113.1:1000")
	if accept() == nil {
		t.Fatal("first connection refused")
	}
	dial(f, "203.0.113.1:1001")
	accept()
	third := dial(f, "203.0.113.1:1002")
	other := dial(f, "198.51.100.9:1000") // Accept skips the refused one and returns this
	if c := accept(); c.RemoteAddr().String() != other.remote.String() || !third.closed {
		t.Fatalf("over the per-address limit: got %v, third closed=%v", c.RemoteAddr(), third.closed)
	}
	// IPv6 addresses in one /64 count together.
	dial(f, "[2001:db8:1:2::a]:1")
	c4 := accept()
	if c4 == nil {
		t.Fatal("IPv6 refused")
	}
	// The listener is now full (4): a new internet peer is refused...
	full := dial(f, "192.0.2.50:1")
	// ...but trusted peers always get in.
	tail := dial(f, "100.64.0.7:1")
	if c := accept(); c.RemoteAddr().String() != tail.remote.String() || !full.closed {
		t.Fatalf("full listener: got %v, internet peer closed=%v", c.RemoteAddr(), full.closed)
	}
	// Closing a connection frees its slot.
	_ = a1
	c4.Close()
	again := dial(f, "192.0.2.50:2")
	if c := accept(); c.RemoteAddr().String() != again.remote.String() {
		t.Fatalf("slot not freed: got %v", c.RemoteAddr())
	}
	c4.Close() // a second Close must not free a second slot
	g := l.(*guardListener)
	if g.total != 4 {
		t.Fatalf("total = %d, want 4", g.total)
	}
}

func TestPeerKey(t *testing.T) {
	if peerKey(netip.MustParseAddr("2001:db8:1:2::1")) != peerKey(netip.MustParseAddr("2001:db8:1:2:ffff::9")) {
		t.Error("one /64 should share a key")
	}
	if peerKey(netip.MustParseAddr("192.0.2.1")) == peerKey(netip.MustParseAddr("192.0.2.2")) {
		t.Error("IPv4 addresses should not share a key")
	}
}

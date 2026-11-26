package main

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"time"

	"byname/internal/logsample"
)

// guardListener limits connections from the internet on a listener that
// faces it, without ever making Accept wait: a peer over its limit is closed
// at once, so one host (or a flood) can't hold the accept queue and lock out
// everyone else. Trusted peers (the private listener's own: tailnet, LAN,
// localhost) are never counted or refused.
type guardListener struct {
	net.Listener
	trusted func(netip.Addr) bool
	max     int // all internet connections
	perPeer int // per IPv4 address or IPv6 /64
	log     *slog.Logger

	// fullLog lets one "listener full" warning through a minute: a flood
	// would otherwise log a line per refused connection.
	fullLog *logsample.Sampler

	mu    sync.Mutex
	total int
	peers map[netip.Prefix]int
}

func guard(l net.Listener, trusted func(netip.Addr) bool, max, perPeer int, log *slog.Logger) net.Listener {
	return &guardListener{Listener: l, trusted: trusted, max: max, perPeer: perPeer, log: log,
		fullLog: logsample.New(time.Minute), peers: make(map[netip.Prefix]int)}
}

// peerKey groups a peer: one IPv4 address, or one IPv6 /64 (one host
// usually owns a whole /64).
func peerKey(ip netip.Addr) netip.Prefix {
	if ip.Is4() {
		return netip.PrefixFrom(ip, 32)
	}
	p, _ := ip.Prefix(64)
	return p
}

func (l *guardListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		ap, err := netip.ParseAddrPort(c.RemoteAddr().String())
		if err != nil {
			c.Close()
			continue
		}
		ip := ap.Addr().Unmap()
		if l.trusted(ip) {
			return c, nil
		}
		key := peerKey(ip)
		l.mu.Lock()
		if l.total >= l.max || l.peers[key] >= l.perPeer {
			full := l.total >= l.max
			l.mu.Unlock()
			c.Close()
			if full {
				if ok, held := l.fullLog.Take(time.Now()); ok {
					l.log.Warn("connection refused: listener full", "remote", ip, "max", l.max, "refused_since_last", held)
				} else {
					l.log.Debug("connection refused: listener full", "remote", ip, "max", l.max)
				}
			} else {
				l.log.Debug("connection refused: too many from one address", "remote", key, "max", l.perPeer)
			}
			continue
		}
		l.total++
		l.peers[key]++
		l.mu.Unlock()
		return &guardedConn{Conn: c, release: func() {
			l.mu.Lock()
			l.total--
			if l.peers[key]--; l.peers[key] <= 0 {
				delete(l.peers, key)
			}
			l.mu.Unlock()
		}}, nil
	}
}

type guardedConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *guardedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}

// serverLogWriter routes net/http's server log lines: TLS handshake errors,
// a steady trickle from internet scanners, go to debug; everything else
// (accept errors, for example) to warn.
type serverLogWriter struct{ log *slog.Logger }

func (w serverLogWriter) Write(p []byte) (int, error) {
	msg := string(bytes.TrimSpace(p))
	level := slog.LevelWarn
	if bytes.Contains(p, []byte("TLS handshake error")) {
		level = slog.LevelDebug
	}
	w.log.Log(context.Background(), level, msg)
	return len(p), nil
}

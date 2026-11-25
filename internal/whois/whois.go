// Package whois asks the local tailscaled who is behind a tailnet address.
//
// It speaks to tailscaled's LocalAPI over its unix socket. On Linux any local
// user may make read-only LocalAPI calls (the socket is 0666 and tailscaled
// grants read permission to unix-socket peers), so the sandboxed, unprivileged
// ot service can call it. tailscaled rejects requests that carry a Referer or
// Origin header or a Host other than "local-tailscaled.sock"; this client sends
// neither.
//
// This deliberately avoids importing tailscale.com: the whole module would be
// linked in for a single GET. The trade-off is that LocalAPI is not a formally
// versioned interface, so callers must treat failures as "identity unknown",
// never as "deny".
package whois

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"
)

// DefaultSocket is tailscaled's socket on Linux.
const DefaultSocket = "/var/run/tailscale/tailscaled.sock"

const localAPIHost = "local-tailscaled.sock"

// Identity is who a tailnet peer is.
type Identity struct {
	Login   string // e.g. alice@example.com ("tagged-devices" for tagged nodes)
	Display string // e.g. Alice Example
	Node    string // short machine name, e.g. alice-phone
}

// ErrUnknown means tailscaled has no peer for the address.
var ErrUnknown = errors.New("whois: no tailnet peer for address")

// Resolver resolves a request's remote address to an identity.
type Resolver interface {
	WhoIs(ctx context.Context, remoteAddr string) (Identity, error)
}

// Nop never knows anyone. Used in development and when WhoIs is disabled.
type Nop struct{}

// WhoIs implements Resolver.
func (Nop) WhoIs(context.Context, string) (Identity, error) { return Identity{}, ErrUnknown }

// Client is a caching LocalAPI WhoIs client.
type Client struct {
	hc  *http.Client
	ttl time.Duration

	mu    sync.Mutex
	cache map[netip.Addr]cached
}

type cached struct {
	id  Identity
	err error
	exp time.Time
}

// New returns a client for the tailscaled socket at path.
func New(socketPath string) *Client {
	d := net.Dialer{Timeout: 2 * time.Second}
	return &Client{
		hc: &http.Client{
			Timeout: 3 * time.Second,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return d.DialContext(ctx, "unix", socketPath)
				},
				MaxIdleConns:    2,
				IdleConnTimeout: 30 * time.Second,
			},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		ttl:   5 * time.Minute,
		cache: make(map[netip.Addr]cached),
	}
}

// whoIsResponse is the subset of tailscale's apitype.WhoIsResponse we use.
type whoIsResponse struct {
	Node *struct {
		Name         string // FQDN with trailing dot
		ComputedName string
	}
	UserProfile *struct {
		LoginName   string
		DisplayName string
	}
}

// WhoIs implements Resolver. remoteAddr is an "ip:port" or "ip".
func (c *Client) WhoIs(ctx context.Context, remoteAddr string) (Identity, error) {
	ip, err := parseIP(remoteAddr)
	if err != nil {
		return Identity{}, err
	}
	now := time.Now()
	c.mu.Lock()
	if e, ok := c.cache[ip]; ok && now.Before(e.exp) {
		c.mu.Unlock()
		return e.id, e.err
	}
	c.mu.Unlock()

	id, err := c.lookup(ctx, ip)
	if err == nil || errors.Is(err, ErrUnknown) {
		c.mu.Lock()
		if len(c.cache) > 1024 { // bounded: household tailnets are small
			clear(c.cache)
		}
		c.cache[ip] = cached{id: id, err: err, exp: now.Add(c.ttl)}
		c.mu.Unlock()
	}
	return id, err
}

func (c *Client) lookup(ctx context.Context, ip netip.Addr) (Identity, error) {
	u := "http://" + localAPIHost + "/localapi/v0/whois?addr=" + url.QueryEscape(ip.String())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Identity{}, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return Identity{}, fmt.Errorf("whois: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Identity{}, fmt.Errorf("whois: %w", err)
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return Identity{}, ErrUnknown
	default:
		return Identity{}, fmt.Errorf("whois: tailscaled returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var r whoIsResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return Identity{}, fmt.Errorf("whois: decode: %w", err)
	}
	var id Identity
	if r.UserProfile != nil {
		id.Login = r.UserProfile.LoginName
		id.Display = r.UserProfile.DisplayName
	}
	if r.Node != nil {
		id.Node = r.Node.ComputedName
		if id.Node == "" {
			id.Node, _, _ = strings.Cut(r.Node.Name, ".")
		}
	}
	return id, nil
}

func parseIP(remoteAddr string) (netip.Addr, error) {
	if ap, err := netip.ParseAddrPort(remoteAddr); err == nil {
		return ap.Addr().Unmap(), nil
	}
	ip, err := netip.ParseAddr(remoteAddr)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("whois: bad address %q", remoteAddr)
	}
	return ip.Unmap(), nil
}

// Package netguard provides an HTTP transport that can only reach an
// allowlist of hosts, never private or loopback addresses, and never
// follows redirects.
package netguard

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"
)

// DefaultHosts are the API hosts Nexus-Hub may reach in process.
var DefaultHosts = []string{"api.github.com", "api.clickup.com", "api.telegram.org", "api.anthropic.com"}

// DeniedError is returned by the dialer for a blocked destination.
type DeniedError struct{ Code string }

func (e *DeniedError) Error() string { return "netguard: " + e.Code }

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// allowedIP rejects loopback, private, link-local, unspecified, multicast
// and carrier-grade NAT addresses.
func allowedIP(ip net.IP) bool {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	addr = addr.Unmap()
	return !(addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() ||
		addr.IsInterfaceLocalMulticast() || addr.IsMulticast() || addr.IsUnspecified() || cgnat.Contains(addr))
}

type guard struct {
	hosts   map[string]bool
	lookup  func(context.Context, string) ([]net.IPAddr, error)
	allowIP func(net.IP) bool
	dialer  *net.Dialer
}

func (g *guard) dial(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, &DeniedError{Code: "host_denied"}
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if !g.hosts[host] {
		return nil, &DeniedError{Code: "host_denied"}
	}
	addrs, err := g.lookup(ctx, host)
	if err != nil {
		return nil, err
	}
	var lastErr error = &DeniedError{Code: "address_denied"}
	for _, a := range addrs {
		if !g.allowIP(a.IP) {
			continue
		}
		// Dial the vetted IP, not the name, so DNS cannot rebind in between.
		conn, err := g.dialer.DialContext(ctx, network, net.JoinHostPort(a.IP.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func newTransport(hosts []string, lookup func(context.Context, string) ([]net.IPAddr, error), allowIP func(net.IP) bool) *http.Transport {
	g := &guard{hosts: map[string]bool{}, lookup: lookup, allowIP: allowIP, dialer: &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}}
	for _, h := range hosts {
		g.hosts[strings.ToLower(h)] = true
	}
	return &http.Transport{
		Proxy:                 nil,
		DialContext:           g.dial,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
	}
}

// NewTransport returns a transport restricted to allowedHosts.
func NewTransport(allowedHosts []string) *http.Transport {
	return newTransport(allowedHosts, net.DefaultResolver.LookupIPAddr, allowedIP)
}

func client(timeout time.Duration, tr http.RoundTripper) *http.Client {
	return &http.Client{
		Transport:     tr,
		Timeout:       timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// Client returns an HTTP client restricted to hosts that never follows
// redirects.
func Client(timeout time.Duration, hosts []string) *http.Client {
	return client(timeout, NewTransport(hosts))
}

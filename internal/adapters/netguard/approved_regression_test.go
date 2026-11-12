package netguard

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// TC-17 (REQ-8): Control refuses before connect, so these four approved
// public DNS destinations are observed without sending real network traffic.
func TestApproved_FourAPIHostsDialOnlyVettedIP(t *testing.T) {
	for _, host := range []string{"api.github.com", "api.clickup.com", "api.telegram.org", "api.anthropic.com"} {
		t.Run(host, func(t *testing.T) {
			lookups, dials := 0, 0
			stop := errors.New("test stopped socket before connect")
			g := &guard{hosts: map[string]bool{host: true}, allowIP: allowedIP, dialer: &net.Dialer{Timeout: time.Second}}
			g.lookup = func(_ context.Context, name string) ([]net.IPAddr, error) {
				lookups++
				if name != host {
					t.Error("wrong DNS name")
				}
				return []net.IPAddr{{IP: net.ParseIP("140.82.112.6")}}, nil
			}
			g.dialer.Control = func(_, address string, _ syscall.RawConn) error {
				dials++
				if address != "140.82.112.6:443" {
					t.Error("dial used an unvetted DNS name or IP")
				}
				return stop
			}
			conn, err := g.dial(context.Background(), "tcp4", net.JoinHostPort(host, "443"))
			if conn != nil || !errors.Is(err, stop) || lookups != 1 || dials != 1 {
				t.Error("allowlisted host did not use exactly one vetted address lookup")
			}
		})
	}
}

// TC-18 (REQ-8): rebinding and spoofing never reach a socket or redirect target.
func TestApproved_DNSRebindingAndHostSpoofBlocked(t *testing.T) {
	dials, lookups := 0, 0
	g := &guard{hosts: map[string]bool{"api.github.com": true}, allowIP: allowedIP, dialer: &net.Dialer{Timeout: time.Second}}
	g.lookup = func(context.Context, string) ([]net.IPAddr, error) {
		lookups++
		ip := "140.82.112.6"
		if lookups > 1 {
			ip = "127.0.0.1"
		}
		return []net.IPAddr{{IP: net.ParseIP(ip)}}, nil
	}
	g.dialer.Control = func(_, _ string, _ syscall.RawConn) error { dials++; return errors.New("test veto before connect") }
	_, _ = g.dial(context.Background(), "tcp4", "api.github.com:443")
	_, err := g.dial(context.Background(), "tcp4", "api.github.com:443")
	var de *DeniedError
	if !errors.As(err, &de) || de.Code != "address_denied" || dials != 1 {
		t.Error("rebound private address reached dialer")
	}
	for _, host := range []string{"api.github.com.attacker.invalid", "api.github.com@attacker.invalid", "github.com", "127.0.0.1", "[::1]"} {
		_, err := g.dial(context.Background(), "tcp", host+":443")
		if !errors.As(err, &de) {
			t.Errorf("spoof host accepted: %s", host)
		}
	}
	var targetCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { targetCalls.Add(1) }))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }))
	defer origin.Close()
	c := client(time.Second, newTransport([]string{"api.github.com"}, mapTo("127.0.0.1"), func(net.IP) bool { return true }))
	resp, err := c.Get("http://api.github.com:" + serverPort(t, origin) + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 302 || targetCalls.Load() != 0 {
		t.Error("redirect target was contacted")
	}
}

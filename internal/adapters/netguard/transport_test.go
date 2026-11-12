package netguard

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// mapTo returns a resolver that maps every name to the given IP.
func mapTo(ip string) func(context.Context, string) ([]net.IPAddr, error) {
	return func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP(ip)}}, nil
	}
}

func serverPort(t *testing.T, s *httptest.Server) string {
	_, port, err := net.SplitHostPort(strings.TrimPrefix(s.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func TestNetguard_AllowsListedHost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "hi") }))
	defer srv.Close()
	tr := newTransport([]string{"api.github.com"}, mapTo("127.0.0.1"), func(net.IP) bool { return true })
	c := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	resp, err := c.Get("http://api.github.com:" + serverPort(t, srv) + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if string(b) != "hi" {
		t.Fatalf("body %q", b)
	}
}

func TestNetguard_DeniesUnlisted(t *testing.T) {
	c := &http.Client{Transport: newTransport([]string{"api.github.com"}, mapTo("140.82.112.6"), allowedIP), Timeout: 2 * time.Second}
	_, err := c.Get("http://evil.example.com/")
	var de *DeniedError
	if !errors.As(err, &de) || de.Code != "host_denied" {
		t.Fatalf("got %v", err)
	}
	// Default constructor also denies.
	_, err = Client(2*time.Second, []string{"api.github.com"}).Get("http://127.0.0.1:1/")
	if !errors.As(err, &de) {
		t.Fatalf("IP literal: got %v", err)
	}
}

func TestNetguard_DeniesPrivateIP(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "10.0.0.5", "192.168.1.1", "172.16.0.1", "169.254.169.254", "0.0.0.0", "::1", "fe80::1", "fc00::1", "100.64.0.1"} {
		c := &http.Client{Transport: newTransport([]string{"api.github.com"}, mapTo(ip), allowedIP), Timeout: 2 * time.Second}
		_, err := c.Get("http://api.github.com/")
		var de *DeniedError
		if !errors.As(err, &de) || de.Code != "address_denied" {
			t.Errorf("%s: got %v", ip, err)
		}
	}
}

func TestNetguard_NoRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest", http.StatusFound)
	}))
	defer srv.Close()
	c := client(5*time.Second, newTransport([]string{"api.github.com"}, mapTo("127.0.0.1"), func(net.IP) bool { return true }))
	resp, err := c.Get("http://api.github.com:" + serverPort(t, srv) + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("redirect followed: %d", resp.StatusCode)
	}
}

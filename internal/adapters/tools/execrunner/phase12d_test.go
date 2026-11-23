package execrunner

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func writeFakeBwrap(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bwrap")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func argValues(args []string, flag string) []string {
	var values []string
	for i := 0; i+2 < len(args); i++ {
		if args[i] == flag {
			values = append(values, args[i+1], args[i+2])
			i += 2
		}
	}
	return values
}

// TC-D-1 (REQ-14): sandbox shape is fixed, and only network operations share
// the host network namespace. Writable mounts are limited to workspace and HOME.
func TestPhase12dSandboxArgvContract(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "workspace")
	home := filepath.Join(base, "runner-home")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	r := &Runner{gitPath: fakeGitPath, home: home}
	for _, tc := range []struct {
		name    string
		network bool
	}{
		{name: "local"},
		{name: "network", network: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args, err := r.sandboxArgs(root, tc.network)
			if err != nil {
				t.Fatal(err)
			}
			joined := "\x00" + strings.Join(args, "\x00") + "\x00"
			for _, want := range []string{"--unshare-all", "--die-with-parent", "--new-session", "--tmpfs\x00/", "--proc\x00/proc", "--dev\x00/dev", "--chdir\x00" + root} {
				if !strings.Contains(joined, "\x00"+want+"\x00") {
					t.Errorf("sandbox argv misses %q: %q", want, args)
				}
			}
			if got := strings.Contains(joined, "\x00--share-net\x00"); got != tc.network {
				t.Errorf("--share-net present=%v, network=%v: %q", got, tc.network, args)
			}
			binds := argValues(args, "--bind")
			wantBinds := []string{root, root, home, home}
			if strings.Join(binds, "\x00") != strings.Join(wantBinds, "\x00") {
				t.Errorf("writable binds=%q, want %q", binds, wantBinds)
			}
			for _, pair := range argValues(args, "--ro-bind") {
				if strings.HasPrefix(pair, base) && pair != fakeGitPath {
					t.Errorf("unexpected test-directory read bind %q", pair)
				}
			}
		})
	}
}

// TC-D-2 (REQ-14): hostile or ambiguous working directories fail before exec.
func TestPhase12dSandboxRejectsWorkingDirectoryTricks(t *testing.T) {
	r := &Runner{gitPath: fakeGitPath, home: t.TempDir(), bwrapPath: "/missing/bwrap", sem: make(chan struct{}, 1)}
	for _, dir := range []string{"relative", t.TempDir() + "/..", "/home", "/home/owner/repo"} {
		_, err := r.Run(context.Background(), Command{Args: []string{"status"}, Dir: dir})
		var denied *DeniedError
		if !errors.As(err, &denied) || denied.Code != "working_directory_denied" {
			t.Errorf("dir %q: got %v", dir, err)
		}
	}
}

func captureBwrap(t *testing.T) (*Runner, string, string) {
	t.Helper()
	dir := t.TempDir()
	argvPath := filepath.Join(dir, "argv")
	envPath := filepath.Join(dir, "env")
	script := fmt.Sprintf(`printf '%%s\n' "$@" > %q
env > %q
while [ "$#" -gt 0 ]; do
  if [ "$1" = "--" ]; then shift; exec "$@"; fi
  shift
done
exit 0`, argvPath, envPath)
	bwrap := writeFakeBwrap(t, script)
	r, err := NewSandboxed(fakeGitPath, t.TempDir(), SandboxConfig{BwrapPath: bwrap, ProbeTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	r.asBytes = 8 << 30
	t.Cleanup(func() { _ = r.Close() })
	return r, argvPath, envPath
}

// TC-D-3 (REQ-14): credentials remain in env, never argv. HTTPS and proxy
// controls are injected only by the sandbox and only when networking is used.
func TestPhase12dSandboxEnvironmentAndTokenBoundary(t *testing.T) {
	r, argvPath, envPath := captureBwrap(t)
	secret := "phase12d-token-must-not-enter-argv"
	_, err := r.Run(context.Background(), Command{
		Args:    []string{"clone", "https://github.com/acme/app.git", "repo"},
		Dir:     t.TempDir(),
		Network: true,
		Env:     []string{"GIT_CONFIG_VALUE_0=x-access-token:" + secret},
	})
	if err != nil {
		t.Fatal(err)
	}
	argv, _ := os.ReadFile(argvPath)
	env, _ := os.ReadFile(envPath)
	if bytes.Contains(argv, []byte(secret)) {
		t.Fatal("token entered bwrap argv")
	}
	for _, want := range []string{"GIT_ALLOW_PROTOCOL=https", "https_proxy=http://127.0.0.1:", "HTTPS_PROXY=http://127.0.0.1:", secret} {
		if !bytes.Contains(env, []byte(want)) {
			t.Errorf("sandbox env misses %q", want)
		}
	}

	_, err = r.Run(context.Background(), Command{Args: []string{"status"}, Dir: t.TempDir(), Network: true})
	if err != nil {
		t.Fatal(err)
	}
	env, _ = os.ReadFile(envPath)
	if !bytes.Contains(env, []byte("GIT_ALLOW_PROTOCOL=https")) || bytes.Contains(env, []byte("https_proxy=")) || bytes.Contains(env, []byte("HTTPS_PROXY=")) {
		t.Fatalf("local sandbox env has wrong network controls: %s", env)
	}
}

// TC-D-4 (REQ-14): the legacy runner contract remains unchanged when the
// explicit development escape hatch selects New.
func TestPhase12dUnsandboxedRunnerDoesNotInjectSandboxEnv(t *testing.T) {
	r := newRunner(t)
	res, err := r.Run(context.Background(), Command{Args: []string{"status"}})
	if err != nil {
		t.Fatal(err)
	}
	env := string(res.Stdout)
	for _, bad := range []string{"GIT_ALLOW_PROTOCOL=", "https_proxy=", "HTTPS_PROXY="} {
		if strings.Contains(env, bad) {
			t.Errorf("unsandboxed runner injected %q", bad)
		}
	}
}

func proxyRequest(t *testing.T, addr, raw string) (net.Conn, *bufio.Reader, int) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(conn, raw); err != nil {
		conn.Close()
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil {
		conn.Close()
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
	}
	return conn, reader, resp.StatusCode
}

// TC-D-5 (REQ-14): the one allowed CONNECT target forms a bidirectional
// tunnel, and the dial seam receives no attacker-selected destination.
func TestPhase12dProxyAllowsOnlyGitHubTunnel(t *testing.T) {
	oldDial := proxyDialContext
	defer func() { proxyDialContext = oldDial }()
	serverSide, upstream := net.Pipe()
	defer upstream.Close()
	proxyDialContext = func(_ context.Context, network, address string) (net.Conn, error) {
		if network != "tcp" || address != allowedConnectTarget {
			t.Fatalf("dial %s %s", network, address)
		}
		return serverSide, nil
	}
	p, err := startConnectProxy()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	client, reader, status := proxyRequest(t, p.Addr(), "CONNECT github.com:443 HTTP/1.1\r\nHost: github.com:443\r\n\r\n")
	defer client.Close()
	if status != http.StatusOK {
		t.Fatalf("status=%d", status)
	}
	if _, err := client.Write([]byte("client-data")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len("client-data"))
	if _, err := io.ReadFull(upstream, buf); err != nil || string(buf) != "client-data" {
		t.Fatalf("upstream read=%q err=%v", buf, err)
	}
	if _, err := upstream.Write([]byte("server-data")); err != nil {
		t.Fatal(err)
	}
	buf = make([]byte, len("server-data"))
	if _, err := io.ReadFull(reader, buf); err != nil || string(buf) != "server-data" {
		t.Fatalf("client read=%q err=%v", buf, err)
	}
}

// TC-D-6 (REQ-14): malformed and disguised proxy requests are refused without
// a dial. Logs expose fixed reason codes only.
func TestPhase12dProxyRejectsHostAndProtocolTricks(t *testing.T) {
	oldDial := proxyDialContext
	defer func() { proxyDialContext = oldDial }()
	var dials atomic.Int32
	proxyDialContext = func(context.Context, string, string) (net.Conn, error) {
		dials.Add(1)
		return nil, errors.New("unexpected dial")
	}
	var logs bytes.Buffer
	oldWriter, oldFlags := log.Writer(), log.Flags()
	log.SetOutput(&logs)
	log.SetFlags(0)
	defer func() { log.SetOutput(oldWriter); log.SetFlags(oldFlags) }()
	p, err := startConnectProxy()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	cases := []struct {
		name string
		raw  string
		want int
	}{
		{"other host", "CONNECT attacker.example:443 HTTP/1.1\r\nHost: attacker.example:443\r\n\r\n", 403},
		{"wrong port", "CONNECT github.com:80 HTTP/1.1\r\nHost: github.com:80\r\n\r\n", 403},
		{"case trick", "CONNECT GitHub.com:443 HTTP/1.1\r\nHost: GitHub.com:443\r\n\r\n", 403},
		{"absolute get", "GET https://github.com/ HTTP/1.1\r\nHost: github.com\r\n\r\n", 403},
		{"plain get", "GET / HTTP/1.1\r\nHost: github.com:443\r\n\r\n", 403},
		{"malformed", "not-http\r\n\r\n", 400},
		{"oversize", "CONNECT github.com:443 HTTP/1.1\r\nX-Large: " + strings.Repeat("x", 20<<10), 400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn, _, status := proxyRequest(t, p.Addr(), tc.raw)
			conn.Close()
			if status != tc.want {
				t.Errorf("status=%d, want %d", status, tc.want)
			}
		})
	}
	if dials.Load() != 0 {
		t.Fatalf("denied requests dialed %d times", dials.Load())
	}
	gotLogs := logs.String()
	for _, want := range []string{"request_denied", "malformed_request"} {
		if !strings.Contains(gotLogs, want) {
			t.Errorf("logs miss %q: %s", want, gotLogs)
		}
	}
	for _, hostile := range []string{"attacker.example", "GitHub.com", "X-Large"} {
		if strings.Contains(gotLogs, hostile) {
			t.Errorf("logs exposed request data %q: %s", hostile, gotLogs)
		}
	}
}

// TC-D-7 (REQ-14): shutdown closes live clients and tunnels and remains
// idempotent instead of waiting forever for io.Copy.
func TestPhase12dProxyCloseStopsOpenTunnel(t *testing.T) {
	oldDial := proxyDialContext
	defer func() { proxyDialContext = oldDial }()
	serverSide, upstream := net.Pipe()
	defer upstream.Close()
	proxyDialContext = func(context.Context, string, string) (net.Conn, error) { return serverSide, nil }
	p, err := startConnectProxy()
	if err != nil {
		t.Fatal(err)
	}
	client, _, status := proxyRequest(t, p.Addr(), "CONNECT github.com:443 HTTP/1.1\r\nHost: github.com:443\r\n\r\n")
	defer client.Close()
	if status != http.StatusOK {
		t.Fatalf("status=%d", status)
	}
	done := make(chan error, 1)
	go func() { done <- p.Close() }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close hung with an open tunnel")
	}
	if err := p.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TC-D-8 (REQ-14): invalid bubblewrap paths and failed probes cannot produce
// a usable sandboxed runner.
func TestPhase12dNewSandboxedFailsClosed(t *testing.T) {
	nonExec := filepath.Join(t.TempDir(), "bwrap")
	if err := os.WriteFile(nonExec, []byte("exit 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	wrongName := filepath.Join(t.TempDir(), "not-bwrap")
	if err := os.WriteFile(wrongName, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	failsProbe := writeFakeBwrap(t, "exit 7")
	timesOut := writeFakeBwrap(t, "sleep 1")
	for _, tc := range []struct {
		name    string
		path    string
		timeout time.Duration
	}{
		{"missing", filepath.Join(t.TempDir(), "bwrap"), time.Second},
		{"non executable", nonExec, time.Second},
		{"wrong basename", wrongName, time.Second},
		{"probe failure", failsProbe, time.Second},
		{"probe timeout", timesOut, 10 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := NewSandboxed(fakeGitPath, t.TempDir(), SandboxConfig{BwrapPath: tc.path, ProbeTimeout: tc.timeout})
			if err == nil || r != nil {
				if r != nil {
					_ = r.Close()
				}
				t.Fatalf("runner=%v err=%v", r, err)
			}
		})
	}
}

// TC-D-15 (REQ-14): caller input cannot select a helper program, force a
// privileged push, or inject process controls. Rejection happens before exec.
func TestPhase12dCallerCannotChangeExecutionPolicy(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "executed")
	bwrap := writeFakeBwrap(t, "touch "+marker+"\nexit 0")
	r := &Runner{gitPath: fakeGitPath, home: t.TempDir(), bwrapPath: bwrap, sem: make(chan struct{}, 1)}
	cases := []Command{
		{Args: []string{"clone", "--upload-pack=/bin/sh", "https://github.com/acme/app.git"}, Network: true},
		{Args: []string{"push", "--force", "origin", "main"}, Network: true},
		{Args: []string{"push", "+main:main", "origin"}, Network: true},
		{Args: []string{"status"}, Env: []string{"LD_PRELOAD=/tmp/evil.so"}},
		{Args: []string{"credential", "fill"}},
	}
	for _, cmd := range cases {
		_, err := r.Run(context.Background(), cmd)
		var denied *DeniedError
		if !errors.As(err, &denied) {
			t.Errorf("command %q was not denied: %v", cmd.Args, err)
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("denied input reached bwrap: %v", err)
	}
}

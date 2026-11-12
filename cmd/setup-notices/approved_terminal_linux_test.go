//go:build linux

package main

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TC-25 (REQ-12): removing no-echo must expose the fake secret on the
// PTY master. Exercise the actual secret-set command, not a mocked reader.
func TestApproved_SecretSetRealTTYDoesNotEchoAndRestoresTerminal(t *testing.T) {
	ctx, pool := setupDB(t)
	ws, _, _ := initWorkspace(t, ctx, pool)
	lookup := keyEnv(t, nil)
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal("PTY unavailable")
	}
	defer master.Close()
	fd := int(master.Fd())
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	n, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer slave.Close()
	original, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	original.Lflag |= unix.ECHO
	if err := unix.IoctlSetTermios(int(slave.Fd()), unix.TCSETS, original); err != nil {
		t.Fatal(err)
	}
	oldIn, oldPrompt := stdin, promptOut
	stdin, promptOut = slave, slave
	defer func() { stdin, promptOut = oldIn, oldPrompt }()
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- runCommand(ctx, pool, []string{"secret", "set", "--workspace-id", ws, "--name", "tty-secret", "--kind", "github_pat"}, &out, lookup)
	}()
	deadline := time.Now().Add(3 * time.Second)
	hidden := false
	for time.Now().Before(deadline) {
		state, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
		if err != nil {
			t.Fatal(err)
		}
		if state.Lflag&unix.ECHO == 0 {
			hidden = true
			break
		}
		time.Sleep(time.Millisecond)
	}
	// Supply input even if echo was left on, so the goroutine is always joined.
	value := "fake-private-terminal-secret"
	if _, err := master.WriteString(value + "\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("TTY command hung")
	}
	if !hidden {
		t.Error("secret input was not hidden before reading")
	}
	state, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
	if err != nil || state.Lflag != original.Lflag {
		t.Error("TTY flags not restored")
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 4096)
	count, err := unix.Read(fd, b)
	if err != nil {
		t.Fatal("cannot read terminal transcript")
	}
	if strings.Contains(string(b[:count]), value) || strings.Contains(out.String(), value) {
		t.Error("terminal or CLI echoed secret")
	}
	if !strings.Contains(string(b[:count]), "input hidden") {
		t.Error("user got no safe hidden-input prompt")
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM credentials WHERE name='tty-secret' AND octet_length(nonce)=12`).Scan(&rows); err != nil || rows != 1 {
		t.Error("TTY secret not encrypted and stored")
	}
}

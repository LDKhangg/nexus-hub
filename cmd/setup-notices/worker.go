package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage"
	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
	"github.com/jackc/pgx/v5/pgxpool"
)

const workerUsage = `usage: worker <command>
  worker devices --workspace-id <uuid> [--json]
  worker revoke --workspace-id <uuid> <name>
  worker enroll --hub <url> [--name <device>] [--capabilities a,b]   (laptop; needs no database)`

var workerNameSanitizeRE = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

// workerCommand serves the hub-side commands that need the database.
func workerCommand(ctx context.Context, pool *pgxpool.Pool, args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(workerUsage)
	}
	store, err := storage.NewWorkerStore(pool)
	if err != nil {
		return errors.New("worker store unavailable")
	}
	f := commandFlags("worker " + args[0])
	ws := f.String("workspace-id", "", "workspace UUID")
	asJSON := f.Bool("json", false, "print JSON")
	switch args[0] {
	case "devices":
		if f.Parse(args[1:]) != nil || f.NArg() != 0 || !tenant.ValidUUID(*ws) {
			return errors.New(workerUsage)
		}
		devices, err := store.ListDevices(ctx, *ws)
		if err != nil {
			return errors.New("database operation failed")
		}
		type row struct {
			ID           string     `json:"id"`
			Name         string     `json:"name"`
			State        string     `json:"state"`
			Capabilities []string   `json:"capabilities"`
			LastSeenAt   *time.Time `json:"last_seen_at"`
			CreatedAt    time.Time  `json:"created_at"`
			RevokedAt    *time.Time `json:"revoked_at"`
		}
		now := time.Now()
		rows := make([]row, 0, len(devices))
		for _, d := range devices {
			caps := d.Capabilities
			if caps == nil {
				caps = []string{}
			}
			rows = append(rows, row{ID: d.ID, Name: d.Name, State: d.DisplayState(now), Capabilities: caps,
				LastSeenAt: d.LastSeenAt, CreatedAt: d.CreatedAt, RevokedAt: d.RevokedAt})
		}
		if *asJSON {
			enc := json.NewEncoder(out)
			enc.SetIndent("", "  ")
			return enc.Encode(rows)
		}
		for _, r := range rows {
			seen := "never"
			if r.LastSeenAt != nil {
				seen = r.LastSeenAt.UTC().Format(time.RFC3339)
			}
			fmt.Fprintf(out, "%s\t%s\tlast seen %s\t%s\n", r.Name, r.State, seen, strings.Join(r.Capabilities, ","))
		}
		return nil
	case "revoke":
		if f.Parse(args[1:]) != nil || f.NArg() != 1 || !tenant.ValidUUID(*ws) {
			return errors.New(workerUsage)
		}
		if err := store.RevokeDevice(ctx, *ws, f.Arg(0)); err != nil {
			if errors.Is(err, behavior.ErrNotFound) {
				return errors.New("no active device with that name")
			}
			return errors.New("database operation failed")
		}
		fmt.Fprintln(out, "revoked")
		return nil
	}
	return errors.New(workerUsage)
}

// workerEnroll runs on the laptop: it trades a one-time code for a device
// token and stores it with mode 0600. The code is read from the terminal or
// stdin, never argv; neither the code nor the token is printed.
func workerEnroll(ctx context.Context, args []string, out io.Writer, lookup func(string) (string, bool)) error {
	f := commandFlags("worker enroll")
	hub := f.String("hub", "", "hub base URL")
	name := f.String("name", "", "device name")
	caps := f.String("capabilities", "", "comma-separated capability profile")
	if f.Parse(args) != nil || f.NArg() != 0 || *hub == "" {
		return errors.New(workerUsage)
	}
	base, err := url.Parse(strings.TrimRight(*hub, "/"))
	if err != nil || base.Host == "" || base.RawQuery != "" || base.Fragment != "" || base.User != nil {
		return errors.New("invalid --hub URL")
	}
	host := base.Hostname()
	loopback := host == "localhost" || host == "127.0.0.1" || host == "::1"
	if base.Scheme != "https" && !(base.Scheme == "http" && loopback) {
		return errors.New("--hub must be an https URL")
	}
	if *name == "" {
		h, _ := os.Hostname()
		*name = workerNameSanitizeRE.ReplaceAllString(h, "-")
		if len(*name) > 40 {
			*name = (*name)[:40]
		}
		if *name == "" || *name == "-" {
			*name = "laptop"
		}
	}
	var capList []string
	for _, c := range strings.Split(*caps, ",") {
		if c = strings.TrimSpace(c); c != "" {
			capList = append(capList, c)
		}
	}
	tokenPath, err := workerTokenPath(lookup)
	if err != nil {
		return err
	}
	code, err := readEnrollCode()
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]any{"code": code, "name": *name, "capabilities": capList})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base.String()+"/worker/v1/enroll", bytes.NewReader(body))
	if err != nil {
		return errors.New("invalid --hub URL")
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("could not reach the hub")
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return errors.New("could not read the hub response")
	}
	if resp.StatusCode != http.StatusOK {
		switch resp.StatusCode {
		case http.StatusForbidden:
			return errors.New("enrollment code is not valid or has expired")
		case http.StatusConflict:
			return errors.New("device name is already in use; pass --name")
		case http.StatusTooManyRequests:
			return errors.New("too many attempts; wait and retry")
		}
		return fmt.Errorf("enrollment failed (HTTP %d)", resp.StatusCode)
	}
	var result struct {
		Name  string `json:"name"`
		Token string `json:"token"`
	}
	if json.Unmarshal(raw, &result) != nil || result.Token == "" {
		return errors.New("unexpected hub response")
	}
	if err := writeWorkerToken(tokenPath, result.Token); err != nil {
		return err
	}
	fmt.Fprintf(out, "enrolled as %s; token saved to %s\n", result.Name, tokenPath)
	return nil
}

func workerTokenPath(lookup func(string) (string, bool)) (string, error) {
	home, _ := lookup("HOME")
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return "", errors.New("home directory unavailable")
		}
		home = h
	}
	return filepath.Join(home, ".config", "nexus-hub", "worker-token"), nil
}

func readEnrollCode() (string, error) {
	var data []byte
	if f, ok := stdin.(*os.File); ok && isTerminal(f) {
		fmt.Fprint(promptOut, "enrollment code (input hidden): ")
		line, err := readHidden(f)
		fmt.Fprintln(promptOut)
		if err != nil {
			return "", errors.New("could not read the code from the terminal")
		}
		data = line
	} else {
		b, err := io.ReadAll(io.LimitReader(stdin, 1024))
		if err != nil {
			return "", errors.New("could not read the code from stdin")
		}
		data = b
	}
	code := strings.TrimSpace(string(data))
	if code == "" || len(code) > 64 {
		return "", errors.New("enrollment code is empty or invalid")
	}
	return code, nil
}

// writeWorkerToken stores the token in a 0700 directory with a 0600 file,
// replacing any earlier token atomically.
func writeWorkerToken(path, token string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return errors.New("could not create the token directory")
	}
	tmp, err := os.CreateTemp(dir, ".worker-token-*")
	if err != nil {
		return errors.New("could not write the token file")
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return errors.New("could not write the token file")
	}
	if _, err := tmp.WriteString(token + "\n"); err != nil {
		tmp.Close()
		return errors.New("could not write the token file")
	}
	if err := tmp.Close(); err != nil {
		return errors.New("could not write the token file")
	}
	if err := os.Rename(tmpName, path); err != nil {
		return errors.New("could not write the token file")
	}
	return nil
}

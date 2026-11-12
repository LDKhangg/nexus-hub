package secrets

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type memStore struct {
	mu   sync.Mutex
	rows map[string]memRow
	gets int
}

type memRow struct {
	kind      string
	nonce, ct []byte
}

func newMemStore() *memStore { return &memStore{rows: map[string]memRow{}} }

func (m *memStore) Put(_ context.Context, ws, name, kind string, nonce, ct []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows[ws+"|"+name] = memRow{kind, nonce, ct}
	return nil
}
func (m *memStore) Get(_ context.Context, ws, name string) (string, []byte, []byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gets++
	r, ok := m.rows[ws+"|"+name]
	if !ok {
		return "", nil, nil, ErrNotFound
	}
	return r.kind, r.nonce, r.ct, nil
}
func (m *memStore) List(context.Context, string) ([]Meta, error) { return nil, nil }
func (m *memStore) Delete(_ context.Context, ws, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.rows, ws+"|"+name)
	return nil
}

type recRedactor struct {
	mu   sync.Mutex
	vals []string
}

func (r *recRedactor) Add(v string) { r.mu.Lock(); r.vals = append(r.vals, v); r.mu.Unlock() }

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func TestResolver_CacheTTL(t *testing.T) {
	st, clk := newMemStore(), &clock{t: time.Unix(1000, 0)}
	r := NewResolver(st, testKey(t), &recRedactor{}, nil, 5*time.Minute, clk.now)
	if err := r.SetSecret(context.Background(), "ws", "gh", "github_pat", "ghp_secret_value"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if v, err := r.Resolve(context.Background(), "ws", "gh"); err != nil || v != "ghp_secret_value" {
			t.Fatalf("%q %v", v, err)
		}
	}
	if st.gets != 1 {
		t.Fatalf("store hits %d", st.gets)
	}
	clk.t = clk.t.Add(5*time.Minute + time.Second)
	if _, err := r.Resolve(context.Background(), "ws", "gh"); err != nil || st.gets != 2 {
		t.Fatalf("after ttl gets=%d %v", st.gets, err)
	}
	// SetSecret invalidates the cached value.
	_ = r.SetSecret(context.Background(), "ws", "gh", "github_pat", "ghp_new_value_x")
	if v, _ := r.Resolve(context.Background(), "ws", "gh"); v != "ghp_new_value_x" {
		t.Fatalf("stale %q", v)
	}
}

func TestResolver_RegistersRedaction(t *testing.T) {
	red := &recRedactor{}
	r := NewResolver(newMemStore(), testKey(t), red, func(string) (string, bool) { return "legacy-token-value", true }, time.Minute, time.Now)
	_ = r.SetSecret(context.Background(), "ws", "gh", "github_pat", "ghp_secret_value")
	_, _ = r.Resolve(context.Background(), "ws", "gh")
	_, _ = r.Resolve(context.Background(), "ws", "env:LEGACY")
	joined := strings.Join(red.vals, ",")
	if !strings.Contains(joined, "ghp_secret_value") || !strings.Contains(joined, "legacy-token-value") {
		t.Fatalf("redactor %v", red.vals)
	}
}

func TestResolver_MissingAndDecryptErrors(t *testing.T) {
	st := newMemStore()
	key := testKey(t)
	r := NewResolver(st, key, &recRedactor{}, nil, time.Minute, time.Now)
	code := func(err error) string {
		var se *Error
		if errors.As(err, &se) {
			return se.Code
		}
		return "?"
	}
	if _, err := r.Resolve(context.Background(), "ws", "nope"); code(err) != CodeMissingCredentials {
		t.Fatalf("missing: %v", err)
	}
	_ = r.SetSecret(context.Background(), "ws", "gh", "github_pat", "ghp_secret_value")
	row := st.rows["ws|gh"]
	st.rows["ws|copied"] = row // copied to another name: AAD mismatch
	_, err := r.Resolve(context.Background(), "ws", "copied")
	if code(err) != CodeDecryptFailed {
		t.Fatalf("copied: %v", err)
	}
	if strings.Contains(err.Error(), "ghp_secret_value") || strings.Contains(err.Error(), string(row.ct)) {
		t.Fatal("error leaks material")
	}
	other := NewResolver(st, testKey(t), &recRedactor{}, nil, time.Minute, time.Now)
	if _, err := other.Resolve(context.Background(), "ws", "gh"); code(err) != CodeDecryptFailed {
		t.Fatalf("wrong key: %v", err)
	}
	nokey := NewResolver(st, nil, &recRedactor{}, nil, time.Minute, time.Now)
	if _, err := nokey.Resolve(context.Background(), "ws", "gh"); code(err) != CodeMasterKeyUnavailable {
		t.Fatalf("nil key: %v", err)
	}
	if err := nokey.SetSecret(context.Background(), "ws", "gh", "github_pat", "x"); code(err) != CodeMasterKeyUnavailable {
		t.Fatalf("nil key set: %v", err)
	}
	if _, err := r.Resolve(context.Background(), "ws", "Bad Ref"); code(err) != CodeMissingCredentials {
		t.Fatalf("bad ref: %v", err)
	}
}

func TestResolver_LegacyEnvRef(t *testing.T) {
	calls := 0
	env := func(name string) (string, bool) {
		calls++
		if name == "NEXUS_TELEGRAM_BOT_TOKEN" {
			return "123456:legacy", true
		}
		return "", false
	}
	r := NewResolver(newMemStore(), nil, &recRedactor{}, env, time.Minute, time.Now)
	if v, err := r.Resolve(context.Background(), "ws", "env:NEXUS_TELEGRAM_BOT_TOKEN"); err != nil || v != "123456:legacy" {
		t.Fatalf("%q %v", v, err)
	}
	var se *Error
	if _, err := r.Resolve(context.Background(), "ws", "env:UNSET"); !errors.As(err, &se) || se.Code != CodeMissingCredentials {
		t.Fatalf("unset: %v", err)
	}
	if _, err := r.Resolve(context.Background(), "ws", "env:bad name"); err == nil {
		t.Fatal("invalid env name accepted")
	}
	noEnv := NewResolver(newMemStore(), nil, &recRedactor{}, nil, time.Minute, time.Now)
	if _, err := noEnv.Resolve(context.Background(), "ws", "env:X"); err == nil {
		t.Fatal("env ref without legacy lookup accepted")
	}
}

package secrets

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func testKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return k
}

func TestSealOpenRoundTrip(t *testing.T) {
	k := testKey(t)
	aad := AAD("ws-1", "gh", "github_pat")
	nonce, ct, err := Seal(k, []byte("ghp_value"), aad)
	if err != nil || len(nonce) != 12 || bytes.Contains(ct, []byte("ghp_value")) {
		t.Fatalf("seal %v", err)
	}
	pt, err := Open(k, nonce, ct, aad)
	if err != nil || string(pt) != "ghp_value" {
		t.Fatalf("open %q %v", pt, err)
	}
}

func TestOpenWrongAADFails(t *testing.T) {
	k := testKey(t)
	nonce, ct, _ := Seal(k, []byte("v"), AAD("ws-1", "gh", "github_pat"))
	for _, aad := range []string{AAD("ws-1", "other", "github_pat"), AAD("ws-2", "gh", "github_pat"), AAD("ws-1", "gh", "telegram_bot")} {
		if _, err := Open(k, nonce, ct, aad); err == nil {
			t.Fatalf("opened under %q", aad)
		}
	}
}

func TestOpenWrongKeyFails(t *testing.T) {
	nonce, ct, _ := Seal(testKey(t), []byte("v"), "a")
	_, err := Open(testKey(t), nonce, ct, "a")
	var se *Error
	if !errors.As(err, &se) || se.Code != CodeDecryptFailed {
		t.Fatalf("err %v", err)
	}
}

func TestNonceUnique(t *testing.T) {
	k := testKey(t)
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		n, _, err := Seal(k, []byte("x"), "a")
		if err != nil || seen[string(n)] {
			t.Fatal("duplicate nonce")
		}
		seen[string(n)] = true
	}
}

func TestLoadMasterKey(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, data []byte, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, data, mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	raw := testKey(t)
	if k, err := LoadMasterKey(write("raw", raw, 0o600)); err != nil || !bytes.Equal(k, raw) {
		t.Fatalf("raw %v", err)
	}
	b64 := base64.StdEncoding.EncodeToString(raw)
	if k, err := LoadMasterKey(write("b64", []byte(b64+"\n"), 0o400)); err != nil || !bytes.Equal(k, raw) {
		t.Fatalf("b64 %v", err)
	}
	code := func(err error) string {
		var se *Error
		if errors.As(err, &se) {
			return se.Code
		}
		return ""
	}
	if _, err := LoadMasterKey(write("open", raw, 0o644)); code(err) != CodeKeyPermissions {
		t.Fatalf("0644: %v", err)
	}
	if _, err := LoadMasterKey(write("short", raw[:31], 0o600)); err == nil {
		t.Fatal("31 bytes accepted")
	}
	if _, err := LoadMasterKey(filepath.Join(dir, "missing")); code(err) != CodeMasterKeyUnavailable {
		t.Fatalf("missing: %v", err)
	}
	if _, err := LoadMasterKey(""); code(err) != CodeMasterKeyUnavailable {
		t.Fatalf("empty path: %v", err)
	}
}

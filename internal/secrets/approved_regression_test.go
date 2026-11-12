package secrets

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TC-19 (REQ-9), TC-20 (REQ-9): tamper and AAD binding never return plaintext
// or silently substitute another workspace's credential with the same name.
func TestApproved_EncryptedCredentialsTamperAndWorkspaceIsolation(t *testing.T) {
	ctx := context.Background()
	key := testKey(t)
	store := newMemStore()
	r := NewResolver(store, key, &recRedactor{}, nil, time.Minute, time.Now)
	value := "fake-system-private-password"
	if err := r.SetSecret(ctx, "workspace-a", "credential", "github_pat", value); err != nil {
		t.Fatal(err)
	}
	row := store.rows["workspace-a|credential"]
	if len(row.nonce) != 12 || bytes.Contains(row.ct, []byte(value)) {
		t.Error("credential not encrypted")
	}
	for _, mode := range []string{"workspace", "name", "kind", "ciphertext", "nonce", "key", "missing"} {
		t.Run(mode, func(t *testing.T) {
			copy := newMemStore()
			copy.rows["workspace-a|credential"] = memRow{kind: row.kind, nonce: append([]byte(nil), row.nonce...), ct: append([]byte(nil), row.ct...)}
			ws, name, k := "workspace-a", "credential", key
			switch mode {
			case "workspace":
				ws = "workspace-b"
				copy.rows[ws+"|"+name] = row
			case "name":
				name = "other"
				copy.rows[ws+"|"+name] = row
			case "kind":
				bad := copy.rows[ws+"|"+name]
				bad.kind = "telegram_bot"
				copy.rows[ws+"|"+name] = bad
			case "ciphertext":
				copy.rows[ws+"|"+name].ct[0] ^= 1
			case "nonce":
				copy.rows[ws+"|"+name].nonce[0] ^= 1
			case "key":
				k = testKey(t)
			case "missing":
				ws = "missing-workspace"
			}
			got, err := NewResolver(copy, k, &recRedactor{}, nil, time.Minute, time.Now).Resolve(ctx, ws, name)
			want := CodeDecryptFailed
			if mode == "missing" {
				want = CodeMissingCredentials
			}
			var se *Error
			if got != "" || !errors.As(err, &se) || se.Code != want {
				t.Error("tamper/missing/AAD error returned plaintext or wrong safe code")
			}
			if err != nil && strings.Contains(err.Error(), value) {
				t.Error("error exposed credential")
			}
		})
	}
}

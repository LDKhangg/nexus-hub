// Package secrets encrypts credentials with AES-256-GCM under a master key
// read from a file, and resolves credential references with a short cache.
package secrets

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"os"
)

// Error codes. Error text never carries plaintext, ciphertext or key bytes.
const (
	CodeMissingCredentials   = "missing_credentials"
	CodeDecryptFailed        = "decrypt_failed"
	CodeMasterKeyUnavailable = "master_key_unavailable"
	CodeKeyPermissions       = "key_permissions"
)

// Error is a secrets failure with a safe code.
type Error struct{ Code string }

func (e *Error) Error() string { return "secrets: " + e.Code }

func errCode(code string) error { return &Error{Code: code} }

// KeySize is the AES-256 key length.
const KeySize = 32

// LoadMasterKey reads exactly 32 raw bytes or their base64 (44 chars,
// optional trailing newline). The file must not be group/other accessible.
func LoadMasterKey(path string) ([]byte, error) {
	if path == "" {
		return nil, errCode(CodeMasterKeyUnavailable)
	}
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() {
		return nil, errCode(CodeMasterKeyUnavailable)
	}
	if st.Mode().Perm()&0o077 != 0 {
		return nil, errCode(CodeKeyPermissions)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, errCode(CodeMasterKeyUnavailable)
	}
	if len(data) == KeySize {
		return data, nil
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 44 {
		key, err := base64.StdEncoding.DecodeString(string(trimmed))
		if err == nil && len(key) == KeySize {
			return key, nil
		}
	}
	return nil, errCode(CodeMasterKeyUnavailable)
}

// AAD binds a ciphertext to its workspace, name and kind.
func AAD(workspaceID, name, kind string) string { return workspaceID + "|" + name + "|" + kind }

func gcm(key []byte) (cipher.AEAD, error) {
	if len(key) != KeySize {
		return nil, errCode(CodeMasterKeyUnavailable)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, errCode(CodeMasterKeyUnavailable)
	}
	return cipher.NewGCM(block)
}

// Seal encrypts plaintext with a fresh random 12-byte nonce.
func Seal(key, plaintext []byte, aad string) (nonce, ciphertext []byte, err error) {
	aead, err := gcm(key)
	if err != nil {
		return nil, nil, err
	}
	nonce = make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, errCode(CodeMasterKeyUnavailable)
	}
	return nonce, aead.Seal(nil, nonce, plaintext, []byte(aad)), nil
}

// Open decrypts and authenticates a ciphertext.
func Open(key, nonce, ciphertext []byte, aad string) ([]byte, error) {
	aead, err := gcm(key)
	if err != nil {
		return nil, err
	}
	if len(nonce) != aead.NonceSize() {
		return nil, errCode(CodeDecryptFailed)
	}
	pt, err := aead.Open(nil, nonce, ciphertext, []byte(aad))
	if err != nil {
		return nil, errCode(CodeDecryptFailed)
	}
	return pt, nil
}

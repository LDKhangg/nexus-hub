package github

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"time"
)

var errInvalidAppKey = errors.New("invalid app key")

// parseAppKey accepts a PKCS#1 or PKCS#8 RSA private key in PEM form.
func parseAppKey(text string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(text))
	if block == nil {
		return nil, errInvalidAppKey
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errInvalidAppKey
	}
	k, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errInvalidAppKey
	}
	return k, nil
}

// signAppJWT signs the RS256 app JWT GitHub requires: iat is backdated 60s
// for clock drift and exp is 9 minutes ahead (GitHub caps it at 10).
func signAppJWT(appID string, key *rsa.PrivateKey, now time.Time) (string, error) {
	enc := base64.RawURLEncoding
	header := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, err := json.Marshal(map[string]any{"iss": appID, "iat": now.Unix() - 60, "exp": now.Add(9 * time.Minute).Unix()})
	if err != nil {
		return "", err
	}
	signing := header + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", errInvalidAppKey
	}
	return signing + "." + enc.EncodeToString(sig), nil
}

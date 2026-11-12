package secrets

import (
	"context"
	"errors"
	"log"
	"regexp"
	"strings"
	"sync"
	"time"
)

// LegacyEnvPrefix marks a Phase 1 *_env reference resolved from the
// process environment until migrate-secrets has run.
const LegacyEnvPrefix = "env:"

var (
	refRe     = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,62}$`)
	envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

type cached struct {
	value   string
	expires time.Time
}

// Resolver resolves credential references to plaintext, registering every
// value with the redactor.
type Resolver struct {
	store     Store
	key       []byte
	redactor  interface{ Add(string) }
	legacyEnv func(string) (string, bool)
	ttl       time.Duration
	now       func() time.Time

	mu     sync.Mutex
	cache  map[string]cached
	warned map[string]bool
}

// NewResolver builds a resolver. A nil key makes every DB reference fail
// with master_key_unavailable while legacy env references keep working.
func NewResolver(store Store, key []byte, redactor interface{ Add(string) }, legacyEnv func(string) (string, bool), ttl time.Duration, now func() time.Time) *Resolver {
	if now == nil {
		now = time.Now
	}
	return &Resolver{store: store, key: key, redactor: redactor, legacyEnv: legacyEnv, ttl: ttl, now: now, cache: map[string]cached{}, warned: map[string]bool{}}
}

// Resolve returns the plaintext of ref in the workspace.
func (r *Resolver) Resolve(ctx context.Context, workspaceID, ref string) (string, error) {
	if name, ok := strings.CutPrefix(ref, LegacyEnvPrefix); ok {
		return r.resolveEnv(name)
	}
	if !refRe.MatchString(ref) {
		return "", errCode(CodeMissingCredentials)
	}
	if r.key == nil {
		return "", errCode(CodeMasterKeyUnavailable)
	}
	cacheKey := workspaceID + "|" + ref
	r.mu.Lock()
	if c, ok := r.cache[cacheKey]; ok && r.now().Before(c.expires) {
		r.mu.Unlock()
		return c.value, nil
	}
	r.mu.Unlock()
	if r.store == nil {
		return "", errCode(CodeMissingCredentials)
	}
	kind, nonce, ct, err := r.store.Get(ctx, workspaceID, ref)
	if errors.Is(err, ErrNotFound) {
		return "", errCode(CodeMissingCredentials)
	}
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", errCode(CodeMissingCredentials)
	}
	pt, err := Open(r.key, nonce, ct, AAD(workspaceID, ref, kind))
	if err != nil {
		return "", errCode(CodeDecryptFailed)
	}
	value := string(pt)
	r.register(value)
	r.mu.Lock()
	r.cache[cacheKey] = cached{value: value, expires: r.now().Add(r.ttl)}
	r.mu.Unlock()
	return value, nil
}

func (r *Resolver) resolveEnv(name string) (string, error) {
	if r.legacyEnv == nil || !envNameRe.MatchString(name) {
		return "", errCode(CodeMissingCredentials)
	}
	r.mu.Lock()
	if !r.warned[name] {
		r.warned[name] = true
		log.Printf("deprecated: credential read from environment variable %s; run migrate-secrets", name)
	}
	r.mu.Unlock()
	v, ok := r.legacyEnv(name)
	if !ok || v == "" {
		return "", errCode(CodeMissingCredentials)
	}
	r.register(v)
	return v, nil
}

func (r *Resolver) register(v string) {
	if r.redactor != nil {
		r.redactor.Add(v)
	}
}

// SetSecret encrypts and stores a credential, replacing any previous value.
func (r *Resolver) SetSecret(ctx context.Context, workspaceID, name, kind, value string) error {
	if r.key == nil {
		return errCode(CodeMasterKeyUnavailable)
	}
	if !refRe.MatchString(name) || !Kinds[kind] || value == "" {
		return errors.New("secrets: invalid credential name, kind or empty value")
	}
	nonce, ct, err := Seal(r.key, []byte(value), AAD(workspaceID, name, kind))
	if err != nil {
		return err
	}
	if err := r.store.Put(ctx, workspaceID, name, kind, nonce, ct); err != nil {
		return errors.New("secrets: store failed")
	}
	r.register(value)
	r.mu.Lock()
	delete(r.cache, workspaceID+"|"+name)
	r.mu.Unlock()
	return nil
}

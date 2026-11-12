package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage"
	"github.com/LDKhangg/nexus-hub/internal/secrets"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Secret values are read from stdin (no-echo prompt on a TTY), never argv.
var (
	stdin     io.Reader = os.Stdin
	promptOut io.Writer = os.Stderr
)

const maxSecretBytes = 64 << 10

func readSecretValue() (string, error) {
	var data []byte
	if f, ok := stdin.(*os.File); ok && isTerminal(f) {
		fmt.Fprint(promptOut, "secret value (input hidden): ")
		line, err := readHidden(f)
		fmt.Fprintln(promptOut)
		if err != nil {
			return "", errors.New("could not read secret from terminal")
		}
		data = line
	} else {
		b, err := io.ReadAll(io.LimitReader(stdin, maxSecretBytes+1))
		if err != nil {
			return "", errors.New("could not read secret from stdin")
		}
		if len(b) > maxSecretBytes {
			return "", errors.New("secret too large")
		}
		data = b
	}
	v := strings.TrimRight(string(data), "\r\n")
	if v == "" {
		return "", errors.New("empty secret")
	}
	return v, nil
}

func openResolver(pool *pgxpool.Pool, lookup func(string) (string, bool)) (*secrets.Resolver, *storage.CredentialStore, error) {
	path, _ := lookup("NEXUS_MASTER_KEY_FILE")
	key, err := secrets.LoadMasterKey(path)
	if err != nil {
		return nil, nil, fmt.Errorf("master key unavailable (%v); set NEXUS_MASTER_KEY_FILE", err)
	}
	store, err := storage.NewCredentialStore(pool)
	if err != nil {
		return nil, nil, err
	}
	return secrets.NewResolver(store, key, nil, lookup, time.Minute, time.Now), store, nil
}

func secretCommand(ctx context.Context, pool *pgxpool.Pool, args []string, out io.Writer, lookup func(string) (string, bool)) error {
	if len(args) == 0 {
		return errors.New("usage: secret set|list|delete")
	}
	f := commandFlags("secret " + args[0])
	ws := f.String("workspace-id", "", "workspace UUID")
	name := f.String("name", "", "credential name")
	kind := f.String("kind", "", "credential kind")
	if f.Parse(args[1:]) != nil || f.NArg() != 0 || !tenant.ValidUUID(*ws) {
		return errors.New("secret commands require a valid --workspace-id and take no positional arguments (values come from stdin)")
	}
	resolver, store, err := openResolver(pool, lookup)
	switch args[0] {
	case "set":
		if err != nil {
			return err
		}
		if !tenant.ValidCredentialRef(*name) || !secrets.Kinds[*kind] {
			return errors.New("secret set requires a valid --name and --kind (telegram_bot|clickup_token|clickup_webhook|github_pat|github_app_key|anthropic_key)")
		}
		value, err := readSecretValue()
		if err != nil {
			return err
		}
		if err := resolver.SetSecret(ctx, *ws, *name, *kind, value); err != nil {
			return errors.New("secret not stored")
		}
		return json.NewEncoder(out).Encode(map[string]string{"workspace_id": *ws, "name": *name, "kind": *kind, "status": "stored"})
	case "list":
		if store == nil {
			if store, err = storage.NewCredentialStore(pool); err != nil {
				return err
			}
		}
		metas, err := store.List(ctx, *ws)
		if err != nil {
			return errors.New("secret list failed")
		}
		for _, m := range metas {
			if err := json.NewEncoder(out).Encode(map[string]any{"name": m.Name, "kind": m.Kind, "updated_at": m.UpdatedAt}); err != nil {
				return err
			}
		}
		return nil
	case "delete":
		if store == nil {
			if store, err = storage.NewCredentialStore(pool); err != nil {
				return err
			}
		}
		if !tenant.ValidCredentialRef(*name) {
			return errors.New("secret delete requires --name")
		}
		if err := store.Delete(ctx, *ws, *name); err != nil {
			return errors.New("secret not found")
		}
		return json.NewEncoder(out).Encode(map[string]string{"name": *name, "status": "deleted"})
	}
	return errors.New("usage: secret set|list|delete")
}

// masterKeyInit writes 32 random bytes (base64) to a new 0600 file.
func masterKeyInit(args []string, out io.Writer) error {
	if len(args) == 0 || args[0] != "init" {
		return errors.New("usage: master-key init --path <file>")
	}
	f := commandFlags("master-key init")
	path := f.String("path", "", "key file")
	if f.Parse(args[1:]) != nil || f.NArg() != 0 || !filepath.IsAbs(*path) {
		return errors.New("master-key init requires an absolute --path")
	}
	key := make([]byte, secrets.KeySize)
	if _, err := rand.Read(key); err != nil {
		return errors.New("random source failed")
	}
	file, err := os.OpenFile(*path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return errors.New("refusing to overwrite an existing key file")
		}
		return errors.New("cannot create key file")
	}
	_, werr := file.WriteString(base64.StdEncoding.EncodeToString(key) + "\n")
	cerr := file.Close()
	if werr != nil || cerr != nil {
		_ = os.Remove(*path)
		return errors.New("cannot write key file")
	}
	return json.NewEncoder(out).Encode(map[string]string{"path": *path, "status": "created"})
}

var refUnsafe = regexp.MustCompile(`[^a-z0-9_.-]+`)

// refNameFor derives a credential name from a legacy env variable name.
func refNameFor(env string) string {
	n := strings.Trim(refUnsafe.ReplaceAllString(strings.ToLower(env), "_"), "_.-")
	if len(n) > 63 {
		n = n[:63]
	}
	if n == "" {
		n = "legacy"
	}
	return n
}

type legacyRef struct {
	integrationID, envKey, refKey, kind, env, ref string
}

// migrateSecrets copies legacy *_env values into credentials and rewrites
// the configs to *_ref. Nothing is changed if any value is missing.
func migrateSecrets(ctx context.Context, pool *pgxpool.Pool, args []string, out io.Writer, lookup func(string) (string, bool)) error {
	f := commandFlags("migrate-secrets")
	ws := f.String("workspace-id", "", "workspace UUID")
	if f.Parse(args) != nil || f.NArg() != 0 || !tenant.ValidUUID(*ws) {
		return errors.New("migrate-secrets requires --workspace-id")
	}
	resolver, store, err := openResolver(pool, lookup)
	if err != nil {
		return err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return errors.New("database operation failed")
	}
	defer rollback(tx)
	rows, err := tx.Query(ctx, `SELECT id, platform, config FROM integrations WHERE workspace_id=$1 ORDER BY id FOR UPDATE`, *ws)
	if err != nil {
		return errors.New("integration scan failed")
	}
	type item struct {
		id  string
		cfg map[string]any
	}
	var items []item
	var refs []legacyRef
	for rows.Next() {
		var id, platform string
		var data []byte
		if rows.Scan(&id, &platform, &data) != nil {
			rows.Close()
			return errors.New("integration decode failed")
		}
		var cfg map[string]any
		if json.Unmarshal(data, &cfg) != nil || cfg == nil {
			continue
		}
		var fields []legacyRef
		switch platform {
		case "telegram":
			fields = []legacyRef{{envKey: "bot_token_env", refKey: "bot_token_ref", kind: "telegram_bot"}}
		case "clickup":
			fields = []legacyRef{{envKey: "webhook_secret_env", refKey: "webhook_secret_ref", kind: "clickup_webhook"}}
		}
		changed := false
		for _, fl := range fields {
			env, _ := cfg[fl.envKey].(string)
			if env == "" {
				continue
			}
			fl.integrationID, fl.env, fl.ref = id, env, refNameFor(env)
			refs = append(refs, fl)
			changed = true
		}
		if changed {
			items = append(items, item{id, cfg})
		}
	}
	rows.Close()
	if rows.Err() != nil {
		return errors.New("integration scan failed")
	}
	if err := checkRefCollisions(ctx, store, *ws, refs); err != nil {
		return err
	}
	values := map[string]string{}
	for _, r := range refs {
		v, ok := lookup(r.env)
		if !ok || v == "" {
			return fmt.Errorf("environment variable %s is not set; nothing migrated", r.env)
		}
		values[r.env] = v
	}
	for _, r := range refs {
		if err := resolver.SetSecret(ctx, *ws, r.ref, r.kind, values[r.env]); err != nil {
			return errors.New("secret not stored; nothing rewritten")
		}
	}
	for _, it := range items {
		for _, r := range refs {
			if r.integrationID != it.id {
				continue
			}
			delete(it.cfg, r.envKey)
			it.cfg[r.refKey] = r.ref
		}
		encoded, _ := json.Marshal(it.cfg)
		if _, err := tx.Exec(ctx, `UPDATE integrations SET config=$2, updated_at=now() WHERE id=$1`, it.id, encoded); err != nil {
			return errors.New("config rewrite failed")
		}
	}
	if tx.Commit(ctx) != nil {
		return errors.New("migration commit failed")
	}
	enc := json.NewEncoder(out)
	for _, r := range refs {
		if err := enc.Encode(map[string]string{"integration_id": r.integrationID, "field": r.refKey, "ref": r.ref, "from_env": r.env}); err != nil {
			return err
		}
	}
	if len(refs) == 0 {
		return enc.Encode(map[string]string{"status": "nothing to migrate"})
	}
	return nil
}

// credentialLister lists the credential names of a workspace.
type credentialLister interface {
	List(ctx context.Context, workspaceID string) ([]secrets.Meta, error)
}

// checkRefCollisions refuses a migration in which two different env names
// (or one env name with two kinds) map to the same credential name, or the
// credential name already exists in the workspace. It runs before anything is
// written; errors name the credential, never a value.
func checkRefCollisions(ctx context.Context, store credentialLister, ws string, refs []legacyRef) error {
	type origin struct{ env, kind string }
	seen := map[string]origin{}
	for _, r := range refs {
		if prev, ok := seen[r.ref]; ok {
			if prev.env != r.env || prev.kind != r.kind {
				return fmt.Errorf("credential name %s is derived from more than one environment variable or kind; nothing migrated", r.ref)
			}
			continue
		}
		seen[r.ref] = origin{r.env, r.kind}
	}
	if len(seen) == 0 {
		return nil
	}
	metas, err := store.List(ctx, ws)
	if err != nil {
		return errors.New("credential list failed; nothing migrated")
	}
	for _, m := range metas {
		if _, ok := seen[m.Name]; ok {
			return fmt.Errorf("credential %s already exists in the workspace; nothing migrated", m.Name)
		}
	}
	return nil
}

type rollbacker interface{ Rollback(context.Context) error }

func rollback(tx rollbacker) {
	c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = tx.Rollback(c)
}

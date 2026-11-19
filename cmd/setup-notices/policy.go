package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage"
	"github.com/LDKhangg/nexus-hub/internal/adapters/tools/catalog"
	"github.com/LDKhangg/nexus-hub/internal/policy"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
	"github.com/jackc/pgx/v5/pgxpool"
)

const policyUsage = `usage: policy <command> [args]
  show --workspace-id <uuid>                  print the active rule set as a policy/v1 file
  validate <file>                             check a rule set file; changes nothing
  apply <file> --workspace-id <uuid>          store the file as a new version and activate it
  history --workspace-id <uuid> [--limit N]   list stored versions, newest first
  rollback <version> --workspace-id <uuid>    activate an old version again, as a new version
  decisions --workspace-id <uuid> [--limit N] list recent policy decisions, newest first
Only this command changes policy. A change applies to the next decision; no
restart is needed.`

// policyKnownActions is the closed set of names a rule set may use; the
// server builds the same set (catalog.PolicyKnownActions).
func policyKnownActions() policy.KnownActions {
	return catalog.PolicyKnownActions()
}

func readPolicyFile(path string) ([]byte, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot read file")
	}
	defer fh.Close()
	data, err := io.ReadAll(io.LimitReader(fh, policy.MaxSourceBytes+1))
	if err != nil {
		return nil, errors.New("cannot read file")
	}
	if len(data) > policy.MaxSourceBytes {
		return nil, fmt.Errorf("file is larger than %d bytes", policy.MaxSourceBytes)
	}
	return data, nil
}

// parsePolicyFile reads, parses and fully validates a rule set file.
func parsePolicyFile(path string, known policy.KnownActions) (policy.RuleSet, error) {
	src, err := readPolicyFile(path)
	if err != nil {
		return policy.RuleSet{}, fmt.Errorf("policy file: %w", err)
	}
	rs, err := policy.Parse(src)
	if err != nil {
		return policy.RuleSet{}, err
	}
	if err := policy.Validate(rs, known); err != nil {
		return policy.RuleSet{}, err
	}
	return rs, nil
}

func formatTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Format(time.RFC3339)
}

func storedRow(s policy.StoredRuleSet) map[string]any {
	row := map[string]any{
		"version": s.Version, "schema": s.Schema, "body_sha256": s.BodySHA256, "created_by": s.CreatedBy,
		"created_at": s.CreatedAt.UTC().Format(time.RFC3339), "activated_at": formatTime(s.ActivatedAt), "active": s.Active,
	}
	if s.RestoredFrom > 0 {
		row["restored_from_version"] = s.RestoredFrom
	}
	return row
}

func policyCommand(ctx context.Context, pool *pgxpool.Pool, args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(policyUsage)
	}
	sub := args[0]
	f := commandFlags("policy " + sub)
	ws := f.String("workspace-id", "", "workspace UUID")
	limit := f.Int("limit", 20, "rows")
	pos, err := parseInterspersed(f, args[1:])
	if err != nil {
		return errors.New(policyUsage)
	}
	set := map[string]bool{}
	f.Visit(func(fl *flag.Flag) { set[fl.Name] = true })
	wantArgs := map[string]int{"show": 0, "validate": 1, "apply": 1, "history": 0, "rollback": 1, "decisions": 0}
	n, ok := wantArgs[sub]
	if !ok || len(pos) != n {
		return errors.New(policyUsage)
	}
	if set["limit"] && sub != "history" && sub != "decisions" {
		return errors.New(policyUsage)
	}
	if set["limit"] && (*limit < 1 || *limit > 500) {
		return errors.New("--limit must be between 1 and 500")
	}
	known := policyKnownActions()
	enc := json.NewEncoder(out)
	if sub == "validate" {
		if set["workspace-id"] {
			return errors.New(policyUsage)
		}
		rs, err := parsePolicyFile(pos[0], known)
		if err != nil {
			return policyError(err)
		}
		_, sum, err := policy.Canonical(rs)
		if err != nil {
			return policyError(err)
		}
		return enc.Encode(map[string]any{"valid": true, "schema": rs.Schema, "body_sha256": sum})
	}
	if !tenant.ValidUUID(*ws) {
		return errors.New(policyUsage)
	}
	store, err := storage.NewPolicyStore(pool)
	if err != nil {
		return err
	}
	switch sub {
	case "show":
		active, ok, err := store.ActiveRuleSet(ctx, *ws)
		if err != nil {
			return errors.New("database operation failed")
		}
		if !ok {
			fmt.Fprintln(out, "# no active rule set: the built-in safe set applies (read allow, write approval_required, dangerous deny)")
			_, err := out.Write(policy.YAML(policy.SafeRuleSet()))
			return err
		}
		rs, lerr := policy.LoadStored(active, known)
		if lerr != nil {
			fmt.Fprintf(out, "# rule set version %d is invalid: the built-in safe set applies; apply a corrected file\n", active.Version)
			_, err := out.Write(policy.YAML(policy.SafeRuleSet()))
			return err
		}
		fmt.Fprintf(out, "# rule set version %d, sha256 %s, activated %v\n", active.Version, active.BodySHA256, formatTime(active.ActivatedAt))
		_, err = out.Write(policy.YAML(rs))
		return err
	case "apply":
		rs, err := parsePolicyFile(pos[0], known)
		if err != nil {
			return policyError(err)
		}
		stored, changed, err := store.ApplyRuleSet(ctx, *ws, rs, policy.CreatedByCLI, 0)
		if err != nil {
			return policyError(err)
		}
		return enc.Encode(map[string]any{"version": stored.Version, "body_sha256": stored.BodySHA256, "active": true, "changed": changed})
	case "rollback":
		v, err := strconv.Atoi(pos[0])
		if err != nil || v < 1 || v > 1<<30 {
			return errors.New("version must be a positive number")
		}
		old, ok, err := store.RuleSetVersion(ctx, *ws, int32(v))
		if err != nil {
			return errors.New("database operation failed")
		}
		if !ok {
			return errors.New("rule set version not found")
		}
		rs, err := policy.LoadStored(old, known)
		if err != nil {
			return errors.New("rule set version is not valid with the current tools; apply a corrected file instead")
		}
		stored, changed, err := store.ApplyRuleSet(ctx, *ws, rs, policy.CreatedByCLI, old.Version)
		if err != nil {
			return policyError(err)
		}
		return enc.Encode(map[string]any{"version": stored.Version, "restored_from_version": old.Version, "body_sha256": stored.BodySHA256, "active": true, "changed": changed})
	case "history":
		rows, err := store.RuleSetHistory(ctx, *ws, *limit)
		if err != nil {
			return errors.New("database operation failed")
		}
		for _, r := range rows {
			if err := enc.Encode(storedRow(r)); err != nil {
				return err
			}
		}
		return nil
	case "decisions":
		rows, err := store.ListDecisions(ctx, *ws, *limit)
		if err != nil {
			return errors.New("database operation failed")
		}
		for _, d := range rows {
			row := map[string]any{
				"id": d.ID, "created_at": d.CreatedAt.UTC().Format(time.RFC3339), "actor_kind": d.ActorKind, "action": d.Action,
				"tier": d.Tier, "resource_key": d.ResourceKey, "effect": d.Effect, "reason": d.Reason, "rule_key": d.RuleKey,
				"rule_set_version": d.RuleSetVersion, "input_sha256": d.InputSHA256,
			}
			if d.ExecutionID > 0 {
				row["execution_id"] = d.ExecutionID
			}
			if d.ToolRunID > 0 {
				row["tool_run_id"] = d.ToolRunID
			}
			if d.ApprovalID > 0 {
				row["approval_id"] = d.ApprovalID
			}
			if err := enc.Encode(row); err != nil {
				return err
			}
		}
		return nil
	}
	return errors.New(policyUsage)
}

// policyError turns policy and store errors into CLI messages without
// leaking database details. Validation errors list each path and problem.
func policyError(err error) error {
	var pe policy.Errors
	switch {
	case err == nil:
		return nil
	case errors.As(err, &pe):
		return pe
	case errors.Is(err, policy.ErrWorkspaceNotFound):
		return errors.New("workspace not found")
	}
	if strings.HasPrefix(err.Error(), "policy file: ") {
		return err
	}
	return errors.New("policy operation failed")
}

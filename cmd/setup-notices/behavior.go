package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage"
	clickuptools "github.com/LDKhangg/nexus-hub/internal/adapters/tools/clickup"
	codetools "github.com/LDKhangg/nexus-hub/internal/adapters/tools/code"
	githubtools "github.com/LDKhangg/nexus-hub/internal/adapters/tools/github"
	"github.com/LDKhangg/nexus-hub/internal/agent/tools"
	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
	"github.com/LDKhangg/nexus-hub/internal/workflow"
	"github.com/jackc/pgx/v5/pgxpool"
)

const behaviorUsage = `usage: behavior <command> --workspace-id <uuid> [args]
  import <file> [--vars <file>]   create or update a draft (.yaml, .yml or .json);
                                  without --vars the stored values are kept
  validate <name>                 report every error of the draft, one per line
  publish <name>                  freeze the valid draft as a new immutable version
  restore <name> --version N      copy an immutable version into the draft
  enable <name> | disable <name>
  list
  export <name> [--version N]     print the stored source (the draft when N is omitted)
  stats <name> [--version N]      per published version: runs, items by class and
                                  status, undos, conflicts, 👍/👎, average confidence
Vars hold plain settings such as IDs, status names and headings, never
secrets; credentials are stored with the secret command.`

// behaviorReadTools returns the names of the read-tier tools a behavior may
// list in reads: the Phase 2 read tools plus memory_search. The tools are
// built without dependencies only to read their specs; nothing runs.
func behaviorReadTools() []string {
	all := append([]tools.Tool{}, githubtools.NewReadTools(nil)...)
	all = append(all, clickuptools.NewTaskTool(nil, nil))
	all = append(all, codetools.NewReadTools(nil, "")...)
	names := []string{behavior.MemorySearchTool}
	for _, t := range all {
		if s := t.Spec(); s.Tier == tenant.TierRead {
			names = append(names, s.Name)
		}
	}
	sort.Strings(names)
	return names
}

// behaviorGitHubAccounts returns the hub GitHub accounts of the workspace's
// ClickUp sources, name to type, for the github.pr_approve account check.
// A source whose agent configuration does not parse adds no account. A name
// configured with different types counts as not "pat".
func behaviorGitHubAccounts(ctx context.Context, pool *pgxpool.Pool, ws string) (map[string]string, error) {
	rows, err := pool.Query(ctx, `SELECT config FROM integrations WHERE workspace_id=$1 AND platform='clickup'`, ws)
	if err != nil {
		return nil, errors.New("database operation failed")
	}
	defer rows.Close()
	known := knownTools()
	out := map[string]string{}
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			return nil, errors.New("database operation failed")
		}
		var cfg map[string]any
		if json.Unmarshal(data, &cfg) != nil {
			continue
		}
		agent, err := tenant.ParseAgentConfig(cfg, known)
		if err != nil {
			continue
		}
		for name, a := range agent.Tools.Accounts {
			if prev, ok := out[name]; ok && prev != a.Type {
				out[name] = "mixed"
				continue
			}
			out[name] = a.Type
		}
	}
	if rows.Err() != nil {
		return nil, errors.New("database operation failed")
	}
	return out, nil
}

// parseInterspersed parses flags that may come before or after positional
// arguments and returns the positional ones.
func parseInterspersed(f *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := f.Parse(args); err != nil {
			return nil, err
		}
		if f.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, f.Arg(0))
		args = f.Args()[1:]
	}
}

func readSourceFile(path string) ([]byte, behavior.Format, error) {
	format, err := behavior.FormatFromPath(path)
	if err != nil {
		return nil, "", err
	}
	fh, err := os.Open(path)
	if err != nil {
		return nil, "", errors.New("cannot read file")
	}
	defer fh.Close()
	data, err := io.ReadAll(io.LimitReader(fh, behavior.MaxSourceBytes+1))
	if err != nil {
		return nil, "", errors.New("cannot read file")
	}
	if len(data) > behavior.MaxSourceBytes {
		return nil, "", fmt.Errorf("file is larger than %d bytes", behavior.MaxSourceBytes)
	}
	return data, format, nil
}

// behaviorError turns store and validation errors into CLI messages
// without leaking database details.
func behaviorError(err error) error {
	var fe behavior.FieldErrors
	switch {
	case err == nil:
		return nil
	case errors.As(err, &fe):
		return fe
	case errors.Is(err, behavior.ErrNotFound):
		return errors.New("behavior or version not found")
	case errors.Is(err, behavior.ErrNotPublished):
		return errors.New("behavior has no published version; publish it first")
	case errors.Is(err, behavior.ErrConflict):
		return errors.New("behavior changed concurrently; try again")
	case errors.Is(err, behavior.ErrInvalid):
		return errors.New("invalid behavior data")
	}
	return errors.New("database operation failed")
}

func errorLines(fe behavior.FieldErrors) []string {
	out := make([]string, len(fe))
	for i, e := range fe {
		out[i] = e.Error()
	}
	return out
}

func behaviorCommand(ctx context.Context, pool *pgxpool.Pool, args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(behaviorUsage)
	}
	sub := args[0]
	f := commandFlags("behavior " + sub)
	ws := f.String("workspace-id", "", "workspace UUID")
	varsFile := f.String("vars", "", "values file")
	version := f.Int("version", 0, "published version")
	pos, err := parseInterspersed(f, args[1:])
	if err != nil || !tenant.ValidUUID(*ws) {
		return errors.New(behaviorUsage)
	}
	set := map[string]bool{}
	f.Visit(func(fl *flag.Flag) { set[fl.Name] = true })
	if (set["vars"] && sub != "import") || (set["version"] && sub != "export" && sub != "stats" && sub != "restore") {
		return errors.New(behaviorUsage)
	}
	wantArgs := 1
	if sub == "list" {
		wantArgs = 0
	}
	if len(pos) != wantArgs {
		return errors.New(behaviorUsage)
	}
	if wantArgs == 1 && sub != "import" && !behavior.NameRe.MatchString(pos[0]) {
		return errors.New("invalid behavior name")
	}
	store, err := storage.NewBehaviorStore(pool)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(out)
	reads := behaviorReadTools()
	var opts []behavior.ValidateOption
	if sub == "import" || sub == "validate" || sub == "publish" {
		accounts, err := behaviorGitHubAccounts(ctx, pool, *ws)
		if err != nil {
			return err
		}
		opts = append(opts, behavior.WithGitHubAccounts(accounts))
		opts = append(opts, behavior.WithWorkflowCapabilities([]string{workflow.CapabilityInProcess}))
	}

	switch sub {
	case "import":
		src, format, err := readSourceFile(pos[0])
		if err != nil {
			return fmt.Errorf("behavior file: %w", err)
		}
		var vars map[string]string
		if *varsFile != "" {
			vsrc, vformat, err := readSourceFile(*varsFile)
			if err != nil {
				return fmt.Errorf("values file: %w", err)
			}
			if vars, err = behavior.ParseVars(vsrc, vformat); err != nil {
				return behaviorError(err)
			}
		}
		b, problems, err := behavior.ImportDraft(ctx, store, *ws, src, format, vars, reads, opts...)
		if err != nil {
			return behaviorError(err)
		}
		res := map[string]any{"name": b.Name, "status": b.Status, "draft": "saved", "latest_version": b.LatestVersion, "valid": len(problems) == 0}
		if len(problems) > 0 {
			res["errors"] = errorLines(problems)
		}
		return enc.Encode(res)
	case "validate":
		b, err := store.GetBehavior(ctx, *ws, pos[0])
		if err != nil {
			return behaviorError(err)
		}
		if err := behavior.ValidateDraft(b, reads, opts...); err != nil {
			return behaviorError(err)
		}
		return enc.Encode(map[string]any{"name": b.Name, "valid": true})
	case "publish":
		v, err := behavior.PublishDraft(ctx, store, *ws, pos[0], reads, opts...)
		if err != nil {
			return behaviorError(err)
		}
		return enc.Encode(map[string]any{"name": v.Name, "version": v.Version, "published_at": v.PublishedAt.UTC().Format(time.RFC3339)})
	case "restore":
		if !set["version"] || *version <= 0 {
			return errors.New("--version must be a positive number")
		}
		b, err := behavior.RestoreDraft(ctx, store, *ws, pos[0], *version)
		if err != nil {
			return behaviorError(err)
		}
		return enc.Encode(map[string]any{"name": b.Name, "restored_version": *version, "draft": "saved", "latest_version": b.LatestVersion})
	case "enable", "disable":
		status := behavior.StatusEnabled
		if sub == "disable" {
			status = behavior.StatusDisabled
		}
		if err := store.SetStatus(ctx, *ws, pos[0], status); err != nil {
			return behaviorError(err)
		}
		return enc.Encode(map[string]any{"name": pos[0], "status": status})
	case "list":
		list, err := store.ListBehaviors(ctx, *ws)
		if err != nil {
			return behaviorError(err)
		}
		for _, b := range list {
			if err := enc.Encode(map[string]any{
				"name": b.Name, "status": b.Status, "latest_version": b.LatestVersion,
				"updated_at": b.UpdatedAt.UTC().Format(time.RFC3339),
			}); err != nil {
				return err
			}
		}
		return nil
	case "stats":
		if *version < 0 {
			return errors.New("--version must be a positive number")
		}
		list, err := store.Stats(ctx, *ws, pos[0])
		if err != nil {
			return behaviorError(err)
		}
		found := false
		for _, st := range list {
			if *version != 0 && st.Version != *version {
				continue
			}
			found = true
			if err := enc.Encode(statsJSON(pos[0], st)); err != nil {
				return err
			}
		}
		if *version != 0 && !found {
			return errors.New("behavior or version not found")
		}
		return nil
	case "export":
		if *version < 0 {
			return errors.New("--version must be a positive number")
		}
		src, err := behavior.Export(ctx, store, *ws, pos[0], *version)
		if err != nil {
			return behaviorError(err)
		}
		_, err = io.WriteString(out, src)
		return err
	}
	return errors.New(behaviorUsage)
}

// statsJSON is one version's line of behavior stats.
func statsJSON(name string, st behavior.VersionStats) map[string]any {
	round := func(f float64) float64 { return math.Round(f*1000) / 1000 }
	return map[string]any{
		"name": name, "version": st.Version,
		"runs":  map[string]int64{"total": st.Runs, "failed": st.FailedRuns, "skipped": st.SkippedRuns, "undone": st.Undos},
		"items": map[string]int64{"auto": st.AutoItems, "notify": st.NotifyItems, "ask": st.AskItems},
		"statuses": map[string]int64{
			"drafted": st.DraftedItems, "asked": st.AskedItems, "done": st.DoneItems, "answered": st.AnsweredItems,
			"expired": st.ExpiredItems, "skipped": st.SkippedItems, "conflict": st.Conflicts, "failed": st.FailedItems,
		},
		"undos":    st.Undos,
		"feedback": map[string]int64{"up": st.RatedUp, "down": st.RatedDown},
		"avg_confidence": map[string]float64{
			"auto": round(st.AvgConfidenceAuto), "notify": round(st.AvgConfidenceNotify), "ask": round(st.AvgConfidenceAsk),
		},
	}
}

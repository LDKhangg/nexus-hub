package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/llm"
	"github.com/LDKhangg/nexus-hub/internal/adapters/llm/clirun"
	"github.com/LDKhangg/nexus-hub/internal/adapters/netguard"
	"github.com/LDKhangg/nexus-hub/internal/adapters/storage"
	"github.com/LDKhangg/nexus-hub/internal/settings"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
	"github.com/jackc/pgx/v5/pgxpool"
)

const llmUsage = "usage: llm check [--workspace-id <uuid>] (the workspace is required for llm_provider anthropic)"

// llmCheckSlack is added to the provider timeout for the whole check.
const llmCheckSlack = 15 * time.Second

// llmCommand runs `llm check`: one tiny structured call through the
// configured provider. It prints the provider, the model, ok or a code,
// the latency and the token counts; never prompt or answer text.
func llmCommand(ctx context.Context, pool *pgxpool.Pool, args []string, out io.Writer, lookup func(string) (string, bool)) error {
	if len(args) == 0 || args[0] != "check" {
		return errors.New(llmUsage)
	}
	f := commandFlags("llm check")
	ws := f.String("workspace-id", "", "workspace UUID")
	if f.Parse(args[1:]) != nil || f.NArg() != 0 || (*ws != "" && !tenant.ValidUUID(*ws)) {
		return errors.New(llmUsage)
	}
	store, err := storage.NewSettingsStore(pool)
	if err != nil {
		return err
	}
	s, err := settings.Load(ctx, store)
	if err != nil {
		return err
	}
	var resolver llm.SecretResolver
	if s.LLMProvider == settings.LLMProviderAnthropic {
		if *ws == "" {
			return errors.New(llmUsage)
		}
		r, _, err := openResolver(pool, lookup)
		if err != nil {
			return err
		}
		resolver = r
	}
	factory, err := llm.NewFactory(s, resolver, netguard.Client(0, netguard.DefaultHosts), "")
	if err != nil {
		return err
	}
	// The setup context has a short fixed deadline; a CLI call may take
	// up to its own timeout.
	timeout := llmCheckSlack + 60*time.Second
	switch s.LLMProvider {
	case settings.LLMProviderClaudeCLI:
		timeout = s.ClaudeCLITimeout + llmCheckSlack
	case settings.LLMProviderOpencodeCLI:
		// A structured opencode call may make one repair call. Each call's
		// session cleanup may be attempted twice.
		timeout = 2*(s.OpencodeCLITimeout+2*clirun.FollowUpTimeout) + llmCheckSlack
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	row := map[string]any{"provider": string(factory.Provider()), "model": s.TriageModel, "ok": false}
	client, err := factory.Client(cctx, *ws)
	if err != nil {
		row["code"] = llm.ErrorCode(err)
		return json.NewEncoder(out).Encode(row)
	}
	res := llm.Check(cctx, client.Default(), s.TriageModel)
	row["ok"] = res.OK
	if !res.OK {
		row["code"] = res.Code
	}
	row["latency_ms"] = res.Latency.Milliseconds()
	row["input_tokens"] = res.InputTokens
	row["output_tokens"] = res.OutputTokens
	row["cache_tokens"] = res.CacheTokens
	if res.CostUSD > 0 {
		row["cost_usd"] = res.CostUSD
	}
	if res.Note != "" {
		row["note"] = res.Note
	}
	return json.NewEncoder(out).Encode(row)
}

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"strings"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage"
	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
	"github.com/jackc/pgx/v5/pgxpool"
)

const profileUsage = `usage: profile show|set --workspace-id <uuid> [fields]
  set fields (only the given ones change):
    --language vi|en|<xx>[-YY]   language of every message the hub writes
    --tone <text>                 tone for the model, at most 200 characters ("" clears)
    --quiet-hours HH:MM-HH:MM|off notify/FYI messages wait until the end (local time)
    --timezone <IANA zone>        e.g. Asia/Ho_Chi_Minh
    --digest per_run|hourly|daily when notify/FYI messages are sent
    --model <id>                  default model of behaviors ("" = settings default)
    --max-tokens N                default run token budget (0 clears)
Behaviors override language, tone, model and budgets when they set them.`

func profileJSON(p behavior.Profile) map[string]any {
	out := map[string]any{
		"language": p.Language, "tone": p.Tone, "quiet_hours": behavior.FormatQuietHours(p), "timezone": p.Timezone,
		"digest": p.DigestMode, "model": p.DefaultModel, "max_tokens": behavior.ProfileMaxTokens(p),
	}
	if !p.UpdatedAt.IsZero() {
		out["updated_at"] = p.UpdatedAt.UTC().Format(time.RFC3339)
	}
	return out
}

func profileCommand(ctx context.Context, pool *pgxpool.Pool, args []string, out io.Writer) error {
	if len(args) == 0 || (args[0] != "show" && args[0] != "set") {
		return errors.New(profileUsage)
	}
	sub := args[0]
	f := commandFlags("profile " + sub)
	ws := f.String("workspace-id", "", "workspace UUID")
	language := f.String("language", "", "language code")
	tone := f.String("tone", "", "tone")
	quiet := f.String("quiet-hours", "", "quiet hours")
	tz := f.String("timezone", "", "IANA time zone")
	digest := f.String("digest", "", "digest mode")
	model := f.String("model", "", "model ID")
	maxTokens := f.Int("max-tokens", 0, "run token budget")
	if f.Parse(args[1:]) != nil || f.NArg() != 0 || !tenant.ValidUUID(*ws) {
		return errors.New(profileUsage)
	}
	set := map[string]bool{}
	f.Visit(func(fl *flag.Flag) { set[fl.Name] = true })
	delete(set, "workspace-id")
	if (sub == "show" && len(set) > 0) || (sub == "set" && len(set) == 0) {
		return errors.New(profileUsage)
	}
	store, err := storage.NewBehaviorStore(pool)
	if err != nil {
		return err
	}
	p, err := store.GetProfile(ctx, *ws)
	if err != nil {
		return errors.New("database operation failed")
	}
	enc := json.NewEncoder(out)
	if sub == "show" {
		return enc.Encode(profileJSON(p))
	}
	if set["language"] {
		p.Language = *language
	}
	if set["tone"] {
		p.Tone = *tone
	}
	if set["quiet-hours"] {
		start, end, err := behavior.ParseQuietHours(*quiet)
		if err != nil {
			return errors.New("quiet hours must be HH:MM-HH:MM (24h, start and end different) or off")
		}
		p.QuietStart, p.QuietEnd = start, end
	}
	if set["timezone"] {
		p.Timezone = *tz
	}
	if set["digest"] {
		p.DigestMode = behavior.DigestMode(*digest)
	}
	if set["model"] {
		p.DefaultModel = *model
	}
	if set["max-tokens"] {
		if err := behavior.SetProfileMaxTokens(&p, *maxTokens); err != nil {
			return errors.New("max tokens must be between 1 and 1000000 (0 clears)")
		}
	}
	if err := p.Validate(); err != nil {
		// Names the field only ("invalid profile timezone"); the value is
		// not echoed.
		return errors.New("invalid " + strings.TrimPrefix(err.Error(), behavior.ErrInvalid.Error()+": "))
	}
	saved, err := store.SaveProfile(ctx, *ws, p)
	if err != nil {
		return errors.New("database operation failed")
	}
	return enc.Encode(profileJSON(saved))
}

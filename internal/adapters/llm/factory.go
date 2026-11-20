package llm

import (
	"context"
	"net/http"

	"github.com/LDKhangg/nexus-hub/internal/adapters/llm/clirun"
	"github.com/LDKhangg/nexus-hub/internal/domain"
	"github.com/LDKhangg/nexus-hub/internal/settings"
)

// Factory builds the workspace LLM client for the configured provider
// (settings.llm_provider). A CLI provider is built once, so its
// concurrency semaphore is shared by every caller.
type Factory struct {
	provider ProviderType
	s        settings.Settings
	resolver SecretResolver
	httpc    *http.Client
	cli      domain.LLMProvider // the CLI provider, nil when unavailable
	meter    *Meter             // nil: calls are not metered
}

// SetMeter makes every client this factory returns count its calls into
// the spend ledger and enforce the daily budget (Phase 12). Call it once
// at startup, before Client is used.
func (f *Factory) SetMeter(m *Meter) { f.meter = m }

// NewFactory prepares the configured provider. tmpRoot is the parent of
// the CLI working directories ("" for the system temp directory). A CLI
// provider whose binary is missing does not fail: every Client call then
// returns ErrLLMUnconfigured, like a missing API key.
func NewFactory(s settings.Settings, resolver SecretResolver, httpc *http.Client, tmpRoot string) (*Factory, error) {
	pt, err := ParseProviderType(s.LLMProvider)
	if err != nil {
		return nil, err
	}
	f := &Factory{provider: pt, s: s, resolver: resolver, httpc: httpc}
	switch pt {
	case ProviderClaudeCLI:
		r, err := clirun.New(clirun.Config{Path: s.ClaudeCLIPath, Timeout: s.ClaudeCLITimeout, Concurrency: int(s.ClaudeCLIConcurrency), TmpRoot: tmpRoot})
		if err == nil {
			f.cli, err = NewClaudeCLI(r, s.ActorModel)
		}
		if err != nil {
			f.cli = nil
		}
	case ProviderOpencodeCLI:
		r, err := clirun.New(clirun.Config{Path: s.OpencodeCLIPath, Timeout: s.OpencodeCLITimeout, Concurrency: int(s.OpencodeCLIConcurrency), TmpRoot: tmpRoot})
		if err == nil {
			f.cli, err = NewOpencodeCLI(r, s.ActorModel)
		}
		if err != nil {
			f.cli = nil
		}
	}
	return f, nil
}

// Provider is the configured provider name (never a credential).
func (f *Factory) Provider() ProviderType { return f.provider }

// Ready reports whether a CLI provider's binary was found at startup
// (always true for the API provider, whose key is resolved per call).
func (f *Factory) Ready() bool { return f.provider == ProviderAnthropic || f.cli != nil }

// Client returns the workspace client. For the API provider the key is
// resolved from the workspace credential on each call.
func (f *Factory) Client(ctx context.Context, workspaceID string) (*Client, error) {
	switch f.provider {
	case ProviderClaudeCLI, ProviderOpencodeCLI:
		if f.cli == nil {
			return nil, ErrLLMUnconfigured
		}
		p := f.meter.Wrap(f.cli, workspaceID, f.provider, f.s.ActorModel)
		return &Client{providers: map[ProviderType]domain.LLMProvider{f.provider: p}, defaultProvider: f.provider}, nil
	default:
		c, err := NewClientFromSecret(ctx, f.resolver, workspaceID, f.s, f.httpc)
		if err != nil || f.meter == nil {
			return c, err
		}
		for pt, p := range c.providers {
			c.providers[pt] = f.meter.Wrap(p, workspaceID, pt, f.s.ActorModel)
		}
		return c, nil
	}
}

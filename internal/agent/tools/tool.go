// Package tools is the agent tool core: tool contracts, the registry and
// the tiered policy executor that every tool call goes through.
package tools

import (
	"context"
	"encoding/json"

	"github.com/LDKhangg/nexus-hub/internal/tenant"
)

// ToolSpec describes a tool to the LLM and to the policy.
type ToolSpec struct {
	Name, Description string
	InputSchema       json.RawMessage
	Tier              tenant.Tier
}

// Call is one authorized invocation. Repo and Account are set for
// repo-scoped tools after the policy resolved the grant.
type Call struct {
	Input           json.RawMessage
	Repo            *tenant.RepoGrant
	Account         *tenant.GitHubAccount
	DryRun          bool
	WorkspaceID     string
	ClickUpTokenRef string
}

// ToolOutput is compact text for the LLM plus a short audit summary.
type ToolOutput struct{ Text, Summary string }

// ToolError is a tool failure with a safe code; its text never carries
// secrets or raw upstream bodies. Denied marks a refusal on policy grounds
// (secret path, branch pattern, repo mismatch...), audited as denied.
type ToolError struct {
	Code      string
	Retryable bool
	Denied    bool
}

func (e *ToolError) Error() string { return e.Code }

// Tool is implemented by every agent tool.
type Tool interface {
	Spec() ToolSpec
	Run(context.Context, Call) (ToolOutput, error)
}

// RepoScoped tools name the repository their input targets.
type RepoScoped interface {
	Repo(json.RawMessage) (string, error)
}

// TierEscalator tools may raise the tier of a specific call (git_push to a
// protected branch becomes dangerous).
type TierEscalator interface {
	EffectiveTier(json.RawMessage, *tenant.RepoGrant) tenant.Tier
}

// NetworkTool tools get the network timeout instead of the read timeout.
type NetworkTool interface {
	UsesNetwork() bool
}

// Sanitizer cleans tool output and audit text (implemented by agent.Redactor).
type Sanitizer interface {
	CleanOutbound(s string, neutralizeMentions bool) string
}

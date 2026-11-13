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
// Ambiguous marks a failure after the request may have reached the remote
// (transport error after sending, 5xx, invalid response, failed push): a
// write or dangerous call is then audited as unknown_outcome and never
// repeated.
type ToolError struct {
	Code      string
	Retryable bool
	Denied    bool
	Ambiguous bool
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

// AccountScoped tools act as a named hub GitHub account on any repository
// the account can reach, without a repo grant (for example approving a pull
// request as the owner). Target returns the account name and the
// repository the input names; the repository is recorded in the audit only.
// The executor resolves the account from the policy's Accounts and passes
// it as Call.Account (Call.Repo stays nil); an unknown account is denied
// with account_not_found. A tool that is also RepoScoped is treated as
// RepoScoped only.
type AccountScoped interface {
	Target(json.RawMessage) (account, repo string, err error)
}

// ActionOnly tools are write actions run only by the behavior engine's
// run-level action executor. The executor offers and runs one only when it
// is both enabled and listed in UnscopedWrites, so an agent job's policy
// can never expose it to the model or call it.
type ActionOnly interface {
	ActionOnly()
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

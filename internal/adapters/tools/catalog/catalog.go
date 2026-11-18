// Package catalog lists every action name the hub can run, for the policy
// engine. The server and the nh policy CLI build the same set from here, so
// a rule set the CLI accepts is the one the server loads.
package catalog

import (
	clickuptools "github.com/LDKhangg/nexus-hub/internal/adapters/tools/clickup"
	codetools "github.com/LDKhangg/nexus-hub/internal/adapters/tools/code"
	githubtools "github.com/LDKhangg/nexus-hub/internal/adapters/tools/github"
	"github.com/LDKhangg/nexus-hub/internal/agent/tools"
	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/LDKhangg/nexus-hub/internal/orchestrator"
	"github.com/LDKhangg/nexus-hub/internal/policy"
)

// Tools returns every tool the hub registers, built without dependencies
// only to read their specs (never run them): GitHub read, write, dangerous
// and approve tools, the ClickUp task, write and chat reaction tools, and
// the code and workspace tools (known even where git is unavailable).
func Tools() []tools.Tool {
	all := append([]tools.Tool{}, githubtools.NewReadTools(nil)...)
	all = append(all, githubtools.NewWriteTools(nil, nil)...)
	all = append(all, githubtools.NewDangerousTools(nil, nil)...)
	all = append(all, githubtools.NewApproveTool(nil))
	all = append(all, clickuptools.NewTaskTool(nil, nil))
	all = append(all, clickuptools.NewWriteTools(nil, nil)...)
	all = append(all, clickuptools.NewChatReactionTool(nil, nil, nil), clickuptools.NewChatReactionRemoveTool(nil, nil, nil))
	all = append(all, codetools.NewReadTools(nil, "/nonexistent")...)
	all = append(all, codetools.NewWorkspaceTools(nil, nil, "/nonexistent", 0, nil)...)
	return all
}

// PolicyKnownActions is the closed set of names a rule set may use: every
// tool of Tools, the behavior engine's memory_search, every orchestrator
// capability, and extra (for example the names of a live registry, so a
// registered tool is never unknown).
func PolicyKnownActions(extra ...string) policy.KnownActions {
	known := policy.KnownActions{}
	for _, t := range Tools() {
		known[t.Spec().Name] = true
	}
	known[behavior.MemorySearchTool] = true
	for _, c := range orchestrator.NewCapabilityRegistry().KnownCapabilities() {
		known[c] = true
	}
	for _, n := range extra {
		if n != "" {
			known[n] = true
		}
	}
	return known
}

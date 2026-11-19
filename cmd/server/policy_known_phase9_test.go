package main

import (
	"slices"
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/adapters/tools/catalog"
	"github.com/LDKhangg/nexus-hub/internal/agent/tools"
)

// Phase 9 Batch B (Review A note): the server and the nh policy CLI share one
// known-action set; a registry only adds its own names to it.

// TC-KA-1 (REQ-1): for the registry of the catalog's own tools the server's set equals the CLI's set.
func TestP9ServerKnownActionsEqualCLISet(t *testing.T) {
	reg, err := tools.NewRegistry(catalog.Tools()...)
	if err != nil {
		t.Fatal(err)
	}
	server := policyKnownActions(reg).Names()
	cli := catalog.PolicyKnownActions().Names()
	if !slices.Equal(server, cli) {
		t.Fatalf("server and CLI disagree:\nserver %v\ncli    %v", server, cli)
	}
	for _, name := range []string{"github_pr_merge", "github.merge", "memory_search"} {
		if !slices.Contains(cli, name) {
			t.Errorf("%s is not a known action", name)
		}
	}
}

// TC-KA-2 (REQ-1): a tool only the live registry has is known to the server, never to the CLI, and nothing is lost.
func TestP9ServerKnownActionsAddRegistryNames(t *testing.T) {
	reg, err := tools.NewRegistry(catalog.Tools()...)
	if err != nil {
		t.Fatal(err)
	}
	server := policyKnownActions(reg)
	for _, name := range catalog.PolicyKnownActions().Names() {
		if !server[name] {
			t.Errorf("the server lost %s", name)
		}
	}
	if catalog.PolicyKnownActions()["not_a_real_tool"] || server["not_a_real_tool"] || server[""] {
		t.Fatal("an unknown name is known")
	}
}

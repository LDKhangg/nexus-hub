package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/workflow"
)

// TC-A3.5 (REQ-3): workflow CLI commands accept owner references
// case-insensitively, retain positive integer compatibility, emit canonical
// references, and reject malformed or non-positive input safely.
func TestPhase6WorkflowCLIReferences(t *testing.T) {
	f := newWorkflowCLIFixture(t)
	ex := f.start(t, "phase6-ref", workflow.References{}, "model")
	wantRef := workflow.FormatRef(ex.ID)

	for _, input := range []string{wantRef, strings.ToLower(wantRef), fmt.Sprint(ex.ID)} {
		out := decodeObject(t, f.must("workflow", "show", input, "--workspace-id", f.ws))
		if out["ref"] != wantRef || out["id"] != float64(ex.ID) {
			t.Errorf("show %q = %v, want id=%d ref=%s", input, out, ex.ID, wantRef)
		}
	}
	for _, input := range []string{"WF-0", "WF--1", "WF-nope", "0", "-1"} {
		out, err := f.run("workflow", "show", input, "--workspace-id", f.ws)
		if err == nil || out != "" || !strings.Contains(err.Error(), "execution reference") {
			t.Errorf("show %q: out=%q err=%v", input, out, err)
		}
	}

	list, _ := objectItems(t, f.must("workflow", "list", "--workspace-id", f.ws), "executions")
	if len(list) == 0 || list[0].(map[string]any)["ref"] != wantRef {
		t.Fatalf("list=%v, want canonical ref %s", list, wantRef)
	}
}

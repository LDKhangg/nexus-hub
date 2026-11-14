package reporting

import (
	"context"
	"strings"
	"testing"
)

// TC-A3.3 (REQ-3): owner templates can render a sanitized workflow reference,
// omit its optional line when absent, and preview it with a stable fake value.
func TestPhase6WorkflowReferenceTemplateAndPreview(t *testing.T) {
	body := "Result\n?{workflow_ref}Run {workflow_ref}"
	out, err := Render(body, KindNotify, Values{WorkflowRef: "WF-42\x00"})
	if err != nil || out != "Result\nRun WF-42" {
		t.Fatalf("render=%q err=%v", out, err)
	}
	out, err = Render(body, KindNotify, Values{})
	if err != nil || out != "Result" {
		t.Fatalf("empty render=%q err=%v", out, err)
	}
	preview, err := Preview(KindNotify, body)
	if err != nil || !strings.Contains(preview, "WF-101") {
		t.Fatalf("preview=%q err=%v", preview, err)
	}
}

// TC-A3.4 (REQ-3): durable default reports show the workflow reference beside
// the trace hint, while legacy reports without a reference remain unchanged.
func TestPhase6DefaultReportWorkflowReferenceIsAdditive(t *testing.T) {
	store := newPhase4Store()
	store.prefs[scopeKey("owner", "ws", "")] = fullPrefs(StyleNormal)
	r := NewRenderer(store, nil)
	base := Report{Kind: KindNotify, UserID: "owner", WorkspaceID: "ws", TraceHint: "trace 7", Values: Values{Outcome: "done"}}
	legacy, err := r.Render(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	withRef := base
	withRef.WorkflowRef = "WF-7"
	got, err := r.Render(context.Background(), withRef)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(legacy, "WF-") || !strings.Contains(got, "trace 7 · WF-7") || strings.Replace(got, "\ntrace 7 · WF-7", "", 1) != legacy {
		t.Fatalf("legacy=%q withRef=%q", legacy, got)
	}
}

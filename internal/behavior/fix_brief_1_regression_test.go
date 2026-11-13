package behavior

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

type fixBriefRunStore struct {
	RunExecStore
	saved json.RawMessage
}

func (s *fixBriefRunStore) SetRunActions(_ context.Context, _ int64, _ string, result json.RawMessage) error {
	s.saved = append(s.saved[:0], result...)
	return nil
}

type fixBriefRunActions struct {
	t      *testing.T
	called int
}

func (r *fixBriefRunActions) RunActions(_ context.Context, rc RunContext, _ RunActionSaver) (RunActionsEnd, error) {
	r.called++
	verdict := DecodeRunActions(rc.Run.RunActions).Verdict
	if verdict == nil || verdict.Code != CodeTriggerTextUnavailable {
		r.t.Fatalf("run actions received verdict %#v", verdict)
	}
	if ok, code := gate(rc.Doc.RunActions[0].When, rc.Doc.Verdict, verdict); ok || code != CodeVerdictUnavailable {
		r.t.Fatalf("gated action was not failed closed: ok=%v code=%q", ok, code)
	}
	return RunActionsEnd{}, nil
}

type fixBriefChatReader struct{ err error }

func (r fixBriefChatReader) ChatMessageText(context.Context, string, string, string, string, string) (string, error) {
	return "", r.err
}

type fixBriefCommentReader struct{ err error }

func (r fixBriefCommentReader) TaskComment(context.Context, string, string, string, string) (TaskComment, error) {
	return TaskComment{}, r.err
}

func TestFixBrief1TriggerTextUnavailableFailsClosed(t *testing.T) {
	// TC-R1 (REQ-E2, REQ-E3).
	tests := []struct {
		name string
		read func(*RunWorker, *collector) *runOutcome
	}{
		{
			name: "TC-R1/chat permanent read failure",
			read: func(w *RunWorker, col *collector) *runOutcome {
				w.d.ChatMessages = fixBriefChatReader{err: &ChatReadError{Code: "not_found"}}
				return w.readTriggerMessage(context.Background(), col, "token", Run{TriggerKind: TriggerChatMessage}, TriggerData{TeamID: "123", ChannelID: "channel_1", MessageID: "message_1"}, true)
			},
		},
		{
			name: "TC-R1/comment invalid trusted id",
			read: func(w *RunWorker, col *collector) *runOutcome {
				w.d.Comments = fixBriefCommentReader{}
				return w.readTriggerComment(context.Background(), col, "token", "123", Run{TriggerKind: TriggerCommentAdded}, TriggerData{TaskID: "../task", CommentID: "comment_1"}, true)
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := &fixBriefRunStore{}
			actions := &fixBriefRunActions{t: t}
			w := NewRunWorker(RunWorkerDeps{Runs: store, RunActions: actions}, RunWorkerSettings{})
			out := tc.read(w, newCollector(nil, "", nil))
			if out == nil || out.retry || out.status != RunFailed || out.code != CodeTriggerTextUnavailable {
				t.Fatalf("read outcome = %#v", out)
			}
			rc := &RunContext{
				Run: Run{ID: 1, ClaimToken: "claim", Attempts: MaxRunAttempts},
				Doc: ResolvedDoc{
					Verdict: &VerdictSpec{Labels: []string{"approve_request", "other"}, MinConfidence: .8},
					RunActions: []ActionSpec{
						{Type: ActionPRApprove, When: &ActionWhen{Verdict: "approve_request"}},
						{Type: ActionTelegramMessage},
					},
				},
			}
			end, err := w.noVerdict(context.Background(), rc, out)
			if err != nil || end == nil || end.status != RunDone || end.code != CodeTriggerTextUnavailable {
				t.Fatalf("noVerdict = %#v, %v", end, err)
			}
			if actions.called != 1 {
				t.Fatalf("report actions called %d times", actions.called)
			}
			if got := DecodeRunActions(store.saved).Verdict; got == nil || got.Code != CodeTriggerTextUnavailable {
				t.Fatalf("stored verdict = %#v", got)
			}
		})
	}

	t.Run("TC-R1/transient failure still retries", func(t *testing.T) {
		w := NewRunWorker(RunWorkerDeps{ChatMessages: fixBriefChatReader{err: &ChatReadError{Code: "timeout", Transient: true}}}, RunWorkerSettings{})
		out := w.readTriggerMessage(context.Background(), newCollector(nil, "", nil), "token", Run{TriggerKind: TriggerChatMessage}, TriggerData{TeamID: "123", ChannelID: "channel_1", MessageID: "message_1"}, true)
		if out == nil || !out.retry || out.code != CodeReadTransient {
			t.Fatalf("transient outcome = %#v", out)
		}
	})
}

func TestFixBrief1TraceOmitsOnlyTriggerTextQuotes(t *testing.T) {
	// TC-R2 (REQ-E3, REQ-E10).
	t.Run("TC-R2/valid trigger locators lose quotes", func(t *testing.T) {
		secret := "PRIVATE TRIGGER TEXT"
		got := traceItem(Item{Citations: []Citation{
			{Locator: "clickup:chat/message_1#text", Quote: secret},
			{Locator: "clickup:comment/comment_1#text", Quote: secret},
		}}, nil)
		for _, citation := range got.Citations {
			if citation.Quote != "" || citation.Locator == "" {
				t.Fatalf("unsafe citation: %#v", citation)
			}
		}
		rendered := renderTrace(Trace{Items: []TraceItem{got}}, nil, true, false)
		if strings.Contains(rendered, secret) || !strings.Contains(rendered, "clickup:chat/message_1#text") {
			t.Fatalf("unsafe rendered trace: %s", rendered)
		}
	})

	t.Run("TC-R2/lookalikes and non-trigger context retain quotes", func(t *testing.T) {
		citations := []Citation{
			{Locator: "clickup:chat/../message#text", Quote: "lookalike"},
			{Locator: "clickup:comment/comment_1#text:L1", Quote: "line quote"},
			{Locator: "clickup:task/task_1#desc:L1", Quote: "ordinary context"},
		}
		got := traceItem(Item{Citations: citations}, nil)
		for i := range citations {
			if got.Citations[i].Quote != citations[i].Quote {
				t.Fatalf("citation %d quote = %q", i, got.Citations[i].Quote)
			}
		}
	})
}

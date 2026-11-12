package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/domain"
	"github.com/LDKhangg/nexus-hub/internal/memory"
)

// TC-8 (REQ-3): both low-confidence Ignore and tool-less Investigate notify.
func TestApproved_LowConfidenceAndUnavailableToolsNotify(t *testing.T) {
	for _, d := range []TriageDecision{{Category: CategoryIgnore, Confidence: .849, Summary: "cần đọc"}, {Category: CategoryInvestigate, Confidence: .99, Summary: "cần đọc", Goal: "tìm lỗi"}} {
		s := &fakeStore{job: job(1), ctx: jobCtx(nil)}
		if err := newOrch(t, s, &fakeTriager{d: d}, &fakeInvestigator{}).processNext(context.Background()); err != nil {
			t.Fatal(err)
		}
		if s.outcome == nil || s.outcome.Outcome != OutcomeNotify || !strings.HasPrefix(s.outcome.Text, "🧠 Nexus") || s.fallback != nil {
			t.Error("low-confidence/no-tool routing silently ignored comment")
		}
	}
}

type approvedCountLLM struct {
	base  fakeLLM
	calls int
}

func (l *approvedCountLLM) Generate(ctx context.Context, r *domain.LLMRequest) (*domain.LLMResponse, error) {
	l.calls++
	return l.base.Generate(ctx, r)
}

// TC-10 (REQ-4): real Tier-1 parsing/error classification and bounded retries,
// rather than feeding pre-classified failures directly into the orchestrator.
func TestApproved_ProviderFailuresEndWithSafeOriginalNotice(t *testing.T) {
	for _, mode := range []string{"malformed", "invented_category", "refusal", "provider_error", "timeout", "missing_credentials"} {
		t.Run(mode, func(t *testing.T) {
			llm := &approvedCountLLM{base: fakeLLM{resp: &domain.LLMResponse{Text: "not JSON"}}}
			want := CodeInvalidDecision
			attempts := 5
			switch mode {
			case "invented_category":
				llm.base.resp.Text = `{"category":"execute_shell","confidence":1,"summary":"x"}`
			case "refusal":
				llm.base.err = domain.ErrLLMRefused
				want = CodeLLMRefused
				attempts = 1
			case "provider_error":
				llm.base.err = errors.New("fake-private-provider-body")
				want = CodeLLMError
			case "timeout":
				llm.base.err = context.DeadlineExceeded
				want = CodeLLMError
			case "missing_credentials":
				want = CodeLLMUnconfigured
				attempts = 1
			}
			s := &fakeStore{ctx: jobCtx(nil)}
			o := newOrch(t, s, &fakeTriager{}, &fakeInvestigator{})
			o.models = func(context.Context, string) (Triager, Investigator, error) {
				if mode == "missing_credentials" {
					return nil, nil, errors.New("fake-private-credential-error")
				}
				return NewTier1Router(llm, "fake-triage"), nil, nil
			}
			for i := 1; i <= attempts; i++ {
				s.job = job(i)
				if err := o.processNext(context.Background()); err != nil {
					t.Fatal(err)
				}
				if i < attempts && s.fallback != nil {
					t.Error("retryable failure fell back too early")
				}
			}
			wantCalls := attempts
			if mode == "missing_credentials" {
				wantCalls = 0
			}
			if llm.calls != wantCalls || s.fallback == nil || s.fallback.code != want || s.outcome != nil {
				t.Error("provider failure did not end with bounded safe fallback")
			}
			if s.fallback != nil {
				if !strings.Contains(s.fallback.text, "login fails with 500") || !strings.Contains(s.fallback.text, "Alice") {
					t.Error("fallback lost original business comment")
				}
				for _, bad := range []string{"fake-private-provider-body", "fake-private-credential-error", "ghp_aaaa"} {
					if strings.Contains(s.fallback.text, bad) {
						t.Error("fallback leaked raw provider/credential/token material")
					}
				}
			}
		})
	}
}

// TC-42 (REQ-F5): provider text, the derived goal and echoed tool inputs
// must be redacted too, not just the original context packet.
func TestApproved_ActorFollowUpAllTextChannelsRedacted(t *testing.T) {
	secret := "private-provider-followup-password"
	s := &fakeStore{job: job(1), ctx: jobCtx([]string{"fake_read"})}
	tr := &fakeTriager{d: TriageDecision{Category: CategoryInvestigate, Confidence: .95, Summary: "Báo cáo", Goal: "Kiểm tra " + secret}}
	llm := &scriptedLLM{responses: []*domain.ToolResponse{
		{Stop: domain.StopToolUse, Text: "provider echoed " + secret, Calls: []domain.ToolCall{call("safe-id", "fake_read", `{"query":"`+secret+`"}`)}},
		answer("Hoàn tất 🧠"),
	}}
	o := newOrch(t, s, tr, &fakeInvestigator{})
	o.redactor = NewRedactor([]string{secret})
	o.models = func(context.Context, string) (Triager, Investigator, error) {
		return tr, NewTier2Actor(llm, "fake-actor"), nil
	}
	if err := o.processNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(llm.reqs) != 2 {
		t.Fatal("two-step actor fixture did not finish")
	}
	for i, r := range llm.reqs {
		b, _ := json.Marshal(r)
		if strings.Contains(string(b), secret) {
			t.Errorf("known secret reached actor LLM request %d", i+1)
		}
	}
}

type approvedCaptureTriager struct {
	packet   memory.ContextPacket
	persona  string
	calls    int
	decision TriageDecision
}

func (c *approvedCaptureTriager) Triage(_ context.Context, p memory.ContextPacket, persona string, _ bool) (TriageDecision, domain.Usage, error) {
	c.packet = p
	c.persona = persona
	c.calls++
	return c.decision, domain.Usage{}, nil
}

// TC-41 (REQ-F5), TC-42 (REQ-F5): inspect every packet text field and persona
// through the real orchestrator boundary; normal Vietnamese remains useful.
func TestApproved_AllTriageInputChannelsRedacted(t *testing.T) {
	known := "ordinary-private-system-password"
	pattern := "ghp_" + strings.Repeat("x", 36)
	jc := jobCtx(nil)
	jc.Title = "Lỗi đăng nhập 🧠 " + known
	jc.TaskURL = "https://app.clickup.com/t/" + known
	jc.Summary = "Tóm tắt " + known + " " + pattern
	jc.Config.Persona = "Kỹ sư Việt Nam " + known + " " + pattern
	jc.History = []memory.Message{
		{ID: 1, SenderRole: memory.SenderRoleExternalUser, SenderName: known, Content: "trước đây " + pattern},
		{ID: 5, SenderRole: memory.SenderRoleExternalUser, SenderName: known, Content: "Nút đăng nhập hỏng 🔎 " + known + " " + pattern},
	}
	s := &fakeStore{job: job(1), ctx: jc}
	tr := &approvedCaptureTriager{decision: TriageDecision{Category: CategoryNotify, Confidence: .9, Summary: "Đã nhận 🧠"}}
	o := newOrch(t, s, &fakeTriager{}, &fakeInvestigator{})
	o.redactor = NewRedactor([]string{known})
	o.models = func(context.Context, string) (Triager, Investigator, error) { return tr, &fakeInvestigator{}, nil }
	if err := o.processNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(tr.packet)
	input := string(b) + tr.persona
	if strings.Contains(input, known) || strings.Contains(input, pattern) {
		t.Error("secret crossed triage packet/persona boundary")
	}
	for _, normal := range []string{"Nút đăng nhập hỏng", "Kỹ sư Việt Nam", "🔎"} {
		if !strings.Contains(input, normal) {
			t.Errorf("ordinary text lost: %s", normal)
		}
	}
	if tr.calls != 1 || s.outcome == nil {
		t.Fatal("normal request not triaged")
	}
}

// TC-50 (REQ-F12): omission count is independently computed from the actual
// captured packet, and Ignore cannot silently advance over unseen comments.
func TestApproved_OmittedCommentsAreNeverSilentlyIgnored(t *testing.T) {
	for _, category := range []Category{CategoryNotify, CategoryIgnore} {
		t.Run(string(category), func(t *testing.T) {
			jc := jobCtx(nil)
			jc.LastTriagedID = 0
			jc.History = nil
			for i := 1; i <= 20; i++ {
				jc.History = append(jc.History, memory.Message{ID: int64(i), SenderRole: memory.SenderRoleExternalUser, SenderName: "Người dùng", Content: strings.Repeat("thông tin ", 60)})
			}
			s := &fakeStore{job: job(1), ctx: jc}
			tr := &approvedCaptureTriager{decision: TriageDecision{Category: category, Confidence: .99, Summary: "đã đọc"}}
			o := newOrch(t, s, &fakeTriager{}, &fakeInvestigator{})
			o.models = func(context.Context, string) (Triager, Investigator, error) { return tr, &fakeInvestigator{}, nil }
			if err := o.processNext(context.Background()); err != nil {
				t.Fatal(err)
			}
			if s.outcome == nil && s.fallback == nil {
				t.Fatal("batch disappeared without any committed result")
			}
			n := 20 - len(tr.packet.NewBatch)
			if n <= 0 {
				t.Fatal("fixture did not exceed packet budget")
			}
			line := fmt.Sprintf("(+%d earlier comment(s) in this batch were not shown to the AI; see the task)", n)
			if s.outcome != nil && s.outcome.ThroughMessageID == 20 && !strings.Contains(s.outcome.Text, line) {
				t.Errorf("advanced through all 20 comments with %d unseen and no exact disclosure", n)
			}
		})
	}
}

type approvedCompressorProbe struct {
	t      *testing.T
	store  *fakeStore
	called bool
}

func (c *approvedCompressorProbe) Compress(ctx context.Context, _, _ string) (bool, domain.Usage, error) {
	c.called = true
	if c.store.outcome == nil {
		c.t.Error("compression began before main report committed")
	}
	if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > time.Minute {
		c.t.Error("background compression has no bounded timeout")
	}
	return false, domain.Usage{}, errors.New("fake summary unavailable")
}

// TC-29 (REQ-14), TC-30 (REQ-14): optional compression cannot make the
// already committed report fail, and must bound a potentially slow provider.
func TestApproved_CompressionAfterCommitIsBoundedAndNonfatal(t *testing.T) {
	s := &fakeStore{job: job(1), ctx: jobCtx(nil)}
	o := newOrch(t, s, &fakeTriager{d: TriageDecision{Category: CategoryNotify, Confidence: .9, Summary: "Báo cáo đã lưu"}}, &fakeInvestigator{})
	probe := &approvedCompressorProbe{t: t, store: s}
	o.compressor = probe
	if err := o.processNext(context.Background()); err != nil {
		t.Error("optional summary error failed the main job")
	}
	if !probe.called || s.outcome == nil || s.fallback != nil || s.retryCode != "" {
		t.Error("main report was not committed before nonfatal compression error")
	}
}

// TC-51 (REQ-F13), TC-52 (REQ-F13): cache usage counts before any new call.
func TestApproved_ExhaustedDurableBudgetMakesNoLLMCall(t *testing.T) {
	for _, usage := range []domain.Usage{{Input: 60000}, {Input: 59000, CacheRead: 500, CacheWrite: 500}, {Output: 6000}} {
		j := job(2)
		j.Usage = usage
		s := &fakeStore{job: j, ctx: jobCtx([]string{"fake_read"})}
		tr := &fakeTriager{d: TriageDecision{Category: CategoryInvestigate, Confidence: .9, Goal: "g", Summary: "s"}}
		inv := &fakeInvestigator{}
		if err := newOrch(t, s, tr, inv).processNext(context.Background()); err != nil {
			t.Fatal(err)
		}
		if tr.calls != 0 || inv.budget.MaxInput != 0 || s.fallback == nil || s.fallback.code != CodeTokenBudget {
			t.Error("exhausted usage made another model call or lost token_budget fallback")
		}
	}
	// Tier-1 can consume the last remaining input; Tier-2 must not begin.
	j := job(1)
	j.Usage = domain.Usage{Input: 59999}
	s := &fakeStore{job: j, ctx: jobCtx([]string{"fake_read"})}
	tr := &fakeTriager{d: TriageDecision{Category: CategoryInvestigate, Confidence: .9, Goal: "g", Summary: "s"}, u: domain.Usage{Input: 1}}
	inv := &fakeInvestigator{}
	if err := newOrch(t, s, tr, inv).processNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if tr.calls != 1 || inv.budget.MaxInput != 0 || s.usage.Input != 1 || s.fallback == nil || s.fallback.code != CodeTokenBudget {
		t.Error("Tier-1 exhaustion did not stop Tier-2 with persisted usage")
	}
}

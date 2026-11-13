package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage/sqlc"
	clickuptools "github.com/LDKhangg/nexus-hub/internal/adapters/tools/clickup"
	"github.com/LDKhangg/nexus-hub/internal/agent"
	"github.com/LDKhangg/nexus-hub/internal/agent/tools"
	"github.com/LDKhangg/nexus-hub/internal/behavior"
	httpdelivery "github.com/LDKhangg/nexus-hub/internal/delivery/http"
	"github.com/LDKhangg/nexus-hub/internal/descblock"
	"github.com/LDKhangg/nexus-hub/internal/domain"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
	"github.com/labstack/echo/v4"
)

// s23HTTP is a completely in-process ClickUp API, not a forwarding transport.
// Unexpected hosts, paths and methods fail closed: no real external calls are possible.
type s23HTTP struct {
	mu          sync.Mutex
	description string
	writes      int
}

func (f *s23HTTP) RoundTrip(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Host != "api.clickup.com" || r.URL.Path != "/api/v2/task/abc" {
		return nil, errors.New("unexpected fake API target")
	}
	var body any
	switch r.Method {
	case http.MethodGet:
		body = map[string]any{"id": "abc", "name": "Review", "list": map[string]string{"id": "list-a"}, "status": map[string]string{"status": "tech spec review"}, "assignees": []map[string]int{{"id": 42}}, "description": f.description, "markdown_description": f.description}
	case http.MethodPut:
		var in struct {
			Markdown string `json:"markdown_content"`
		}
		if json.NewDecoder(r.Body).Decode(&in) != nil || in.Markdown == "" {
			return nil, errors.New("invalid fake write")
		}
		f.description = in.Markdown
		f.writes++
		body = map[string]string{"id": "abc"}
	default:
		return nil, errors.New("unexpected fake API method")
	}
	raw, _ := json.Marshal(body)
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(raw))), Request: r}, nil
}

func (f *s23HTTP) state() (string, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.description, f.writes
}
func (f *s23HTTP) edit(s string) { f.mu.Lock(); defer f.mu.Unlock(); f.description = s }

type s23Secrets struct{}

func (s23Secrets) Resolve(context.Context, string, string) (string, error) {
	return "obviously-fake-clickup-token", nil
}

type s23Policy struct{}

func (s23Policy) RunPolicy(context.Context, string, string) (tenant.AgentConfig, error) {
	return tenant.AgentConfig{ClickUpTokenRef: "clickup", Tools: tenant.ToolPolicyConfig{Enabled: []string{"clickup_get_task", behavior.ToolSetDescriptionBlock}, MaxWritesPerJob: 30}}, nil
}

type s23Lookup struct{}

func (s23Lookup) LookupWebhookIntegration(context.Context, string) (tenant.WebhookIntegration, error) {
	return tenant.WebhookIntegration{ID: integrationA, WorkspaceID: workspaceA, Platform: "telegram", Role: tenant.IntegrationRoleControl, IsEnabled: true}, nil
}

// P3-HTTP-01 (REQ-5): the actual webhook and actual inbox enforce auth/owner gate,
// fast durable acknowledgement and update_id replay protection before any work.
func TestS23TelegramWebhookDurablyQueuesOnlyAuthenticatedOwner(t *testing.T) {
	ctx, pool, s := s23Store(t)
	mustExec(t, ctx, pool, `INSERT INTO integrations(id,workspace_id,platform,role,config) VALUES($1,$2,'telegram','control','{}')`, integrationA, workspaceA)
	e := echo.New()
	h := httpdelivery.NewTelegramHandler(s23Lookup{}, s23Secrets{}, s, s, 42)
	e.POST("/webhook/telegram/:integrationID", h.Handle)
	request := func(secret string, chat int64, update int64) *httptest.ResponseRecorder {
		t.Helper()
		body := fmt.Sprintf(`{"update_id":%d,"message":{"message_id":7,"chat":{"id":%d},"from":{"id":%d},"text":"/review abc"}}`, update, chat, chat)
		r := httptest.NewRequest(http.MethodPost, "/webhook/telegram/"+integrationA, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set(httpdelivery.TelegramSecretHeader, secret)
		out := httptest.NewRecorder()
		start := time.Now()
		e.ServeHTTP(out, r)
		if time.Since(start) >= 2*time.Second {
			t.Error("webhook exceeded two-second acknowledgement budget")
		}
		return out
	}
	for _, secret := range []string{"", "wrong-fake-secret"} {
		if out := request(secret, 42, 1); out.Code != http.StatusUnauthorized {
			t.Fatalf("bad secret response=%d", out.Code)
		}
	}
	if out := request("obviously-fake-clickup-token", 99, 2); out.Code != http.StatusOK {
		t.Fatalf("foreign chat response=%d", out.Code)
	}
	if u, err := s.ClaimUpdate(ctx, time.Now(), time.Minute); err != nil || u != nil {
		t.Fatal("unauthorized/non-owner update stored")
	}
	if out := request("obviously-fake-clickup-token", 42, 3); out.Code != http.StatusOK || !strings.Contains(out.Body.String(), "queued") {
		t.Fatalf("owner response=%d", out.Code)
	}
	if out := request("obviously-fake-clickup-token", 42, 3); out.Code != http.StatusOK || !strings.Contains(out.Body.String(), "duplicate") {
		t.Fatal("webhook retry not deduplicated")
	}
	u, err := s.ClaimUpdate(ctx, time.Now().Add(time.Second), time.Minute)
	if err != nil || u == nil || u.UpdateID != 3 || u.ChatID != 42 || u.Command != "/review" || u.Args != "abc" || u.IntegrationID != integrationA {
		t.Fatalf("normalized work not durable: %v", err)
	}
	if err := s.FinishUpdate(ctx, u.ID, u.ClaimToken, ""); err != nil {
		t.Fatal(err)
	}
	if next, err := s.ClaimUpdate(ctx, time.Now().Add(2*time.Minute), time.Minute); err != nil || next != nil {
		t.Fatal("duplicate owner work remained")
	}
}

// P3-HTTP-02 (REQ-5, REQ-14; R5): answer A must not consume B's mutable slot,
// and replaying A's older update must not mutate an already queued newer reply.
func TestS23QueuedRepliesRemainBoundToTheirOwnUpdate(t *testing.T) {
	for _, scenario := range []string{"burst_preserves_first_answer", "duplicate_cannot_overwrite_newer_answer", "secret_redacted_before_persistence"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, pool, s := s23Store(t)
			mustExec(t, ctx, pool, `INSERT INTO integrations(id,workspace_id,platform,role,config) VALUES($1,$2,'telegram','control','{}')`, integrationA, workspaceA)
			v := s23Version(t, ctx, s, "replies")
			run := s23Run(t, ctx, s, v, "reply")
			item := s23Item(t, ctx, s, run, "ask", behavior.ClassAsk)
			a, err := s.CreateApproval(ctx, workspaceA, behavior.NewApproval{Kind: behavior.ApprovalItemAnswer, RunID: run.ID, ItemID: item.ID, Nonce: "s23_reply_nonce_000001", ExpiresAt: time.Now().Add(time.Hour)})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.SetReplyMessage(ctx, workspaceA, a.ID, 777); err != nil {
				t.Fatal(err)
			}
			e := echo.New()
			e.POST("/webhook/telegram/:integrationID", httpdelivery.NewTelegramHandler(s23Lookup{}, s23Secrets{}, s, s, 42).Handle)
			send := func(id int64, text string) {
				t.Helper()
				raw, _ := json.Marshal(map[string]any{"update_id": id, "message": map[string]any{"message_id": id, "chat": map[string]int{"id": 42}, "from": map[string]int{"id": 42}, "text": text, "reply_to_message": map[string]int{"message_id": 777}}})
				r := httptest.NewRequest(http.MethodPost, "/webhook/telegram/"+integrationA, strings.NewReader(string(raw)))
				r.Header.Set(httpdelivery.TelegramSecretHeader, "obviously-fake-clickup-token")
				out := httptest.NewRecorder()
				e.ServeHTTP(out, r)
				if out.Code != http.StatusOK {
					t.Fatalf("reply acknowledgement=%d", out.Code)
				}
			}
			if scenario == "secret_redacted_before_persistence" {
				secret := "ghp_" + strings.Repeat("B", 36)
				send(10, "Trả lời có token "+secret)
				stored, err := s.GetApproval(ctx, workspaceA, a.ID)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(stored.PendingAnswer, secret) {
					t.Fatal("R1: webhook persisted raw secret in pending answer")
				}
				return
			}
			send(10, "Answer A")
			send(11, "Answer B")
			if scenario == "duplicate_cannot_overwrite_newer_answer" {
				before, err := s.GetApproval(ctx, workspaceA, a.ID)
				if err != nil {
					t.Fatal(err)
				}
				send(10, "Answer A")
				after, err := s.GetApproval(ctx, workspaceA, a.ID)
				if err != nil {
					t.Fatal(err)
				}
				if after.PendingAnswer != before.PendingAnswer {
					t.Fatal("R5: duplicate older update changed pending answer")
				}
				return
			}
			u, err := s.ClaimUpdate(ctx, time.Now().Add(time.Second), time.Minute)
			if err != nil || u == nil || u.UpdateID != 10 {
				t.Fatalf("oldest queued reply unavailable: %v", err)
			}
			stored, err := s.GetApprovalByNonce(ctx, workspaceA, u.Args)
			if err != nil {
				t.Fatal(err)
			}
			if stored.PendingAnswer != "Answer A" {
				t.Fatal("R5: processing update A would consume answer B")
			}
		})
	}
}

type s23TaskReader struct{ r *clickuptools.TaskReader }

func (r s23TaskReader) ReadTask(ctx context.Context, ws, ref, id string) (behavior.TaskState, error) {
	s, err := r.r.ReadTask(ctx, ws, ref, id)
	return behavior.TaskState{ID: s.ID, Status: s.Status, AssigneeIDs: s.AssigneeIDs, Description: s.Description, Markdown: s.Markdown}, err
}

type s23Telegram struct{ messages []behavior.OutMessage }

func (m *s23Telegram) SendMessage(_ context.Context, to behavior.ChatRef, msg behavior.OutMessage) (int64, error) {
	if to.WorkspaceID != workspaceA || to.ChatID != 42 {
		return 0, errors.New("wrong owner chat")
	}
	m.messages = append(m.messages, msg)
	return int64(len(m.messages)), nil
}
func (*s23Telegram) RemoveButtons(context.Context, behavior.ChatRef, int64) error { return nil }
func (*s23Telegram) AnswerCallback(context.Context, behavior.ChatRef, string, string) error {
	return nil
}
func (*s23Telegram) OwnerChat(context.Context, string, behavior.TriggerData) (behavior.ChatRef, error) {
	return behavior.ChatRef{WorkspaceID: workspaceA, ChatID: 42}, nil
}

type s23Model struct {
	calls  int
	output string
}

// Stop after one durable completion rather than polling/sleeping in a background goroutine.
type s23OneInbox struct {
	*BehaviorStore
	cancel    context.CancelFunc
	completed bool
}

func (s *s23OneInbox) FinishUpdate(ctx context.Context, id int64, token, code string) error {
	err := s.BehaviorStore.FinishUpdate(ctx, id, token, code)
	if err == nil {
		s.completed = true
		s.cancel()
	}
	return err
}

func s23DeliverTelegram(t *testing.T, ctx context.Context, s *BehaviorStore, engine *behavior.Engine, messenger *s23Telegram, payload any) {
	t.Helper()
	e := echo.New()
	e.POST("/webhook/telegram/:integrationID", httpdelivery.NewTelegramHandler(s23Lookup{}, s23Secrets{}, s, s, 42).Handle)
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/webhook/telegram/"+integrationA, strings.NewReader(string(raw)))
	req.Header.Set(httpdelivery.TelegramSecretHeader, "obviously-fake-clickup-token")
	out := httptest.NewRecorder()
	e.ServeHTTP(out, req)
	if out.Code != http.StatusOK || !strings.Contains(out.Body.String(), "queued") {
		t.Fatalf("Telegram journey update not queued: status=%d", out.Code)
	}
	workCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	inbox := &s23OneInbox{BehaviorStore: s, cancel: cancel}
	worker := behavior.NewInboxWorker(behavior.InboxDeps{Inbox: inbox, Approvals: s, Defs: s, Messenger: messenger, Callbacks: engine}, behavior.InboxSettings{})
	if err := worker.Run(workCtx); err != nil {
		t.Fatal(err)
	}
	if !inbox.completed {
		t.Fatal("Telegram journey update not durably processed")
	}
}

func (m *s23Model) Generate(_ context.Context, r *domain.LLMRequest) (*domain.LLMResponse, error) {
	m.calls++
	if len(r.Messages) == 0 || !strings.Contains(r.Messages[0].Text, "clickup:task/abc#desc") {
		return nil, errors.New("context missing from model request")
	}
	return &domain.LLMResponse{Text: m.output, Usage: domain.Usage{Input: 100, Output: 50}}, nil
}

// P3-JOURNEY-01 (REQ-1..REQ-4, REQ-8..REQ-20, REQ-23, REQ-25): actual example,
// actual DB, read/write adapters and policy/audit executor; only HTTP, LLM and Telegram are fake.
func TestS23ExampleStatusDraftPostAskDigestTraceUndo(t *testing.T) {
	for _, scenario := range []string{"full_loop", "outside_conflict", "broken_markers", "undo_conflict", "crash_after_decision_before_write"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, pool, s := s23Store(t)
			mustExec(t, ctx, pool, `INSERT INTO integrations(id,workspace_id,platform,role,config) VALUES($1,$2,'telegram','control','{}')`, integrationA, workspaceA)
			source, err := os.ReadFile("../../../examples/behaviors/spec-review.yaml")
			if err != nil {
				t.Fatal(err)
			}
			reads := []string{"clickup_get_task", "github_pr_view", "github_pr_files", "github_issue_view", behavior.MemorySearchTool}
			b, problems, err := behavior.ImportDraft(ctx, s, workspaceA, source, behavior.FormatYAML, nil, reads)
			if err != nil || len(problems) != 0 {
				t.Fatalf("acceptance example invalid: errors=%v error=%v", problems, err)
			}
			v, err := behavior.PublishDraft(ctx, s, workspaceA, b.Name, reads)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.SetStatus(ctx, workspaceA, b.Name, behavior.StatusEnabled); err != nil {
				t.Fatal(err)
			}
			mustExec(t, ctx, pool, `INSERT INTO app_settings(key,value) VALUES('owner_clickup_user_id','"42"')`)
			ws, _ := parseDBID(workspaceA)
			triggers := newIngestTriggers(sqlc.New(pool), ws)
			event := domain.NormalizedEvent{Source: domain.SourceClickup, EventType: domain.EventStatusChanged, EventKey: "status-1", ExternalID: "abc", Metadata: map[string]string{"status_to": "tech spec review", "list_id": "list-a"}}
			if count, err := triggers.start(ctx, event); err != nil || count != 1 {
				t.Fatalf("status did not start example: count=%d error=%v", count, err)
			}
			if count, err := triggers.start(ctx, event); err != nil || count != 0 {
				t.Fatal("duplicate event started second run")
			}
			r, err := s.LatestRunBySubject(ctx, workspaceA, behavior.TaskSubject("abc"))
			if err != nil {
				t.Fatal(err)
			}
			before := "Evidence: use cursor pagination\n" + descblock.StartMarker("oq-answers") + "\noriginal answer\n" + descblock.EndMarker("oq-answers") + "\nHuman footer 🧪"
			remote := &s23HTTP{description: before}
			httpc := &http.Client{Transport: remote}
			all := []tools.Tool{clickuptools.NewTaskTool(httpc, s23Secrets{}), behavior.NewMemorySearchTool(s)}
			all = append(all, clickuptools.NewWriteTools(httpc, s23Secrets{})...)
			reg, err := tools.NewRegistry(all...)
			if err != nil {
				t.Fatal(err)
			}
			audit, err := NewBehaviorToolRunStore(pool)
			if err != nil {
				t.Fatal(err)
			}
			san := agent.NewRedactor(nil)
			telegram := &s23Telegram{}
			engine, err := behavior.NewEngine(behavior.ActionDeps{Store: s, Defs: s, Profiles: s, Policies: s23Policy{}, Tasks: s23TaskReader{clickuptools.NewTaskReader(httpc, s23Secrets{})}, Chats: telegram, Messenger: telegram, Callers: behavior.ActionExecutorFactory(reg, audit, san, tools.DefaultLimits()), Decisions: behavior.MemoryRecorder{Store: s}}, behavior.ActionSettings{OwnerClickUpUserID: "42"})
			if err != nil {
				t.Fatal(err)
			}
			model := &s23Model{output: `{"items":[
		{"key":"oq-1","label":"Pagination","text":"Chọn phân trang?","proposal":"Use cursor pagination","citations":[{"locator":"clickup:task/abc#desc:L1","quote":"Evidence: use cursor pagination"}],"topic":"api","confidence":0.99,"informational":false},
		{"key":"oq-2","label":"Scope","text":"Có mở rộng phạm vi?","proposal":"Không mở rộng","citations":[],"topic":"scope","confidence":0.99,"informational":false},
		{"key":"finding-note","label":"Info","text":"Đã kiểm tra 🧪","proposal":"","citations":[],"topic":"testing","confidence":0.5,"informational":true}]}`}
			worker := behavior.NewRunWorker(behavior.RunWorkerDeps{Runs: s, Defs: s, Profiles: s, Policies: s23Policy{}, Models: func(context.Context, string) (domain.LLMProvider, error) { return model, nil }, Callers: behavior.ExecutorFactory(reg, audit, san, tools.DefaultLimits()), Dispatcher: engine, Notifier: engine}, behavior.RunWorkerSettings{DefaultModel: "fake-model"})
			if worked, err := worker.ProcessNext(ctx); err != nil || !worked {
				t.Fatalf("run worker: %v", err)
			}
			r, err = s.GetRun(ctx, workspaceA, r.ID)
			if err != nil || r.Status != behavior.RunDone || r.VersionID != v.ID || r.Usage.Steps != 1 || model.calls != 1 {
				t.Fatalf("run failed: status=%s code=%s error=%v", r.Status, r.ErrorCode, err)
			}
			items, err := s.ListItems(ctx, workspaceA, r.ID)
			if err != nil || len(items) != 3 {
				t.Fatalf("stored items: %v", err)
			}
			byKey := map[string]behavior.Item{}
			for _, it := range items {
				byKey[it.Key] = it
			}
			if byKey["oq-1"].Class != behavior.ClassAuto || byKey["oq-1"].Status != behavior.ItemDrafted || byKey["oq-2"].Class != behavior.ClassAsk || byKey["oq-2"].Status != behavior.ItemAsked || byKey["finding-note"].Class != behavior.ClassNotify {
				t.Fatal("engine did not classify and draft/ask")
			}
			if _, writes := remote.state(); writes != 0 {
				t.Fatal("dry-run wrote before button press")
			}
			post, digests := false, 0
			for _, m := range telegram.messages {
				if strings.Contains(m.Text, "Đã kiểm tra") {
					digests++
				}
				for _, row := range m.Buttons {
					for _, button := range row {
						if button.Text == "Đăng" {
							post = true
						}
					}
				}
			}
			if !post || digests != 1 {
				t.Fatalf("draft post button=%v notify digests=%d", post, digests)
			}
			approvals, err := s.ListApprovals(ctx, workspaceA, r.ID)
			if err != nil {
				t.Fatal(err)
			}
			var draft, ask behavior.Approval
			for _, a := range approvals {
				switch a.Kind {
				case behavior.ApprovalPostDraft:
					draft = a
				case behavior.ApprovalItemAnswer:
					ask = a
				}
			}
			if draft.ID == 0 || ask.ID == 0 {
				t.Fatal("missing durable draft/ask approval")
			}
			if scenario == "outside_conflict" {
				remote.edit("Concurrent human edit\n" + before)
			}
			if scenario == "broken_markers" {
				remote.edit(strings.Replace(before, descblock.EndMarker("oq-answers"), "", 1))
			}
			// P3-JOURNEY-02 (REQ-12, REQ-14; R4): exact crash boundary after
			// the approved decision committed but before the action handler wrote.
			if scenario == "crash_after_decision_before_write" {
				if _, err := s.Decide(ctx, workspaceA, draft.Nonce, behavior.DecisionApproved, "", "telegram:42", time.Now()); err != nil {
					t.Fatal(err)
				}
			}
			press := behavior.CallbackAction{WorkspaceID: workspaceA, ChatID: 42, Approval: draft, Data: behavior.CallbackData{Kind: behavior.CallbackApprove, Nonce: draft.Nonce, Option: -1}, Actor: "telegram:42"}
			encoded, err := press.Data.Encode()
			if err != nil {
				t.Fatal(err)
			}
			s23DeliverTelegram(t, ctx, s, engine, telegram, map[string]any{"update_id": 100, "callback_query": map[string]any{"id": "post1", "from": map[string]int{"id": 42}, "message": map[string]any{"message_id": draft.TelegramMessageID, "chat": map[string]int{"id": 42}}, "data": encoded}})
			if _, err := engine.HandleCallback(ctx, press); !errors.Is(err, behavior.ErrDecided) {
				t.Fatalf("second post not idempotent: %v", err)
			}
			if scenario == "outside_conflict" || scenario == "broken_markers" {
				it, err := s.GetItem(ctx, workspaceA, draft.ItemID)
				if err != nil || it.Status != behavior.ItemConflict {
					t.Fatalf("target corruption not conflict: status=%s error=%v", it.Status, err)
				}
				if _, writes := remote.state(); writes != 0 {
					t.Fatal("conflicting target written")
				}
				return
			}
			posted, writes := remote.state()
			if scenario == "crash_after_decision_before_write" && writes == 0 {
				t.Fatal("R4: persisted approval lost its action after restart; retried callback treated as stale")
			}
			if writes != 1 || !strings.Contains(posted, "Use cursor pagination") || !strings.HasPrefix(posted, "Evidence: use cursor pagination\n") || !strings.HasSuffix(posted, "Human footer 🧪") {
				t.Fatal("post damaged outside description or missing answer")
			}
			if err := s.SetReplyMessage(ctx, workspaceA, ask.ID, 777); err != nil {
				t.Fatal(err)
			}
			s23DeliverTelegram(t, ctx, s, engine, telegram, map[string]any{"update_id": 101, "message": map[string]any{"message_id": 778, "chat": map[string]int{"id": 42}, "from": map[string]int{"id": 42}, "text": "Giữ nguyên phạm vi", "reply_to_message": map[string]int{"message_id": 777}}})
			after, writes := remote.state()
			if writes != 2 || !strings.Contains(after, "Use cursor pagination") || !strings.Contains(after, "Giữ nguyên phạm vi") {
				t.Fatal("ask answer failed to preserve posted answer")
			}
			if _, err := engine.HandleFreeText(ctx, behavior.FreeTextAnswer{WorkspaceID: workspaceA, ChatID: 42, Approval: ask, Answer: "second answer", Actor: "telegram:42"}); !errors.Is(err, behavior.ErrDecided) {
				t.Fatal("ask replay changed answer")
			}
			trace, err := behavior.BuildTrace(ctx, s, workspaceA, r.CorrelationID, "")
			if err != nil || len(trace.Items) != 3 || trace.Run.Version != 1 || trace.Run.CorrelationID != r.CorrelationID || len(trace.ToolCalls) < 3 {
				t.Fatalf("incomplete correlated trace: %v", err)
			}
			if _, err := behavior.BuildTrace(ctx, s, workspaceB, fmt.Sprint(r.ID), ""); !errors.Is(err, behavior.ErrNotFound) {
				t.Fatal("trace leaked workspace")
			}
			memory, err := s.ListDecisions(ctx, workspaceA, false, 100, 0)
			if err != nil || len(memory) != 2 {
				t.Fatalf("decisions not recorded: count=%d error=%v", len(memory), err)
			}
			for _, d := range memory {
				if d.Scope.Behavior != b.Name || d.Scope.List != "list-a" || d.RunID != r.ID {
					t.Fatal("decision missing run/scope")
				}
			}
			if scenario == "undo_conflict" {
				remote.edit(after + "\nnew human text")
			}
			if _, err := engine.Undo(ctx, behavior.UndoRequest{WorkspaceID: workspaceA, RunID: r.ID, Actor: "telegram:42"}); err != nil {
				t.Fatal(err)
			}
			undone, writes := remote.state()
			if scenario == "undo_conflict" {
				if writes != 2 || undone != after+"\nnew human text" {
					t.Fatal("undo overwrote concurrent edit")
				}
				return
			}
			if writes != 3 || undone != before {
				t.Fatal("undo did not restore exact previous snapshot")
			}
			if _, err := engine.Undo(ctx, behavior.UndoRequest{WorkspaceID: workspaceA, RunID: r.ID, Actor: "telegram:42"}); err != nil {
				t.Fatal(err)
			}
			if _, writes := remote.state(); writes != 3 {
				t.Fatal("undo repeated remote effect")
			}
			// Only the auto answer becomes non-reusable after undo; the explicit ask answer remains evidence.
			memory, err = s.SearchReusableDecisions(ctx, workspaceA, "pagination", 10)
			if err != nil || len(memory) != 0 {
				t.Fatal("undone auto answer remains evidence")
			}
		})
	}
}

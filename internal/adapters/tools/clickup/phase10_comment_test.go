package clickuptools

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/agent/tools"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
)

// Phase 10 Batch A, W1 (REQ-6): clickup_post_comment with threaded replies
// and mentions. Request shapes are the documented ones (U1, U2 stay live checks).

type p10Req struct {
	method, path, auth string
	body               map[string]any
	raw                string
}

func p10Comment(t *testing.T, status int, resp string) (*writeTool, *[]p10Req, *sync.Mutex) {
	t.Helper()
	var mu sync.Mutex
	var reqs []p10Req
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		mu.Lock()
		reqs = append(reqs, p10Req{r.Method, r.URL.Path, r.Header.Get("Authorization"), m, string(b)})
		mu.Unlock()
		w.WriteHeader(status)
		_, _ = w.Write([]byte(resp))
	}))
	t.Cleanup(s.Close)
	for _, tl := range NewWriteTools(s.Client(), &res{val: "pk_token"}) {
		if tl.Spec().Name == "clickup_post_comment" {
			wt := tl.(*writeTool)
			wt.c.baseURL = s.URL
			return wt, &reqs, &mu
		}
	}
	t.Fatal("clickup_post_comment not registered")
	return nil, nil, nil
}

func p10Run(tl tools.Tool, input string) (tools.ToolOutput, error) {
	return tl.Run(context.Background(), tools.Call{Input: json.RawMessage(input), WorkspaceID: "ws", ClickUpTokenRef: "clickup"})
}

// TC-W1b (REQ-6): an input without the new fields sends exactly the pre-Phase-10 request.
func TestP10Comment_OldInputSendsIdenticalRequest(t *testing.T) {
	tl, reqs, mu := p10Comment(t, 200, `{"id":"90010"}`)
	out, err := p10Run(tl, `{"task_id":"86abc","text":"🤖 xong"}`)
	if err != nil {
		t.Fatal(err)
	}
	snap := func() []p10Req {
		mu.Lock()
		defer mu.Unlock()
		return append([]p10Req(nil), (*reqs)...)
	}
	if len(snap()) != 1 {
		t.Fatalf("%d requests", len(snap()))
	}
	r := snap()[0]
	if r.method != "POST" || r.path != "/api/v2/task/86abc/comment" || r.auth != "pk_token" {
		t.Fatalf("request %+v", r)
	}
	if len(r.body) != 2 || r.body["comment_text"] != "🤖 xong" || r.body["notify_all"] != false {
		t.Fatalf("body %s", r.raw)
	}
	if !strings.Contains(out.Text, "comment_id=90010") {
		t.Fatalf("output %q", out.Text)
	}
	// An explicit null parent and an empty mention list are the old shape too.
	if _, err := p10Run(tl, `{"task_id":"86abc","text":"x","parent_comment_id":null,"mention_user_ids":[]}`); err != nil {
		t.Fatal(err)
	}
	last := snap()[1]
	if last.path != "/api/v2/task/86abc/comment" || last.body["comment_text"] != "x" || last.body["comment"] != nil {
		t.Fatalf("null/empty new fields changed the request: %s %s", last.path, last.raw)
	}
}

// TC-W1c (REQ-6): a reply goes to the reply endpoint, still plain text.
func TestP10Comment_ReplyUsesReplyEndpoint(t *testing.T) {
	tl, reqs, mu := p10Comment(t, 200, `{"id":123}`)
	out, err := p10Run(tl, `{"task_id":"86abc","text":"🤖 trả lời","parent_comment_id":"90210"}`)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	r := (*reqs)[0]
	if r.method != "POST" || r.path != "/api/v2/comment/90210/reply" || r.body["comment_text"] != "🤖 trả lời" || r.body["notify_all"] != false {
		t.Fatalf("request %+v", r)
	}
	if !strings.Contains(out.Text, "comment_id=123") {
		t.Fatalf("numeric id not read: %q", out.Text)
	}
}

// TC-W1c (REQ-6): mentions use the structured comment array: text first, then one tag per user, numeric ids.
func TestP10Comment_MentionsUseStructuredComment(t *testing.T) {
	tl, reqs, mu := p10Comment(t, 200, `{"id":"1"}`)
	if _, err := p10Run(tl, `{"task_id":"86abc","text":"🤖 anh xem giúp","mention_user_ids":["111","222"]}`); err != nil {
		t.Fatal(err)
	}
	if _, err := p10Run(tl, `{"task_id":"86abc","text":"🤖 reply","parent_comment_id":"5","mention_user_ids":["333"]}`); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	a, b := (*reqs)[0], (*reqs)[1]
	if a.path != "/api/v2/task/86abc/comment" || b.path != "/api/v2/comment/5/reply" {
		t.Fatalf("paths %s %s", a.path, b.path)
	}
	if _, has := a.body["comment_text"]; has {
		t.Fatalf("both comment_text and comment sent: %s", a.raw)
	}
	parts, _ := a.body["comment"].([]any)
	if len(parts) != 5 {
		t.Fatalf("parts %s", a.raw)
	}
	first, _ := parts[0].(map[string]any)
	if first["text"] != "🤖 anh xem giúp" {
		t.Fatalf("text part %v", first)
	}
	var tagged []float64
	for _, p := range parts[1:] {
		m := p.(map[string]any)
		if m["type"] == "tag" {
			tagged = append(tagged, m["user"].(map[string]any)["id"].(float64))
		}
	}
	if len(tagged) != 2 || tagged[0] != 111 || tagged[1] != 222 {
		t.Fatalf("tags %v in %s", tagged, a.raw)
	}
	if a.body["notify_all"] != false || b.body["notify_all"] != false {
		t.Fatal("notify_all must stay false")
	}
}

// TC-W1a, TC-W1d (REQ-6): strict validation; a rejected input sends no request.
func TestP10Comment_InputValidation(t *testing.T) {
	tl, reqs, mu := p10Comment(t, 200, `{"id":"1"}`)
	long := strings.Repeat("a", 8001)
	bad := map[string]string{
		"six mentions":        `{"task_id":"t1","text":"x","mention_user_ids":["1","2","3","4","5","6"]}`,
		"duplicate mention":   `{"task_id":"t1","text":"x","mention_user_ids":["7","7"]}`,
		"non numeric mention": `{"task_id":"t1","text":"x","mention_user_ids":["abc"]}`,
		"mention list inject": `{"task_id":"t1","text":"x","mention_user_ids":["1,2"]}`,
		"mention path inject": `{"task_id":"t1","text":"x","mention_user_ids":["1/../2"]}`,
		"zero mention":        `{"task_id":"t1","text":"x","mention_user_ids":["0"]}`,
		"number mention":      `{"task_id":"t1","text":"x","mention_user_ids":[5]}`,
		"mention not a list":  `{"task_id":"t1","text":"x","mention_user_ids":"5"}`,
		"parent not numeric":  `{"task_id":"t1","text":"x","parent_comment_id":"12a"}`,
		"parent path inject":  `{"task_id":"t1","text":"x","parent_comment_id":"1/reply/../../x"}`,
		"parent empty":        `{"task_id":"t1","text":"x","parent_comment_id":""}`,
		"parent number":       `{"task_id":"t1","text":"x","parent_comment_id":90210}`,
		"unknown field":       `{"task_id":"t1","text":"x","notify_all":true}`,
		"unknown field 2":     `{"task_id":"t1","text":"x","assignee":5}`,
		"empty text":          `{"task_id":"t1","text":""}`,
		"blank text":          `{"task_id":"t1","text":"  \n"}`,
		"too long":            `{"task_id":"t1","text":"` + long + `"}`,
		"bad task":            `{"task_id":"../x","text":"x"}`,
		"missing task":        `{"text":"x"}`,
		"not json":            `nope`,
	}
	for name, in := range bad {
		t.Run(name, func(t *testing.T) {
			_, err := p10Run(tl, in)
			var te *tools.ToolError
			if err == nil || !asToolError(err, &te) || te.Code != "invalid_input" || te.Retryable || te.Ambiguous {
				t.Fatalf("err %v", err)
			}
		})
	}
	mu.Lock()
	defer mu.Unlock()
	if len(*reqs) != 0 {
		t.Fatalf("%d requests sent for invalid inputs", len(*reqs))
	}
	// Boundaries that must pass: 5 mentions, 8000 runes.
	ok := []string{
		`{"task_id":"t1","text":"x","mention_user_ids":["1","2","3","4","5"]}`,
		`{"task_id":"t1","text":"` + strings.Repeat("ệ", 8000) + `"}`,
		`{"task_id":"t1","text":"x","parent_comment_id":"00123"}`,
	}
	mu.Unlock()
	for _, in := range ok {
		if _, err := p10Run(tl, in); err != nil {
			t.Fatalf("%.40s: %v", in, err)
		}
	}
	mu.Lock()
}

func asToolError(err error, target **tools.ToolError) bool {
	te, ok := err.(*tools.ToolError)
	if ok {
		*target = te
	}
	return ok
}

// TC-W1a (REQ-6): the declared schema matches the code: tier write, no extra properties, 5 mentions, numeric ids.
func TestP10Comment_SpecSchemaAndTier(t *testing.T) {
	tl, _, _ := p10Comment(t, 200, `{}`)
	spec := tl.Spec()
	if spec.Tier != tenant.TierWrite || spec.Name != "clickup_post_comment" {
		t.Fatalf("spec %+v", spec)
	}
	var s struct {
		AdditionalProperties bool `json:"additionalProperties"`
		Properties           map[string]struct {
			Type     string `json:"type"`
			MaxItems int    `json:"maxItems"`
			Pattern  string `json:"pattern"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(spec.InputSchema, &s); err != nil {
		t.Fatal(err)
	}
	if s.AdditionalProperties {
		t.Fatal("additionalProperties must be false")
	}
	if s.Properties["mention_user_ids"].MaxItems != 5 || s.Properties["mention_user_ids"].Type != "array" {
		t.Fatalf("mention_user_ids %+v", s.Properties["mention_user_ids"])
	}
	if s.Properties["parent_comment_id"].Pattern == "" {
		t.Fatal("parent_comment_id has no pattern")
	}
	if len(s.Required) != 2 {
		t.Fatalf("required %v", s.Required)
	}
}

// TC-W1c (REQ-6, human): ClickUp failures give safe codes; a 5xx after the write is ambiguous; no text leaks.
func TestP10Comment_FailureCodesAreSafe(t *testing.T) {
	for _, status := range []int{401, 404, 429, 500} {
		tl, _, _ := p10Comment(t, status, `{"err":"secret body pk_leak"}`)
		_, err := p10Run(tl, `{"task_id":"t1","text":"bí mật riêng","parent_comment_id":"9"}`)
		te, ok := err.(*tools.ToolError)
		if !ok {
			t.Fatalf("status %d: err %v", status, err)
		}
		if strings.Contains(te.Error(), "bí mật") || strings.Contains(te.Error(), "pk_leak") || strings.Contains(te.Error(), "pk_token") {
			t.Fatalf("status %d leaks: %q", status, te.Error())
		}
		if status == 500 && !te.Ambiguous {
			t.Fatal("5xx on a write must be ambiguous")
		}
		if status == 404 && te.Ambiguous {
			t.Fatal("404 must not be ambiguous")
		}
	}
}

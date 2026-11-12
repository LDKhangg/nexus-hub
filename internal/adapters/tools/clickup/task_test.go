package clickuptools

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/agent/tools"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
)

type res struct {
	val     string
	ws, ref string
}

func (r *res) Resolve(_ context.Context, ws, ref string) (string, error) {
	r.ws, r.ref = ws, ref
	if r.val == "" {
		return "", errors.New("missing")
	}
	return r.val, nil
}

func newTool(t *testing.T, h http.HandlerFunc, r *res) (*taskTool, *httptest.Server) {
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	tl := NewTaskTool(s.Client(), r).(*taskTool)
	tl.baseURL = s.URL
	return tl, s
}

func call(tl tools.Tool, input string) (tools.ToolOutput, error) {
	return tl.Run(context.Background(), tools.Call{Input: json.RawMessage(input), WorkspaceID: "ws", ClickUpTokenRef: "clickup"})
}

func TestClickUpGetTask_FieldsOnly(t *testing.T) {
	var auth, path string
	tl, _ := newTool(t, func(w http.ResponseWriter, r *http.Request) {
		auth, path = r.Header.Get("Authorization"), r.URL.Path
		w.Write([]byte(`{"id":"abc123","name":"Login broken","status":{"status":"in progress"},"priority":{"priority":"high"},
			"assignees":[{"username":"alice","email":"alice@example.com"}],"due_date":"1767225600000","url":"https://app.clickup.com/t/abc123",
			"tags":[{"name":"backend"}],"description":"` + strings.Repeat("d", 4000) + `","custom_fields":[{"name":"secret","value":"hidden"}],"creator":{"email":"x@y.z"}}`))
	}, &res{val: "pk_token"})
	if tl.Spec().Tier != tenant.TierRead {
		t.Fatal("tier")
	}
	out, err := call(tl, `{"task_id":"abc123"}`)
	if err != nil {
		t.Fatal(err)
	}
	if auth != "pk_token" || path != "/api/v2/task/abc123" {
		t.Fatalf("auth %q path %q", auth, path)
	}
	for _, want := range []string{"Login broken", "in progress", "high", "alice", "backend", "https://app.clickup.com/t/abc123", "2026-01-01"} {
		if !strings.Contains(out.Text, want) {
			t.Fatalf("missing %q:\n%s", want, out.Text)
		}
	}
	for _, leak := range []string{"alice@example.com", "hidden", "x@y.z"} {
		if strings.Contains(out.Text, leak) {
			t.Fatalf("leaked %q", leak)
		}
	}
	if len([]rune(out.Text)) > 2200 {
		t.Fatalf("too long %d", len(out.Text))
	}
}

func TestClickUpGetTask_Errors(t *testing.T) {
	for status, want := range map[int]string{401: "unauthorized", 404: "not_found", 429: "rate_limited", 500: "server_error"} {
		tl, _ := newTool(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			w.Write([]byte(`{"err":"pk_token leaked?"}`))
		}, &res{val: "pk_token"})
		_, err := call(tl, `{"task_id":"abc"}`)
		te, ok := err.(*tools.ToolError)
		if !ok || te.Code != want || te.Retryable != (status >= 429) {
			t.Errorf("%d: %v", status, err)
		}
	}
	hit := false
	tl, _ := newTool(t, func(http.ResponseWriter, *http.Request) { hit = true }, &res{})
	if _, err := call(tl, `{"task_id":"abc"}`); err == nil || err.(*tools.ToolError).Code != "missing_credentials" || hit {
		t.Fatalf("missing token: %v", err)
	}
	for _, in := range []string{`{}`, `{"task_id":"../x"}`, `{"task_id":"a","x":1}`, `{"task_id":"` + strings.Repeat("a", 65) + `"}`} {
		if _, err := call(tl, in); err == nil || err.(*tools.ToolError).Code != "invalid_input" {
			t.Errorf("%s: %v", in, err)
		}
	}
	if _, err := tl.Run(context.Background(), tools.Call{Input: json.RawMessage(`{"task_id":"a"}`), WorkspaceID: "ws"}); err == nil || err.(*tools.ToolError).Code != "missing_credentials" {
		t.Fatalf("no ref: %v", err)
	}
}

func TestClickUpGetTask_NoRedirect(t *testing.T) {
	followed := false
	var srv *httptest.Server
	tl, srv := newTool(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			followed = true
			return
		}
		http.Redirect(w, r, srv.URL+"/elsewhere", http.StatusFound)
	}, &res{val: "pk_token"})
	if _, err := call(tl, `{"task_id":"abc"}`); err == nil || followed {
		t.Fatalf("redirect followed=%v err=%v", followed, err)
	}
}

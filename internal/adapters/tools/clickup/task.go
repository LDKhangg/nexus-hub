// Package clickuptools exposes a read-only view of ClickUp tasks to the agent.
package clickuptools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/agent/tools"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
)

// SecretResolver resolves credential references (secrets.Resolver).
type SecretResolver interface {
	Resolve(ctx context.Context, workspaceID, ref string) (string, error)
}

const (
	apiBase          = "https://api.clickup.com"
	maxResponseBytes = 2 << 20
	maxDescRunes     = 1500
)

var taskIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

type taskTool struct {
	http     *http.Client
	resolver SecretResolver
	baseURL  string
}

// NewTaskTool returns clickup_get_task, authenticated with the agent's
// clickup_token_ref. Redirects are never followed.
func NewTaskTool(httpc *http.Client, resolver SecretResolver) tools.Tool {
	if httpc == nil {
		httpc = &http.Client{Timeout: 30 * time.Second}
	}
	cp := *httpc
	cp.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &taskTool{http: &cp, resolver: resolver, baseURL: apiBase}
}

func (t *taskTool) Spec() tools.ToolSpec {
	return tools.ToolSpec{
		Name:        "clickup_get_task",
		Description: "Fetch a ClickUp task: name, status, priority, assignees, due date, tags, URL and a clipped description.",
		InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"task_id":{"type":"string","pattern":"^[A-Za-z0-9_-]{1,64}$"}},"required":["task_id"]}`),
		Tier:        tenant.TierRead,
	}
}

func (t *taskTool) UsesNetwork() bool { return true }

func toolErr(code string, retryable bool) error {
	return &tools.ToolError{Code: code, Retryable: retryable}
}

func (t *taskTool) Run(ctx context.Context, c tools.Call) (tools.ToolOutput, error) {
	var in struct {
		TaskID string `json:"task_id"`
	}
	dec := json.NewDecoder(bytes.NewReader(c.Input))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil || !taskIDRe.MatchString(in.TaskID) {
		return tools.ToolOutput{}, toolErr("invalid_input", false)
	}
	if t.resolver == nil || c.ClickUpTokenRef == "" {
		return tools.ToolOutput{}, toolErr("missing_credentials", false)
	}
	token, err := t.resolver.Resolve(ctx, c.WorkspaceID, c.ClickUpTokenRef)
	if err != nil || token == "" {
		return tools.ToolOutput{}, toolErr("missing_credentials", false)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.baseURL+"/api/v2/task/"+in.TaskID, nil)
	if err != nil {
		return tools.ToolOutput{}, toolErr("invalid_input", false)
	}
	req.Header.Set("Authorization", token)
	req.Header.Set("Accept", "application/json")
	resp, err := t.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return tools.ToolOutput{}, ctx.Err()
		}
		return tools.ToolOutput{}, toolErr("network_error", true)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if code, retry, bad := statusCode(resp.StatusCode); bad {
		return tools.ToolOutput{}, toolErr(code, retry)
	}
	var task struct {
		ID, Name, URL, Description string
		DueDate                    string `json:"due_date"`
		Status                     struct{ Status string }
		Priority                   *struct{ Priority string }
		Assignees                  []struct{ Username string }
		Tags                       []struct{ Name string }
	}
	if err := json.Unmarshal(data, &task); err != nil {
		return tools.ToolOutput{}, toolErr("invalid_response", false)
	}
	names := func(n int, get func(int) string) string {
		out := make([]string, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, get(i))
		}
		return strings.Join(out, ", ")
	}
	priority := "none"
	if task.Priority != nil && task.Priority.Priority != "" {
		priority = task.Priority.Priority
	}
	due := "none"
	if ms, err := strconv.ParseInt(task.DueDate, 10, 64); err == nil && ms > 0 {
		due = time.UnixMilli(ms).UTC().Format("2006-01-02")
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s (%s)\nstatus: %s | priority: %s | due: %s\nassignees: %s\ntags: %s\n%s\n",
		task.Name, task.ID, task.Status.Status, priority, due,
		names(len(task.Assignees), func(i int) string { return task.Assignees[i].Username }),
		names(len(task.Tags), func(i int) string { return task.Tags[i].Name }), task.URL)
	if d := strings.TrimSpace(task.Description); d != "" {
		r := []rune(d)
		if len(r) > maxDescRunes {
			d = string(r[:maxDescRunes]) + "…"
		}
		sb.WriteString("description:\n" + d)
	}
	return tools.ToolOutput{Text: sb.String(), Summary: "task " + task.ID + " " + task.Status.Status}, nil
}

func statusCode(s int) (string, bool, bool) {
	switch {
	case s >= 200 && s < 300:
		return "", false, false
	case s >= 300 && s < 400:
		return "redirect_denied", false, true
	case s == http.StatusUnauthorized || s == http.StatusForbidden:
		return "unauthorized", false, true
	case s == http.StatusNotFound:
		return "not_found", false, true
	case s == http.StatusTooManyRequests:
		return "rate_limited", true, true
	case s >= 500:
		return "server_error", true, true
	}
	return "http_" + strconv.Itoa(s), false, true
}

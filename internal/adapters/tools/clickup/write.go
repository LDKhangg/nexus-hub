package clickuptools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/LDKhangg/nexus-hub/internal/adapters/netguard"
	"github.com/LDKhangg/nexus-hub/internal/agent/tools"
	"github.com/LDKhangg/nexus-hub/internal/descblock"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
)

// This file holds the ClickUp write tools of the behavior action library
// and the exact task reader the engine uses for conflict checks. Every
// write tool is tier write, runs only through the executor (policy, audit,
// idempotency), authenticates with the call's clickup_token_ref, follows
// no redirect, reads at most 2 MiB of a response and never logs content.
//
// Descriptions are read as markdown (GET ?include_markdown_description=true,
// field markdown_description) and written as markdown (PUT field
// markdown_content), so formatting outside a managed block survives.

const (
	maxCommentRunes = 8000
	maxBlockBytes   = 14 << 10
	maxStatusRunes  = 100
)

var (
	userIDRe = regexp.MustCompile(`^[0-9]{1,20}$`)
	blockRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 _.-]{0,62}$`)
	sha256Re = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// TaskState is an exact read of a task for conflict checks.
type TaskState struct {
	ID          string
	Status      string
	AssigneeIDs []string
	// Description is ClickUp's plain-text description; Markdown is the
	// markdown description the engine writes blocks into.
	Description string
	Markdown    string
}

// client does authenticated ClickUp calls.
type client struct {
	http     *http.Client
	resolver SecretResolver
	baseURL  string
}

func newClient(httpc *http.Client, resolver SecretResolver) client {
	if httpc == nil {
		httpc = &http.Client{Timeout: 30 * time.Second}
	}
	cp := *httpc
	cp.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return client{http: &cp, resolver: resolver, baseURL: apiBase}
}

func (c client) token(ctx context.Context, workspaceID, ref string) (string, error) {
	if c.resolver == nil || ref == "" {
		return "", toolErr("missing_credentials", false)
	}
	token, err := c.resolver.Resolve(ctx, workspaceID, ref)
	if err != nil || token == "" {
		return "", toolErr("missing_credentials", false)
	}
	return token, nil
}

// do sends one request. write marks a request with an effect: a failure
// after it may have reached ClickUp is ambiguous.
func (c client) do(ctx context.Context, token, method, path string, body any, write bool) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, toolErr("invalid_input", false)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rd)
	if err != nil {
		return nil, toolErr("invalid_input", false)
	}
	req.Header.Set("Authorization", token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var de *netguard.DeniedError
		if errors.As(err, &de) {
			return nil, &tools.ToolError{Code: de.Code, Denied: true}
		}
		return nil, &tools.ToolError{Code: "network_error", Retryable: true, Ambiguous: write}
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if code, retry, bad := statusCode(resp.StatusCode); bad {
		return nil, &tools.ToolError{Code: code, Retryable: retry, Ambiguous: write && resp.StatusCode >= 500}
	}
	return data, nil
}

func (c client) readTask(ctx context.Context, token, taskID string) (TaskState, error) {
	data, err := c.do(ctx, token, http.MethodGet, "/api/v2/task/"+taskID+"?include_markdown_description=true", nil, false)
	if err != nil {
		return TaskState{}, err
	}
	var task struct {
		ID                  string
		Description         string
		MarkdownDescription *string `json:"markdown_description"`
		Status              struct{ Status string }
		Assignees           []struct{ ID any }
	}
	if err := json.Unmarshal(data, &task); err != nil || task.ID == "" {
		return TaskState{}, toolErr("invalid_response", false)
	}
	st := TaskState{ID: task.ID, Status: task.Status.Status, Description: task.Description}
	if task.MarkdownDescription != nil {
		st.Markdown = *task.MarkdownDescription
	} else {
		st.Markdown = task.Description
	}
	for _, a := range task.Assignees {
		if id := idString(a.ID); id != "" {
			st.AssigneeIDs = append(st.AssigneeIDs, id)
		}
	}
	return st, nil
}

// TaskReader reads tasks exactly (markdown description, status, assignee
// IDs) for the engine's conflict checks. It is not a tool: it never writes.
type TaskReader struct{ c client }

func NewTaskReader(httpc *http.Client, resolver SecretResolver) *TaskReader {
	return &TaskReader{c: newClient(httpc, resolver)}
}

// ReadTask reads one task with the workspace credential tokenRef. Errors
// are *tools.ToolError with safe codes.
func (r *TaskReader) ReadTask(ctx context.Context, workspaceID, tokenRef, taskID string) (TaskState, error) {
	if !taskIDRe.MatchString(taskID) {
		return TaskState{}, toolErr("invalid_input", false)
	}
	token, err := r.c.token(ctx, workspaceID, tokenRef)
	if err != nil {
		return TaskState{}, err
	}
	return r.c.readTask(ctx, token, taskID)
}

// decodeStrict decodes a tool input with no unknown field.
func decodeStrict(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return toolErr("invalid_input", false)
	}
	return nil
}

// writeTool is the shared shape of the ClickUp write tools.
type writeTool struct {
	c    client
	spec tools.ToolSpec
	run  func(ctx context.Context, c client, token string, call tools.Call) (tools.ToolOutput, error)
	// check validates the input before any credential or network use.
	check func(raw json.RawMessage) error
}

func (t *writeTool) Spec() tools.ToolSpec { return t.spec }
func (t *writeTool) UsesNetwork() bool    { return true }

func (t *writeTool) Run(ctx context.Context, call tools.Call) (tools.ToolOutput, error) {
	if err := t.check(call.Input); err != nil {
		return tools.ToolOutput{}, err
	}
	token, err := t.c.token(ctx, call.WorkspaceID, call.ClickUpTokenRef)
	if err != nil {
		return tools.ToolOutput{}, err
	}
	return t.run(ctx, t.c, token, call)
}

// NewWriteTools returns the ClickUp write tools of the action library:
// clickup_set_description_block, clickup_post_comment, clickup_set_status
// and clickup_remove_assignee.
func NewWriteTools(httpc *http.Client, resolver SecretResolver) []tools.Tool {
	c := newClient(httpc, resolver)
	return []tools.Tool{
		&writeTool{c: c, spec: blockSpec, check: checkBlockInput, run: runBlock},
		&writeTool{c: c, spec: commentSpec, check: checkCommentInput, run: runComment},
		&writeTool{c: c, spec: statusSpec, check: checkStatusInput, run: runStatus},
		&writeTool{c: c, spec: unassignSpec, check: checkUnassignInput, run: runUnassign},
	}
}

// --- clickup_set_description_block ---

var blockSpec = tools.ToolSpec{
	Name: "clickup_set_description_block",
	Description: "Replace the body of one managed block (between its ⟦nexus:<block>:start⟧ and ⟦nexus:<block>:end⟧ marker lines) " +
		"in a ClickUp task description, appending the block when absent, or remove the block. Text outside the block never changes.",
	InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{` +
		`"task_id":{"type":"string","pattern":"^[A-Za-z0-9_-]{1,64}$"},` +
		`"block":{"type":"string","pattern":"^[A-Za-z0-9][A-Za-z0-9 _.-]{0,62}$"},` +
		`"body":{"type":"string","maxLength":14336},` +
		`"remove":{"type":"boolean"},` +
		`"if_outside_sha256":{"type":"string","pattern":"^[0-9a-f]{64}$"},` +
		`"if_current_sha256":{"type":"string","pattern":"^[0-9a-f]{64}$"}},` +
		`"required":["task_id","block"]}`),
	Tier: tenant.TierWrite,
}

type blockInput struct {
	TaskID          string `json:"task_id"`
	Block           string `json:"block"`
	Body            string `json:"body"`
	Remove          bool   `json:"remove"`
	IfOutsideSHA256 string `json:"if_outside_sha256"`
	IfCurrentSHA256 string `json:"if_current_sha256"`
}

func parseBlockInput(raw json.RawMessage) (blockInput, error) {
	var in blockInput
	if err := decodeStrict(raw, &in); err != nil {
		return in, err
	}
	switch {
	case !taskIDRe.MatchString(in.TaskID), !blockRe.MatchString(in.Block),
		len(in.Body) > maxBlockBytes, !utf8.ValidString(in.Body),
		in.Remove && in.Body != "",
		in.IfOutsideSHA256 != "" && !sha256Re.MatchString(in.IfOutsideSHA256),
		in.IfCurrentSHA256 != "" && !sha256Re.MatchString(in.IfCurrentSHA256):
		return in, toolErr("invalid_input", false)
	}
	if descblock.CheckBody(in.Body) != nil {
		return in, toolErr("block_body_invalid", false)
	}
	return in, nil
}

func checkBlockInput(raw json.RawMessage) error {
	_, err := parseBlockInput(raw)
	return err
}

// runBlock re-reads the task, checks the guards and the markers, and writes
// the new markdown description. The output names the hashes of the
// previous description, the written one as ClickUp returns it, and its
// text outside every block:
// "desc_sha256=<h> outside_sha256=<h> prev_sha256=<h>".
func runBlock(ctx context.Context, c client, token string, call tools.Call) (tools.ToolOutput, error) {
	in, err := parseBlockInput(call.Input)
	if err != nil {
		return tools.ToolOutput{}, err
	}
	cur, err := c.readTask(ctx, token, in.TaskID)
	if err != nil {
		return tools.ToolOutput{}, err
	}
	prev := descblock.Hash(cur.Markdown)
	if in.IfCurrentSHA256 != "" && in.IfCurrentSHA256 != prev {
		return tools.ToolOutput{}, toolErr("conflict", false)
	}
	outside, err := descblock.OutsideHash(cur.Markdown)
	if err != nil {
		return tools.ToolOutput{}, toolErr("block_markers_invalid", false)
	}
	if in.IfOutsideSHA256 != "" && in.IfOutsideSHA256 != outside {
		return tools.ToolOutput{}, toolErr("conflict", false)
	}
	var next string
	if in.Remove {
		next, err = descblock.Remove(cur.Markdown, in.Block)
	} else {
		next, err = descblock.Replace(cur.Markdown, in.Block, in.Body)
	}
	if err != nil {
		return tools.ToolOutput{}, toolErr("block_markers_invalid", false)
	}
	written := next
	if next != cur.Markdown {
		content := next
		if strings.TrimSpace(content) == "" {
			content = " " // ClickUp clears a description only with a space
		}
		data, err := c.do(ctx, token, http.MethodPut, "/api/v2/task/"+in.TaskID+"?include_markdown_description=true",
			map[string]any{"markdown_content": content}, true)
		if err != nil {
			return tools.ToolOutput{}, err
		}
		var resp struct {
			MarkdownDescription *string `json:"markdown_description"`
		}
		if json.Unmarshal(data, &resp) == nil && resp.MarkdownDescription != nil {
			written = *resp.MarkdownDescription
		} else if after, rerr := c.readTask(ctx, token, in.TaskID); rerr == nil {
			written = after.Markdown
		}
	}
	wo, err := descblock.OutsideHash(written)
	if err != nil {
		wo = ""
	}
	text := "desc_sha256=" + descblock.Hash(written) + " outside_sha256=" + wo + " prev_sha256=" + prev
	return tools.ToolOutput{Text: text, Summary: text}, nil
}

// --- clickup_post_comment ---

var commentSpec = tools.ToolSpec{
	Name:        "clickup_post_comment",
	Description: "Post a plain-text comment on a ClickUp task without notifying everyone.",
	InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{` +
		`"task_id":{"type":"string","pattern":"^[A-Za-z0-9_-]{1,64}$"},"text":{"type":"string","minLength":1,"maxLength":8000}},` +
		`"required":["task_id","text"]}`),
	Tier: tenant.TierWrite,
}

type commentInput struct {
	TaskID string `json:"task_id"`
	Text   string `json:"text"`
}

func parseCommentInput(raw json.RawMessage) (commentInput, error) {
	var in commentInput
	if err := decodeStrict(raw, &in); err != nil {
		return in, err
	}
	if !taskIDRe.MatchString(in.TaskID) || strings.TrimSpace(in.Text) == "" || !utf8.ValidString(in.Text) ||
		utf8.RuneCountInString(in.Text) > maxCommentRunes {
		return in, toolErr("invalid_input", false)
	}
	return in, nil
}

func checkCommentInput(raw json.RawMessage) error {
	_, err := parseCommentInput(raw)
	return err
}

func runComment(ctx context.Context, c client, token string, call tools.Call) (tools.ToolOutput, error) {
	in, err := parseCommentInput(call.Input)
	if err != nil {
		return tools.ToolOutput{}, err
	}
	data, err := c.do(ctx, token, http.MethodPost, "/api/v2/task/"+in.TaskID+"/comment",
		map[string]any{"comment_text": in.Text, "notify_all": false}, true)
	if err != nil {
		return tools.ToolOutput{}, err
	}
	var resp struct{ ID any }
	_ = json.Unmarshal(data, &resp)
	id := idString(resp.ID)
	text := "comment_id=" + id
	return tools.ToolOutput{Text: text, Summary: "comment posted on task " + in.TaskID + " " + text}, nil
}

// --- clickup_set_status ---

var statusSpec = tools.ToolSpec{
	Name:        "clickup_set_status",
	Description: "Set a ClickUp task's status; with if_status, only when the current status is still that one.",
	InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{` +
		`"task_id":{"type":"string","pattern":"^[A-Za-z0-9_-]{1,64}$"},"status":{"type":"string","minLength":1,"maxLength":100},` +
		`"if_status":{"type":"string","maxLength":100}},"required":["task_id","status"]}`),
	Tier: tenant.TierWrite,
}

type statusInput struct {
	TaskID   string  `json:"task_id"`
	Status   string  `json:"status"`
	IfStatus *string `json:"if_status"`
}

func validStatus(s string) bool {
	return strings.TrimSpace(s) != "" && utf8.ValidString(s) && utf8.RuneCountInString(s) <= maxStatusRunes &&
		!strings.ContainsAny(s, "\n\r\t")
}

func parseStatusInput(raw json.RawMessage) (statusInput, error) {
	var in statusInput
	if err := decodeStrict(raw, &in); err != nil {
		return in, err
	}
	if !taskIDRe.MatchString(in.TaskID) || !validStatus(in.Status) || (in.IfStatus != nil && !validStatus(*in.IfStatus)) {
		return in, toolErr("invalid_input", false)
	}
	return in, nil
}

func checkStatusInput(raw json.RawMessage) error {
	_, err := parseStatusInput(raw)
	return err
}

func runStatus(ctx context.Context, c client, token string, call tools.Call) (tools.ToolOutput, error) {
	in, err := parseStatusInput(call.Input)
	if err != nil {
		return tools.ToolOutput{}, err
	}
	if in.IfStatus != nil {
		cur, err := c.readTask(ctx, token, in.TaskID)
		if err != nil {
			return tools.ToolOutput{}, err
		}
		if !strings.EqualFold(cur.Status, *in.IfStatus) {
			return tools.ToolOutput{}, toolErr("conflict", false)
		}
	}
	if _, err := c.do(ctx, token, http.MethodPut, "/api/v2/task/"+in.TaskID, map[string]any{"status": in.Status}, true); err != nil {
		return tools.ToolOutput{}, err
	}
	return tools.ToolOutput{Text: "status set", Summary: "status set on task " + in.TaskID}, nil
}

// --- clickup_remove_assignee ---

var unassignSpec = tools.ToolSpec{
	Name:        "clickup_remove_assignee",
	Description: "Remove one assignee (a ClickUp user ID) from a ClickUp task.",
	InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{` +
		`"task_id":{"type":"string","pattern":"^[A-Za-z0-9_-]{1,64}$"},"user_id":{"type":"string","pattern":"^[0-9]{1,20}$"}},` +
		`"required":["task_id","user_id"]}`),
	Tier: tenant.TierWrite,
}

type unassignInput struct {
	TaskID string `json:"task_id"`
	UserID string `json:"user_id"`
}

func parseUnassignInput(raw json.RawMessage) (unassignInput, int64, error) {
	var in unassignInput
	if err := decodeStrict(raw, &in); err != nil {
		return in, 0, err
	}
	if !taskIDRe.MatchString(in.TaskID) || !userIDRe.MatchString(in.UserID) {
		return in, 0, toolErr("invalid_input", false)
	}
	id, err := strconv.ParseInt(in.UserID, 10, 64)
	if err != nil || id <= 0 {
		return in, 0, toolErr("invalid_input", false)
	}
	return in, id, nil
}

func checkUnassignInput(raw json.RawMessage) error {
	_, _, err := parseUnassignInput(raw)
	return err
}

func runUnassign(ctx context.Context, c client, token string, call tools.Call) (tools.ToolOutput, error) {
	in, id, err := parseUnassignInput(call.Input)
	if err != nil {
		return tools.ToolOutput{}, err
	}
	body := map[string]any{"assignees": map[string]any{"add": []int64{}, "rem": []int64{id}}}
	if _, err := c.do(ctx, token, http.MethodPut, "/api/v2/task/"+in.TaskID, body, true); err != nil {
		return tools.ToolOutput{}, err
	}
	return tools.ToolOutput{Text: "assignee removed", Summary: "assignee removed from task " + in.TaskID}, nil
}

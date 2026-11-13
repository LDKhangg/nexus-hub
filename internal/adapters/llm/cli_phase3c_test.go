package llm

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/domain"
)

func TestClaudeCLIPhase3C_ArgumentsAndOutputMapping(t *testing.T) {
	// TC-47, TC-48, TC-51 (REQ-C2, REQ-C3, REQ-C5).
	args := claudeArgs("claude-test", "fixed system", json.RawMessage(`{"type":"object"}`))
	got := strings.Join(args, "\x00")
	for _, want := range []string{"-p", "--output-format\x00json", "--no-session-persistence", "--strict-mcp-config", "--setting-sources\x00", "--tools\x00", "--model\x00claude-test", "--json-schema"} {
		if !strings.Contains(got, want) { t.Errorf("missing argument sequence %q in %q", want, got) }
	}
	if strings.Contains(got, "private user prompt") { t.Fatal("prompt appeared in argv") }

	plain, err := parseClaudeOutput([]byte(`{"type":"result","subtype":"success","result":"answer","usage":{"input_tokens":7,"output_tokens":3,"cache_read_input_tokens":2,"cache_creation_input_tokens":1}}`))
	if err != nil || plain.Result != "answer" || plain.usage() != (domain.Usage{Input: 7, Output: 3, CacheRead: 2, CacheWrite: 1}) { t.Fatalf("plain mapping: %#v %v", plain, err) }
	structured, err := parseClaudeOutput([]byte(`[{"type":"noise"},{"type":"result","structured_output":{"ok":true}}]`))
	if err != nil || string(structured.StructuredOutput) != `{"ok":true}` { t.Fatalf("structured mapping: %#v %v", structured, err) }
	for _, bad := range [][]byte{nil, []byte(`private malformed answer`), []byte(`[]`)} {
		if _, err := parseClaudeOutput(bad); err == nil || strings.Contains(err.Error(), string(bad)) { t.Fatalf("unsafe parse error for %q: %v", bad, err) }
	}
}

func TestCLIFailurePhase3C_UsesOnlySafeCodes(t *testing.T) {
	// TC-47, TC-48, TC-53, TC-59 (REQ-C3, REQ-C6, REQ-C10, REQ-C11).
	cases := []struct{ text, code string }{
		{"please login; token=private", domain.LLMCodeUnconfigured},
		{"rate_limit private payload", domain.LLMCodeUnavailable},
		{"unknown private provider body", domain.LLMCodeError},
	}
	for _, tc := range cases {
		err := ClassifyCLIFailure(0, tc.text)
		if err.Code != tc.code || strings.Contains(err.Error(), "private") { t.Errorf("%q => %#v", tc.text, err) }
	}
}

func TestOpencodeCLIPhase3C_DenyConfigParsingAndCleanup(t *testing.T) {
	// TC-56..TC-59 (REQ-C9, REQ-C10).
	args := strings.Join(opencodeArgs("openai/gpt-test"), " ")
	if args != "run --standalone --format json --model openai/gpt-test --agent nexus-hub --title nexus-hub" { t.Fatalf("args: %s", args) }
	cfg, err := opencodeConfig("trusted system")
	if err != nil { t.Fatal(err) }
	var decoded ocConfig
	if err := json.Unmarshal(cfg, &decoded); err != nil { t.Fatal(err) }
	agent := decoded.Agents[opencodeAgentName]
	if !strings.Contains(agent.System, "no tools") || len(agent.Permissions) < 2 || len(decoded.Policies) < 4 { t.Fatalf("unsafe config: %s", cfg) }
	for _, rules := range [][]ocRule{agent.Permissions, decoded.Permissions, decoded.Policies} {
		for _, rule := range rules { if rule.Effect != "deny" { t.Fatalf("non-deny rule: %#v", rule) } }
	}

	ndjson := strings.Join([]string{
		`{"type":"text","sessionID":"ses_1","part":{"id":"p1","text":"old"}}`,
		`{"type":"text","part":{"id":"p1","text":"new"}}`,
		`{"type":"text","part":{"id":"p2","text":" answer"}}`,
		`{"type":"step_finish","part":{"reason":"stop","cost":0.1,"tokens":{"input":9,"output":2,"reasoning":3,"cache":{"read":4,"write":5}}}}`,
	}, "\n")
	out, err := parseOpencodeOutput([]byte(ndjson))
	if err != nil || out.text != "new answer" || out.usage != (domain.Usage{Input: 9, Output: 5, CacheRead: 4, CacheWrite: 5}) { t.Fatalf("parse: %#v %v", out, err) }
	if got := strings.Join(opencodeCleanup([]byte(ndjson)), " "); got != "session delete --standalone ses_1" { t.Fatalf("cleanup: %q", got) }
	for _, unsafe := range []string{"--help", "../session", "id with spaces"} {
		line := []byte(`{"sessionID":` + strconvQuote(unsafe) + `}`)
		if got := opencodeCleanup(line); got != nil { t.Fatalf("unsafe session used: %q => %v", unsafe, got) }
	}
	tool, err := parseOpencodeOutput([]byte(`{"type":"tool_use","part":{"text":"private"}}`))
	if err != nil || !tool.toolUse { t.Fatalf("tool use not detected: %#v %v", tool, err) }
}

func strconvQuote(s string) string { b, _ := json.Marshal(s); return string(b) }

func TestOpencodeCLIPhase3C_StructuredValidationDoesNotEchoAnswer(t *testing.T) {
	// TC-58, TC-59 (REQ-C10).
	schema := json.RawMessage(`{"type":"object","required":["ok"],"additionalProperties":false,"properties":{"ok":{"type":"boolean"}}}`)
	if err := ValidateJSONSchema(schema, json.RawMessage(`{"ok":true}`)); err != nil { t.Fatal(err) }
	secret := "PRIVATE_MODEL_ANSWER"
	err := ValidateJSONSchema(schema, json.RawMessage(`{"ok":"`+secret+`"}`))
	if err == nil || strings.Contains(err.Error(), secret) { t.Fatalf("unsafe schema error: %v", err) }
	if _, err := extractJSONObject("```json\n{\"ok\":true}\n```"); err != nil { t.Fatal(err) }
	if _, err := extractJSONObject(`{"ok":true} trailing-private`); err == nil { t.Fatal("accepted trailing text") }
}

func TestToolEmulationPhase3C_DropsUnofferedCalls(t *testing.T) {
	// TC-49, TC-50 (REQ-C4).
	offered := []domain.ToolDef{{Name: "safe_read", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	raw := json.RawMessage(`{"text":"done","calls":[{"id":"a","name":"safe_read","input":{}},{"id":"b","name":"shell","input":{"cmd":"id"}}],"stop":"tool_use"}`)
	resp, dropped, err := ParseToolReply(raw, offered)
	if err != nil || dropped != 1 || len(resp.Calls) != 1 || resp.Calls[0].Name != "safe_read" { t.Fatalf("response=%#v dropped=%d err=%v", resp, dropped, err) }
	request := &domain.ToolRequest{Tools: offered, Turns: []domain.Turn{{Role: domain.UserRole, Text: "untrusted"}}}
	prompt, schema, err := ToolPrompt(request)
	if err != nil || !strings.Contains(prompt, "safe_read") || !json.Valid(schema) { t.Fatalf("prompt/schema: %v", err) }
}

func TestRunnerErrorPhase3C_IsSafe(t *testing.T) {
	// TC-48 (REQ-C3).
	err := runnerErr(context.Background(), errors.New("PRIVATE child output"))
	if strings.Contains(err.Error(), "PRIVATE") { t.Fatal(err) }
}

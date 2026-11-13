package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"regexp"

	"github.com/LDKhangg/nexus-hub/internal/adapters/llm/clirun"
	"github.com/LDKhangg/nexus-hub/internal/domain"
)

// ClaudeCLI is an LLM provider that runs the Claude Code CLI already signed
// in on the host, in headless mode. The prompt and context go on stdin;
// argv carries only fixed flags, the model id, the engine system prompt
// and, for structured calls, the JSON schema. No built-in tool, MCP
// server, project setting or session file is used.
type ClaudeCLI struct {
	run          *clirun.Runner
	defaultModel string
}

// claudeModelRe matches the Claude model ids the settings accept.
var claudeModelRe = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,99}$`)

// NewClaudeCLI returns a provider on runner r.
func NewClaudeCLI(r *clirun.Runner, defaultModel string) (*ClaudeCLI, error) {
	if r == nil {
		return nil, errors.New("claude cli: nil runner")
	}
	if defaultModel == "" {
		defaultModel = defaultAnthropicModel
	}
	return &ClaudeCLI{run: r, defaultModel: defaultModel}, nil
}

// claudeArgs builds the exact argv after the binary (REQ-C2).
func claudeArgs(model, system string, schema json.RawMessage) []string {
	args := []string{
		"-p", "--output-format", "json", "--no-session-persistence", "--strict-mcp-config",
		"--setting-sources", "",
		"--tools", "",
		"--model", model,
		"--system-prompt", system,
	}
	if schema != nil {
		args = append(args, "--json-schema", string(schema))
	}
	return args
}

// claudeResult is the single JSON object `claude -p --output-format json`
// prints.
type claudeResult struct {
	Type             string          `json:"type"`
	Subtype          string          `json:"subtype"`
	IsError          bool            `json:"is_error"`
	Result           string          `json:"result"`
	StructuredOutput json.RawMessage `json:"structured_output"`
	StopReason       string          `json:"stop_reason"`
	TotalCostUSD     float64         `json:"total_cost_usd"`
	APIErrorStatus   int             `json:"api_error_status"`
	Usage            struct {
		InputTokens              int64 `json:"input_tokens"`
		OutputTokens             int64 `json:"output_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	} `json:"usage"`
}

func (r *claudeResult) usage() domain.Usage {
	return domain.Usage{Input: r.Usage.InputTokens, Output: r.Usage.OutputTokens, CacheRead: r.Usage.CacheReadInputTokens, CacheWrite: r.Usage.CacheCreationInputTokens}
}

// parseClaudeOutput reads the result object. A JSON array of messages (as
// some versions print with verbose output) yields its last "result" entry.
func parseClaudeOutput(stdout []byte) (*claudeResult, error) {
	data := bytes.TrimSpace(stdout)
	if len(data) == 0 {
		return nil, errors.New("empty output")
	}
	if data[0] == '[' {
		var all []json.RawMessage
		if err := json.Unmarshal(data, &all); err != nil {
			return nil, errors.New("bad output")
		}
		for i := len(all) - 1; i >= 0; i-- {
			var r claudeResult
			if json.Unmarshal(all[i], &r) == nil && r.Type == "result" {
				return &r, nil
			}
		}
		return nil, errors.New("no result")
	}
	var r claudeResult
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, errors.New("bad output")
	}
	return &r, nil
}

// call runs one CLI invocation and maps every failure to a safe error.
func (c *ClaudeCLI) call(ctx context.Context, model, system, prompt string, schema json.RawMessage) (*claudeResult, error) {
	if model == "" {
		model = c.defaultModel
	}
	if !claudeModelRe.MatchString(model) {
		return nil, &domain.LLMError{Code: domain.LLMCodeError}
	}
	sys, err := cliSystemPrompt(system)
	if err != nil {
		return nil, err
	}
	res, err := c.run.Run(ctx, clirun.Call{Args: claudeArgs(model, sys, schema), Stdin: []byte(prompt)})
	if err != nil {
		return nil, runnerErr(ctx, err)
	}
	out, perr := parseClaudeOutput(res.Stdout)
	if perr != nil {
		if res.ExitCode != 0 {
			return nil, ClassifyCLIFailure(0, string(res.Stderr))
		}
		return nil, &domain.LLMError{Code: domain.LLMCodeError, Retryable: true}
	}
	if out.IsError || res.ExitCode != 0 || (out.Subtype != "" && out.Subtype != "success") {
		return nil, ClassifyCLIFailure(out.APIErrorStatus, out.Subtype, out.Result, string(res.Stderr))
	}
	if out.StopReason == "refusal" {
		return nil, ErrRefused
	}
	return out, nil
}

// runnerErr maps runner failures. Cancellation of the caller's context is
// returned as is, like the API client does.
func runnerErr(ctx context.Context, err error) error {
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.Is(err, clirun.ErrTimeout):
		return &domain.LLMError{Code: domain.LLMCodeTimeout, Retryable: true}
	case errors.Is(err, clirun.ErrStart):
		return &domain.LLMError{Code: domain.LLMCodeUnconfigured}
	default:
		return &domain.LLMError{Code: domain.LLMCodeError, Retryable: true}
	}
}

// Generate runs a plain text call; the answer is the CLI's "result".
// MaxTokens is not enforced: the CLI has no output-token flag.
func (c *ClaudeCLI) Generate(ctx context.Context, req *domain.LLMRequest) (*domain.LLMResponse, error) {
	if req == nil {
		return nil, errors.New("claude cli generate: nil request")
	}
	prompt, err := RenderMessages(req.Messages)
	if err != nil {
		return nil, err
	}
	out, err := c.call(ctx, req.Model, req.System, prompt, nil)
	if err != nil {
		return nil, err
	}
	return &domain.LLMResponse{Text: out.Result, Usage: out.usage()}, nil
}

// GenerateStructured runs a structured call (--json-schema); the answer is
// the CLI's "structured_output".
func (c *ClaudeCLI) GenerateStructured(ctx context.Context, req *StructuredRequest) (*StructuredResponse, error) {
	if req == nil || !json.Valid(req.Schema) {
		return nil, errors.New("claude cli structured: invalid request")
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, req.Schema); err != nil {
		return nil, errors.New("claude cli structured: invalid schema")
	}
	out, err := c.call(ctx, req.Model, req.System, req.Prompt, compact.Bytes())
	if err != nil {
		return nil, err
	}
	so := bytes.TrimSpace(out.StructuredOutput)
	if len(so) == 0 || bytes.Equal(so, []byte("null")) || !json.Valid(so) {
		return nil, &domain.LLMError{Code: domain.LLMCodeError, Retryable: true}
	}
	return &StructuredResponse{JSON: json.RawMessage(so), Usage: out.usage(), CostUSD: out.TotalCostUSD}, nil
}

// GenerateWithTools emulates one tool-calling step: the tools and the
// transcript go on stdin, the reply is a structured {text, calls, stop}.
// The CLI itself has no tool (--tools ""); the hub's executor runs every
// returned call. Calls to tools not offered are dropped.
func (c *ClaudeCLI) GenerateWithTools(ctx context.Context, req *domain.ToolRequest) (*domain.ToolResponse, error) {
	prompt, schema, err := ToolPrompt(req)
	if err != nil {
		return nil, err
	}
	sr, err := c.GenerateStructured(ctx, &StructuredRequest{Model: req.Model, System: req.System, Prompt: prompt, Schema: schema})
	if err != nil {
		return nil, err
	}
	resp, dropped, err := ParseToolReply(sr.JSON, req.Tools)
	if err != nil {
		return nil, err
	}
	if dropped > 0 {
		log.Printf("claude_cli: dropped %d tool call(s) that were not offered or malformed", dropped)
	}
	resp.Usage = sr.Usage
	return resp, nil
}

var (
	_ domain.LLMProvider         = (*ClaudeCLI)(nil)
	_ domain.ToolCallingProvider = (*ClaudeCLI)(nil)
	_ StructuredProvider         = (*ClaudeCLI)(nil)
)

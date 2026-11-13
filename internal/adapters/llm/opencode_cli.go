package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"regexp"
	"strconv"
	"strings"

	"github.com/LDKhangg/nexus-hub/internal/adapters/llm/clirun"
	"github.com/LDKhangg/nexus-hub/internal/domain"
	"github.com/LDKhangg/nexus-hub/internal/settings"
)

// OpencodeCLI is an LLM provider that runs the opencode CLI already signed
// in on the host (for example with an OpenAI account), headless:
//
//	opencode run --standalone --format json --model <provider/model> --agent nexus-hub --title nexus-hub
//
// The prompt and context go on stdin. The engine's system prompt goes in
// an opencode.json written into the call's fresh working directory, which
// defines the agent nexus-hub with every permission denied and no MCP
// server; the child's XDG_CONFIG_HOME is a fresh empty directory, so no
// user configuration, plugin or MCP server is loaded. opencode v2 still
// offers its inert `skill` tool to every agent: any tool_use event fails
// the call (llm_error, note cli_tool_use) and its text is discarded. The
// session the run creates is deleted afterwards.
type OpencodeCLI struct {
	run          *clirun.Runner
	defaultModel string
}

const (
	// opencodeAgentName is the agent the generated config defines.
	opencodeAgentName = "nexus-hub"
	// opencodeConfigFile is the project config opencode reads from its
	// working directory.
	opencodeConfigFile = "opencode.json"
	// opencodeNoTools is appended to every system prompt.
	opencodeNoTools = "\n\nYou have no tools. No tool, function, skill, command or file exists in this conversation; " +
		"never try to call one. Answer with text only, from the conversation alone."
	// NoteCLIToolUse marks a call that failed because the CLI's model
	// tried to call a tool.
	NoteCLIToolUse = "cli_tool_use"
	// NoteCLISessionCleanupFailed marks a successful call whose opencode
	// session could not be deleted after two attempts.
	NoteCLISessionCleanupFailed = "cli_session_cleanup_failed"
	// structuredSuffix ends every structured prompt.
	structuredSuffix = "Reply with exactly one JSON object and nothing else."
)

// opencodeSessionRe matches the session ids the hub passes back to
// `opencode session delete` (never flag-like).
var opencodeSessionRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_]{0,127}$`)

// NewOpencodeCLI returns a provider on runner r. defaultModel is used
// when a request names no model.
func NewOpencodeCLI(r *clirun.Runner, defaultModel string) (*OpencodeCLI, error) {
	if r == nil {
		return nil, errors.New("opencode cli: nil runner")
	}
	return &OpencodeCLI{run: r, defaultModel: defaultModel}, nil
}

// opencodeArgs builds the exact argv after the binary (REQ-C9). No
// message argument: the prompt goes on stdin.
func opencodeArgs(model string) []string {
	return []string{
		"run", "--standalone", "--format", "json",
		"--model", model,
		"--agent", opencodeAgentName,
		"--title", opencodeAgentName,
	}
}

type ocRule struct {
	Action   string `json:"action"`
	Resource string `json:"resource"`
	Effect   string `json:"effect"`
}

type ocAgent struct {
	Description string   `json:"description"`
	Mode        string   `json:"mode"`
	System      string   `json:"system"`
	Permissions []ocRule `json:"permissions"`
}

type ocConfig struct {
	Agents      map[string]ocAgent `json:"agents"`
	Permissions []ocRule           `json:"permissions"`
	Policies    []ocRule           `json:"policies"`
}

// opencodeDenyAll denies every permission, and the skill permission by
// name (opencode's browser plugin adds a skill rule after the config's).
var opencodeDenyAll = []ocRule{
	{Action: "*", Resource: "*", Effect: "deny"},
	{Action: "skill", Resource: "*", Effect: "deny"},
}

// opencodePolicies deny tool use, MCP, skill loading and plugins.
var opencodePolicies = []ocRule{
	{Action: "tool.use", Resource: "*", Effect: "deny"},
	{Action: "mcp.use", Resource: "*", Effect: "deny"},
	{Action: "skill.use", Resource: "*", Effect: "deny"},
	{Action: "plugin.use", Resource: "*", Effect: "deny"},
}

// opencodeConfig renders the opencode.json for one call.
func opencodeConfig(system string) ([]byte, error) {
	cfg := ocConfig{
		Agents: map[string]ocAgent{opencodeAgentName: {
			Description: "Nexus-Hub LLM provider",
			Mode:        "primary",
			System:      system + opencodeNoTools,
			Permissions: opencodeDenyAll,
		}},
		Permissions: opencodeDenyAll,
		Policies:    opencodePolicies,
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		return nil, errors.New("opencode cli: cannot render config")
	}
	return data, nil
}

// ocEvent is one NDJSON line of `opencode run --format json`.
type ocEvent struct {
	Type      string `json:"type"`
	SessionID string `json:"sessionID"`
	Part      struct {
		ID     string  `json:"id"`
		Text   string  `json:"text"`
		Tool   string  `json:"tool"`
		Reason string  `json:"reason"`
		Cost   float64 `json:"cost"`
		Tokens *struct {
			Input     int64 `json:"input"`
			Output    int64 `json:"output"`
			Reasoning int64 `json:"reasoning"`
			Cache     struct {
				Read  int64 `json:"read"`
				Write int64 `json:"write"`
			} `json:"cache"`
		} `json:"tokens"`
	} `json:"part"`
	Error *struct {
		Type    string `json:"type"`
		Name    string `json:"name"`
		Message string `json:"message"`
		Data    struct {
			Message string `json:"message"`
		} `json:"data"`
	} `json:"error"`
}

// ocOutput is a parsed run. Text must never be logged.
type ocOutput struct {
	text      string
	usage     domain.Usage
	cost      float64
	reason    string // the last step_finish reason
	steps     int    // step_finish events
	toolUse   bool
	errType   string // the error event's type or name
	errText   string // the error event's message; classification only
	hasError  bool
	sessionID string
	note      string // fixed safe detail about a completed call
}

// parseOpencodeOutput reads the NDJSON events. A line that is not a JSON
// object is an error. Text parts are concatenated in order; a part that
// appears more than once (an update) keeps its last text.
func parseOpencodeOutput(stdout []byte) (*ocOutput, error) {
	out := &ocOutput{}
	var order []string
	texts := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(stdout))
	sc.Buffer(make([]byte, 0, 64<<10), clirun.MaxOutputBytes)
	n := 0
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var ev ocEvent
		if line[0] != '{' || json.Unmarshal(line, &ev) != nil {
			return nil, errors.New("bad output")
		}
		n++
		if out.sessionID == "" && ev.SessionID != "" {
			out.sessionID = ev.SessionID
		}
		switch ev.Type {
		case "text":
			id := ev.Part.ID
			if id == "" {
				id = "#" + strconv.Itoa(len(order)) // a part without an id never merges
			}
			if _, seen := texts[id]; !seen {
				order = append(order, id)
			}
			texts[id] = ev.Part.Text
		case "tool_use":
			out.toolUse = true
		case "step_finish":
			out.steps++
			out.reason = ev.Part.Reason
			out.cost += ev.Part.Cost
			if t := ev.Part.Tokens; t != nil {
				out.usage.Add(domain.Usage{Input: t.Input, Output: t.Output + t.Reasoning, CacheRead: t.Cache.Read, CacheWrite: t.Cache.Write})
			}
		case "error":
			out.hasError = true
			if ev.Error != nil {
				out.errType = ev.Error.Type
				if out.errType == "" {
					out.errType = ev.Error.Name
				}
				out.errText = ev.Error.Message + "\n" + ev.Error.Data.Message
			}
		}
	}
	if sc.Err() != nil {
		return nil, errors.New("bad output")
	}
	if n == 0 {
		return nil, errors.New("empty output")
	}
	var sb strings.Builder
	for _, id := range order {
		sb.WriteString(texts[id])
	}
	out.text = sb.String()
	return out, nil
}

// opencodeSessionID returns the first session id in (possibly partial)
// NDJSON output, or "" when there is none or it is not a safe id.
func opencodeSessionID(stdout []byte) string {
	sc := bufio.NewScanner(bytes.NewReader(stdout))
	sc.Buffer(make([]byte, 0, 64<<10), clirun.MaxOutputBytes)
	for sc.Scan() {
		var ev struct {
			SessionID string `json:"sessionID"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) == nil && ev.SessionID != "" {
			if opencodeSessionRe.MatchString(ev.SessionID) {
				return ev.SessionID
			}
			return ""
		}
	}
	return ""
}

// opencodeCleanup is the runner follow-up that deletes the session a run
// created, in the same working directory (best effort).
func opencodeCleanup(stdout []byte) []string {
	id := opencodeSessionID(stdout)
	if id == "" {
		return nil
	}
	return []string{"session", "delete", "--standalone", id}
}

// classifyOpencodeError maps an error event to a safe error. The texts are
// only inspected, never kept.
func classifyOpencodeError(o *ocOutput, stderr []byte) *domain.LLMError {
	t := strings.ToLower(o.errType)
	switch {
	case strings.Contains(t, "no-route"):
		// No sign-in for the provider, or an unknown model.
		return &domain.LLMError{Code: domain.LLMCodeUnconfigured}
	case strings.Contains(strings.ToLower(o.errText), "agent not found"):
		return &domain.LLMError{Code: domain.LLMCodeError, Retryable: true}
	}
	return ClassifyCLIFailure(0, o.errType, o.errText, string(stderr))
}

// call runs one opencode invocation and maps every failure to a safe
// error. The returned text must never be logged.
func (c *OpencodeCLI) call(ctx context.Context, model, system, prompt string) (*ocOutput, error) {
	if model == "" {
		model = c.defaultModel
	}
	if !settings.IsOpencodeModelID(model) {
		// A Claude-shaped id with the opencode provider: a configuration
		// mismatch, not a transient failure.
		return nil, &domain.LLMError{Code: domain.LLMCodeUnconfigured}
	}
	sys, err := cliSystemPrompt(system)
	if err != nil {
		return nil, err
	}
	cfg, err := opencodeConfig(sys)
	if err != nil {
		return nil, err
	}
	res, err := c.run.Run(ctx, clirun.Call{
		Args:        opencodeArgs(model),
		Stdin:       []byte(prompt),
		Files:       map[string][]byte{opencodeConfigFile: cfg},
		EmptyDirEnv: []string{"XDG_CONFIG_HOME"},
		FollowUp:    opencodeCleanup,
	})
	if err != nil {
		return nil, runnerErr(ctx, err)
	}
	out, perr := parseOpencodeOutput(res.Stdout)
	if perr != nil {
		if res.ExitCode != 0 {
			return nil, ClassifyCLIFailure(0, string(res.Stderr))
		}
		return nil, &domain.LLMError{Code: domain.LLMCodeError, Retryable: true}
	}
	switch {
	case out.toolUse:
		log.Printf("opencode_cli: the model tried to call a tool; call rejected (%s)", NoteCLIToolUse)
		return nil, &domain.LLMError{Code: domain.LLMCodeError, Retryable: true, Note: NoteCLIToolUse}
	case out.hasError:
		return nil, classifyOpencodeError(out, res.Stderr)
	case res.ExitCode != 0:
		return nil, ClassifyCLIFailure(0, string(res.Stderr))
	case out.reason == "content-filter":
		return nil, ErrRefused
	case out.steps == 0 || out.reason != "stop":
		return nil, &domain.LLMError{Code: domain.LLMCodeError, Retryable: true}
	}
	if res.FollowUpError != nil {
		out.note = NoteCLISessionCleanupFailed
		log.Printf("opencode_cli: warning (%s)", NoteCLISessionCleanupFailed)
	}
	return out, nil
}

// Generate runs a plain text call; the answer is the concatenated text
// parts. MaxTokens is not enforced: the CLI has no output-token flag.
func (c *OpencodeCLI) Generate(ctx context.Context, req *domain.LLMRequest) (*domain.LLMResponse, error) {
	if req == nil {
		return nil, errors.New("opencode cli generate: nil request")
	}
	prompt, err := RenderMessages(req.Messages)
	if err != nil {
		return nil, err
	}
	out, err := c.call(ctx, req.Model, req.System, prompt)
	if err != nil {
		return nil, err
	}
	return &domain.LLMResponse{Text: out.text, Usage: out.usage, Note: out.note}, nil
}

// structuredPrompt appends the schema and the one-object instruction.
func structuredPrompt(prompt string, schema []byte) string {
	return prompt + "\n\nYour reply must be one JSON object that matches this JSON schema:\n<schema>\n" +
		string(schema) + "\n</schema>\n" + structuredSuffix + "\n"
}

// extractJSONObject returns the JSON object an answer holds: the whole
// answer, optionally inside one Markdown code fence.
func extractJSONObject(text string) (json.RawMessage, error) {
	s := stripFence(text)
	if !strings.HasPrefix(s, "{") {
		return nil, errors.New("the reply is not a JSON object")
	}
	dec := json.NewDecoder(strings.NewReader(s))
	var v json.RawMessage
	if dec.Decode(&v) != nil {
		return nil, errors.New("the reply is not valid JSON")
	}
	if dec.More() {
		return nil, errors.New("the reply has text after the JSON object")
	}
	return v, nil
}

// GenerateStructured asks for one JSON object matching req.Schema in the
// prompt, then validates the answer. One repair attempt is made with the
// validation error (never the answer) fed back; a second failure is a
// provider error.
func (c *OpencodeCLI) GenerateStructured(ctx context.Context, req *StructuredRequest) (*StructuredResponse, error) {
	if req == nil || !json.Valid(req.Schema) {
		return nil, errors.New("opencode cli structured: invalid request")
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, req.Schema); err != nil {
		return nil, errors.New("opencode cli structured: invalid schema")
	}
	schema := compact.Bytes()
	base := structuredPrompt(req.Prompt, schema)
	prompt := base
	var usage domain.Usage
	var cost float64
	var note string
	for try := 0; try < 2; try++ {
		out, err := c.call(ctx, req.Model, req.System, prompt)
		if err != nil {
			return nil, err
		}
		usage.Add(out.usage)
		cost += out.cost
		if out.note != "" {
			note = out.note
		}
		obj, verr := extractJSONObject(out.text)
		if verr == nil {
			verr = ValidateJSONSchema(schema, obj)
		}
		if verr == nil {
			return &StructuredResponse{JSON: obj, Usage: usage, CostUSD: cost, Note: note}, nil
		}
		// The validation error names only schema paths and rules.
		prompt = base + "\nA previous reply to this request was rejected: " + verr.Error() +
			". Write a new reply that fixes this. " + structuredSuffix + "\n"
	}
	return nil, &domain.LLMError{Code: domain.LLMCodeError, Retryable: true}
}

// GenerateWithTools emulates one tool-calling step through a structured
// call ({text, calls, stop}); the hub's executor runs every returned call.
// Calls to tools not offered are dropped.
func (c *OpencodeCLI) GenerateWithTools(ctx context.Context, req *domain.ToolRequest) (*domain.ToolResponse, error) {
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
		log.Printf("opencode_cli: dropped %d tool call(s) that were not offered or malformed", dropped)
	}
	resp.Usage = sr.Usage
	resp.Note = sr.Note
	return resp, nil
}

var (
	_ domain.LLMProvider         = (*OpencodeCLI)(nil)
	_ domain.ToolCallingProvider = (*OpencodeCLI)(nil)
	_ StructuredProvider         = (*OpencodeCLI)(nil)
)

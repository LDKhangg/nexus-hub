package worker

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// The executor reads its task from this file in the worktree's private
// .nexus directory (git-excluded, never committed). Prompts on argv are
// fixed text only.
const (
	privateDirName      = ".nexus"
	instructionFileName = "instructions.md"
	execPrompt          = "Read .nexus/instructions.md in the current directory and carry out the task it describes in this repository. Edit files only inside this directory. Do not commit, push, or open pull requests; the worker does that after you finish. End with a short summary of what you changed."
	followUpPrompt      = "The owner added follow-up instructions at the end of .nexus/instructions.md (section \"Follow-up\"). Apply them in this repository, then end with a short summary of everything you changed."
	maxExecStdout       = 8 << 20
	maxSummaryRunes     = 2000
)

// ExecRequest is one executor run.
type ExecRequest struct {
	Dir      string        // the worktree
	MaxTurns int           // remaining turn budget
	Timeout  time.Duration // remaining wall time
	Resume   string        // executor session to continue ("" = new)
	FollowUp bool          // use the follow-up prompt
	OnStart  func(pid int)
}

// ExecResult is what an executor run produced. FinalMessage is the
// executor's last message (bounded); it is shown only to the owner.
type ExecResult struct {
	ExitCode     int
	TimedOut     bool
	TurnLimit    bool
	Cancelled    bool
	FinalMessage string
	SessionID    string
	Turns        int64
	InputTokens  int64
	OutputTokens int64
}

// Executor runs a coding agent in a worktree. Tests use a fake.
type Executor interface {
	Name() string
	Run(ctx context.Context, req ExecRequest) (ExecResult, error)
	// Version returns the executor's version string, or "" when unknown.
	Version(ctx context.Context) string
}

// ClaudeExecutor runs `claude -p` with the owner's subscription login.
// Permissions: --permission-mode acceptEdits plus an explicit
// --allowedTools allow-list and --disallowedTools deny-list; never
// --dangerously-skip-permissions. Anything not allowed is refused in
// headless mode.
type ClaudeExecutor struct {
	Bin          string
	Model        string
	AllowedTools []string
	Runner       Runner
}

// Name implements Executor.
func (e *ClaudeExecutor) Name() string { return ExecutorClaude }

// Run implements Executor.
func (e *ClaudeExecutor) Run(ctx context.Context, req ExecRequest) (ExecResult, error) {
	prompt := execPrompt
	if req.FollowUp {
		prompt = followUpPrompt
	}
	args := []string{"-p", prompt, "--output-format", "json", "--max-turns", strconv.Itoa(max(req.MaxTurns, 1)),
		"--permission-mode", "acceptEdits",
		"--allowedTools", strings.Join(e.AllowedTools, ","),
		"--disallowedTools", strings.Join(claudeDisallowedTools, ",")}
	if e.Model != "" {
		args = append(args, "--model", e.Model)
	}
	if req.Resume != "" && sessionIDRE.MatchString(req.Resume) {
		args = append(args, "--resume", req.Resume)
	}
	out := newCapBuffer(maxExecStdout)
	res, err := runExec(ctx, e.Runner, Cmd{Dir: req.Dir, Name: e.Bin, Args: args, Stdout: out, OnStart: req.OnStart}, req.Timeout)
	if err != nil {
		return res, err
	}
	parseClaudeJSON(out.String(), &res)
	return res, nil
}

var sessionIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// parseClaudeJSON reads the `--output-format json` result object.
func parseClaudeJSON(out string, res *ExecResult) {
	out = strings.TrimSpace(out)
	if i := strings.LastIndex(out, "\n{"); i >= 0 {
		out = out[i+1:]
	}
	var r struct {
		Subtype   string `json:"subtype"`
		IsError   bool   `json:"is_error"`
		NumTurns  int64  `json:"num_turns"`
		Result    string `json:"result"`
		SessionID string `json:"session_id"`
		Usage     struct {
			InputTokens         int64 `json:"input_tokens"`
			OutputTokens        int64 `json:"output_tokens"`
			CacheCreationTokens int64 `json:"cache_creation_input_tokens"`
			CacheReadTokens     int64 `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal([]byte(out), &r) != nil {
		return
	}
	res.FinalMessage = clipRunes(r.Result, maxSummaryRunes)
	if sessionIDRE.MatchString(r.SessionID) {
		res.SessionID = r.SessionID
	}
	res.Turns = max(r.NumTurns, 0)
	res.InputTokens = max(r.Usage.InputTokens+r.Usage.CacheCreationTokens+r.Usage.CacheReadTokens, 0)
	res.OutputTokens = max(r.Usage.OutputTokens, 0)
	if r.Subtype == "error_max_turns" {
		res.TurnLimit = true
	}
}

// OpencodeExecutor runs `opencode run`. It has no turn limit flag, so only
// the wall-clock limit applies; usage is unknown (0).
type OpencodeExecutor struct {
	Bin    string
	Model  string
	Agent  string
	Runner Runner
}

// Name implements Executor.
func (e *OpencodeExecutor) Name() string { return ExecutorOpencode }

// Run implements Executor.
func (e *OpencodeExecutor) Run(ctx context.Context, req ExecRequest) (ExecResult, error) {
	args := []string{"run"}
	if e.Model != "" {
		args = append(args, "--model", e.Model)
	}
	if e.Agent != "" {
		args = append(args, "--agent", e.Agent)
	}
	prompt := execPrompt
	if req.FollowUp {
		prompt = followUpPrompt
		args = append(args, "--continue")
	}
	args = append(args, prompt)
	out := newTailBuffer(64 << 10)
	res, err := runExec(ctx, e.Runner, Cmd{Dir: req.Dir, Name: e.Bin, Args: args, Stdout: out, OnStart: req.OnStart}, req.Timeout)
	if err != nil {
		return res, err
	}
	res.FinalMessage = clipRunesTail(strings.TrimSpace(stripANSI(out.String())), maxSummaryRunes)
	return res, nil
}

// runExec runs c with a wall-clock timeout. A timeout or a cancel is a
// result, not an error; err is only "could not start".
func runExec(ctx context.Context, r Runner, c Cmd, timeout time.Duration) (ExecResult, error) {
	if timeout <= 0 {
		return ExecResult{ExitCode: -1, TimedOut: true}, nil
	}
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	exit, err := r.Run(tctx, c)
	res := ExecResult{ExitCode: exit}
	switch {
	case errors.Is(err, ErrNotStarted):
		return res, ErrNotStarted
	case err != nil && ctx.Err() != nil:
		res.Cancelled = true
	case err != nil:
		res.TimedOut = true
	}
	return res, nil
}

var versionRE = regexp.MustCompile(`[0-9]+\.[0-9]+(\.[0-9]+)?([A-Za-z0-9.+_-]*)?`)

func binVersion(ctx context.Context, r Runner, bin string) string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out := newCapBuffer(4 << 10)
	if exit, err := r.Run(ctx, Cmd{Name: bin, Args: []string{"--version"}, Stdout: out}); err != nil || exit != 0 {
		return ""
	}
	v := versionRE.FindString(out.String())
	if len(v) > 64 {
		v = v[:64]
	}
	return v
}

// Version implements Executor.
func (e *ClaudeExecutor) Version(ctx context.Context) string { return binVersion(ctx, e.Runner, e.Bin) }

// Version implements Executor.
func (e *OpencodeExecutor) Version(ctx context.Context) string {
	return binVersion(ctx, e.Runner, e.Bin)
}

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

func stripANSI(s string) string { return ansiRE.ReplaceAllString(s, "") }

// clipRunes keeps the first n runes of valid UTF-8.
func clipRunes(s string, n int) string {
	s = strings.ToValidUTF8(s, "")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

// clipRunesTail keeps the last n runes.
func clipRunesTail(s string, n int) string {
	s = strings.ToValidUTF8(s, "")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return "…" + string(r[len(r)-n+1:])
}

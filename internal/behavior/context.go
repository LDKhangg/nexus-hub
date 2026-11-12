package behavior

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/LDKhangg/nexus-hub/internal/agent/tools"
)

// This file collects a run's context through its allowed read tools and
// turns every fetched text into fragments with stable locators.
//
// Locator scheme. A fragment's source names where its lines came from; a
// line is cited as "<source>:L<n>" (or a range "<source>:L<a>-L<b>"),
// where n counts the lines of that fragment as fetched (1-based, after the
// executor's sanitizing), not the line numbers of the original file:
//
//   clickup:task/<id>#meta            identifier, name, status... lines
//   clickup:task/<id>#desc            the task description
//   github:<owner>/<repo>#<n>:@pr          a pull request's header lines
//   github:<owner>/<repo>#<n>:@description a pull request's description
//   github:<owner>/<repo>#<n>:@checks      CI check summary
//   github:<owner>/<repo>#<n>:@files       one line per changed file
//   github:<owner>/<repo>#<n>:<path>       the clipped patch of <path>
//   github:<owner>/<repo>#<n>:@issue       an issue's header lines
//   github:<owner>/<repo>#<n>:@body        an issue's body
//
// The engine knows, per read tool, how to derive the tool's input from
// the run's subject or the context already collected (readRules). Tools
// without a rule are noted as not read.

// Context limits.
const (
	ContextSnapshotVersion = 1
	// MaxSnapshotBytes caps the text kept for one run (and given to the
	// model).
	MaxSnapshotBytes = 512 << 10
	// DefaultReadBytes caps one read when the behavior sets no max_bytes.
	DefaultReadBytes = 32 << 10
	// MaxRefsPerKind caps the GitHub pull requests (and, separately, the
	// issues) one run reads.
	MaxRefsPerKind = 5
)

// Read note codes: why something was not read, or was cut.
const (
	NoteNotGranted     = "not_granted"     // the repo has no grant in the workspace
	NoteRefCap         = "ref_cap"         // more references than MaxRefsPerKind
	NoteNoInputRule    = "no_input_rule"   // the engine cannot derive this tool's input
	NoteNoSubject      = "no_subject"      // the subject does not name what the tool reads
	NoteNotImplemented = "not_implemented" // a read the engine does not serve yet
	NoteTruncated      = "truncated"       // the read was cut at its byte cap
	NoteSnapshotCap    = "snapshot_cap"    // the run's context reached MaxSnapshotBytes
)

// Fragment is a part of a fetched text, split into lines.
type Fragment struct {
	Source string   `json:"source"`
	Tool   string   `json:"tool"`
	Lines  []string `json:"lines"`
}

// ReadNote records a read that did not happen, failed or was cut. Ref
// holds identifiers only (a task ID, "owner/repo#n").
type ReadNote struct {
	Tool string `json:"tool"`
	Ref  string `json:"ref,omitempty"`
	Code string `json:"code"`
}

// ContextSnapshot is what a run read: the fragments the model saw, by
// source, and the notes about what was not read. It is stored on the run
// so citations can be verified and traced later. Secrets are redacted by
// the executor before text lands here.
type ContextSnapshot struct {
	Version   int        `json:"version"`
	Fragments []Fragment `json:"fragments"`
	Notes     []ReadNote `json:"notes,omitempty"`
	Truncated bool       `json:"truncated,omitempty"`
}

// Fragment returns the fragment of a source, or nil.
func (s *ContextSnapshot) Fragment(source string) *Fragment {
	for i := range s.Fragments {
		if s.Fragments[i].Source == source {
			return &s.Fragments[i]
		}
	}
	return nil
}

// Bytes is the text size of the snapshot's lines.
func (s *ContextSnapshot) Bytes() int {
	n := 0
	for _, f := range s.Fragments {
		for _, l := range f.Lines {
			n += len(l) + 1
		}
	}
	return n
}

// DecodeContextSnapshot reads a run's stored snapshot; empty input gives an
// empty snapshot.
func DecodeContextSnapshot(raw json.RawMessage) (ContextSnapshot, error) {
	s := ContextSnapshot{Version: ContextSnapshotVersion}
	if len(raw) == 0 {
		return s, nil
	}
	err := json.Unmarshal(raw, &s)
	return s, err
}

// ToolCaller runs one tool call through the Phase 2 policy, audit and caps
// (tools.Executor).
type ToolCaller interface {
	Call(ctx context.Context, step int, name string, input json.RawMessage) tools.CallResult
}

// readCall is one derived tool call.
type readCall struct {
	ref   string // identifiers only, for notes
	base  string // the locator base of what it reads
	input json.RawMessage
}

// readRule says how the engine feeds one read tool. Stage 0 reads from the
// subject; stage 1 reads references found in stage 0's fragments.
type readRule struct {
	stage  int
	inputs func(c *collector, spec ReadSpec) ([]readCall, []ReadNote)
	split  func(base, text string) []Fragment
}

var readRules = map[string]readRule{
	"clickup_get_task":  {stage: 0, inputs: subjectTaskInputs, split: splitTask},
	"github_pr_view":    {stage: 1, inputs: githubRefInputs(refPull), split: splitHeaderBody(":@pr", "description:", ":@description")},
	"github_pr_files":   {stage: 1, inputs: githubRefInputs(refPull), split: splitPRFiles},
	"github_pr_checks":  {stage: 1, inputs: githubRefInputs(refPull), split: splitWhole(":@checks")},
	"github_issue_view": {stage: 1, inputs: githubRefInputs(refIssue), split: splitHeaderBody(":@issue", "body:", ":@body")},
}

// readError is a read failure the run should retry later.
type readError struct{ code string }

func (e *readError) Error() string { return "behavior: read failed: " + e.code }

// transientCodes are tool failures worth retrying the run for.
var transientCodes = map[string]bool{
	"timeout": true, "network_error": true, "rate_limited": true, "server_error": true, "audit_unavailable": true,
}

var codeRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,40}$`)

// resultCode extracts the safe code of a failed call ("error: x",
// "denied: x").
func resultCode(res tools.CallResult) string {
	s := res.Content
	if i := strings.IndexByte(s, ':'); i >= 0 {
		s = s[i+1:]
	}
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, " ("); i >= 0 {
		s = s[:i]
	}
	if !codeRe.MatchString(s) {
		return "tool_error"
	}
	return s
}

// collector reads a run's context.
type collector struct {
	caller  ToolCaller
	subject string
	taskID  string            // from the subject; empty when not a task
	grants  map[string]string // lower-case repo -> granted spelling
	step    int
	cache   map[string]tools.CallResult
	snap    ContextSnapshot
	stage0  []Fragment
}

func newCollector(caller ToolCaller, subject string, grantedRepos []string) *collector {
	c := &collector{
		caller: caller, subject: subject, grants: map[string]string{}, cache: map[string]tools.CallResult{},
		snap: ContextSnapshot{Version: ContextSnapshotVersion, Fragments: []Fragment{}},
	}
	if id, ok := strings.CutPrefix(subject, SubjectTaskPrefix); ok && taskIDRe.MatchString(id) {
		c.taskID = id
	}
	for _, r := range grantedRepos {
		c.grants[strings.ToLower(r)] = r
	}
	return c
}

// call runs a tool once per (tool, input) for this run.
func (c *collector) call(ctx context.Context, tool string, input json.RawMessage) tools.CallResult {
	key := tool + "\x00" + string(input)
	if res, ok := c.cache[key]; ok {
		return res
	}
	c.step++
	res := c.caller.Call(ctx, c.step, tool, input)
	if res.Status != tools.StatusDenied && (res.Status == tools.StatusTimeout || (res.IsError && transientCodes[resultCode(res)])) {
		return res // not cached: a later call may succeed
	}
	c.cache[key] = res
	return res
}

func (c *collector) note(tool, ref, code string) {
	for _, n := range c.snap.Notes {
		if n == (ReadNote{Tool: tool, Ref: ref, Code: code}) {
			return
		}
	}
	c.snap.Notes = append(c.snap.Notes, ReadNote{Tool: tool, Ref: ref, Code: code})
}

func readCap(spec ReadSpec) int {
	if spec.MaxBytes > 0 {
		return spec.MaxBytes
	}
	return DefaultReadBytes
}

// taskInput is the clickup_get_task input for a task; the description clip
// follows the read's byte cap.
func taskInput(taskID string, maxBytes int) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"task_id": taskID, "description_chars": maxBytes})
	return b
}

// collect runs the behavior's reads in stage order. A transient failure
// returns a *readError so the run is retried; other failures are noted.
func (c *collector) collect(ctx context.Context, reads []ReadSpec) (ContextSnapshot, error) {
	for stage := 0; stage <= 1; stage++ {
		for _, spec := range reads {
			if spec.Tool == MemorySearchTool {
				if stage == 0 {
					c.note(spec.Tool, "", NoteNotImplemented)
				}
				continue
			}
			rule, ok := readRules[spec.Tool]
			if !ok {
				if stage == 0 {
					c.note(spec.Tool, "", NoteNoInputRule)
				}
				continue
			}
			if rule.stage != stage {
				continue
			}
			calls, notes := rule.inputs(c, spec)
			for _, n := range notes {
				c.note(n.Tool, n.Ref, n.Code)
			}
			for _, rc := range calls {
				res := c.call(ctx, spec.Tool, rc.input)
				if ctx.Err() != nil {
					return c.snap, ctx.Err()
				}
				if res.IsError {
					code := resultCode(res)
					if res.Status == tools.StatusTimeout || (res.Status != tools.StatusDenied && transientCodes[code]) {
						return c.snap, &readError{code: code}
					}
					c.note(spec.Tool, rc.ref, code)
					continue
				}
				text, cut := capBytes(res.Content, readCap(spec))
				if cut {
					c.note(spec.Tool, rc.ref, NoteTruncated)
				}
				frags := rule.split(rc.base, text)
				for i := range frags {
					frags[i].Tool = spec.Tool
				}
				c.add(frags, spec.Tool, rc.ref)
				if stage == 0 {
					c.stage0 = append(c.stage0, frags...)
				}
			}
		}
	}
	return c.snap, nil
}

// add appends fragments within MaxSnapshotBytes; a fragment with a source
// already present gets a numbered suffix so sources stay unique.
func (c *collector) add(frags []Fragment, tool, ref string) {
	used := c.snap.Bytes()
	for _, f := range frags {
		if len(f.Lines) == 0 {
			continue
		}
		f.Source = safeSource(f.Source)
		src := f.Source
		for n := 2; c.snap.Fragment(src) != nil; n++ {
			src = fmt.Sprintf("%s~%d", f.Source, n)
		}
		f.Source = src
		var kept []string
		for _, l := range f.Lines {
			if used+len(l)+1 > MaxSnapshotBytes {
				c.snap.Truncated = true
				c.note(tool, ref, NoteSnapshotCap)
				break
			}
			used += len(l) + 1
			kept = append(kept, l)
		}
		if len(kept) > 0 {
			f.Lines = kept
			c.snap.Fragments = append(c.snap.Fragments, f)
		}
		if c.snap.Truncated {
			return
		}
	}
}

// safeSource keeps a source name printable and free of the characters the
// prompt uses around it.
func safeSource(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '"' || r == '<' || r == '>' || r == '`':
			return '_'
		case r < 0x20 || r == 0x7f:
			return '_'
		}
		return r
	}, s)
}

// capBytes cuts s to at most max bytes at a rune boundary.
func capBytes(s string, max int) (string, bool) {
	if max <= 0 || len(s) <= max {
		return s, false
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}

// --- input derivation ---

func subjectTaskInputs(c *collector, spec ReadSpec) ([]readCall, []ReadNote) {
	if c.taskID == "" {
		return nil, []ReadNote{{Tool: spec.Tool, Code: NoteNoSubject}}
	}
	return []readCall{{ref: c.taskID, base: "clickup:task/" + c.taskID, input: taskInput(c.taskID, readCap(spec))}}, nil
}

type refKind string

const (
	refPull  refKind = "pull"
	refIssue refKind = "issues"
)

// githubRefRe finds GitHub pull request and issue URLs in fetched text.
var githubRefRe = regexp.MustCompile(`https?://github\.com/([A-Za-z0-9][A-Za-z0-9-]{0,38})/([A-Za-z0-9._-]{1,100})/(pull|issues)/([1-9][0-9]{0,8})\b`)

type githubRef struct {
	repo   string // as written in the text
	number string
}

// refsIn returns the distinct references of a kind in order of appearance.
func refsIn(frags []Fragment, kind refKind) []githubRef {
	var out []githubRef
	seen := map[string]bool{}
	for _, f := range frags {
		for _, l := range f.Lines {
			for _, m := range githubRefRe.FindAllStringSubmatch(l, -1) {
				if refKind(m[3]) != kind {
					continue
				}
				repo := strings.TrimSuffix(m[1]+"/"+m[2], ".git")
				repo = strings.TrimRight(repo, ".")
				k := strings.ToLower(repo) + "#" + m[4]
				if seen[k] {
					continue
				}
				seen[k] = true
				out = append(out, githubRef{repo: repo, number: m[4]})
			}
		}
	}
	return out
}

// githubRefInputs reads each reference of a kind found in the subject's
// fragments, only in repos the workspace granted, at most MaxRefsPerKind.
func githubRefInputs(kind refKind) func(c *collector, spec ReadSpec) ([]readCall, []ReadNote) {
	return func(c *collector, spec ReadSpec) ([]readCall, []ReadNote) {
		var calls []readCall
		var notes []ReadNote
		for _, r := range refsIn(c.stage0, kind) {
			ref := r.repo + "#" + r.number
			granted, ok := c.grants[strings.ToLower(r.repo)]
			if !ok {
				notes = append(notes, ReadNote{Tool: spec.Tool, Ref: ref, Code: NoteNotGranted})
				continue
			}
			if len(calls) == MaxRefsPerKind {
				notes = append(notes, ReadNote{Tool: spec.Tool, Ref: ref, Code: NoteRefCap})
				continue
			}
			var n int
			fmt.Sscanf(r.number, "%d", &n)
			input, _ := json.Marshal(map[string]any{"repo": granted, "number": n})
			calls = append(calls, readCall{ref: granted + "#" + r.number, base: "github:" + granted + "#" + r.number, input: input})
		}
		return calls, notes
	}
}

// --- splitting ---

// lines splits fetched text into lines, neutralizing markup the prompt
// uses as delimiters so fetched text cannot close or open a prompt section.
func lines(text string) []string {
	text = strings.ReplaceAll(text, "\r", "")
	if strings.TrimSpace(text) == "" {
		return nil
	}
	out := strings.Split(text, "\n")
	for i, l := range out {
		out[i] = neutralizeDelimiters(l)
	}
	return out
}

var delimiterRe = regexp.MustCompile(`(?i)<(\s*/?\s*)(context|source|instructions|system|output)`)

func neutralizeDelimiters(s string) string { return delimiterRe.ReplaceAllString(s, "‹$1$2") }

// splitTask splits clickup_get_task output: lines before the tool's
// "description:" marker are #meta, the rest #desc.
func splitTask(base, text string) []Fragment {
	ls := lines(text)
	for i, l := range ls {
		if l == "description:" {
			return []Fragment{{Source: base + "#meta", Lines: ls[:i]}, {Source: base + "#desc", Lines: ls[i+1:]}}
		}
	}
	return []Fragment{{Source: base + "#meta", Lines: ls}}
}

// splitHeaderBody splits at the first line equal to marker.
func splitHeaderBody(head, marker, body string) func(base, text string) []Fragment {
	return func(base, text string) []Fragment {
		ls := lines(text)
		for i, l := range ls {
			if l == marker {
				return []Fragment{{Source: base + head, Lines: ls[:i]}, {Source: base + body, Lines: ls[i+1:]}}
			}
		}
		return []Fragment{{Source: base + head, Lines: ls}}
	}
}

func splitWhole(suffix string) func(base, text string) []Fragment {
	return func(base, text string) []Fragment { return []Fragment{{Source: base + suffix, Lines: lines(text)}} }
}

// prFileRe is a github_pr_files file header: "<path> (<status> +a -d)".
var prFileRe = regexp.MustCompile(`^(\S.*) \((added|removed|modified|renamed|copied|changed|unchanged) \+\d+ -\d+\)$`)

// splitPRFiles gives one @files fragment of file headers and one fragment
// per file with its patch lines.
func splitPRFiles(base, text string) []Fragment {
	files := Fragment{Source: base + ":@files"}
	var out []Fragment
	var cur *Fragment
	for _, l := range lines(text) {
		if m := prFileRe.FindStringSubmatch(l); m != nil {
			files.Lines = append(files.Lines, l)
			out = append(out, Fragment{Source: base + ":" + m[1]})
			cur = &out[len(out)-1]
			continue
		}
		if cur == nil {
			files.Lines = append(files.Lines, l)
			continue
		}
		cur.Lines = append(cur.Lines, l)
	}
	return append([]Fragment{files}, out...)
}

// --- rendering ---

// renderContext renders the snapshot for the model: each fragment in a
// source block, each line prefixed with its number.
func renderContext(s ContextSnapshot) string {
	var sb strings.Builder
	for _, f := range s.Fragments {
		fmt.Fprintf(&sb, "<source id=\"%s\">\n", f.Source)
		for i, l := range f.Lines {
			fmt.Fprintf(&sb, "L%d| %s\n", i+1, l)
		}
		sb.WriteString("</source>\n")
	}
	if len(s.Notes) > 0 {
		sb.WriteString("Not read or cut:\n")
		for _, n := range s.Notes {
			if n.Ref != "" {
				fmt.Fprintf(&sb, "- %s %s: %s\n", n.Tool, n.Ref, n.Code)
			} else {
				fmt.Fprintf(&sb, "- %s: %s\n", n.Tool, n.Code)
			}
		}
	}
	return sb.String()
}

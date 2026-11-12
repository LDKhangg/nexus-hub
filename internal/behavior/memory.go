package behavior

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/LDKhangg/nexus-hub/internal/agent/tools"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
)

// This file is decision memory (REQ-23): the recorder that stores the
// owner's decisions, the memory_search read tool, and how a run builds its
// memory query and turns results into citable fragments.
//
// Undone decisions. A decision recorded when the owner undid an auto write
// (SourceAutoUndone) is stored and listed, but memory_search never returns
// it, nor any other decision about the same item: a search result is
// offered to the model as evidence it may cite for an answer, and an undone
// answer must not count as one.

// MemoryRecorder stores decisions in the memory store (DecisionRecorder).
type MemoryRecorder struct{ Store MemoryStore }

var _ DecisionRecorder = MemoryRecorder{}

func (m MemoryRecorder) RecordDecision(ctx context.Context, workspaceID string, d NewDecision) error {
	if m.Store == nil {
		return nil
	}
	_, err := m.Store.AddDecision(ctx, workspaceID, d)
	return err
}

// DecisionSearcher finds decisions that may be reused as evidence
// (storage.BehaviorStore.SearchReusableDecisions).
type DecisionSearcher interface {
	SearchReusableDecisions(ctx context.Context, workspaceID, query string, limit int) ([]RememberedDecision, error)
}

// memory_search limits.
const (
	MemorySearchDefaultLimit = 5
	MemorySearchMaxLimit     = 10
	MemoryQueryMaxRunes      = 500
	memoryFieldRunes         = 1500
)

type memorySearchTool struct{ store DecisionSearcher }

// NewMemorySearchTool returns the memory_search read tool over the
// workspace's decision memory. It reads only the calling workspace's
// decisions (tools.Call.WorkspaceID) and needs no credential.
func NewMemorySearchTool(store DecisionSearcher) tools.Tool { return &memorySearchTool{store: store} }

func (t *memorySearchTool) Spec() tools.ToolSpec {
	return tools.ToolSpec{
		Name: MemorySearchTool,
		Description: "Search the owner's past decisions (answers to earlier questions). Each result is a source " +
			"\"memory:decision/<id>\" with one line per field: topic, question, answer, scope, source, date.",
		InputSchema: json.RawMessage(fmt.Sprintf(`{"type":"object","additionalProperties":false,"properties":{"query":{"type":"string","minLength":1,"maxLength":%d},"limit":{"type":"integer","minimum":1,"maximum":%d}},"required":["query"]}`, MemoryQueryMaxRunes, MemorySearchMaxLimit)),
		Tier:        tenant.TierRead,
	}
}

func (t *memorySearchTool) Run(ctx context.Context, c tools.Call) (tools.ToolOutput, error) {
	var in struct {
		Query string `json:"query"`
		Limit *int   `json:"limit"`
	}
	dec := json.NewDecoder(bytes.NewReader(c.Input))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return tools.ToolOutput{}, &tools.ToolError{Code: "invalid_input"}
	}
	in.Query = strings.TrimSpace(in.Query)
	limit := MemorySearchDefaultLimit
	if in.Limit != nil {
		limit = *in.Limit
	}
	if in.Query == "" || utf8.RuneCountInString(in.Query) > MemoryQueryMaxRunes || limit < 1 || limit > MemorySearchMaxLimit {
		return tools.ToolOutput{}, &tools.ToolError{Code: "invalid_input"}
	}
	if t.store == nil || c.WorkspaceID == "" {
		return tools.ToolOutput{}, &tools.ToolError{Code: "unavailable"}
	}
	list, err := t.store.SearchReusableDecisions(ctx, c.WorkspaceID, in.Query, limit)
	if err != nil {
		if errors.Is(err, ErrInvalid) {
			return tools.ToolOutput{}, &tools.ToolError{Code: "invalid_input"}
		}
		return tools.ToolOutput{}, &tools.ToolError{Code: "server_error", Retryable: true}
	}
	return tools.ToolOutput{Text: RenderDecisions(list), Summary: fmt.Sprintf("%d decisions", len(list))}, nil
}

// memoryHeaderRe is the first line of one decision in memory_search output.
var memoryHeaderRe = regexp.MustCompile(`^decision (memory:decision/[1-9][0-9]{0,18})$`)

// oneLine flattens a field to one line, so no stored text can start a line
// of its own (and fake a decision header).
func oneLine(s string, n int) string { return capRunes(strings.Join(strings.Fields(s), " "), n) }

// RenderDecisions is memory_search's output: per decision a header line
// "decision memory:decision/<id>", then one line per field, then a blank
// line.
func RenderDecisions(list []RememberedDecision) string {
	if len(list) == 0 {
		return "no matching decisions"
	}
	var sb strings.Builder
	for _, d := range list {
		var scope []string
		if d.Scope.Behavior != "" {
			scope = append(scope, "behavior="+oneLine(d.Scope.Behavior, 100))
		}
		if d.Scope.List != "" {
			scope = append(scope, "list="+oneLine(d.Scope.List, 100))
		}
		if d.Scope.Repo != "" {
			scope = append(scope, "repo="+oneLine(d.Scope.Repo, 300))
		}
		fmt.Fprintf(&sb, "decision %s\n", d.Locator())
		fmt.Fprintf(&sb, "topic: %s\n", oneLine(d.Topic, 200))
		fmt.Fprintf(&sb, "question: %s\n", oneLine(d.Question, memoryFieldRunes))
		fmt.Fprintf(&sb, "answer: %s\n", oneLine(d.Answer, memoryFieldRunes))
		fmt.Fprintf(&sb, "scope: %s\n", strings.Join(scope, " "))
		fmt.Fprintf(&sb, "source: %s\n", d.Source)
		fmt.Fprintf(&sb, "date: %s\n\n", d.CreatedAt.UTC().Format("2006-01-02"))
	}
	return sb.String()
}

// splitDecisions turns memory_search output into one fragment per
// decision, with the decision's locator as source and its field lines.
func splitDecisions(_, text string) []Fragment {
	var out []Fragment
	var cur *Fragment
	for _, l := range lines(text) {
		if m := memoryHeaderRe.FindStringSubmatch(l); m != nil {
			out = append(out, Fragment{Source: m[1]})
			cur = &out[len(out)-1]
			continue
		}
		if cur == nil || l == "" {
			continue
		}
		cur.Lines = append(cur.Lines, l)
	}
	return out
}

// memoryInputs derives the memory_search call of a run: a query of the
// subject's title words and the most frequent words of the context read
// so far. Context text only chooses which of the workspace's own past
// decisions are shown; it never chooses a tool, class or target.
func memoryInputs(c *collector, spec ReadSpec) ([]readCall, []ReadNote) {
	q := memoryQuery(c.snap.Fragments)
	if q == "" {
		return nil, []ReadNote{{Tool: spec.Tool, Code: NoteNoSubject}}
	}
	input, _ := json.Marshal(map[string]any{"query": q, "limit": MemorySearchDefaultLimit})
	return []readCall{{ref: "", base: "memory", input: input}}, nil
}

// memoryStopwords are frequent words that carry no topic.
var memoryStopwords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "that": true, "this": true, "from": true, "are": true,
	"was": true, "were": true, "have": true, "has": true, "not": true, "but": true, "you": true, "your": true,
	"our": true, "will": true, "can": true, "should": true, "would": true, "could": true, "into": true, "onto": true,
	"about": true, "there": true, "their": true, "they": true, "them": true, "then": true, "than": true, "what": true,
	"when": true, "which": true, "who": true, "how": true, "all": true, "any": true, "each": true, "also": true,
	"none": true, "null": true, "true": true, "false": true, "http": true, "https": true, "www": true,
	"của": true, "các": true, "những": true, "được": true, "cho": true, "với": true, "này": true, "khi": true,
	"không": true, "một": true, "trong": true, "thì": true, "có": true, "là": true, "và": true, "để": true,
	"đã": true, "sẽ": true, "nếu": true, "theo": true, "như": true, "từ": true, "đến": true, "vào": true,
	"cần": true, "phải": true, "nên": true, "cũng": true, "hay": true, "hoặc": true, "thêm": true,
}

// identifierLine marks lines that carry identifiers or tool metadata only.
var identifierLine = regexp.MustCompile(`^(list_id|assignee_ids|status|assignees|tags|https?):?`)

func memoryWords(line string) []string {
	var out []string
	for _, w := range strings.FieldsFunc(strings.ToLower(line), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		if utf8.RuneCountInString(w) < 3 || utf8.RuneCountInString(w) > 40 || memoryStopwords[w] {
			continue
		}
		if _, err := strconv.ParseFloat(w, 64); err == nil {
			continue
		}
		out = append(out, w)
	}
	return out
}

// memoryQuery builds the memory query from fragments: the words of each
// "#meta" fragment's title line (the first line that is not an identifier
// line) first, then the most frequent other words, up to 16 words.
func memoryQuery(frags []Fragment) string {
	const maxWords = 16
	var words []string
	seen := map[string]bool{}
	add := func(w string) {
		if !seen[w] && len(words) < maxWords {
			seen[w] = true
			words = append(words, w)
		}
	}
	count := map[string]int{}
	first := map[string]int{}
	pos := 0
	for _, f := range frags {
		title := strings.HasSuffix(f.Source, "#meta")
		for _, l := range f.Lines {
			if identifierLine.MatchString(strings.TrimSpace(l)) {
				continue
			}
			if title {
				// "<name> (<id>)": drop the trailing identifier.
				if i := strings.LastIndex(l, " ("); i > 0 && strings.HasSuffix(l, ")") {
					l = l[:i]
				}
				for _, w := range memoryWords(l) {
					add(w)
				}
				title = false
				continue
			}
			for _, w := range memoryWords(l) {
				if _, ok := first[w]; !ok {
					first[w] = pos
					pos++
				}
				count[w]++
			}
		}
	}
	rest := make([]string, 0, len(count))
	for w := range count {
		rest = append(rest, w)
	}
	sort.Slice(rest, func(i, j int) bool {
		if count[rest[i]] != count[rest[j]] {
			return count[rest[i]] > count[rest[j]]
		}
		return first[rest[i]] < first[rest[j]]
	})
	for _, w := range rest {
		add(w)
	}
	return capRunes(strings.Join(words, " "), MemoryQueryMaxRunes)
}

// contextRepos returns the GitHub repositories the run read, from the
// snapshot's sources, sorted.
func contextRepos(snap ContextSnapshot) []string {
	seen := map[string]bool{}
	var out []string
	for _, f := range snap.Fragments {
		rest, ok := strings.CutPrefix(f.Source, "github:")
		if !ok {
			continue
		}
		if i := strings.IndexByte(rest, '#'); i > 0 {
			rest = rest[:i]
		}
		if !seen[rest] {
			seen[rest] = true
			out = append(out, rest)
		}
	}
	sort.Strings(out)
	return out
}

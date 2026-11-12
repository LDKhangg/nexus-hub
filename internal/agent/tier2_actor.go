package agent

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/LDKhangg/nexus-hub/internal/agent/tools"
	"github.com/LDKhangg/nexus-hub/internal/domain"
	"github.com/LDKhangg/nexus-hub/internal/memory"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
)

// Tier-2 limits.
const (
	MaxActorSteps       = 5
	ActorMaxTokens      = 1024
	ReportMaxRunes      = 1500
	ReportMaxBullets    = 8
	KeepFullResultSteps = 2
)

// Stop reasons.
const (
	StopStepLimit   = "step_limit"
	StopMaxTokens   = "max_tokens"
	StopTokenBudget = "token_budget"
)

// TokenBudget is the per-job token budget; Used carries spend from earlier
// attempts of the same job.
type TokenBudget struct {
	MaxInput, MaxOutput int64
	Used                domain.Usage
}

func (b TokenBudget) exhausted() bool {
	in := b.Used.Input + b.Used.CacheRead + b.Used.CacheWrite
	return (b.MaxInput > 0 && in >= b.MaxInput) || (b.MaxOutput > 0 && b.Used.Output >= b.MaxOutput)
}

// CallRecord summarizes one tool call for the report.
type CallRecord struct {
	Step    int
	Name    string
	Status  string
	Tier    tenant.Tier
	IsError bool
}

// ActorReport is the outcome of an investigation. Usage covers this
// attempt only.
type ActorReport struct {
	Text    string
	Steps   int
	Calls   []CallRecord
	Stopped string
	Usage   domain.Usage
}

// Tier2Actor runs the bounded ReAct tool loop.
type Tier2Actor struct {
	llm   domain.ToolCallingProvider
	model string
}

func NewTier2Actor(llm domain.ToolCallingProvider, model string) *Tier2Actor {
	return &Tier2Actor{llm: llm, model: model}
}

type resultMeta struct {
	turn    int
	step    int
	entries []struct {
		name, status string
		bytes        int
	}
}

// Investigate runs up to MaxActorSteps model steps. Every tool call goes
// through exec; denials and errors are fed back as is_error results.
func (a *Tier2Actor) Investigate(ctx context.Context, packet memory.ContextPacket, d TriageDecision, persona string, specs []tools.ToolSpec, exec *tools.Executor, budget TokenBudget) (ActorReport, error) {
	var report ActorReport
	defs := make([]domain.ToolDef, 0, len(specs))
	for _, s := range specs {
		defs = append(defs, domain.ToolDef{Name: s.Name, Description: s.Description, InputSchema: s.InputSchema})
	}
	turns := []domain.Turn{{Role: domain.UserRole, Text: packet.Render() + "\nGoal: " + d.Goal}}
	var metas []resultMeta
	lastText := ""

	for step := 1; step <= MaxActorSteps; step++ {
		if budget.exhausted() {
			report.Stopped = StopTokenBudget
			break
		}
		compactResults(turns, metas, step)
		resp, err := a.llm.GenerateWithTools(ctx, &domain.ToolRequest{
			Model: a.model, System: actorSystem(persona), MaxTokens: ActorMaxTokens,
			Tools: defs, Turns: turns, CacheStatic: true,
		})
		if err != nil {
			if ctx.Err() != nil {
				return report, ctx.Err()
			}
			return report, &TriageError{Code: CodeLLMError, Retryable: true}
		}
		report.Steps = step
		report.Usage.Add(resp.Usage)
		budget.Used.Add(resp.Usage)
		if t := strings.TrimSpace(resp.Text); t != "" {
			lastText = t
		}
		switch resp.Stop {
		case domain.StopRefusal:
			return report, &TriageError{Code: CodeLLMRefused}
		case domain.StopMaxTokens:
			report.Stopped = StopMaxTokens
		case domain.StopToolUse:
			if len(resp.Calls) == 0 {
				report.Text = clipReport(lastText)
				return report, nil
			}
			if step == MaxActorSteps {
				// Results of calls made now would never be read.
				report.Stopped = StopStepLimit
				break
			}
			turns = append(turns, domain.Turn{Role: domain.AIRole, Text: resp.Text, Calls: resp.Calls})
			meta := resultMeta{turn: len(turns), step: step}
			results := make([]domain.ToolResult, 0, len(resp.Calls))
			for _, c := range resp.Calls {
				r := exec.Call(ctx, step, c.Name, c.Input)
				results = append(results, domain.ToolResult{CallID: c.ID, Content: r.Content, IsError: r.IsError})
				// The report names the tool as audited, never an unregistered
				// name the LLM made up.
				name := c.Name
				if r.Tool != "" {
					name = r.Tool
				}
				report.Calls = append(report.Calls, CallRecord{Step: step, Name: name, Status: r.Status, Tier: r.Tier, IsError: r.IsError})
				meta.entries = append(meta.entries, struct {
					name, status string
					bytes        int
				}{c.Name, r.Status, len(r.Content)})
			}
			turns = append(turns, domain.Turn{Role: domain.UserRole, Results: results})
			metas = append(metas, meta)
			continue
		default:
			report.Text = clipReport(lastText)
			return report, nil
		}
		break
	}
	if report.Stopped == "" {
		report.Stopped = StopStepLimit
	}
	text := lastText
	if text == "" {
		text = fmt.Sprintf("Investigation stopped (%s) after %d tool call(s) without a final report.", report.Stopped, len(report.Calls))
	}
	report.Text = clipReport(text)
	return report, nil
}

// compactResults elides tool_result contents from steps older than the
// last KeepFullResultSteps, keeping each result paired with its call.
func compactResults(turns []domain.Turn, metas []resultMeta, step int) {
	for _, m := range metas {
		if m.step > step-KeepFullResultSteps-1 {
			continue
		}
		t := &turns[m.turn]
		for i := range t.Results {
			if strings.HasPrefix(t.Results[i].Content, "[elided:") || i >= len(m.entries) {
				continue
			}
			e := m.entries[i]
			t.Results[i].Content = fmt.Sprintf("[elided: %s %s, %d bytes]", e.name, e.status, e.bytes)
		}
	}
}

// clipReport keeps at most ReportMaxBullets bullet lines and
// ReportMaxRunes runes.
func clipReport(s string) string {
	var out []string
	bullets := 0
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "-") || strings.HasPrefix(trim, "*") || strings.HasPrefix(trim, "•") {
			bullets++
			if bullets > ReportMaxBullets {
				continue
			}
		}
		out = append(out, line)
	}
	s = strings.Join(out, "\n")
	if utf8.RuneCountInString(s) > ReportMaxRunes {
		marker := memory.TruncatedMarker
		r := []rune(s)
		s = strings.TrimRight(string(r[:ReportMaxRunes-utf8.RuneCountInString(marker)]), " \n") + marker
	}
	return s
}

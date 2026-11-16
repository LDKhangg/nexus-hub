package behavior

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/domain"
)

// Phase 6a Fix Brief #1 regressions: R-1 follow-up escalation, R-2 router
// without CacheHistory, R-3 separate router usage.

// p6aSeed puts one earlier exchange into the job's history.
func p6aSeed(e *p4Env, question, answer string) {
	hist, _ := e.assistant.chatHistoryFor(p4WS, "bot-a", 77)
	hist.add(question, answer)
}

// p6aSeedSay seeds the history that e.say uses (the env's integration).
func p6aSeedSay(e *p4Env) {
	hist, _ := e.assistant.chatHistoryFor(p4WS, e.update(77, UpdateText, "", "").IntegrationID, 77)
	hist.add("BIMAT-Q", "Làm không?")
}

// TC-6A60 (REQ-7, fix R-1): when the newest assistant message ends with a
// question, the owner's reply skips the router and runs the tool loop on
// assistant_model with route work_followup; otherwise the router runs.
func TestPhase6aFix1_FollowupToAssistantQuestion(t *testing.T) {
	for _, tc := range []struct {
		name, answer string
		followup     bool
	}{
		{"ascii question", "Bạn có muốn mình tạo task không?", true},
		{"fullwidth question", "Bạn muốn tạo task không？", true},
		{"question then blank lines", "Mình tạo nhé?\n\n  \n\t", true},
		{"question with trailing spaces", "Tạo không?   ", true},
		{"statement", "Mình đã xem xong.", false},
		{"question on an earlier line", "Bạn muốn không?\nMình đã xem xong.", false},
		{"question mark inside", "Câu hỏi? Không, xong rồi.", false},
		{"empty answer", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &p6aModel{json: []p6aJSONStep{{resp: p6aRouter(`{"route":"answer","reply":"FAST-REPLY","tier":"none"}`)}}}
			e := p6aEnv(m)
			p6aSeed(e, "câu trước", tc.answer)
			res, err := e.w.converse(context.Background(), p6aJob("ok", 0, ""))
			if err != nil {
				t.Fatal(err)
			}
			if tc.followup {
				if len(m.jsonReqs) != 0 || len(m.toolReqs) != 1 || m.toolReqs[0].Model != p6aWork || res.route != routeWorkFollowup || res.model != p6aWork || res.routerModel != "" {
					t.Fatalf("json=%d tools=%d route=%q model=%q router=%q", len(m.jsonReqs), len(m.toolReqs), res.route, res.model, res.routerModel)
				}
				return
			}
			if len(m.jsonReqs) != 1 || res.route != routeAnswer || len(m.toolReqs) != 0 {
				t.Fatalf("router did not run: json=%d route=%q", len(m.jsonReqs), res.route)
			}
		})
	}
}

// TC-6A61 (REQ-7, fix R-1): only the newest assistant message counts, and an
// owner question mark does not trigger the follow-up rule.
func TestPhase6aFix1_FollowupUsesNewestAssistantMessage(t *testing.T) {
	m := &p6aModel{}
	e := p6aEnv(m)
	p6aSeed(e, "q1", "Muốn tạo không?")
	p6aSeed(e, "có phải không?", "Đã tạo xong.")
	res, err := e.w.converse(context.Background(), p6aJob("ok", 0, ""))
	if err != nil || len(m.jsonReqs) != 1 || res.route != routeWork {
		t.Fatalf("route=%q json=%d err=%v", res.route, len(m.jsonReqs), err)
	}
}

// TC-6A62 (REQ-7, fix R-1): the follow-up rule applies only where the
// router would run: WF target and CLI provider keep skip, /deep keeps deep,
// an unset fast model keeps skip.
func TestPhase6aFix1_FollowupKeepsOtherLabels(t *testing.T) {
	const asked = "Bạn có muốn mình tạo task không?"
	ctx := context.Background()

	m := &p6aModel{}
	e := p6aEnv(m)
	p6aSeed(e, "q", asked)
	if res, _ := e.w.converse(ctx, p6aJob("ok", 101, "")); res.route != routeSkip || res.model != p6aWork {
		t.Fatalf("workflow: route=%q model=%q", res.route, res.model)
	}
	if res, _ := e.w.converse(ctx, p6aJob("ok", 0, assistantTierDeep)); res.route != routeDeep || res.model != p6aDeep {
		t.Fatalf("deep: route=%q model=%q", res.route, res.model)
	}
	if len(m.jsonReqs) != 0 {
		t.Fatalf("router calls=%d", len(m.jsonReqs))
	}

	nf := p6aEnv(&p6aModel{})
	nf.assistant.FastModel = ""
	p6aSeed(nf, "q", asked)
	if res, _ := nf.w.converse(ctx, p6aJob("ok", 0, "")); res.route != routeSkip {
		t.Fatalf("no fast model: route=%q", res.route)
	}

	pe := p6aEnv(nil)
	pe.llm.replies = []string{"trả lời"}
	pe.assistant.Models = func(context.Context, string) (domain.LLMProvider, error) { return pe.llm, nil }
	p6aSeed(pe, "q", asked)
	if res, _ := pe.w.converse(ctx, p6aJob("ok", 0, "")); res.route != routeSkip || pe.llm.reqs[0].Model != p6aWork {
		t.Fatalf("plain provider: route=%q", res.route)
	}
}

// TC-6A63 (REQ-7, REQ-4, REQ-5, fix R-1): the full journey: the assistant
// offers, the owner says "ok", the tools run on Sonnet without a router
// call; the log says work_followup with empty router fields.
func TestPhase6aFix1_FollowupJourney(t *testing.T) {
	logs := p6aCaptureLog(t)
	m := &p6aModel{tools: []*domain.ToolResponse{
		{Text: "Task 86xyz đang chờ review. Bạn có muốn mình nhắc người review không?", Stop: domain.StopEndTurn, Usage: p6aToolUsage},
		{Text: "Đã gửi đề xuất.", Stop: domain.StopEndTurn, Usage: p6aToolUsage},
	}}
	e := p6aEnv(m)
	e.say(t, "task 86xyz đang thế nào?")
	if out := e.say(t, "ok"); out.code != "assistant_answer" {
		t.Fatalf("outcome=%+v", out)
	}
	if len(m.jsonReqs) != 1 || len(m.toolReqs) != 2 || m.toolReqs[1].Model != p6aWork {
		t.Fatalf("json=%d tools=%d", len(m.jsonReqs), len(m.toolReqs))
	}
	if got := e.msg.texts(); got[len(got)-1] != "Đã gửi đề xuất.\n· sonnet" {
		t.Fatalf("sent=%q", got)
	}
	line := logs.requireTurn(t, 2, routeWorkFollowup, p6aWork)
	if line["router_model"] != "" || line["router_input_tokens"] != float64(0) || line["input_tokens"] != float64(100) || line["steps"] != float64(1) {
		t.Fatalf("usage line=%v", line)
	}
}

// TC-6A64 (REQ-6, fix R-2): the router request does not ask for history
// caching; the tool loop and the plain path still do.
func TestPhase6aFix1_RouterWithoutCacheHistory(t *testing.T) {
	m := &p6aModel{}
	e := p6aEnv(m)
	e.say(t, "task?")
	if len(m.jsonReqs) != 1 || m.jsonReqs[0].CacheHistory {
		t.Fatalf("router CacheHistory=%v", m.jsonReqs[0].CacheHistory)
	}
	if len(m.toolReqs) != 1 || !m.toolReqs[0].CacheHistory || !m.toolReqs[0].CacheStatic {
		t.Fatalf("tool loop cache flags=%+v", m.toolReqs)
	}
	pe := p6aEnv(nil)
	pe.assistant.Models = func(context.Context, string) (domain.LLMProvider, error) { return pe.llm, nil }
	pe.say(t, "chào")
	if !pe.llm.reqs[0].CacheHistory {
		t.Fatal("plain path lost CacheHistory")
	}
}

// TC-6A65 (REQ-4, fix R-3): router_* fields carry the router call on work,
// deep and work_fallback (router_model set even with zero usage); turn
// token fields exclude it; answer, skip and follow-up turns have empty
// router fields; no text in the log.
func TestPhase6aFix1_RouterUsageFields(t *testing.T) {
	type want struct {
		route, model, routerModel string
		in, rin                   float64
	}
	cases := map[string]struct {
		step  p6aJSONStep
		setup func(e *p4Env)
		want  want
	}{
		"work":     {step: p6aJSONStep{resp: p6aRouter(`{"route":"escalate","reply":"","tier":"work"}`)}, want: want{routeWork, p6aWork, p6aFast, 100, 10}},
		"deep":     {step: p6aJSONStep{resp: p6aRouter(`{"route":"escalate","reply":"","tier":"deep"}`)}, want: want{routeDeep, p6aDeep, p6aFast, 100, 10}},
		"error":    {step: p6aJSONStep{err: errors.New("BIMAT-ERR")}, want: want{routeWorkFallback, p6aWork, p6aFast, 100, 0}},
		"refusal":  {step: p6aJSONStep{resp: &domain.JSONResponse{Stop: domain.StopRefusal, Usage: domain.Usage{Input: 7}}}, want: want{routeWorkFallback, p6aWork, p6aFast, 100, 7}},
		"answer":   {step: p6aJSONStep{resp: p6aRouter(`{"route":"answer","reply":"BIMAT-REPLY","tier":"none"}`)}, want: want{routeAnswer, p6aFast, "", 10, 0}},
		"skip":     {setup: func(e *p4Env) { e.assistant.FastModel = "" }, want: want{routeSkip, p6aWork, "", 100, 0}},
		"followup": {setup: p6aSeedSay, want: want{routeWorkFollowup, p6aWork, "", 100, 0}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			logs := p6aCaptureLog(t)
			m := &p6aModel{}
			if tc.step.resp != nil || tc.step.err != nil {
				m.json = []p6aJSONStep{tc.step}
			}
			e := p6aEnv(m)
			if tc.setup != nil {
				tc.setup(e)
			}
			e.say(t, "BIMAT-OWNER task?")
			line := logs.requireTurn(t, 1, tc.want.route, tc.want.model)
			if line["router_model"] != tc.want.routerModel || line["input_tokens"] != tc.want.in || line["router_input_tokens"] != tc.want.rin {
				t.Fatalf("usage line=%v", line)
			}
			if tc.want.routerModel == "" {
				for _, k := range []string{"router_input_tokens", "router_output_tokens", "router_cache_read_tokens", "router_cache_write_tokens"} {
					if line[k] != float64(0) {
						t.Fatalf("%s=%v on a turn without router line", k, line[k])
					}
				}
			}
			wantErr := map[string]string{"error": "provider_error", "refusal": "stop_refusal"}[name]
			if line["router_error"] != wantErr {
				t.Fatalf("router_error=%v, want %q", line["router_error"], wantErr)
			}
			if strings.Contains(logs.String(), "BIMAT") {
				t.Fatal("text in the log")
			}
		})
	}
}

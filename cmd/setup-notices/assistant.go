package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/llm"
	"github.com/LDKhangg/nexus-hub/internal/adapters/netguard"
	"github.com/LDKhangg/nexus-hub/internal/adapters/storage"
	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/LDKhangg/nexus-hub/internal/domain"
	"github.com/LDKhangg/nexus-hub/internal/settings"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
	"github.com/jackc/pgx/v5/pgxpool"
)

const assistantUsage = `usage: assistant route-eval --workspace-id <uuid> [--file cases.txt] [--thread-history general|task]
  route-eval  run the assistant router prompt on a fixed set of owner messages
              (the built-in set, or --file: one "<expected>|<message>" per line,
              expected is answer, work, deep or escalate; # starts a comment)
              and print, per case, the expected and got route, tier,
              router_error and token counts. The reply text is never printed.
              Exit status is not 0 when a case does not match.`

// routeEvalCase is one owner message with the route it should get.
type routeEvalCase struct{ expected, text string }

// defaultRouteEvalCases covers greetings, thanks, general questions,
// task/status questions, action requests, follow-ups and design requests.
var defaultRouteEvalCases = []routeEvalCase{
	{"answer", "hi em"},
	{"answer", "chào buổi sáng"},
	{"answer", "cảm ơn nhé"},
	{"answer", "ok, hiểu rồi"},
	{"answer", "HTTP 429 nghĩa là gì?"},
	{"answer", "what is the difference between a mutex and a semaphore?"},
	{"work", "task 86c1x2 đang ở trạng thái nào?"},
	{"work", "hôm nay có PR nào đang chờ approve không?"},
	{"work", "WF-12 chạy tới đâu rồi?"},
	{"work", "tạo giúp anh một comment trên task đó nói là đã xong"},
	{"work", "hủy workflow đang chạy đi"},
	{"work", "nhớ giúp anh là báo cáo dạng ngắn"},
	{"work", "ừ làm đi"},
	{"deep", "hãy thiết kế kiến trúc tách service thanh toán khỏi monolith: các bước migrate, rủi ro, rollback và lộ trình theo tuần"},
	{"deep", "review kỹ thiết kế này giúp anh: hàng đợi job dùng Postgres SKIP LOCKED, phân tích race condition, retry và ordering"},
}

// assistantCommand runs `assistant route-eval`.
func assistantCommand(ctx context.Context, pool *pgxpool.Pool, args []string, out io.Writer, lookup func(string) (string, bool)) error {
	if len(args) == 0 || args[0] != "route-eval" {
		return errors.New(assistantUsage)
	}
	f := commandFlags("assistant route-eval")
	ws := f.String("workspace-id", "", "workspace UUID")
	file := f.String("file", "", "cases file")
	hist := f.String("thread-history", "", "synthetic history: general or task")
	if f.Parse(args[1:]) != nil || f.NArg() != 0 || !tenant.ValidUUID(*ws) {
		return errors.New(assistantUsage)
	}
	history, ok := behavior.RouteEvalHistory(*hist)
	if !ok {
		return errors.New(assistantUsage)
	}
	cases := defaultRouteEvalCases
	if *file != "" {
		var err error
		if cases, err = readRouteEvalCases(*file); err != nil {
			return err
		}
	}
	if len(cases) == 0 {
		return errors.New("route-eval: no cases")
	}
	store, err := storage.NewSettingsStore(pool)
	if err != nil {
		return err
	}
	s, err := settings.Load(ctx, store)
	if err != nil {
		return err
	}
	var resolver llm.SecretResolver
	if s.LLMProvider == settings.LLMProviderAnthropic {
		r, _, err := openResolver(pool, lookup)
		if err != nil {
			return err
		}
		resolver = r
	}
	factory, err := llm.NewFactory(s, resolver, netguard.Client(0, netguard.DefaultHosts), "")
	if err != nil {
		return err
	}
	// The setup context has a short fixed deadline; each router call has its
	// own 20 s limit.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Duration(len(cases))*30*time.Second+llmCheckSlack)
	defer cancel()
	client, err := factory.Client(rctx, *ws)
	if err != nil {
		return errors.New("route-eval: LLM provider not usable (" + llm.ErrorCode(err) + ")")
	}
	jp, ok := client.Default().(domain.JSONProvider)
	if !ok {
		return errors.New("route-eval: the provider has no structured calls (use llm_provider anthropic)")
	}
	enc := json.NewEncoder(out)
	mismatches, errorsSeen := 0, 0
	var inTok, outTok int64
	for i, c := range cases {
		res := behavior.EvaluateRoute(rctx, jp, s.AssistantFastModel, history, c.text)
		match := routeMatches(c.expected, res)
		if !match {
			mismatches++
		}
		if res.RouterError != "" {
			errorsSeen++
		}
		inTok += res.Usage.Input
		outTok += res.Usage.Output
		if err := enc.Encode(map[string]any{
			"case": i + 1, "expected": c.expected, "route": res.Route, "tier": res.Tier, "router_error": res.RouterError,
			"input_tokens": res.Usage.Input, "output_tokens": res.Usage.Output, "ok": match,
		}); err != nil {
			return err
		}
	}
	if err := enc.Encode(map[string]any{
		"summary": true, "cases": len(cases), "mismatches": mismatches, "router_errors": errorsSeen,
		"model": s.AssistantFastModel, "history": strings.TrimSpace(*hist), "input_tokens": inTok, "output_tokens": outTok,
	}); err != nil {
		return err
	}
	if mismatches > 0 {
		return fmt.Errorf("route-eval: %d of %d cases did not match", mismatches, len(cases))
	}
	return nil
}

// routeMatches compares the expectation with the result; a router failure
// never matches, so it is visible.
func routeMatches(expected string, res behavior.RouteEvalResult) bool {
	if res.RouterError != "" {
		return false
	}
	if expected == "escalate" {
		return res.Route == "work" || res.Route == "deep"
	}
	return res.Route == expected
}

// readRouteEvalCases reads "<expected>|<message>" lines.
func readRouteEvalCases(path string) ([]routeEvalCase, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, errors.New("route-eval: cases file not readable")
	}
	defer fh.Close()
	var cases []routeEvalCase
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 0, 64*1024), 64*1024)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		exp, text, found := strings.Cut(line, "|")
		exp, text = strings.ToLower(strings.TrimSpace(exp)), strings.TrimSpace(text)
		switch exp {
		case "answer", "work", "deep", "escalate":
		default:
			return nil, fmt.Errorf("route-eval: line %d: expected must be answer, work, deep or escalate", n)
		}
		if !found || text == "" || len(text) > 2000 {
			return nil, fmt.Errorf("route-eval: line %d: needs <expected>|<message> (message up to 2000 bytes)", n)
		}
		cases = append(cases, routeEvalCase{expected: exp, text: text})
		if len(cases) > 200 {
			return nil, errors.New("route-eval: at most 200 cases")
		}
	}
	if sc.Err() != nil {
		return nil, errors.New("route-eval: cases file not readable")
	}
	return cases, nil
}

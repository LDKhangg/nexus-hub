package behavior

import (
	"context"

	"github.com/LDKhangg/nexus-hub/internal/domain"
)

// RouteEvalResult is the outcome of one router call for an offline check.
// It never carries the router's reply text.
type RouteEvalResult struct {
	// Route is "answer", "work", "deep", or "work_fallback" when the router
	// failed and the turn would escalate to the work model.
	Route string
	// Tier is "none" for an answer, else "work" or "deep".
	Tier        string
	RouterError string // fixed code, "" when the router decided
	Usage       domain.Usage
}

// EvaluateRoute runs the assistant router prompt once on text with an
// optional earlier history, on the fast model, exactly as a Telegram turn
// would. It changes no prompt and stores nothing.
func EvaluateRoute(ctx context.Context, p domain.JSONProvider, fastModel string, history []domain.Message, text string) RouteEvalResult {
	a := &Assistant{FastModel: fastModel}
	d, usage, err := a.route(ctx, p, history, text)
	if err != nil {
		return RouteEvalResult{Route: routeWorkFallback, Tier: "work", RouterError: routerErrorCode(err), Usage: usage}
	}
	tier := "none"
	switch d.route {
	case routeWork:
		tier = "work"
	case routeDeep:
		tier = "deep"
	}
	return RouteEvalResult{Route: d.route, Tier: tier, Usage: usage}
}

// RouteEvalHistory returns a small synthetic history for the route check:
// "general" is a finished greeting exchange of the General conversation;
// "task" is an exchange about one task, as a task topic would hold.
func RouteEvalHistory(kind string) ([]domain.Message, bool) {
	switch kind {
	case "", "none":
		return nil, true
	case "general":
		return []domain.Message{
			{Role: domain.UserRole, Text: "chào em"},
			{Role: domain.AIRole, Text: "Chào anh, em có thể giúp gì ạ."},
		}, true
	case "task":
		return []domain.Message{
			{Role: domain.UserRole, Text: "[Context] This message is in the topic of ClickUp task 86c1x2.\ntình hình task này sao rồi"},
			{Role: domain.AIRole, Text: "Task đang ở trạng thái đang làm, chưa có workflow nào đang chạy."},
		}, true
	}
	return nil, false
}

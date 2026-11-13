// Package reporting defines report preference and template contracts.
package reporting

import "context"

// Style controls the amount of detail in a report.
type Style string

const (
	StyleShort    Style = "short"
	StyleNormal   Style = "normal"
	StyleDetailed Style = "detailed"
)

// Kind identifies an owner-customizable report template.
type Kind string

const (
	KindPRApprove          Kind = "pr_approve"
	KindPRSkipped          Kind = "pr_skipped"
	KindVerdictUnavailable Kind = "verdict_unavailable"
	KindMention            Kind = "mention"
	KindNotify             Kind = "notify"
	KindFYI                Kind = "fyi"
	KindDigest             Kind = "digest"
	KindFailure            Kind = "failure"
)

// Prefs controls report detail, optional sections and language. An empty
// Language means that the user's profile language applies.
type Prefs struct {
	Style         Style
	ShowReaction  bool
	ShowAISummary bool
	ShowLinks     bool
	ShowPRTitles  bool
	ShowTraceHint bool
	Language      string
}

// DefaultOwnerPrefs returns the preferences used when the owner has no stored
// row: short reports with every toggle on except reactions.
func DefaultOwnerPrefs() Prefs {
	return Prefs{
		Style:         StyleShort,
		ShowAISummary: true,
		ShowLinks:     true,
		ShowPRTitles:  true,
		ShowTraceHint: true,
	}
}

// Store resolves report preferences and optional templates.
type Store interface {
	// Prefs resolves a behavior override before the workspace default.
	Prefs(ctx context.Context, userID, workspaceID, behavior string) (Prefs, error)
	// Template resolves a behavior override before the workspace default.
	Template(ctx context.Context, userID, workspaceID, behavior string, kind Kind) (body string, ok bool, err error)
}

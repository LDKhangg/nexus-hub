package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// ErrInvalidDecision wraps every Tier-1 parse or validation failure.
var ErrInvalidDecision = errors.New("invalid triage decision")

const (
	maxSummaryRunes = 200
	maxGoalRunes    = 300
)

type rawDecision struct {
	Category   string   `json:"category"`
	Urgency    string   `json:"urgency"`
	Confidence *float64 `json:"confidence"`
	Summary    string   `json:"summary"`
	Goal       string   `json:"goal"`
}

// ParseTriageDecision extracts the first JSON object from model output
// (plain, fenced or surrounded by prose) and validates it.
func ParseTriageDecision(s string) (TriageDecision, error) {
	raw, err := firstObject(s)
	if err != nil {
		return TriageDecision{}, err
	}
	d := TriageDecision{
		Category: Category(raw.Category),
		Urgency:  Urgency(raw.Urgency),
		Summary:  strings.TrimSpace(raw.Summary),
		Goal:     strings.TrimSpace(raw.Goal),
	}
	switch d.Category {
	case CategoryIgnore, CategoryNotify, CategoryInvestigate:
	default:
		return TriageDecision{}, fmt.Errorf("%w: unknown category", ErrInvalidDecision)
	}
	switch d.Urgency {
	case "":
		d.Urgency = UrgencyNormal
	case UrgencyLow, UrgencyNormal, UrgencyHigh:
	default:
		return TriageDecision{}, fmt.Errorf("%w: unknown urgency", ErrInvalidDecision)
	}
	if raw.Confidence == nil || *raw.Confidence < 0 || *raw.Confidence > 1 {
		return TriageDecision{}, fmt.Errorf("%w: confidence out of range", ErrInvalidDecision)
	}
	d.Confidence = *raw.Confidence
	if utf8.RuneCountInString(d.Summary) > maxSummaryRunes {
		return TriageDecision{}, fmt.Errorf("%w: summary too long", ErrInvalidDecision)
	}
	if utf8.RuneCountInString(d.Goal) > maxGoalRunes {
		return TriageDecision{}, fmt.Errorf("%w: goal too long", ErrInvalidDecision)
	}
	if d.Category == CategoryInvestigate && d.Goal == "" {
		return TriageDecision{}, fmt.Errorf("%w: investigate requires goal", ErrInvalidDecision)
	}
	return d, nil
}

// firstObject decodes the first well-formed JSON object found in s.
func firstObject(s string) (rawDecision, error) {
	for i := 0; i < len(s); i++ {
		if s[i] != '{' {
			continue
		}
		var raw rawDecision
		dec := json.NewDecoder(strings.NewReader(s[i:]))
		if err := dec.Decode(&raw); err == nil {
			return raw, nil
		}
	}
	return rawDecision{}, fmt.Errorf("%w: no JSON object", ErrInvalidDecision)
}

// RoutingPolicy configures Route.
type RoutingPolicy struct {
	Threshold      float64
	ToolsAvailable bool
}

// Route maps a decision to an outcome. Ignore requires confidence at or
// above the threshold; Investigate requires a usable tool; everything else
// is Notify so no comment is silently dropped.
func Route(d TriageDecision, p RoutingPolicy) Outcome {
	switch d.Category {
	case CategoryIgnore:
		if d.Confidence >= p.Threshold {
			return OutcomeIgnore
		}
	case CategoryInvestigate:
		if p.ToolsAvailable {
			return OutcomeInvestigate
		}
	}
	return OutcomeNotify
}

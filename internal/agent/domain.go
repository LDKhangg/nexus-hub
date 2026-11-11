package agent

// Category is the Tier-1 triage category returned by the router model.
type Category string

const (
	CategoryIgnore      Category = "ignore"
	CategoryNotify      Category = "notify"
	CategoryInvestigate Category = "investigate"
)

// Urgency is the Tier-1 urgency estimate.
type Urgency string

const (
	UrgencyLow    Urgency = "low"
	UrgencyNormal Urgency = "normal"
	UrgencyHigh   Urgency = "high"
)

// TriageDecision is the validated Tier-1 output.
type TriageDecision struct {
	Category   Category
	Urgency    Urgency
	Confidence float64
	Summary    string
	Goal       string
}

// Outcome is what the orchestrator does with a triage decision.
type Outcome int

const (
	OutcomeIgnore Outcome = iota + 1
	OutcomeNotify
	OutcomeInvestigate
)

func (o Outcome) String() string {
	switch o {
	case OutcomeIgnore:
		return "ignore"
	case OutcomeNotify:
		return "notify"
	case OutcomeInvestigate:
		return "investigate"
	default:
		return "unknown"
	}
}

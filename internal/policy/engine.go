package policy

import (
	"context"
	"errors"
	"regexp"
	"time"
)

// Actor kinds of a decision (closed set; the DB CHECK matches).
const (
	ActorAgent    = "agent"
	ActorBehavior = "behavior"
	ActorWorkflow = "workflow"
	ActorDraft    = "draft"
	ActorSystem   = "system"
)

func validActor(k string) bool {
	switch k {
	case ActorAgent, ActorBehavior, ActorWorkflow, ActorDraft, ActorSystem:
		return true
	}
	return false
}

// Decision reasons (closed set, ^[a-z0-9_]+$).
const (
	ReasonActionRule        = "action_rule"
	ReasonTierDefault       = "tier_default"
	ReasonUnknownAction     = "unknown_action"
	ReasonInvalidRequest    = "invalid_request"
	ReasonFallbackNoSet     = "fallback_no_rule_set"
	ReasonFallbackInvalid   = "fallback_invalid_rule_set"
	ReasonFallbackStoreErr  = "fallback_store_error"
	ReasonRecordFailed      = "record_failed"
	ReasonEngineUnavailable = "engine_unavailable"
	// ReasonRevalidateDenied: the re-check right before an approved action
	// (FR-130) refused it, because the rule now denies it or the approval
	// grant did not verify. Nothing ran.
	ReasonRevalidateDenied = "revalidate_denied"
)

// MaxResourceKeyBytes bounds a resource key (for example "owner/repo#12").
const MaxResourceKeyBytes = 256

var (
	hexSHA256Re = regexp.MustCompile(`^[0-9a-f]{64}$`)
	resourceRe  = regexp.MustCompile(`^[\x21-\x7e]*$`)
)

// Request is one question to the engine. It carries ids, names and a hash
// only: never inputs, text, prompts or tokens.
type Request struct {
	WorkspaceID string
	ActorKind   string
	Action      string
	Tier        Tier
	// ResourceKey names the target, such as "owner/repo#12" (printable
	// ASCII, no spaces, at most MaxResourceKeyBytes).
	ResourceKey string
	ExecutionID int64
	ToolRunID   int64
	ApprovalID  int64
	// InputSHA256 is the hex SHA-256 of the action's canonical input, or "".
	InputSHA256 string
	// Revalidate marks the re-check right before an approved action runs:
	// a deny is then recorded with reason revalidate_denied.
	Revalidate bool
	// GrantRejected marks a re-check whose approval grant did not verify:
	// the decision is deny, reason revalidate_denied, whatever the rules say.
	GrantRejected bool
}

// Decision is the engine's answer.
type Decision struct {
	Effect Effect
	Reason string
	// RuleKey is the rule that decided: "actions.<name>", "defaults.<tier>"
	// or "fallback.<tier>"; "" when the request itself was refused.
	RuleKey string
	// RuleSetVersion is the stored version used; 0 for the built-in safe set.
	RuleSetVersion int32
	// ApprovalTTL is the rule set's approval lifetime.
	ApprovalTTL time.Duration
}

// Allowed reports whether the action may run now without an approval.
func (d Decision) Allowed() bool { return d.Effect == EffectAllow }

// StoredRuleSet is one stored version.
type StoredRuleSet struct {
	WorkspaceID  string
	Version      int32
	Schema       string
	Body         []byte
	BodySHA256   string
	CreatedBy    string
	CreatedAt    time.Time
	ActivatedAt  *time.Time
	Active       bool
	RestoredFrom int32
}

// DecisionRecord is one policy_decisions row.
type DecisionRecord struct {
	ID             int64
	WorkspaceID    string
	Action         string
	Tier           Tier
	ResourceKey    string
	Effect         Effect
	Reason         string
	RuleSetVersion int32
	RuleKey        string
	ActorKind      string
	ExecutionID    int64
	ToolRunID      int64
	ApprovalID     int64
	InputSHA256    string
	CreatedAt      time.Time
}

// EngineStore is the storage the engine needs.
type EngineStore interface {
	// ActiveRuleSet returns the workspace's active version; ok=false when
	// it has none.
	ActiveRuleSet(ctx context.Context, workspaceID string) (StoredRuleSet, bool, error)
	RecordDecision(ctx context.Context, rec DecisionRecord) error
}

// Store is the full rule set storage used by the CLI.
type Store interface {
	EngineStore
	// ApplyRuleSet stores rs as a new version and activates it, in one
	// transaction. When rs equals the active version nothing is stored and
	// changed is false. restoredFrom is the rolled-back version or 0.
	ApplyRuleSet(ctx context.Context, workspaceID string, rs RuleSet, createdBy string, restoredFrom int32) (stored StoredRuleSet, changed bool, err error)
	RuleSetVersion(ctx context.Context, workspaceID string, version int32) (StoredRuleSet, bool, error)
	RuleSetHistory(ctx context.Context, workspaceID string, limit int) ([]StoredRuleSet, error)
	ListDecisions(ctx context.Context, workspaceID string, limit int) ([]DecisionRecord, error)
}

// Created-by values of a stored version (closed set; the DB CHECK matches).
const (
	CreatedByMigration = "migration"
	CreatedByInit      = "init"
	CreatedByCLI       = "cli"
)

// ErrWorkspaceNotFound is returned by ApplyRuleSet for an unknown workspace.
var ErrWorkspaceNotFound = errors.New("policy: workspace not found")

// LoadStored decodes and fully validates a stored version. The canonical
// hash of the decoded body must equal the stored hash, so a body edited in
// the database outside the CLI counts as invalid.
func LoadStored(s StoredRuleSet, known KnownActions) (RuleSet, error) {
	if s.Schema != SchemaV1 {
		return RuleSet{}, errors.New("policy: unsupported stored schema")
	}
	rs, err := DecodeStored(s.Body)
	if err != nil {
		return RuleSet{}, err
	}
	if err := Validate(rs, known); err != nil {
		return RuleSet{}, err
	}
	_, sum, err := Canonical(rs)
	if err != nil {
		return RuleSet{}, err
	}
	if sum != s.BodySHA256 {
		return RuleSet{}, errors.New("policy: stored rule set hash mismatch")
	}
	return rs, nil
}

// Evaluate is the pure rule lookup: the exact action rule first, then the
// tier default. A missing or invalid effect is deny.
func Evaluate(rs RuleSet, action string, tier Tier) (Effect, string, bool) {
	if e, ok := rs.Actions[action]; ok {
		if !e.Valid() {
			return EffectDeny, "actions." + action, true
		}
		return e, "actions." + action, true
	}
	e, ok := rs.Defaults[tier]
	if !ok || !e.Valid() {
		return EffectDeny, "defaults." + string(tier), false
	}
	return e, "defaults." + string(tier), false
}

// validRequest checks the request's shape.
func validRequest(r Request) bool {
	return r.WorkspaceID != "" && validActor(r.ActorKind) && ActionNameRe.MatchString(r.Action) && r.Tier.Valid() &&
		len(r.ResourceKey) <= MaxResourceKeyBytes && resourceRe.MatchString(r.ResourceKey) &&
		(r.InputSHA256 == "" || hexSHA256Re.MatchString(r.InputSHA256)) &&
		r.ExecutionID >= 0 && r.ToolRunID >= 0 && r.ApprovalID >= 0
}

// Resolve is the pure decision function. active is the validated active
// rule set (nil when none could be used, then fallbackReason says why and
// the built-in safe set decides). An invalid request or an unknown action
// is denied whatever the rules say.
func Resolve(active *RuleSet, version int32, fallbackReason string, known KnownActions, req Request) Decision {
	if !validRequest(req) {
		return Decision{Effect: EffectDeny, Reason: ReasonInvalidRequest}
	}
	rs, fromStore := SafeRuleSet(), false
	if active != nil {
		rs, fromStore = *active, true
	}
	d := Decision{ApprovalTTL: rs.ApprovalTTL}
	if fromStore {
		d.RuleSetVersion = version
	}
	if !known[req.Action] {
		d.Effect, d.Reason = EffectDeny, ReasonUnknownAction
		if req.Revalidate || req.GrantRejected {
			d.Reason = ReasonRevalidateDenied
		}
		return d
	}
	effect, key, byAction := Evaluate(rs, req.Action, req.Tier)
	d.Effect, d.RuleKey = effect, key
	switch {
	case !fromStore:
		d.Reason = fallbackReason
		if d.Reason == "" {
			d.Reason = ReasonFallbackNoSet
		}
		d.RuleKey = "fallback." + string(req.Tier)
	case byAction:
		d.Reason = ReasonActionRule
	default:
		d.Reason = ReasonTierDefault
	}
	if req.GrantRejected || (req.Revalidate && d.Effect == EffectDeny) {
		d.Effect, d.Reason = EffectDeny, ReasonRevalidateDenied
	}
	return d
}

// Engine decides every privileged action. The zero value denies.
type Engine struct {
	Store EngineStore
	Known KnownActions
	Now   func() time.Time
}

// NewEngine returns an engine over store that knows the given actions.
func NewEngine(store EngineStore, known KnownActions) *Engine {
	return &Engine{Store: store, Known: known, Now: time.Now}
}

// Decide answers req and records the decision. It always returns a usable
// decision; the error is for logging only. A store error while loading the
// rule set uses the built-in safe set; a failed record turns the decision
// into deny.
func (e *Engine) Decide(ctx context.Context, req Request) (Decision, error) {
	if e == nil || e.Store == nil {
		return Decision{Effect: EffectDeny, Reason: ReasonEngineUnavailable}, errors.New("policy: engine not configured")
	}
	var (
		active   *RuleSet
		version  int32
		fallback string
		loadErr  error
	)
	if validRequest(req) {
		stored, ok, err := e.Store.ActiveRuleSet(ctx, req.WorkspaceID)
		switch {
		case err != nil:
			fallback, loadErr = ReasonFallbackStoreErr, errors.New("policy: load active rule set failed")
		case !ok:
			fallback = ReasonFallbackNoSet
		default:
			rs, err := LoadStored(stored, e.Known)
			if err != nil {
				fallback, loadErr = ReasonFallbackInvalid, errors.New("policy: active rule set is invalid")
			} else {
				active, version = &rs, stored.Version
			}
		}
	}
	d := Resolve(active, version, fallback, e.Known, req)
	if err := e.Store.RecordDecision(ctx, e.record(req, d)); err != nil {
		return Decision{Effect: EffectDeny, Reason: ReasonRecordFailed, RuleKey: d.RuleKey, RuleSetVersion: d.RuleSetVersion, ApprovalTTL: d.ApprovalTTL},
			errors.New("policy: record decision failed")
	}
	return d, loadErr
}

// record builds the audit row. Fields of an invalid request that do not fit
// their shape are blanked, so the refusal itself is still recorded.
func (e *Engine) record(req Request, d Decision) DecisionRecord {
	now := time.Now
	if e.Now != nil {
		now = e.Now
	}
	r := DecisionRecord{
		WorkspaceID: req.WorkspaceID, Action: req.Action, Tier: req.Tier, ResourceKey: req.ResourceKey,
		Effect: d.Effect, Reason: d.Reason, RuleSetVersion: d.RuleSetVersion, RuleKey: d.RuleKey,
		ActorKind: req.ActorKind, ExecutionID: req.ExecutionID, ToolRunID: req.ToolRunID, ApprovalID: req.ApprovalID,
		InputSHA256: req.InputSHA256, CreatedAt: now(),
	}
	if !ActionNameRe.MatchString(r.Action) {
		r.Action = ""
	}
	if !r.Tier.Valid() {
		r.Tier = ""
	}
	if len(r.ResourceKey) > MaxResourceKeyBytes || !resourceRe.MatchString(r.ResourceKey) {
		r.ResourceKey = ""
	}
	if !validActor(r.ActorKind) {
		r.ActorKind = ""
	}
	if r.InputSHA256 != "" && !hexSHA256Re.MatchString(r.InputSHA256) {
		r.InputSHA256 = ""
	}
	if r.ExecutionID < 0 {
		r.ExecutionID = 0
	}
	if r.ToolRunID < 0 {
		r.ToolRunID = 0
	}
	if r.ApprovalID < 0 {
		r.ApprovalID = 0
	}
	if len(r.RuleKey) > 100 {
		r.RuleKey = ""
	}
	return r
}

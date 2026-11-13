package behavior

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ResolvedDoc is a behavior with defaults applied and vars filled in. It is
// what a run uses.
type ResolvedDoc struct {
	Name        string
	Version     int // set when resolved from a published version
	Description string
	Triggers    []ResolvedTrigger
	Reads       []ReadSpec
	// Instructions is the owner's text with vars filled in.
	Instructions string
	Autonomy     AutonomyRules
	// Actions maps each class to its allowed actions, in document order.
	Actions map[Class][]ActionSpec
	// RunActions are the run-level actions (actions.run), in document
	// order. They run once per run, after the model step (if any), on
	// trusted trigger data only.
	RunActions     []ActionSpec
	DryRun         bool
	AskExpiryHours int
	Budgets        Budgets
	Model          string // empty: the profile or settings default
	Language       string // empty: the profile language
	Tone           string // empty: the profile tone
}

// ResolvedTrigger is one trigger with vars filled in.
type ResolvedTrigger struct {
	Type     string
	ToStatus []string
	Assignee string // "", "me" or a ClickUp user ID
	Lists    []string
	From     string // "", "any" or "others"; a chat trigger defaults to "others"
	Command  string
	Args     []string
	// Chat message filters. Channels defaults to ["dm"] for a chat trigger;
	// Senders empty means anyone.
	Channels  []string
	HasPRLink bool
	Senders   []string
}

// ReadSpec is one allowed read tool. MaxBytes 0 means the executor default.
type ReadSpec struct {
	Tool     string
	MaxBytes int
}

// AutonomyRules decide how much the engine does alone.
type AutonomyRules struct {
	ConfidenceThreshold     float64
	AlwaysAskTopics         []string
	RequireCitations        bool
	NotifyWhenInformational bool
}

// ActionSpec is one allowed action with its parameters filled in. On is
// "" or OnSuccessOfPrevious (run-level actions only).
type ActionSpec struct {
	Type   string
	Params map[string]string
	On     string
}

// Budgets cap one run. MaxTokens 0 means the profile or settings default.
type Budgets struct {
	MaxTokens, MaxItems, MaxSteps int
}

// Resolve validates the document (except read tool names and GitHub
// account existence, which the publisher checked), applies defaults and
// fills in vars. An invalid document returns FieldErrors.
func Resolve(doc Doc, vars map[string]string) (ResolvedDoc, error) {
	if errs := validate(doc, vars, nil, nil); len(errs) > 0 {
		return ResolvedDoc{}, errs
	}
	values := map[string]string{}
	for name, def := range doc.Vars {
		if def.Default != nil {
			values[name] = *def.Default
		}
	}
	for name, v := range vars {
		values[name] = v
	}
	r := func(s string) string { return replaceVars(s, values) }
	rl := func(in []string) []string {
		if len(in) == 0 {
			return nil
		}
		out := make([]string, len(in))
		for i, s := range in {
			out[i] = r(s)
		}
		return out
	}

	out := ResolvedDoc{
		Name: doc.Name, Description: doc.Description, Instructions: r(doc.Instructions),
		Autonomy: AutonomyRules{
			ConfidenceThreshold: DefaultConfidenceThreshold, RequireCitations: true, NotifyWhenInformational: true,
		},
		Actions:        map[Class][]ActionSpec{},
		DryRun:         true,
		AskExpiryHours: DefaultAskExpiryHours,
		Budgets:        Budgets{MaxItems: DefaultMaxItems, MaxSteps: DefaultMaxSteps},
		Model:          doc.Model,
	}
	for _, t := range doc.Triggers {
		rt := ResolvedTrigger{
			Type: t.Type, ToStatus: rl(t.Filter.ToStatus), Assignee: r(t.Filter.Assignee), Lists: rl(t.Filter.List),
			From: r(t.Filter.From), Command: r(t.Command), Args: append([]string(nil), t.Args...),
			Channels: rl(t.Filter.Channel), Senders: rl(t.Filter.Senders),
			HasPRLink: t.Filter.HasPRLink != nil && *t.Filter.HasPRLink,
		}
		if t.Type == TriggerChatMessage {
			if rt.From == "" {
				rt.From = FromOthers
			}
			if len(rt.Channels) == 0 {
				rt.Channels = []string{ChannelDM}
			}
		}
		out.Triggers = append(out.Triggers, rt)
	}
	for _, rd := range doc.Reads {
		out.Reads = append(out.Reads, ReadSpec{Tool: rd.Tool, MaxBytes: rd.MaxBytes})
	}
	if a := doc.Autonomy; a != nil {
		if a.ConfidenceThreshold != nil {
			out.Autonomy.ConfidenceThreshold = *a.ConfidenceThreshold
		}
		out.Autonomy.AlwaysAskTopics = append([]string(nil), a.AlwaysAskTopics...)
		if a.RequireCitations != nil {
			out.Autonomy.RequireCitations = *a.RequireCitations
		}
		if a.NotifyWhenInformational != nil {
			out.Autonomy.NotifyWhenInformational = *a.NotifyWhenInformational
		}
	}
	classes := make([]string, 0, len(doc.Actions))
	for k := range doc.Actions {
		classes = append(classes, k)
	}
	sort.Strings(classes)
	for _, class := range classes {
		for _, a := range doc.Actions[class] {
			params := map[string]string{}
			for k, v := range a.Params {
				params[k] = r(v)
			}
			if a.Type == ActionDescriptionBlock && params["block"] == "" {
				params["block"] = doc.Name
			}
			spec := ActionSpec{Type: a.Type, Params: params, On: a.On}
			if class == ActionsRun {
				out.RunActions = append(out.RunActions, spec)
				continue
			}
			out.Actions[Class(class)] = append(out.Actions[Class(class)], spec)
		}
	}
	if doc.DryRun != nil {
		out.DryRun = *doc.DryRun
	}
	if doc.AskExpiryHours != nil {
		out.AskExpiryHours = *doc.AskExpiryHours
	}
	if b := doc.Budgets; b != nil {
		if b.MaxTokens != nil {
			out.Budgets.MaxTokens = *b.MaxTokens
		}
		if b.MaxItems != nil {
			out.Budgets.MaxItems = *b.MaxItems
		}
		if b.MaxSteps != nil {
			out.Budgets.MaxSteps = *b.MaxSteps
		}
	}
	if p := doc.Profile; p != nil {
		out.Language, out.Tone = p.Language, p.Tone
	}
	return out, nil
}

// ActionsFor returns the allowed actions of a class.
func (d ResolvedDoc) ActionsFor(c Class) []ActionSpec { return d.Actions[c] }

// HasModelStep reports whether a run of the behavior calls the model. A
// behavior without instructions (allowed only for chat-triggered behaviors
// that have only run-level actions) skips the model step and runs its
// run-level actions directly: the run executor then collects no context,
// calls no model and stores no items.
func (d ResolvedDoc) HasModelStep() bool { return strings.TrimSpace(d.Instructions) != "" }

// decodeStored decodes a stored document and its var values.
func decodeStored(document, vars json.RawMessage) (Doc, map[string]string, error) {
	doc, err := DecodeDocument(document)
	if err != nil {
		return Doc{}, nil, err
	}
	values, err := DecodeVars(vars)
	if err != nil {
		return Doc{}, nil, err
	}
	return doc, values, nil
}

// ValidateDraft validates a stored draft.
func ValidateDraft(b Behavior, allowedReads []string, opts ...ValidateOption) error {
	doc, vars, err := decodeStored(b.Draft, b.DraftVars)
	if err != nil {
		return err
	}
	errs := FieldErrors(Validate(doc, vars, allowedReads, opts...))
	if doc.Name != b.Name && NameRe.MatchString(doc.Name) {
		errs = append(errs, FieldError{Path: "name", Msg: fmt.Sprintf("does not match the stored behavior %q", b.Name)})
	}
	if len(errs) > 0 {
		return errs
	}
	return nil
}

// PublishDraft freezes the behavior's draft as a new immutable version. The
// draft is validated under the store's lock, so an invalid draft is never
// published; validation errors come back as FieldErrors.
func PublishDraft(ctx context.Context, store DefinitionStore, workspaceID, name string, allowedReads []string, opts ...ValidateOption) (Version, error) {
	return store.Publish(ctx, workspaceID, name, func(b Behavior) error {
		return ValidateDraft(b, allowedReads, opts...)
	})
}

// ResolveVersion resolves a published version for a run. The result
// depends only on the version row, so a run is reproducible and later
// versions never change it.
func ResolveVersion(v Version) (ResolvedDoc, error) {
	doc, vars, err := decodeStored(v.Document, v.Vars)
	if err != nil {
		return ResolvedDoc{}, err
	}
	out, err := Resolve(doc, vars)
	if err != nil {
		return ResolvedDoc{}, err
	}
	out.Version = v.Version
	return out, nil
}

// LoadVersion loads and resolves one published version; version 0 is the
// latest.
func LoadVersion(ctx context.Context, store DefinitionStore, workspaceID, name string, version int) (ResolvedDoc, Version, error) {
	v, err := store.GetVersion(ctx, workspaceID, name, version)
	if err != nil {
		return ResolvedDoc{}, Version{}, err
	}
	doc, err := ResolveVersion(v)
	if err != nil {
		return ResolvedDoc{}, Version{}, errors.Join(ErrInvalid, err)
	}
	return doc, v, nil
}

// Export returns the source text a behavior was imported from: the draft
// when version is 0, otherwise that published version.
func Export(ctx context.Context, store DefinitionStore, workspaceID, name string, version int) (string, error) {
	if version < 0 {
		return "", ErrInvalid
	}
	if version == 0 {
		b, err := store.GetBehavior(ctx, workspaceID, name)
		if err != nil {
			return "", err
		}
		return b.DraftSource, nil
	}
	v, err := store.GetVersion(ctx, workspaceID, name, version)
	if err != nil {
		return "", err
	}
	return v.Source, nil
}

// ImportDraft parses a behavior source and saves it as the behavior's
// draft, creating the behavior when it is new. vars nil keeps the values
// already stored with the draft. A source that does not parse, or has no
// valid name, is not saved (err is FieldErrors). A parsed draft is saved
// even when it does not validate; its problems come back as problems.
func ImportDraft(ctx context.Context, store DefinitionStore, workspaceID string, src []byte, format Format, vars map[string]string, allowedReads []string, opts ...ValidateOption) (b Behavior, problems FieldErrors, err error) {
	doc, err := Parse(src, format)
	if err != nil {
		return Behavior{}, nil, err
	}
	if !NameRe.MatchString(doc.Name) {
		return Behavior{}, nil, FieldErrors{{Path: "name", Msg: "must be 1-63 of a-z, 0-9, _ or -, starting with a letter or digit"}}
	}
	if vars == nil {
		prev, gerr := store.GetBehavior(ctx, workspaceID, doc.Name)
		switch {
		case gerr == nil:
			if vars, err = DecodeVars(prev.DraftVars); err != nil {
				return Behavior{}, nil, err
			}
		case errors.Is(gerr, ErrNotFound):
			vars = map[string]string{}
		default:
			return Behavior{}, nil, gerr
		}
	}
	draft, err := Encode(doc)
	if err != nil {
		return Behavior{}, nil, err
	}
	rawVars, err := EncodeVars(vars)
	if err != nil {
		return Behavior{}, nil, err
	}
	b, err = store.SaveDraft(ctx, workspaceID, doc.Name, draft, rawVars, string(src))
	if err != nil {
		return Behavior{}, nil, err
	}
	return b, FieldErrors(Validate(doc, vars, allowedReads, opts...)), nil
}

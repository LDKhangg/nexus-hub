package behavior

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// SchemaVersion is the only behavior document schema this engine accepts.
const SchemaVersion = 1

// MaxSourceBytes caps an imported behavior or values file. It matches the
// storage limit on the kept source text.
const MaxSourceBytes = 262144

// Trigger types (closed list).
const (
	TriggerTaskStatusChanged = "clickup.task_status_changed"
	TriggerTaskAssigned      = "clickup.task_assigned"
	TriggerCommentAdded      = "clickup.comment_added"
	TriggerTelegramCommand   = "telegram.command"
	// TriggerChatMessage is a new ClickUp Chat message, found by polling.
	TriggerChatMessage = "clickup.chat_message"
)

// Action types (closed list).
const (
	ActionTelegramMessage  = "telegram.message"
	ActionClickUpComment   = "clickup.comment"
	ActionDescriptionBlock = "clickup.description_block"
	ActionSetStatus        = "clickup.set_status"
	ActionUnassign         = "clickup.unassign"
	// ActionPRApprove approves the pull requests in trusted chat trigger
	// data. Allowed only in actions.run.
	ActionPRApprove = "github.pr_approve"
	// ActionChatReaction adds a reaction to the triggering chat message.
	// Allowed only in actions.run.
	ActionChatReaction = "clickup.chat_reaction"
)

// ActionsRun is the actions key of run-level actions: actions that run once
// per run on trusted trigger data, not per item or class.
const ActionsRun = "run"

// OnSuccessOfPrevious is the only value of an action's "on" field: run the
// action only when the action before it ended OK.
const OnSuccessOfPrevious = "success_of_previous"

// MemorySearchTool is the read tool over decision memory. Callers add it to
// the read tool names they pass to Validate.
const MemorySearchTool = "memory_search"

// Filter values with a fixed meaning.
const (
	AssigneeMe     = "me"
	FromAny        = "any"
	FromOthers     = "others"
	CommandArgTask = "task" // a ClickUp task ID or URL
	// ChannelDM, ChannelGroupDM and ChannelChannel are the chat channel
	// filter values. A trigger on ChannelChannel (ClickUp Chat channels)
	// must also have the filter mentions: me.
	ChannelDM      = "dm"
	ChannelGroupDM = "group_dm"
	ChannelChannel = "channel"
	// MentionsMe is the only value of the mentions filter: the comment or
	// message tags the owner.
	MentionsMe = "me"
)

// Doc is a behavior document as the owner writes it (YAML or JSON).
// Optional scalars are pointers so Resolve can tell "absent" from "zero".
type Doc struct {
	Schema         int                    `json:"schema" yaml:"schema"`
	Name           string                 `json:"name" yaml:"name"`
	Description    string                 `json:"description,omitempty" yaml:"description,omitempty"`
	Triggers       []TriggerDoc           `json:"triggers" yaml:"triggers"`
	Reads          []ReadDoc              `json:"reads,omitempty" yaml:"reads,omitempty"`
	Instructions   string                 `json:"instructions" yaml:"instructions"`
	Autonomy       *AutonomyDoc           `json:"autonomy,omitempty" yaml:"autonomy,omitempty"`
	Actions        map[string][]ActionDoc `json:"actions,omitempty" yaml:"actions,omitempty"`
	DryRun         *bool                  `json:"dry_run,omitempty" yaml:"dry_run,omitempty"`
	AskExpiryHours *int                   `json:"ask_expiry_hours,omitempty" yaml:"ask_expiry_hours,omitempty"`
	Budgets        *BudgetsDoc            `json:"budgets,omitempty" yaml:"budgets,omitempty"`
	Model          string                 `json:"model,omitempty" yaml:"model,omitempty"`
	Profile        *ProfileDoc            `json:"profile,omitempty" yaml:"profile,omitempty"`
	Vars           map[string]VarDoc      `json:"vars,omitempty" yaml:"vars,omitempty"`
	// Verdict asks the model step for one structured verdict per run, which
	// run-level actions may require with "when".
	Verdict *VerdictDoc `json:"verdict,omitempty" yaml:"verdict,omitempty"`
}

// VerdictDoc declares the closed list of verdict labels the model may
// return and the confidence a verdict needs to pass a "when" gate.
type VerdictDoc struct {
	Labels        []string `json:"labels" yaml:"labels"`
	MinConfidence *float64 `json:"min_confidence,omitempty" yaml:"min_confidence,omitempty"`
}

// TriggerDoc is one trigger. Which filter fields apply depends on Type.
type TriggerDoc struct {
	Type    string    `json:"type" yaml:"type"`
	Filter  FilterDoc `json:"filter,omitempty" yaml:"filter,omitempty"`
	Command string    `json:"command,omitempty" yaml:"command,omitempty"`
	Args    []string  `json:"args,omitempty" yaml:"args,omitempty"`
}

// FilterDoc holds every trigger filter field.
type FilterDoc struct {
	ToStatus []string `json:"to_status,omitempty" yaml:"to_status,omitempty"`
	Assignee string   `json:"assignee,omitempty" yaml:"assignee,omitempty"`
	List     []string `json:"list,omitempty" yaml:"list,omitempty"`
	From     string   `json:"from,omitempty" yaml:"from,omitempty"`
	// Channel lists the chat channel types (dm, group_dm, channel); one
	// value may be written as a plain string.
	Channel StringList `json:"channel,omitempty" yaml:"channel,omitempty"`
	// HasPRLink true matches only messages with a GitHub pull request link.
	HasPRLink *bool `json:"has_pr_link,omitempty" yaml:"has_pr_link,omitempty"`
	// Senders lists the ClickUp user IDs a chat message may come from;
	// empty means anyone.
	Senders []string `json:"senders,omitempty" yaml:"senders,omitempty"`
	// Mentions "me" matches only a comment or chat message that tags the
	// owner.
	Mentions string `json:"mentions,omitempty" yaml:"mentions,omitempty"`
}

// StringList is a list of strings that may be written as a single string.
type StringList []string

// UnmarshalJSON accepts a string or a list of strings.
func (l *StringList) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*l = StringList{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*l = many
	return nil
}

var stringListType = reflect.TypeOf(StringList{})

// ReadDoc allows one read tool, with an optional output cap.
type ReadDoc struct {
	Tool     string `json:"tool" yaml:"tool"`
	MaxBytes int    `json:"max_bytes,omitempty" yaml:"max_bytes,omitempty"`
}

// AutonomyDoc holds the rules that decide how much the engine does alone.
type AutonomyDoc struct {
	ConfidenceThreshold     *float64 `json:"confidence_threshold,omitempty" yaml:"confidence_threshold,omitempty"`
	AlwaysAskTopics         []string `json:"always_ask_topics,omitempty" yaml:"always_ask_topics,omitempty"`
	RequireCitations        *bool    `json:"require_citations,omitempty" yaml:"require_citations,omitempty"`
	NotifyWhenInformational *bool    `json:"notify_when_informational,omitempty" yaml:"notify_when_informational,omitempty"`
}

// ActionDoc is one allowed action and its parameters. On and When are set
// only on a run-level action (actions.run): On is OnSuccessOfPrevious, When
// gates the action on the run's verdict.
type ActionDoc struct {
	Type   string            `json:"type" yaml:"type"`
	Params map[string]string `json:"params,omitempty" yaml:"params,omitempty"`
	On     string            `json:"on,omitempty" yaml:"on,omitempty"`
	When   *WhenDoc          `json:"when,omitempty" yaml:"when,omitempty"`
}

// WhenDoc is the condition of a run-level action: the run's verdict must
// have the label Verdict (one of verdict.labels) with enough confidence.
type WhenDoc struct {
	Verdict string `json:"verdict" yaml:"verdict"`
}

// BudgetsDoc caps one run.
type BudgetsDoc struct {
	MaxTokens *int `json:"max_tokens,omitempty" yaml:"max_tokens,omitempty"`
	MaxItems  *int `json:"max_items,omitempty" yaml:"max_items,omitempty"`
	MaxSteps  *int `json:"max_steps,omitempty" yaml:"max_steps,omitempty"`
}

// ProfileDoc overrides profile fields for this behavior.
type ProfileDoc struct {
	Language string `json:"language,omitempty" yaml:"language,omitempty"`
	Tone     string `json:"tone,omitempty" yaml:"tone,omitempty"`
}

// VarDoc declares a var. Values come from the values file; Default is used
// when the values file has none. Vars are plain settings, never secrets.
type VarDoc struct {
	Description string  `json:"description,omitempty" yaml:"description,omitempty"`
	Default     *string `json:"default,omitempty" yaml:"default,omitempty"`
}

// Format is a source file format.
type Format string

const (
	FormatYAML Format = "yaml"
	FormatJSON Format = "json"
)

// FormatFromPath picks the format from a file extension.
func FormatFromPath(path string) (Format, error) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".yaml", ".yml":
		return FormatYAML, nil
	case ".json":
		return FormatJSON, nil
	}
	return "", errors.New("file extension must be .yaml, .yml or .json")
}

// FieldError is one validation error at a document path.
type FieldError struct {
	Path string
	Msg  string
}

func (e FieldError) Error() string {
	if e.Path == "" {
		return e.Msg
	}
	return e.Path + ": " + e.Msg
}

// FieldErrors is a list of validation errors, one per line.
type FieldErrors []FieldError

func (es FieldErrors) Error() string {
	lines := make([]string, len(es))
	for i, e := range es {
		lines[i] = e.Error()
	}
	return strings.Join(lines, "\n")
}

// decodeGeneric turns YAML or JSON source into JSON-shaped values: objects
// are map[string]any, arrays []any, numbers json.Number, int or float64.
func decodeGeneric(src []byte, format Format) (any, error) {
	if len(src) > MaxSourceBytes {
		return nil, FieldErrors{{Msg: fmt.Sprintf("source is larger than %d bytes", MaxSourceBytes)}}
	}
	var v any
	switch format {
	case FormatYAML:
		dec := yaml.NewDecoder(bytes.NewReader(src))
		if err := dec.Decode(&v); err != nil {
			if errors.Is(err, io.EOF) {
				return nil, FieldErrors{{Msg: "document is empty"}}
			}
			return nil, FieldErrors{{Msg: "invalid YAML: " + yamlMsg(err)}}
		}
		var extra any
		if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
			return nil, FieldErrors{{Msg: "only one YAML document is allowed"}}
		}
	case FormatJSON:
		dec := json.NewDecoder(bytes.NewReader(src))
		dec.UseNumber()
		if err := dec.Decode(&v); err != nil {
			return nil, FieldErrors{{Msg: "invalid JSON: " + jsonMsg(err)}}
		}
		if dec.More() {
			return nil, FieldErrors{{Msg: "invalid JSON: data after the document"}}
		}
		if _, err := dec.Token(); !errors.Is(err, io.EOF) {
			return nil, FieldErrors{{Msg: "invalid JSON: data after the document"}}
		}
	default:
		return nil, FieldErrors{{Msg: "unknown format"}}
	}
	if v == nil {
		return nil, FieldErrors{{Msg: "document is empty"}}
	}
	return v, nil
}

// yamlMsg keeps the line information of a YAML syntax error and drops Go
// type names.
func yamlMsg(err error) string {
	msg := strings.TrimPrefix(err.Error(), "yaml: ")
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i]
	}
	return msg
}

func jsonMsg(err error) string {
	var se *json.SyntaxError
	if errors.As(err, &se) {
		return fmt.Sprintf("%s at byte %d", se.Error(), se.Offset)
	}
	return "malformed document"
}

// Parse decodes a behavior document. Unknown fields and wrong value types
// anywhere are errors; all of them are returned as FieldErrors with their
// paths. Parse does not check values; Validate does.
func Parse(src []byte, format Format) (Doc, error) {
	v, err := decodeGeneric(src, format)
	if err != nil {
		return Doc{}, err
	}
	return docFromGeneric(v)
}

// DecodeDocument decodes a stored (JSON) document with the same strict
// checks as Parse.
func DecodeDocument(raw json.RawMessage) (Doc, error) {
	return Parse(raw, FormatJSON)
}

func docFromGeneric(v any) (Doc, error) {
	var errs FieldErrors
	checkShape(v, reflect.TypeOf(Doc{}), "", &errs)
	if len(errs) > 0 {
		return Doc{}, errs
	}
	data, err := json.Marshal(v)
	if err != nil {
		return Doc{}, FieldErrors{{Msg: "document cannot be encoded"}}
	}
	var doc Doc
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return Doc{}, FieldErrors{{Msg: "document does not match the schema"}}
	}
	return doc, nil
}

// Encode returns the document as a JSON object for storage.
func Encode(doc Doc) (json.RawMessage, error) {
	return json.Marshal(doc)
}

// ParseVars decodes a values file: one object mapping var names to string
// values. Values are plain settings and must never be secrets.
func ParseVars(src []byte, format Format) (map[string]string, error) {
	v, err := decodeGeneric(src, format)
	if err != nil {
		return nil, err
	}
	var errs FieldErrors
	checkShape(v, reflect.TypeOf(map[string]string{}), "", &errs)
	if len(errs) > 0 {
		for i := range errs {
			errs[i].Path = joinPath("values", errs[i].Path)
		}
		return nil, errs
	}
	out := map[string]string{}
	for k, val := range v.(map[string]any) {
		if s, ok := val.(string); ok {
			out[k] = s
		}
	}
	return out, nil
}

// EncodeVars returns var values as a JSON object for storage.
func EncodeVars(vars map[string]string) (json.RawMessage, error) {
	if vars == nil {
		vars = map[string]string{}
	}
	return json.Marshal(vars)
}

// DecodeVars decodes stored var values.
func DecodeVars(raw json.RawMessage) (map[string]string, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return map[string]string{}, nil
	}
	return ParseVars(raw, FormatJSON)
}

func joinPath(base, key string) string {
	switch {
	case base == "":
		return key
	case key == "":
		return base
	case strings.HasPrefix(key, "["):
		return base + key
	}
	return base + "." + key
}

// jsonFieldName returns the JSON name of a struct field, or "" when the
// field is not part of the document.
func jsonFieldName(f reflect.StructField) string {
	tag := f.Tag.Get("json")
	if tag == "-" || !f.IsExported() {
		return ""
	}
	name, _, _ := strings.Cut(tag, ",")
	if name == "" {
		return f.Name
	}
	return name
}

// checkShape walks a decoded value against a Go type and records unknown
// fields and wrong value types with their paths. A null is treated as
// absent.
func checkShape(v any, t reflect.Type, path string, errs *FieldErrors) {
	if v == nil {
		return
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	add := func(msg string) { *errs = append(*errs, FieldError{Path: path, Msg: msg}) }
	if t == stringListType {
		if _, ok := v.(string); ok {
			return
		}
	}
	switch t.Kind() {
	case reflect.Struct:
		m, ok := v.(map[string]any)
		if !ok {
			add("must be an object")
			return
		}
		fields := map[string]reflect.StructField{}
		for i := 0; i < t.NumField(); i++ {
			if name := jsonFieldName(t.Field(i)); name != "" {
				fields[name] = t.Field(i)
			}
		}
		for _, k := range sortedKeys(m) {
			f, ok := fields[k]
			if !ok {
				*errs = append(*errs, FieldError{Path: joinPath(path, k), Msg: "unknown field"})
				continue
			}
			checkShape(m[k], f.Type, joinPath(path, k), errs)
		}
	case reflect.Map:
		m, ok := v.(map[string]any)
		if !ok {
			add("must be an object with string keys")
			return
		}
		for _, k := range sortedKeys(m) {
			if m[k] == nil {
				*errs = append(*errs, FieldError{Path: joinPath(path, k), Msg: "must not be null"})
				continue
			}
			checkShape(m[k], t.Elem(), joinPath(path, k), errs)
		}
	case reflect.Slice:
		s, ok := v.([]any)
		if !ok {
			add("must be a list")
			return
		}
		for i, e := range s {
			p := fmt.Sprintf("%s[%d]", path, i)
			if e == nil {
				*errs = append(*errs, FieldError{Path: p, Msg: "must not be null"})
				continue
			}
			checkShape(e, t.Elem(), p, errs)
		}
	case reflect.String:
		if _, ok := v.(string); !ok {
			add("must be a string (quote numbers and IDs)")
		}
	case reflect.Bool:
		if _, ok := v.(bool); !ok {
			add("must be true or false")
		}
	case reflect.Int:
		f, ok := number(v)
		if !ok || f != math.Trunc(f) || math.Abs(f) > 1e15 {
			add("must be a whole number")
		}
	case reflect.Float64:
		if _, ok := number(v); !ok {
			add("must be a number")
		}
	default:
		add("unsupported value")
	}
}

func number(v any) (float64, bool) {
	switch n := v.(type) {
	case json.Number:
		f, err := n.Float64()
		return f, err == nil && !math.IsInf(f, 0) && !math.IsNaN(f)
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint64:
		return float64(n), true
	case float64:
		return n, !math.IsInf(n, 0) && !math.IsNaN(n)
	}
	return 0, false
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

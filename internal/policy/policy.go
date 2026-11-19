// Package policy holds the owner's versioned rule sets and the deny-by-default
// policy engine (Phase 9). A rule set maps an action (a tool name or an
// orchestrator capability) or, failing that, the action's tier to an effect.
// Anything the rules do not allow is denied; a missing, invalid or
// unreadable rule set falls back to the built-in safe set, which never allows
// more than a write approval.
//
// Only the owner changes rule sets, through the CLI (nh policy). No assistant
// tool, behavior action, workflow step or capability writes them.
package policy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// SchemaV1 is the only rule set schema of Phase 9.
const SchemaV1 = "policy/v1"

// MaxSourceBytes bounds a rule set file.
const MaxSourceBytes = 64 << 10

// Approval TTL bounds, in hours.
const (
	MinApprovalTTLHours     = 1
	MaxApprovalTTLHours     = 168
	DefaultApprovalTTLHours = 24
)

// Effect is the outcome of a rule.
type Effect string

const (
	EffectAllow            Effect = "allow"
	EffectApprovalRequired Effect = "approval_required"
	EffectDeny             Effect = "deny"
)

// Valid reports whether e is in the closed set of effects.
func (e Effect) Valid() bool {
	switch e {
	case EffectAllow, EffectApprovalRequired, EffectDeny:
		return true
	}
	return false
}

// rank orders effects by strictness: allow < approval_required < deny. An
// effect outside the closed set ranks as deny.
func (e Effect) rank() int {
	switch e {
	case EffectAllow:
		return 0
	case EffectApprovalRequired:
		return 1
	}
	return 2
}

// Stricter returns the stricter of a and b (deny > approval_required >
// allow); an invalid effect counts as deny.
func Stricter(a, b Effect) Effect {
	if !a.Valid() || !b.Valid() {
		return EffectDeny
	}
	if b.rank() > a.rank() {
		return b
	}
	return a
}

// Tier is the permission tier of an action (same values as tenant.Tier).
type Tier string

const (
	TierRead      Tier = "read"
	TierWrite     Tier = "write"
	TierDangerous Tier = "dangerous"
)

// Tiers lists every tier in a fixed order.
var Tiers = []Tier{TierRead, TierWrite, TierDangerous}

// Valid reports whether t is in the closed set of tiers.
func (t Tier) Valid() bool {
	switch t {
	case TierRead, TierWrite, TierDangerous:
		return true
	}
	return false
}

// ActionNameRe is the shape of an action key: a tool name (github_pr_merge)
// or an orchestrator capability (github.merge).
var ActionNameRe = regexp.MustCompile(`^[a-z][a-z0-9_.]{0,63}$`)

// RuleSet is one parsed, versionless rule set.
type RuleSet struct {
	Schema      string
	Defaults    map[Tier]Effect
	Actions     map[string]Effect
	ApprovalTTL time.Duration
}

// KnownActions is the closed set of action names a rule set may name: the
// tool registry's tool names plus the orchestrator capability names. A nil
// set knows no action.
type KnownActions map[string]bool

// NewKnownActions builds a set from names.
func NewKnownActions(names ...string) KnownActions {
	k := KnownActions{}
	for _, n := range names {
		k[n] = true
	}
	return k
}

// Names returns the known names, sorted.
func (k KnownActions) Names() []string {
	out := make([]string, 0, len(k))
	for n := range k {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// FieldError is one problem of a rule set. Path names the key; values are
// never echoed.
type FieldError struct {
	Path    string
	Problem string
}

func (e FieldError) Error() string {
	if e.Path == "" {
		return e.Problem
	}
	return e.Path + ": " + e.Problem
}

// Errors is every problem found in a rule set.
type Errors []FieldError

func (e Errors) Error() string {
	parts := make([]string, len(e))
	for i, f := range e {
		parts[i] = f.Error()
	}
	return "policy: " + strings.Join(parts, "; ")
}

// Lines returns one line per problem.
func (e Errors) Lines() []string {
	out := make([]string, len(e))
	for i, f := range e {
		out[i] = f.Error()
	}
	return out
}

var safeKeyRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// pathKey names a key in an error: the key itself when it is a plain
// identifier, otherwise its position, so no odd content is echoed.
func pathKey(prefix, key string, index int) string {
	k := key
	if !safeKeyRe.MatchString(key) {
		k = fmt.Sprintf("(key #%d)", index+1)
	}
	if prefix == "" {
		return k
	}
	return prefix + "." + k
}

// Parse reads a policy/v1 YAML rule set. It checks the shape and the closed
// sets (schema, keys, tiers, effects, TTL range, duplicates); Validate also
// checks the action names against the known actions. Errors name the path
// only.
func Parse(src []byte) (RuleSet, error) {
	if len(src) > MaxSourceBytes {
		return RuleSet{}, Errors{{Problem: fmt.Sprintf("file is larger than %d bytes", MaxSourceBytes)}}
	}
	dec := yaml.NewDecoder(bytes.NewReader(src))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return RuleSet{}, Errors{{Problem: "empty document"}}
		}
		return RuleSet{}, Errors{{Problem: "invalid YAML"}}
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return RuleSet{}, Errors{{Problem: "exactly one YAML document is allowed"}}
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return RuleSet{}, Errors{{Problem: "the document must be a mapping"}}
	}
	root := doc.Content[0]
	rs := RuleSet{Defaults: map[Tier]Effect{}, Actions: map[string]Effect{}, ApprovalTTL: DefaultApprovalTTLHours * time.Hour}
	var errs Errors
	seen := map[string]bool{}
	for i := 0; i+1 < len(root.Content); i += 2 {
		k, v := root.Content[i], root.Content[i+1]
		idx := i / 2
		if k.Kind != yaml.ScalarNode {
			errs = append(errs, FieldError{Path: fmt.Sprintf("(key #%d)", idx+1), Problem: "key must be a string"})
			continue
		}
		path := pathKey("", k.Value, idx)
		if seen[k.Value] {
			errs = append(errs, FieldError{Path: path, Problem: "duplicate key"})
			continue
		}
		seen[k.Value] = true
		switch k.Value {
		case "schema":
			if v.Kind != yaml.ScalarNode || v.Tag != "!!str" {
				errs = append(errs, FieldError{Path: path, Problem: "must be a string"})
			} else if v.Value != SchemaV1 {
				errs = append(errs, FieldError{Path: path, Problem: "unsupported schema; want " + SchemaV1})
			} else {
				rs.Schema = v.Value
			}
		case "defaults":
			errs = append(errs, parseEffects(path, v, func(key string, e Effect) *FieldError {
				t := Tier(key)
				if !t.Valid() {
					return &FieldError{Problem: "unknown tier"}
				}
				rs.Defaults[t] = e
				return nil
			})...)
		case "actions":
			errs = append(errs, parseEffects(path, v, func(key string, e Effect) *FieldError {
				if !ActionNameRe.MatchString(key) {
					return &FieldError{Problem: "invalid action name"}
				}
				rs.Actions[key] = e
				return nil
			})...)
		case "approval_ttl_hours":
			n, err := strconv.Atoi(v.Value)
			if v.Kind != yaml.ScalarNode || v.Tag != "!!int" || err != nil {
				errs = append(errs, FieldError{Path: path, Problem: "must be a whole number of hours"})
			} else if n < MinApprovalTTLHours || n > MaxApprovalTTLHours {
				errs = append(errs, FieldError{Path: path, Problem: fmt.Sprintf("must be between %d and %d", MinApprovalTTLHours, MaxApprovalTTLHours)})
			} else {
				rs.ApprovalTTL = time.Duration(n) * time.Hour
			}
		default:
			errs = append(errs, FieldError{Path: path, Problem: "unknown key"})
		}
	}
	if !seen["schema"] {
		errs = append(errs, FieldError{Path: "schema", Problem: "required"})
	}
	if !seen["defaults"] {
		errs = append(errs, FieldError{Path: "defaults", Problem: "required"})
	} else if v := findValue(root, "defaults"); v != nil && v.Kind == yaml.MappingNode {
		for _, t := range Tiers {
			if _, ok := rs.Defaults[t]; !ok && !hasKey(v, string(t)) {
				errs = append(errs, FieldError{Path: "defaults." + string(t), Problem: "required"})
			}
		}
	}
	if len(errs) > 0 {
		return RuleSet{}, errs
	}
	return rs, nil
}

func findValue(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Kind == yaml.ScalarNode && m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func hasKey(m *yaml.Node, key string) bool { return findValue(m, key) != nil }

// parseEffects reads a mapping of name to effect; set checks and stores one
// entry and returns its problem (Path is filled in here).
func parseEffects(path string, v *yaml.Node, set func(key string, e Effect) *FieldError) Errors {
	if v.Kind != yaml.MappingNode {
		return Errors{{Path: path, Problem: "must be a mapping"}}
	}
	var errs Errors
	seen := map[string]bool{}
	for i := 0; i+1 < len(v.Content); i += 2 {
		k, val := v.Content[i], v.Content[i+1]
		idx := i / 2
		if k.Kind != yaml.ScalarNode {
			errs = append(errs, FieldError{Path: fmt.Sprintf("%s.(key #%d)", path, idx+1), Problem: "key must be a string"})
			continue
		}
		p := pathKey(path, k.Value, idx)
		if seen[k.Value] {
			errs = append(errs, FieldError{Path: p, Problem: "duplicate key"})
			continue
		}
		seen[k.Value] = true
		if val.Kind != yaml.ScalarNode || val.Tag != "!!str" || !Effect(val.Value).Valid() {
			errs = append(errs, FieldError{Path: p, Problem: "effect must be allow, approval_required or deny"})
			continue
		}
		if fe := set(k.Value, Effect(val.Value)); fe != nil {
			fe.Path = p
			errs = append(errs, *fe)
		}
	}
	return errs
}

// Validate checks a rule set completely: schema, every tier default, the
// effects, the TTL range and that every action key is a known action.
func Validate(rs RuleSet, known KnownActions) error {
	var errs Errors
	if rs.Schema != SchemaV1 {
		errs = append(errs, FieldError{Path: "schema", Problem: "unsupported schema; want " + SchemaV1})
	}
	for t := range rs.Defaults {
		if !t.Valid() {
			errs = append(errs, FieldError{Path: "defaults", Problem: "unknown tier"})
			break
		}
	}
	for _, t := range Tiers {
		e, ok := rs.Defaults[t]
		switch {
		case !ok:
			errs = append(errs, FieldError{Path: "defaults." + string(t), Problem: "required"})
		case !e.Valid():
			errs = append(errs, FieldError{Path: "defaults." + string(t), Problem: "effect must be allow, approval_required or deny"})
		}
	}
	names := make([]string, 0, len(rs.Actions))
	for n := range rs.Actions {
		names = append(names, n)
	}
	sort.Strings(names)
	for i, n := range names {
		p := pathKey("actions", n, i)
		switch {
		case !ActionNameRe.MatchString(n):
			errs = append(errs, FieldError{Path: p, Problem: "invalid action name"})
		case !known[n]:
			errs = append(errs, FieldError{Path: p, Problem: "unknown action"})
		case !rs.Actions[n].Valid():
			errs = append(errs, FieldError{Path: p, Problem: "effect must be allow, approval_required or deny"})
		}
	}
	h := rs.ApprovalTTL / time.Hour
	if rs.ApprovalTTL%time.Hour != 0 || h < MinApprovalTTLHours || h > MaxApprovalTTLHours {
		errs = append(errs, FieldError{Path: "approval_ttl_hours", Problem: fmt.Sprintf("must be between %d and %d", MinApprovalTTLHours, MaxApprovalTTLHours)})
	}
	if len(errs) > 0 {
		return errs
	}
	return nil
}

// wireRuleSet is the canonical stored JSON form. encoding/json writes the
// fields in this order and map keys sorted, so equal rule sets give equal
// bytes and equal hashes.
type wireRuleSet struct {
	Schema           string            `json:"schema"`
	Defaults         map[string]string `json:"defaults"`
	Actions          map[string]string `json:"actions"`
	ApprovalTTLHours int               `json:"approval_ttl_hours"`
}

// Canonical returns the canonical JSON body of rs and its SHA-256 (hex).
func Canonical(rs RuleSet) ([]byte, string, error) {
	w := wireRuleSet{Schema: rs.Schema, Defaults: map[string]string{}, Actions: map[string]string{}, ApprovalTTLHours: int(rs.ApprovalTTL / time.Hour)}
	for t, e := range rs.Defaults {
		w.Defaults[string(t)] = string(e)
	}
	for n, e := range rs.Actions {
		w.Actions[n] = string(e)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(w); err != nil {
		return nil, "", errors.New("policy: encode rule set")
	}
	body := bytes.TrimRight(buf.Bytes(), "\n")
	return body, SHA256Hex(body), nil
}

// SHA256Hex is the lowercase hex SHA-256 of b.
func SHA256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// DecodeStored reads a stored JSON body (strict: unknown fields are an
// error). The caller still runs Validate.
func DecodeStored(body []byte) (RuleSet, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var w wireRuleSet
	if err := dec.Decode(&w); err != nil {
		return RuleSet{}, errors.New("policy: stored rule set is not valid JSON")
	}
	if dec.More() {
		return RuleSet{}, errors.New("policy: stored rule set has trailing data")
	}
	rs := RuleSet{Schema: w.Schema, Defaults: map[Tier]Effect{}, Actions: map[string]Effect{}, ApprovalTTL: time.Duration(w.ApprovalTTLHours) * time.Hour}
	for t, e := range w.Defaults {
		rs.Defaults[Tier(t)] = Effect(e)
	}
	for n, e := range w.Actions {
		rs.Actions[n] = Effect(e)
	}
	return rs, nil
}

// YAML renders rs as a policy/v1 file that Parse reads back unchanged.
// Every key and value is from a closed set or matches ActionNameRe, so
// plain scalars are safe.
func YAML(rs RuleSet) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "schema: %s\n", rs.Schema)
	b.WriteString("defaults:\n")
	for _, t := range Tiers {
		if e, ok := rs.Defaults[t]; ok {
			fmt.Fprintf(&b, "  %s: %s\n", t, e)
		}
	}
	if len(rs.Actions) == 0 {
		b.WriteString("actions: {}\n")
	} else {
		b.WriteString("actions:\n")
		names := make([]string, 0, len(rs.Actions))
		for n := range rs.Actions {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			fmt.Fprintf(&b, "  %s: %s\n", n, rs.Actions[n])
		}
	}
	fmt.Fprintf(&b, "approval_ttl_hours: %d\n", int(rs.ApprovalTTL/time.Hour))
	return []byte(b.String())
}

// DefaultV1JSON is rule set v1, seeded for every workspace on upgrade: it
// reproduces the behaviour before Phase 9 (read allow, write allow,
// dangerous approval_required). It is the canonical form (Canonical of
// DefaultV1). Migration 00035 seeds exactly this literal and hashes it in
// SQL, and nh init applies DefaultV1; a test keeps the three equal.
const DefaultV1JSON = `{"schema":"policy/v1","defaults":{"dangerous":"approval_required","read":"allow","write":"allow"},"actions":{"github_pr_merge":"approval_required"},"approval_ttl_hours":24}`

// DefaultV1 returns rule set v1.
func DefaultV1() RuleSet {
	rs, err := DecodeStored([]byte(DefaultV1JSON))
	if err != nil {
		panic("policy: DefaultV1JSON does not decode")
	}
	return rs
}

// SafeRuleSet is the built-in fallback: read allow, write approval_required,
// dangerous deny, no action rules. It never allows more than a write
// approval.
func SafeRuleSet() RuleSet {
	return RuleSet{
		Schema:      SchemaV1,
		Defaults:    map[Tier]Effect{TierRead: EffectAllow, TierWrite: EffectApprovalRequired, TierDangerous: EffectDeny},
		Actions:     map[string]Effect{},
		ApprovalTTL: DefaultApprovalTTLHours * time.Hour,
	}
}

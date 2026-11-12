package behavior

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/LDKhangg/nexus-hub/internal/agent"
)

// Limits and defaults of a behavior document.
const (
	MaxInstructionsChars = 20000
	MaxDescriptionChars  = 500
	MaxVarValueChars     = 2000
	MaxTriggers          = 20
	MaxReads             = 30
	MaxActionsPerClass   = 10
	MaxTopics            = 50
	MaxReadBytes         = 1 << 20
	MaxRunTokens         = 1000000

	DefaultConfidenceThreshold = 0.8
	DefaultAskExpiryHours      = 72
	DefaultMaxItems            = 30
	DefaultMaxSteps            = 8
)

var (
	// NameRe is a behavior name; it matches the storage constraint.
	NameRe        = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	varNameRe     = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	placeholderRe = regexp.MustCompile(`\{\{vars\.([a-z][a-z0-9_]*)\}\}`)
	listIDRe      = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	userIDRe      = regexp.MustCompile(`^[0-9]{1,20}$`)
	commandRe     = regexp.MustCompile(`^/[a-z][a-z0-9_]{0,31}$`)
	blockNameRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 _.-]{0,62}$`)
	// clickUpTokenRe adds ClickUp personal tokens to the shared secret
	// patterns.
	clickUpTokenRe = regexp.MustCompile(`\bpk_[0-9]+_[A-Za-z0-9]{20,}`)
	secretProbe    = agent.NewRedactor(nil)
)

// Engine-reserved command names that a behavior may not take.
var reservedCommands = map[string]bool{"/help": true, "/trace": true, "/undo": true, "/start": true}

var triggerTypes = map[string]bool{
	TriggerTaskStatusChanged: true, TriggerTaskAssigned: true, TriggerCommentAdded: true, TriggerTelegramCommand: true,
}

// actionParams lists the parameters each action type takes and whether
// each is required.
var actionParams = map[string]map[string]bool{
	ActionTelegramMessage:  {},
	ActionClickUpComment:   {},
	ActionDescriptionBlock: {"block": false},
	ActionSetStatus:        {"status": true},
	ActionUnassign:         {"user": true},
}

// RequiresConfirmation reports whether an action always needs the owner's
// confirmation, whatever its class.
func RequiresConfirmation(actionType string) bool {
	return actionType == ActionSetStatus || actionType == ActionUnassign
}

// TriggerTypes returns the closed list of trigger types.
func TriggerTypes() []string { return sortedSet(triggerTypes) }

// ActionTypes returns the closed list of action types.
func ActionTypes() []string {
	out := make([]string, 0, len(actionParams))
	for k := range actionParams {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// looksSecret reports whether a value has the shape of a credential.
func looksSecret(v string) bool {
	return secretProbe.RedactSecrets(v) != v || clickUpTokenRe.MatchString(v)
}

func chars(s string) int { return utf8.RuneCountInString(s) }

type checker struct {
	errs FieldErrors
}

func (c *checker) add(path, format string, args ...any) {
	c.errs = append(c.errs, FieldError{Path: path, Msg: fmt.Sprintf(format, args...)})
}

// Validate checks a document with its var values against the schema. Read
// tools must be in allowedReads. It returns every error, in document
// order; nil means valid. Messages never contain var values.
func Validate(doc Doc, vars map[string]string, allowedReads []string) []FieldError {
	allowed := map[string]bool{}
	for _, n := range allowedReads {
		allowed[n] = true
	}
	return validate(doc, vars, allowed)
}

// validate checks reads against allowed when it is non-nil.
func validate(doc Doc, vars map[string]string, allowed map[string]bool) FieldErrors {
	c := &checker{}
	if doc.Schema != SchemaVersion {
		c.add("schema", "must be %d", SchemaVersion)
	}
	if !NameRe.MatchString(doc.Name) {
		c.add("name", "must be 1-63 of a-z, 0-9, _ or -, starting with a letter or digit")
	}
	if chars(doc.Description) > MaxDescriptionChars {
		c.add("description", "must be at most %d characters", MaxDescriptionChars)
	}
	values := c.checkVars(doc, vars)

	// Placeholders are checked where they appear; values are checked only
	// after substitution, so a bad value is reported at its use site.
	sub := func(path, s string) (string, bool) {
		out, ok := c.substitute(path, s, doc.Vars, values)
		return out, ok
	}

	c.checkTriggers(doc.Triggers, sub)
	c.checkReads(doc.Reads, allowed)

	instructions, ok := sub("instructions", doc.Instructions)
	switch {
	case strings.TrimSpace(doc.Instructions) == "":
		c.add("instructions", "is required")
	case ok && chars(instructions) > MaxInstructionsChars:
		c.add("instructions", "must be at most %d characters after vars are filled in", MaxInstructionsChars)
	}

	if a := doc.Autonomy; a != nil {
		if t := a.ConfidenceThreshold; t != nil && (*t < 0 || *t > 1) {
			c.add("autonomy.confidence_threshold", "must be between 0 and 1")
		}
		if len(a.AlwaysAskTopics) > MaxTopics {
			c.add("autonomy.always_ask_topics", "must have at most %d topics", MaxTopics)
		}
		seen := map[string]bool{}
		for i, t := range a.AlwaysAskTopics {
			p := fmt.Sprintf("autonomy.always_ask_topics[%d]", i)
			key := strings.ToLower(strings.TrimSpace(t))
			switch {
			case key == "" || chars(t) > 100:
				c.add(p, "must be 1-100 characters")
			case seen[key]:
				c.add(p, "duplicate topic")
			}
			seen[key] = true
		}
	}

	c.checkActions(doc.Actions, sub)

	if h := doc.AskExpiryHours; h != nil && (*h < 1 || *h > 720) {
		c.add("ask_expiry_hours", "must be between 1 and 720")
	}
	if b := doc.Budgets; b != nil {
		if v := b.MaxTokens; v != nil && (*v < 1 || *v > MaxRunTokens) {
			c.add("budgets.max_tokens", "must be between 1 and %d", MaxRunTokens)
		}
		if v := b.MaxItems; v != nil && (*v < 1 || *v > 100) {
			c.add("budgets.max_items", "must be between 1 and 100")
		}
		if v := b.MaxSteps; v != nil && (*v < 1 || *v > 20) {
			c.add("budgets.max_steps", "must be between 1 and 20")
		}
	}
	if doc.Model != "" && !modelRe.MatchString(doc.Model) {
		c.add("model", "must be a model ID")
	}
	if p := doc.Profile; p != nil {
		if p.Language != "" && !languageRe.MatchString(p.Language) {
			c.add("profile.language", "must be a language code such as vi or en-US")
		}
		if chars(p.Tone) > 200 {
			c.add("profile.tone", "must be at most 200 characters")
		}
	}
	return c.errs
}

// checkVars checks var declarations and values, and returns the effective
// value of every declared var that has one.
func (c *checker) checkVars(doc Doc, vars map[string]string) map[string]string {
	values := map[string]string{}
	for _, name := range sortedVarNames(doc.Vars) {
		def := doc.Vars[name]
		p := "vars." + name
		if !varNameRe.MatchString(name) || len(name) > 64 {
			c.add(p, "var name must be 1-64 of a-z, 0-9 or _, starting with a letter")
		}
		if chars(def.Description) > MaxDescriptionChars {
			c.add(p+".description", "must be at most %d characters", MaxDescriptionChars)
		}
		if def.Default != nil {
			c.checkValue(p+".default", *def.Default)
			values[name] = *def.Default
		}
	}
	names := make([]string, 0, len(vars))
	for k := range vars {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, name := range names {
		p := "values." + name
		if _, ok := doc.Vars[name]; !ok {
			c.add(p, "var is not declared in the behavior")
			continue
		}
		c.checkValue(p, vars[name])
		values[name] = vars[name]
	}
	return values
}

func (c *checker) checkValue(path, v string) {
	if chars(v) > MaxVarValueChars {
		c.add(path, "must be at most %d characters", MaxVarValueChars)
	}
	if looksSecret(v) {
		c.add(path, "looks like a secret; vars must not hold secrets (store credentials with the secret command)")
	}
}

func sortedVarNames(m map[string]VarDoc) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// substitute fills in {{vars.NAME}} placeholders in one pass (a value is
// never expanded again). A "{{vars." that is not a well-formed placeholder,
// an undeclared var, or a var without a value is an error.
func (c *checker) substitute(path, s string, decl map[string]VarDoc, values map[string]string) (string, bool) {
	ok := true
	if strings.Count(s, "{{vars.") != len(placeholderRe.FindAllStringIndex(s, -1)) {
		c.add(path, "malformed var placeholder; use {{vars.name}}")
		ok = false
	}
	for _, m := range placeholderRe.FindAllStringSubmatch(s, -1) {
		name := m[1]
		if _, declared := decl[name]; !declared {
			c.add(path, "undefined var %q", name)
			ok = false
		} else if _, has := values[name]; !has {
			c.add(path, "var %q has no value and no default", name)
			ok = false
		}
	}
	if !ok {
		return "", false
	}
	return replaceVars(s, values), true
}

// replaceVars substitutes known placeholders as plain text.
func replaceVars(s string, values map[string]string) string {
	return placeholderRe.ReplaceAllStringFunc(s, func(m string) string {
		name := m[len("{{vars.") : len(m)-len("}}")]
		if v, ok := values[name]; ok {
			return v
		}
		return m
	})
}

type subFunc func(path, s string) (string, bool)

// filterAllowed lists the filter fields each trigger type takes.
var filterAllowed = map[string]map[string]bool{
	TriggerTaskStatusChanged: {"to_status": true, "assignee": true, "list": true},
	TriggerTaskAssigned:      {"assignee": true, "list": true},
	TriggerCommentAdded:      {"list": true, "from": true},
	TriggerTelegramCommand:   {},
}

func (c *checker) checkTriggers(ts []TriggerDoc, sub subFunc) {
	if len(ts) == 0 {
		c.add("triggers", "at least one trigger is required")
	}
	if len(ts) > MaxTriggers {
		c.add("triggers", "must have at most %d triggers", MaxTriggers)
	}
	commands := map[string]bool{}
	for i, t := range ts {
		p := fmt.Sprintf("triggers[%d]", i)
		if !triggerTypes[t.Type] {
			c.add(p+".type", "unknown trigger %q", t.Type)
			continue
		}
		allowed := filterAllowed[t.Type]
		f := t.Filter
		used := map[string]bool{"to_status": len(f.ToStatus) > 0, "assignee": f.Assignee != "", "list": len(f.List) > 0, "from": f.From != ""}
		for _, k := range []string{"to_status", "assignee", "list", "from"} {
			if used[k] && !allowed[k] {
				c.add(p+".filter."+k, "not a filter of %s", t.Type)
			}
		}
		if allowed["to_status"] {
			for j, s := range f.ToStatus {
				fp := fmt.Sprintf("%s.filter.to_status[%d]", p, j)
				if v, ok := sub(fp, s); ok && (strings.TrimSpace(v) == "" || chars(v) > 100) {
					c.add(fp, "must be a status name of 1-100 characters")
				}
			}
		}
		if allowed["assignee"] && f.Assignee != "" {
			fp := p + ".filter.assignee"
			if v, ok := sub(fp, f.Assignee); ok && v != AssigneeMe && !userIDRe.MatchString(v) {
				c.add(fp, "must be %q or a ClickUp user ID", AssigneeMe)
			}
		}
		if allowed["list"] {
			for j, s := range f.List {
				fp := fmt.Sprintf("%s.filter.list[%d]", p, j)
				if v, ok := sub(fp, s); ok && !listIDRe.MatchString(v) {
					c.add(fp, "must be a ClickUp list ID")
				}
			}
		}
		if allowed["from"] && f.From != "" {
			fp := p + ".filter.from"
			if v, ok := sub(fp, f.From); ok && v != FromAny && v != FromOthers {
				c.add(fp, "must be %q or %q", FromAny, FromOthers)
			}
		}
		if t.Type != TriggerTelegramCommand {
			if t.Command != "" {
				c.add(p+".command", "only telegram.command takes a command")
			}
			if len(t.Args) > 0 {
				c.add(p+".args", "only telegram.command takes args")
			}
			continue
		}
		cp := p + ".command"
		if t.Command == "" {
			c.add(cp, "is required for telegram.command")
		} else if v, ok := sub(cp, t.Command); ok {
			switch {
			case !commandRe.MatchString(v):
				c.add(cp, "must be a command such as /name (a-z, 0-9, _)")
			case reservedCommands[v]:
				c.add(cp, "is a built-in command")
			case commands[v]:
				c.add(cp, "duplicate command")
			}
			commands[v] = true
		}
		seen := map[string]bool{}
		for j, a := range t.Args {
			ap := fmt.Sprintf("%s.args[%d]", p, j)
			switch {
			case a != CommandArgTask:
				c.add(ap, "unknown argument %q (allowed: %s)", a, CommandArgTask)
			case seen[a]:
				c.add(ap, "duplicate argument")
			}
			seen[a] = true
		}
	}
}

func (c *checker) checkReads(rs []ReadDoc, allowed map[string]bool) {
	if len(rs) > MaxReads {
		c.add("reads", "must have at most %d entries", MaxReads)
	}
	seen := map[string]bool{}
	for i, r := range rs {
		p := fmt.Sprintf("reads[%d]", i)
		switch {
		case r.Tool == "":
			c.add(p+".tool", "is required")
		case allowed != nil && !allowed[r.Tool]:
			c.add(p+".tool", "unknown read tool %q", r.Tool)
		case seen[r.Tool]:
			c.add(p+".tool", "duplicate read tool")
		}
		seen[r.Tool] = true
		if r.MaxBytes < 0 || r.MaxBytes > MaxReadBytes {
			c.add(p+".max_bytes", "must be between 1 and %d", MaxReadBytes)
		}
	}
}

func (c *checker) checkActions(actions map[string][]ActionDoc, sub subFunc) {
	classes := make([]string, 0, len(actions))
	for k := range actions {
		classes = append(classes, k)
	}
	sort.Strings(classes)
	for _, class := range classes {
		list := actions[class]
		cp := "actions." + class
		switch Class(class) {
		case ClassAuto, ClassNotify, ClassAsk:
		default:
			c.add(cp, "unknown class (allowed: auto, notify, ask)")
			continue
		}
		if len(list) > MaxActionsPerClass {
			c.add(cp, "must have at most %d actions", MaxActionsPerClass)
		}
		for i, a := range list {
			p := fmt.Sprintf("%s[%d]", cp, i)
			params, known := actionParams[a.Type]
			if !known {
				c.add(p+".type", "unknown action %q", a.Type)
				continue
			}
			for _, k := range sortedParamKeys(a.Params) {
				if _, ok := params[k]; !ok {
					c.add(p+".params."+k, "unknown parameter of %s", a.Type)
				}
			}
			for _, k := range sortedBoolKeys(params) {
				pp := p + ".params." + k
				raw, present := a.Params[k]
				if !present {
					if params[k] {
						c.add(pp, "is required for %s", a.Type)
					}
					continue
				}
				v, ok := sub(pp, raw)
				if !ok {
					continue
				}
				switch k {
				case "block":
					if !blockNameRe.MatchString(v) {
						c.add(pp, "must be 1-63 of letters, digits, space, _, . or -")
					}
				case "status":
					if strings.TrimSpace(v) == "" || chars(v) > 100 {
						c.add(pp, "must be a status name of 1-100 characters")
					}
				case "user":
					if v != AssigneeMe && !userIDRe.MatchString(v) {
						c.add(pp, "must be %q or a ClickUp user ID", AssigneeMe)
					}
				}
			}
		}
	}
}

func sortedParamKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedBoolKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

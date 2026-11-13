package behavior

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/LDKhangg/nexus-hub/internal/agent"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
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
	MinVerdictLabels     = 2
	MaxVerdictLabels     = 8

	DefaultConfidenceThreshold = 0.8
	DefaultAskExpiryHours      = 72
	DefaultMaxItems            = 30
	DefaultMaxSteps            = 8
	// DefaultVerdictMinConfidence is verdict.min_confidence when absent.
	DefaultVerdictMinConfidence = 0.8
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
	// VerdictLabelRe is a verdict label: a short snake_case word.
	VerdictLabelRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	// clickUpTokenRe adds ClickUp personal tokens to the shared secret
	// patterns.
	clickUpTokenRe = regexp.MustCompile(`\bpk_[0-9]+_[A-Za-z0-9]{20,}`)
	secretProbe    = agent.NewRedactor(nil)
)

// Engine-reserved command names that a behavior may not take.
var reservedCommands = map[string]bool{"/help": true, "/trace": true, "/undo": true, "/start": true}

var triggerTypes = map[string]bool{
	TriggerTaskStatusChanged: true, TriggerTaskAssigned: true, TriggerCommentAdded: true, TriggerTelegramCommand: true,
	TriggerChatMessage: true,
}

// actionParams lists the parameters each action type takes and whether
// each is required.
var actionParams = map[string]map[string]bool{
	ActionTelegramMessage:  {},
	ActionClickUpComment:   {},
	ActionDescriptionBlock: {"block": false},
	ActionSetStatus:        {"status": true},
	ActionUnassign:         {"user": true},
	ActionPRApprove:        {"account": true},
	ActionChatReaction:     {"reaction": true},
}

// runActionTypes lists the action types allowed in actions.run.
var runActionTypes = map[string]bool{ActionPRApprove: true, ActionChatReaction: true, ActionTelegramMessage: true}

// taskActions lists the class action types that write to the run's
// ClickUp task; a behavior whose triggers are all chat messages has none.
var taskActions = map[string]bool{ActionClickUpComment: true, ActionDescriptionBlock: true, ActionSetStatus: true, ActionUnassign: true}

// runOnlyActions lists the action types allowed only in actions.run.
var runOnlyActions = map[string]bool{ActionPRApprove: true, ActionChatReaction: true}

// reactionRe is a chat reaction name: a lower-case emoji name.
var reactionRe = regexp.MustCompile(`^[a-z0-9_+-]{1,64}$`)

// GitHubAccountPAT is the hub GitHub account type that acts as a person (a
// personal access token); "app" accounts act as a bot.
const GitHubAccountPAT = "pat"

// ValidateOption adds a check against the hub's environment to Validate.
type ValidateOption func(*validateEnv)

// validateEnv holds environment facts that a document alone does not have.
// A nil env (run-time resolution) skips those checks: the publisher made
// them.
type validateEnv struct {
	// githubAccounts maps hub GitHub account names to their type; nil when
	// the caller did not supply them.
	githubAccounts map[string]string
}

// WithGitHubAccounts makes validation check that every github.pr_approve
// "account" names a hub GitHub account of type "pat". accounts maps account
// names to their type ("pat" or "app"). Without this option only the
// account name's syntax is checked.
func WithGitHubAccounts(accounts map[string]string) ValidateOption {
	return func(e *validateEnv) {
		e.githubAccounts = map[string]string{}
		for k, v := range accounts {
			e.githubAccounts[k] = v
		}
	}
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
	env  *validateEnv
}

func (c *checker) add(path, format string, args ...any) {
	c.errs = append(c.errs, FieldError{Path: path, Msg: fmt.Sprintf(format, args...)})
}

// Validate checks a document with its var values against the schema. Read
// tools must be in allowedReads; opts add environment checks such as
// WithGitHubAccounts. It returns every error, in document order; nil means
// valid. Messages never contain var values.
func Validate(doc Doc, vars map[string]string, allowedReads []string, opts ...ValidateOption) []FieldError {
	allowed := map[string]bool{}
	for _, n := range allowedReads {
		allowed[n] = true
	}
	env := &validateEnv{}
	for _, o := range opts {
		if o != nil {
			o(env)
		}
	}
	return validate(doc, vars, allowed, env)
}

// validate checks reads against allowed when it is non-nil, and the
// environment facts env holds when it is non-nil.
func validate(doc Doc, vars map[string]string, allowed map[string]bool, env *validateEnv) FieldErrors {
	c := &checker{env: env}
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
		if !runActionsOnly(doc) {
			c.add("instructions", "is required (it may be empty only when every trigger is %s and the only actions are actions.%s)", TriggerChatMessage, ActionsRun)
		} else if len(doc.Reads) > 0 {
			c.add("reads", "needs instructions: without instructions the model is not called")
		}
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

	labels := c.checkVerdict(doc)
	c.checkActions(doc.Actions, doc.Triggers, doc.Verdict != nil, labels, sub)

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
	if doc.Model != "" && !validModelID(doc.Model) {
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

// checkVerdict checks the verdict block and returns its valid labels (nil
// when the behavior has no verdict).
func (c *checker) checkVerdict(doc Doc) map[string]bool {
	v := doc.Verdict
	if v == nil {
		return nil
	}
	if strings.TrimSpace(doc.Instructions) == "" {
		c.add("verdict", "needs instructions: the verdict comes from the model step")
	}
	if n := len(v.Labels); n < MinVerdictLabels || n > MaxVerdictLabels {
		c.add("verdict.labels", "must have %d to %d labels", MinVerdictLabels, MaxVerdictLabels)
	}
	labels := map[string]bool{}
	for i, l := range v.Labels {
		p := fmt.Sprintf("verdict.labels[%d]", i)
		switch {
		case !VerdictLabelRe.MatchString(l):
			c.add(p, "must be 1-32 of a-z, 0-9 or _, starting with a letter")
		case labels[l]:
			c.add(p, "duplicate label")
		default:
			labels[l] = true
		}
	}
	if m := v.MinConfidence; m != nil && (*m < 0 || *m > 1) {
		c.add("verdict.min_confidence", "must be between 0 and 1")
	}
	return labels
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
	TriggerCommentAdded:      {"list": true, "from": true, "mentions": true},
	TriggerTelegramCommand:   {},
	TriggerChatMessage:       {"channel": true, "from": true, "has_pr_link": true, "senders": true, "mentions": true},
}

// runActionsOnly reports whether a behavior may skip the model step: every
// trigger is a chat message, it has run-level actions and no class
// actions.
func runActionsOnly(doc Doc) bool {
	if len(doc.Triggers) == 0 || len(doc.Actions[ActionsRun]) == 0 {
		return false
	}
	for _, t := range doc.Triggers {
		if t.Type != TriggerChatMessage {
			return false
		}
	}
	for class, list := range doc.Actions {
		if class != ActionsRun && len(list) > 0 {
			return false
		}
	}
	return true
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
		used := map[string]bool{
			"to_status": len(f.ToStatus) > 0, "assignee": f.Assignee != "", "list": len(f.List) > 0, "from": f.From != "",
			"channel": len(f.Channel) > 0, "has_pr_link": f.HasPRLink != nil, "senders": len(f.Senders) > 0,
			"mentions": f.Mentions != "",
		}
		for _, k := range []string{"to_status", "assignee", "list", "from", "channel", "has_pr_link", "senders", "mentions"} {
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
		mentionsMe := false
		if allowed["mentions"] && f.Mentions != "" {
			fp := p + ".filter.mentions"
			if v, ok := sub(fp, f.Mentions); ok {
				if v == MentionsMe {
					mentionsMe = true
				} else {
					c.add(fp, "must be %q", MentionsMe)
				}
			}
		}
		if allowed["channel"] {
			if len(f.Channel) > 3 {
				c.add(p+".filter.channel", "must have at most 3 values")
			}
			seen := map[string]bool{}
			for j, s := range f.Channel {
				fp := fmt.Sprintf("%s.filter.channel[%d]", p, j)
				v, ok := sub(fp, s)
				switch {
				case !ok:
				case v != ChannelDM && v != ChannelGroupDM && v != ChannelChannel:
					c.add(fp, "must be %q, %q or %q", ChannelDM, ChannelGroupDM, ChannelChannel)
				case seen[v]:
					c.add(fp, "duplicate channel")
				case v == ChannelChannel && !mentionsMe:
					// A channel trigger without mentions: me would process
					// every channel message.
					c.add(fp, "%q needs the filter mentions: %s", ChannelChannel, MentionsMe)
				}
				seen[v] = true
			}
		}
		if allowed["senders"] {
			if len(f.Senders) > 100 {
				c.add(p+".filter.senders", "must have at most 100 user IDs")
			}
			for j, s := range f.Senders {
				fp := fmt.Sprintf("%s.filter.senders[%d]", p, j)
				if v, ok := sub(fp, s); ok && !userIDRe.MatchString(v) {
					c.add(fp, "must be a ClickUp user ID")
				}
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

func (c *checker) checkActions(actions map[string][]ActionDoc, triggers []TriggerDoc, hasVerdict bool, labels map[string]bool, sub subFunc) {
	allChat, allPRLink := len(triggers) > 0, len(triggers) > 0
	for _, t := range triggers {
		if t.Type != TriggerChatMessage {
			allChat, allPRLink = false, false
			continue
		}
		if t.Filter.HasPRLink == nil || !*t.Filter.HasPRLink {
			allPRLink = false
		}
	}
	classes := make([]string, 0, len(actions))
	for k := range actions {
		classes = append(classes, k)
	}
	sort.Strings(classes)
	for _, class := range classes {
		list := actions[class]
		cp := "actions." + class
		run := class == ActionsRun
		switch {
		case run:
		case Class(class) == ClassAuto, Class(class) == ClassNotify, Class(class) == ClassAsk:
		default:
			c.add(cp, "unknown class (allowed: auto, notify, ask, %s)", ActionsRun)
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
			if run {
				c.checkRunAction(p, i, len(list), a, allChat, allPRLink)
				c.checkWhen(p, a, hasVerdict, labels)
			} else {
				if runOnlyActions[a.Type] {
					c.add(p+".type", "%s is allowed only in actions.%s", a.Type, ActionsRun)
				}
				if allChat && taskActions[a.Type] {
					c.add(p+".type", "%s needs a ClickUp task, and %s triggers have none", a.Type, TriggerChatMessage)
				}
				if a.On != "" {
					c.add(p+".on", "is allowed only in actions.%s", ActionsRun)
				}
				if a.When != nil {
					c.add(p+".when", "is allowed only in actions.%s", ActionsRun)
				}
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
				case "reaction":
					if !reactionRe.MatchString(v) {
						c.add(pp, "must be a lower-case emoji name of 1-64 of a-z, 0-9, _, + or -")
					}
				case "account":
					c.checkGitHubAccount(pp, v)
				}
			}
		}
	}
}

// checkRunAction checks one run-level action (actions.run[i] of n) beyond
// its parameters.
func (c *checker) checkRunAction(p string, i, n int, a ActionDoc, allChat, allPRLink bool) {
	if !runActionTypes[a.Type] {
		c.add(p+".type", "%s is not allowed in actions.%s (allowed: %s, %s, %s)", a.Type, ActionsRun, ActionPRApprove, ActionChatReaction, ActionTelegramMessage)
		return
	}
	switch a.On {
	case "":
	case OnSuccessOfPrevious:
		if i == 0 {
			c.add(p+".on", "the first action has no previous action")
		}
		if a.Type == ActionTelegramMessage {
			c.add(p+".on", "%s always runs and reports every outcome", ActionTelegramMessage)
		}
	default:
		c.add(p+".on", "must be %q", OnSuccessOfPrevious)
	}
	if a.Type == ActionTelegramMessage && i != n-1 {
		c.add(p+".type", "%s must be the last action of actions.%s", ActionTelegramMessage, ActionsRun)
	}
	if runOnlyActions[a.Type] && !allChat {
		c.add(p+".type", "%s needs every trigger to be %s", a.Type, TriggerChatMessage)
	}
	if a.Type == ActionPRApprove && allChat && !allPRLink {
		c.add(p+".type", "%s needs has_pr_link: true on every trigger", ActionPRApprove)
	}
}

// checkWhen checks the verdict condition of a run-level action. labels
// holds the valid labels of the behavior's verdict block.
func (c *checker) checkWhen(p string, a ActionDoc, hasVerdict bool, labels map[string]bool) {
	w := a.When
	if w == nil {
		return
	}
	wp := p + ".when.verdict"
	switch {
	case a.Type == ActionTelegramMessage:
		c.add(p+".when", "%s always runs and reports every outcome", ActionTelegramMessage)
	case !hasVerdict:
		c.add(p+".when", "needs a verdict block with labels")
	case w.Verdict == "":
		c.add(wp, "is required")
	case !labels[w.Verdict]:
		c.add(wp, "must be one of verdict.labels")
	}
}

// checkGitHubAccount checks a github.pr_approve account name, and when the
// caller supplied the hub's accounts, that it names a "pat" account.
func (c *checker) checkGitHubAccount(path, v string) {
	if !tenant.ValidCredentialRef(v) {
		c.add(path, "must be a GitHub account name")
		return
	}
	if c.env == nil || c.env.githubAccounts == nil {
		return
	}
	switch typ, ok := c.env.githubAccounts[v]; {
	case !ok:
		c.add(path, "unknown GitHub account (add it with the github-account command)")
	case typ != GitHubAccountPAT:
		c.add(path, "must be a GitHub account of type %s (an app account approves as a bot)", GitHubAccountPAT)
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

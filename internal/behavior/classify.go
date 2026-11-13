package behavior

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Classification rules stored on each item: "<class>:<reason>".
const (
	RuleAutoAllChecks       = "auto:all_checks"
	RuleNotifyInformational = "notify:informational"
	RuleAskInformational    = "ask:informational"    // informational, but notify_when_informational is off
	RuleAskInstructionLike  = "ask:instruction_like" // cited context or the item reads like instructions to the AI
	RuleAskTopicAlwaysAsk   = "ask:topic_always_ask" // the topic is in always_ask_topics
	RuleAskCitationMissing  = "ask:citation_missing" // require_citations and no citation
	RuleAskCitationInvalid  = "ask:citation_invalid" // require_citations and a quote is not at its locator
	RuleAskLowConfidence    = "ask:low_confidence"   // confidence below confidence_threshold
	RuleAskNoProposal       = "ask:no_proposal"      // not informational and nothing to act on
)

// Item flags.
const (
	FlagInstructionLike = "instruction_like"
	FlagCitationInvalid = "citation_invalid"
)

// Citation verification.
//
// A locator is "<source>:L<n>" or "<source>:L<a>-L<b>" (also "L<a>-<b>"),
// naming lines of one snapshot fragment. A citation is valid when its
// quote, with every whitespace run collapsed to one space and the ends
// trimmed, appears in the fragment's lines a-2 .. b+2 (a tolerance of two
// lines either side, joined with single spaces), and the quote is at least
// MinQuoteChars long or equals one whole line. A range may span at most
// MaxCitedLines lines.
const (
	CitationLineTolerance = 2
	MaxCitedLines         = 50
	MinQuoteChars         = 8
)

var locatorRe = regexp.MustCompile(`^(.+):L([0-9]{1,7})(?:-L?([0-9]{1,7}))?$`)

// citedLines returns the lines a locator names, widened by the tolerance,
// or ok=false when the locator does not name lines of the snapshot.
func citedLines(snap *ContextSnapshot, locator string) ([]string, bool) {
	m := locatorRe.FindStringSubmatch(locator)
	if m == nil {
		return nil, false
	}
	f := snap.Fragment(m[1])
	if f == nil {
		return nil, false
	}
	a, _ := strconv.Atoi(m[2])
	b := a
	if m[3] != "" {
		b, _ = strconv.Atoi(m[3])
	}
	if a < 1 || b < a || b-a+1 > MaxCitedLines || a > len(f.Lines) {
		return nil, false
	}
	lo := max(a-CitationLineTolerance, 1)
	hi := min(b+CitationLineTolerance, len(f.Lines))
	return f.Lines[lo-1 : hi], true
}

func normalizeSpace(s string) string { return strings.Join(strings.Fields(s), " ") }

// VerifyCitation reports whether the quote appears at the locator. window
// is the text the locator names (with tolerance), also when the quote does
// not match; nil when the locator names nothing.
func VerifyCitation(snap *ContextSnapshot, c Citation) (valid bool, window []string) {
	ls, ok := citedLines(snap, c.Locator)
	if !ok {
		return false, nil
	}
	q := normalizeSpace(c.Quote)
	if q == "" {
		return false, ls
	}
	if chars(q) < MinQuoteChars {
		for _, l := range ls {
			if normalizeSpace(l) == q {
				return true, ls
			}
		}
		return false, ls
	}
	parts := make([]string, 0, len(ls))
	for _, l := range ls {
		if n := normalizeSpace(l); n != "" {
			parts = append(parts, n)
		}
	}
	return strings.Contains(strings.Join(parts, " "), q), ls
}

// instructionPatterns detect text that addresses the AI or tries to steer
// the engine. The heuristic is generic and deliberately small: a match
// only moves an item to ask, never towards auto.
var instructionPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(ignore|disregard|forget|override)\b.{0,30}\b(previous|prior|above|earlier|all|any|your|these|the)\b.{0,20}\b(instructions?|rules?|prompts?|guidelines?|directions?)\b`),
	regexp.MustCompile(`(?im)^\W{0,5}(hey |dear |attention,? )?(ai|assistant|claude|chatgpt|gpt|llm)\s*[,:]`),
	regexp.MustCompile(`(?i)\b(dear|hey|hi|hello|attention)\s+(ai|assistant|claude|chatgpt|gpt|llm|bot)\b`),
	regexp.MustCompile(`(?i)\b(as an? |you are (now )?(an? )?)(ai|assistant|language model|llm|chatbot)\b`),
	regexp.MustCompile(`(?i)\b(system prompt|system instructions?|developer (message|instructions?))\b`),
	regexp.MustCompile(`(?im)^\W{0,5}assistant\s*:`),
	regexp.MustCompile(`(?i)\b(new|updated|real|actual|hidden)\s+instructions?\s*:`),
	regexp.MustCompile(`(?i)\b(mark|classify|label|treat|flag)\b.{0,30}\bas\s+(auto|automatic|approved|high[- ]confidence|confident)\b`),
	regexp.MustCompile(`(?i)\b(set|raise|increase|give)\b.{0,15}\bconfidence\b`),
	regexp.MustCompile(`(?i)\bconfidence\s*[:=]\s*(1(\.0+)?|100\s*%?)(\s|$|[.,;])`),
	youMustPattern,
	regexp.MustCompile(`(?i)\bdo not (tell|inform|notify|ask) (the )?(user|owner|human)\b`),
	regexp.MustCompile(`(?i)[<‹]\s*/?\s*(system|instructions?|context|source|output)\b`),
	regexp.MustCompile(`(?i)(bỏ qua|phớt lờ).{0,30}(hướng dẫn|chỉ dẫn|chỉ thị|quy tắc)`),
	regexp.MustCompile(`(?i)đánh dấu.{0,20}(auto|tự động)`),
}

// youMustPattern is "you must/should/need to approve, merge, reply...".
var youMustPattern = regexp.MustCompile(`(?i)\byou\s+(must|should|need to|have to|are required to)\s+(post|publish|approve|merge|reply|answer|mark|classify)\b`)

// verdictInstructionPatterns are applied to the untrusted content a
// verdict classifies (the triggering message). They are the item patterns
// without youMustPattern: in a message written to the owner, "you" is the
// owner, so "you must approve this PR" is an ordinary (if pushy) human
// request, which is exactly what a verdict classifies. They add patterns
// that try to set the verdict itself. Item classification keeps its own
// list unchanged.
var verdictInstructionPatterns = func() []*regexp.Regexp {
	var out []*regexp.Regexp
	for _, re := range instructionPatterns {
		if re != youMustPattern {
			out = append(out, re)
		}
	}
	return append(out,
		regexp.MustCompile(`(?i)\b(verdict|label)\s*["']?\s*[:=]`),
		regexp.MustCompile(`(?i)\b(set|choose|return|output)\b.{0,20}\b(verdict|label)\b`),
	)
}()

// InstructionLike reports whether any of the texts reads like instructions
// to the AI or the engine.
func InstructionLike(texts ...string) bool { return matchAny(instructionPatterns, texts) }

// VerdictInstructionLike reports whether any of the texts tries to
// instruct the AI or the engine, by the verdict's patterns
// (verdictInstructionPatterns). An ordinary request from a person to the
// owner ("approve giúp em PR này") does not match.
func VerdictInstructionLike(texts ...string) bool { return matchAny(verdictInstructionPatterns, texts) }

func matchAny(patterns []*regexp.Regexp, texts []string) bool {
	for _, t := range texts {
		if t == "" {
			continue
		}
		for _, re := range patterns {
			if re.MatchString(t) {
				return true
			}
		}
	}
	return false
}

// Classification is the engine's decision for one item.
type Classification struct {
	Class Class
	Rule  string
	Flags []string
}

// Classify decides an item's class from the behavior's autonomy rules and
// the engine's checks; the model never chooses it. citationsValid has one
// entry per citation. The order of the checks fixes which rule is stored
// when several fail.
//
//   - informational with no proposal: notify (or ask when
//     notify_when_informational is off);
//   - instruction-like: ask;
//   - topic in always_ask_topics (case-insensitive): ask;
//   - require_citations and no citation, or any invalid one: ask;
//   - confidence below the threshold: ask;
//   - not informational and no proposal: ask (nothing to act on);
//   - otherwise auto.
func Classify(it ModelItem, rules AutonomyRules, citationsValid []bool, instructionLike bool) Classification {
	var flags []string
	if instructionLike {
		flags = append(flags, FlagInstructionLike)
	}
	anyInvalid := false
	for _, ok := range citationsValid {
		if !ok {
			anyInvalid = true
		}
	}
	if anyInvalid {
		flags = append(flags, FlagCitationInvalid)
	}
	sort.Strings(flags)
	c := func(class Class, rule string) Classification {
		return Classification{Class: class, Rule: rule, Flags: flags}
	}
	if it.Informational && strings.TrimSpace(it.Proposal) == "" {
		if rules.NotifyWhenInformational {
			return c(ClassNotify, RuleNotifyInformational)
		}
		return c(ClassAsk, RuleAskInformational)
	}
	if instructionLike {
		return c(ClassAsk, RuleAskInstructionLike)
	}
	topic := strings.TrimSpace(it.Topic)
	for _, t := range rules.AlwaysAskTopics {
		if strings.EqualFold(strings.TrimSpace(t), topic) {
			return c(ClassAsk, RuleAskTopicAlwaysAsk)
		}
	}
	if rules.RequireCitations {
		if len(it.Citations) == 0 {
			return c(ClassAsk, RuleAskCitationMissing)
		}
		if anyInvalid {
			return c(ClassAsk, RuleAskCitationInvalid)
		}
	}
	if !(it.Confidence >= rules.ConfidenceThreshold) {
		return c(ClassAsk, RuleAskLowConfidence)
	}
	if strings.TrimSpace(it.Proposal) == "" {
		return c(ClassAsk, RuleAskNoProposal)
	}
	return c(ClassAuto, RuleAutoAllChecks)
}

package behavior

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/LDKhangg/nexus-hub/internal/tenant"
)

// This file holds the clickup.chat_message trigger: its subject, the
// deterministic pull request extractor the chat poller uses, and the
// trigger's filter matching.

// SubjectChatPrefix starts the subject of a run about one ClickUp Chat
// message.
const SubjectChatPrefix = "clickup:chat:"

// ChatSubject returns the run subject, and the event key, of a ClickUp Chat
// message.
func ChatSubject(messageID string) string { return SubjectChatPrefix + messageID }

// MaxPRRefs caps the pull requests taken from one message.
const MaxPRRefs = 5

// maxPRNumber caps a pull request number.
const maxPRNumber = 1000000000

// chatIDRe accepts ClickUp Chat message and channel IDs.
var chatIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// prURLRe finds GitHub pull request URLs. What follows the number is
// checked separately, so "/pull/12abc" is not read as #12.
var prURLRe = regexp.MustCompile(`https://github\.com/([A-Za-z0-9][A-Za-z0-9-]{0,38})/([A-Za-z0-9._-]{1,100})/pull/([0-9]{1,10})`)

// PRRef is one GitHub pull request: "owner/repo" and its number.
type PRRef struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
}

// String returns "owner/repo#n".
func (p PRRef) String() string { return p.Repo + "#" + strconv.Itoa(p.Number) }

// ExtractPRRefs returns the GitHub pull requests linked from text
// (https://github.com/<owner>/<repo>/pull/<n>, optionally followed by a
// path, query or fragment such as /files or #discussion), in order of first
// appearance, deduplicated (repo names compare case-insensitively), at most
// MaxPRRefs. The owner and repo must be valid GitHub names and n in
// 1..1e9. It is deterministic and keeps nothing else from text.
func ExtractPRRefs(text string) []PRRef {
	var out []PRRef
	seen := map[string]bool{}
	for _, m := range prURLRe.FindAllStringSubmatchIndex(text, -1) {
		if end := m[1]; end < len(text) && prURLTailRejects(text[end]) {
			continue
		}
		repo := text[m[2]:m[3]] + "/" + text[m[4]:m[5]]
		n, err := strconv.Atoi(text[m[6]:m[7]])
		if err != nil || n < 1 || n > maxPRNumber || !tenant.ValidRepo(repo) {
			continue
		}
		key := strings.ToLower(repo) + "#" + strconv.Itoa(n)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, PRRef{Repo: repo, Number: n})
		if len(out) == MaxPRRefs {
			break
		}
	}
	return out
}

// prURLTailRejects reports whether the byte after a matched pull request
// number continues the number or a name, so the match is not a PR URL.
func prURLTailRejects(b byte) bool {
	return b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b == '_' || b == '-'
}

// matchChat applies a clickup.chat_message trigger's filters. A message
// from the owner never matches, whatever "from" says; "others" (the
// default) cannot tell the owner apart when the owner is not set, so it
// does not match then.
func matchChat(t ResolvedTrigger, ev Event, owner string) bool {
	if !chatIDRe.MatchString(ev.MessageID) || ev.SenderID == "" {
		return false
	}
	if owner != "" && ev.SenderID == owner {
		return false
	}
	if t.From != FromAny && owner == "" {
		return false
	}
	channels := t.Channels
	if len(channels) == 0 {
		channels = []string{ChannelDM}
	}
	ok := false
	for _, c := range channels {
		if strings.EqualFold(c, ev.ChannelType) {
			ok = true
			break
		}
	}
	if !ok {
		return false
	}
	if t.HasPRLink && len(ev.PRs) == 0 {
		return false
	}
	if len(t.Senders) > 0 && !containsFold(t.Senders, ev.SenderID, false) {
		return false
	}
	if t.Mentions == MentionsMe && !ev.Mentioned {
		return false
	}
	// A channel message never matches without mentions: me (validation
	// requires it); this keeps the hub from acting on every channel
	// message even if a stored document skipped validation.
	if strings.EqualFold(ev.ChannelType, ChannelChannel) && t.Mentions != MentionsMe {
		return false
	}
	return true
}

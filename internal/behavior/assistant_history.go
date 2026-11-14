package behavior

import (
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/LDKhangg/nexus-hub/internal/domain"
)

// The assistant's chat history: the owner's recent questions and the
// assistant's final answers, per workspace and chat, in memory only. It is never stored,
// never logged and is lost on restart. Tool calls and tool results are not
// kept (the answers already carry what the owner saw). When a cap is hit
// the oldest exchanges are dropped; a short, bounded digest of the dropped
// questions stays, so the model knows what came earlier.

const (
	// assistantHistoryTurns is the number of exchanges (owner message plus
	// answer) kept per workspace and chat.
	assistantHistoryTurns = 20
	// assistantHistoryBytes bounds the transmitted history of one workspace/chat,
	// including digest formatting.
	assistantHistoryBytes = 64 << 10
	// assistantHistoryAnswerRunes bounds one kept answer.
	assistantHistoryAnswerRunes = 12000
	// assistantDigestItems and assistantDigestRunes bound the digest of
	// dropped questions.
	assistantDigestItems = 8
	assistantDigestRunes = 120

	// assistantPartUnits is the size of one sent part, in UTF-16 units
	// (Telegram counts those; its limit is 4096 and the client clips at
	// 4000).
	assistantPartUnits = 3900
)

type assistantExchange struct {
	question, answer string
}

func (e assistantExchange) size() int { return len(e.question) + len(e.answer) }

// assistantHistory is one workspace/chat's bounded history.
type assistantHistory struct {
	mu        sync.Mutex
	exchanges []assistantExchange
	dropped   int      // exchanges dropped so far
	digest    []string // short excerpts of the newest dropped questions
}

type assistantHistoryKey struct {
	workspaceID string
	chatID      int64
}

// chatHistory returns the workspace/chat history, creating it.
func (a *Assistant) chatHistory(workspaceID string, chatID int64) *assistantHistory {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.history == nil {
		a.history = map[assistantHistoryKey]*assistantHistory{}
	}
	key := assistantHistoryKey{workspaceID: workspaceID, chatID: chatID}
	h := a.history[key]
	if h == nil {
		h = &assistantHistory{}
		a.history[key] = h
	}
	return h
}

// add keeps one exchange and trims the oldest ones over the caps.
func (h *assistantHistory) add(question, answer string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.exchanges = append(h.exchanges, assistantExchange{question: question, answer: capRunes(answer, assistantHistoryAnswerRunes)})
	for len(h.exchanges) > assistantHistoryTurns || (len(h.exchanges) > 1 && h.messageBytes() > assistantHistoryBytes) {
		old := h.exchanges[0]
		h.exchanges = h.exchanges[1:]
		h.dropped++
		h.digest = append(h.digest, oneLine(old.question, assistantDigestRunes))
		if len(h.digest) > assistantDigestItems {
			h.digest = h.digest[len(h.digest)-assistantDigestItems:]
		}
	}
	if len(h.exchanges) == 1 {
		prefix := ""
		if h.dropped > 0 {
			prefix = h.digestNote()
		}
		budget := max(assistantHistoryBytes-len(prefix), 0)
		h.exchanges[0] = fitAssistantExchange(h.exchanges[0], budget)
	}
}

func (h *assistantHistory) messageBytes() int {
	total := 0
	for _, e := range h.exchanges {
		total += e.size()
	}
	if h.dropped > 0 && len(h.exchanges) > 0 {
		total += len(h.digestNote())
	}
	return total
}

func fitAssistantExchange(e assistantExchange, budget int) assistantExchange {
	if e.size() <= budget {
		return e
	}
	qLimit := min(len(e.question), budget/2)
	aLimit := min(len(e.answer), budget-qLimit)
	qLimit = min(len(e.question), budget-aLimit)
	return assistantExchange{
		question: truncateAssistantBytes(e.question, qLimit),
		answer:   truncateAssistantBytes(e.answer, aLimit),
	}
}

func truncateAssistantBytes(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(s) <= limit {
		return s
	}
	for limit > 0 && !utf8.RuneStart(s[limit]) {
		limit--
	}
	return s[:limit]
}

// messages returns the history as alternating user/assistant messages,
// oldest first. When exchanges were dropped, the first question carries a
// note with the digest.
func (h *assistantHistory) messages() []domain.Message {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]domain.Message, 0, 2*len(h.exchanges))
	for i, e := range h.exchanges {
		q := e.question
		if i == 0 && h.dropped > 0 {
			q = h.digestNote() + q
		}
		out = append(out, domain.Message{Role: domain.UserRole, Text: q}, domain.Message{Role: domain.AIRole, Text: e.answer})
	}
	return out
}

func (h *assistantHistory) digestNote() string {
	var b strings.Builder
	b.WriteString("[Hub note: ")
	b.WriteString(strconv.Itoa(h.dropped))
	b.WriteString(" earlier exchange(s) of this chat were dropped from memory.")
	if len(h.digest) > 0 {
		b.WriteString(" The owner's latest dropped questions, oldest first:")
		for _, d := range h.digest {
			b.WriteString("\n- ")
			b.WriteString(d)
		}
	}
	b.WriteString("]\n\n")
	return b.String()
}

// cleanAssistantReply makes the model's reply safe plain text: no control
// or bidi characters (line breaks and tabs kept), @-mentions broken. There
// is no length cap: splitAssistantReply sends long answers in parts.
func cleanAssistantReply(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case unicode.IsControl(r) || isAssistantBidi(r):
			b.WriteRune(' ')
		case r == '@':
			b.WriteString("@​")
		default:
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}

func isAssistantBidi(r rune) bool {
	return (r >= '‪' && r <= '‮') || (r >= '⁦' && r <= '⁩') || r == '‎' || r == '‏'
}

// assistantUTF16Len is the length of s in UTF-16 units, as Telegram counts it.
func assistantUTF16Len(s string) int {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
	}
	return n
}

// splitAssistantReply splits text into ordered parts of at most
// assistantPartUnits UTF-16 units, preferring paragraph, then line, then
// word boundaries. Every part is returned: a legitimate long answer is not
// silently truncated.
func splitAssistantReply(text string) []string {
	var parts []string
	rest := strings.TrimSpace(text)
	for rest != "" {
		if assistantUTF16Len(rest) <= assistantPartUnits {
			parts = append(parts, rest)
			break
		}
		cut := assistantSplitPoint(rest, assistantPartUnits)
		part := strings.TrimRight(rest[:cut], " \t\n")
		if part != "" {
			parts = append(parts, part)
		}
		rest = strings.TrimLeft(rest[cut:], " \t\n")
	}
	return parts
}

// assistantSplitPoint returns a byte index in s where the prefix fits in
// units UTF-16 units: the last paragraph break, else line break, else
// space in the second half of the window, else the hard limit.
func assistantSplitPoint(s string, units int) int {
	limit, n := 0, 0
	for i, r := range s {
		w := utf16.RuneLen(r)
		if n+w > units {
			break
		}
		n += w
		limit = i + utf8.RuneLen(r)
	}
	if limit == 0 {
		_, size := utf8.DecodeRuneInString(s)
		return size
	}
	window := s[:limit]
	half := limit / 2
	for _, sep := range []string{"\n\n", "\n", " "} {
		if i := strings.LastIndex(window, sep); i > half {
			return i + len(sep)
		}
	}
	return limit
}

package behavior

import (
	"context"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/LDKhangg/nexus-hub/internal/domain"
	"github.com/LDKhangg/nexus-hub/internal/people"
)

// draft_clickup_comment (Phase 10 REQ-8): the model only drafts. The tool
// calls ClickUpDrafts.Create, which stores a draft and sends the owner an
// exact preview with Đăng / Sửa / Huỷ; only the owner's Đăng posts it. The
// task, the parent comment and the user ids to tag never come from the
// model: the task is the topic's, the parent comes from the Telegram reply
// metadata and the ids from the people directory.

const (
	toolDraftClickUpComment = "draft_clickup_comment"
	// assistantMentionNameRunes bounds one mention name.
	assistantMentionNameRunes = 80
	// assistantMentionCandidates bounds the candidates told to the model.
	assistantMentionCandidates = 5
	// assistantMentionSearchLimit bounds one directory search. A full page
	// may hide another match, so it never resolves a name by itself.
	assistantMentionSearchLimit = 50
)

var draftClickUpCommentTool = domain.ToolDef{Name: toolDraftClickUpComment,
	Description: "Draft a comment on a ClickUp task for the owner to review. It never posts: the owner gets a preview with Đăng / Sửa / Huỷ and only the owner's Đăng posts it. " +
		"Use it when the owner asks to comment on the task, tag someone, ask someone something on the task or change its status, for example \"comment lên task là …\", \"tag anh X hỏi deadline\", \"đổi status sang review\". " +
		"Inside a task topic the task is the topic's task; elsewhere pass task_id. Put the comment in text, in the owner's words. " +
		"mentions are the names of people to tag; the hub finds them, never pass user ids. If the result lists candidates or says a name was not found, ask the owner which person and call the tool again. " +
		"new_status is an optional status to set after the comment. The result only says a preview is waiting for the owner; never tell the owner the comment is posted.",
	InputSchema: toolSchema(`"text":{"type":"string","description":"the comment text"},"task_id":{"type":"string","description":"task ID, custom task ID or ClickUp task URL; optional inside a task topic"},"mentions":{"type":"array","maxItems":5,"items":{"type":"string"},"description":"names of people to tag"},"new_status":{"type":"string","description":"optional status to set after the comment"}`, "text")}

// draftToolAvailable reports whether draft_clickup_comment is offered.
func (w *InboxWorker) draftToolAvailable() bool {
	return w.d.ReportDrafts != nil
}

// stringList returns a string-array argument, trimmed, at most max items of
// at most runes runes each.
func (a toolArgs) stringList(key string, max, runes int) ([]string, error) {
	v, ok := a[key]
	if !ok || v == nil {
		return nil, nil
	}
	items, ok := v.([]any)
	if !ok || len(items) > max {
		return nil, errToolArgs
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		s, ok := it.(string)
		if !ok {
			return nil, errToolArgs
		}
		s = strings.TrimSpace(s)
		if s == "" || utf8.RuneCountInString(s) > runes {
			return nil, errToolArgs
		}
		out = append(out, s)
	}
	return out, nil
}

// toolDraftComment makes one ClickUp comment draft. The result has status
// codes only, never the text.
func (w *InboxWorker) toolDraftComment(ctx context.Context, tt *assistantToolTurn, args toolArgs) (string, bool) {
	text, err := args.strMax("text", maxDraftRunes)
	if err != nil || text == "" {
		return toolErr(errToolArgs)
	}
	rawTask, err := args.str("task_id")
	if err != nil {
		return toolErr(errToolArgs)
	}
	status, err := args.strMax("new_status", 100)
	if err != nil || !validDraftStatus(status) {
		return toolErr(errToolArgs)
	}
	names, err := args.stringList("mentions", maxDraftMentions, assistantMentionNameRunes)
	if err != nil {
		return toolErr(errToolArgs)
	}
	if !w.draftToolAvailable() {
		return toolJSON(map[string]any{"status": planStatusNotAvailable})
	}
	taskID, status2 := w.draftTask(ctx, tt, rawTask)
	if status2 != "" {
		return toolJSON(map[string]any{"status": status2})
	}
	parent, status2 := w.draftParent(ctx, tt, taskID)
	if status2 != "" {
		return toolJSON(map[string]any{"status": status2})
	}
	ids, report := w.resolveMentions(ctx, tt.u.WorkspaceID, names)
	if report != nil {
		return toolJSON(report)
	}
	key := toolDraftClickUpComment + "|" + taskID + "|" + draftHash(text+"\x00"+status+"\x00"+strings.Join(ids, ","))
	if tt.proposed == nil {
		tt.proposed = make(map[string]bool)
	}
	if tt.proposed[key] {
		return toolJSON(map[string]any{"status": "already_drafted", "posted": false})
	}
	if tt.proposals >= assistantMaxProposals {
		return toolJSON(map[string]any{"status": "too_many_drafts", "posted": false})
	}
	draftID, err := w.d.ReportDrafts.Create(ctx, DraftRequest{
		Chat: tt.to, TaskID: taskID, Text: text, ParentCommentID: parent, MentionUserIDs: ids,
		NewStatus: status, Source: DraftSourceAssistant,
	})
	switch {
	case errors.Is(err, ErrInvalid):
		return toolErr(errToolArgs)
	case err != nil && draftID > 0:
		tt.proposed[key] = true
		tt.proposals++
		return toolJSON(map[string]any{"status": "preview_not_sent", "posted": false})
	case err != nil:
		return toolJSON(map[string]any{"status": "draft_failed", "posted": false})
	}
	tt.proposed[key] = true
	tt.proposals++
	return toolJSON(map[string]any{"status": "preview_waiting", "posted": false, "threaded_reply": parent != ""})
}

// draftTask picks the draft's task: the topic's task (an explicit task must
// be the same one), else the explicit task. The status is "" on success.
func (w *InboxWorker) draftTask(ctx context.Context, tt *assistantToolTurn, rawTask string) (string, string) {
	var explicit string
	if rawTask != "" {
		ref, ok := taskURLTaskRef(rawTask)
		if !ok {
			ref, ok = ParseTaskRefTeam(rawTask)
		}
		if !ok {
			return "", "task_invalid"
		}
		id, err := w.resolveTask(ctx, tt.u.WorkspaceID, ref)
		if err != nil || !taskIDRe.MatchString(id) {
			return "", "task_unreadable"
		}
		explicit = id
	}
	ref, ok, err := w.topicTaskRefChecked(ctx, tt.u)
	if err != nil {
		// Never fall back to the model's task when the topic is unreadable.
		return "", "task_unreadable"
	}
	if ok {
		if explicit != "" && explicit != ref.ID {
			return "", "task_mismatch"
		}
		return ref.ID, ""
	}
	if explicit == "" {
		return "", "task_required"
	}
	return explicit, ""
}

// draftParent returns the ClickUp comment the owner's message replies to,
// from the trusted reply metadata. "" when it replies to no comment; a
// status when the lookup failed or the comment is of another task.
func (w *InboxWorker) draftParent(ctx context.Context, tt *assistantToolTurn, taskID string) (string, string) {
	if tt.u.ReplyToMessageID <= 0 || w.d.CommentLinks == nil {
		return "", ""
	}
	link, ok, err := lookupReplyComment(ctx, w.d.CommentLinks, tt.u.WorkspaceID, tt.u.IntegrationID, tt.u.ChatID, tt.u.ReplyToMessageID)
	if err != nil {
		return "", "reply_unreadable"
	}
	if !ok {
		return "", ""
	}
	if link.TaskID != taskID {
		return "", "reply_task_mismatch"
	}
	return link.CommentID, ""
}

// resolveMentions turns names into ClickUp user ids through the people
// directory. report is non-nil (and no draft is made) when a name is
// unknown or ambiguous: it lists the candidates for the model to ask.
func (w *InboxWorker) resolveMentions(ctx context.Context, workspaceID string, names []string) ([]string, map[string]any) {
	if len(names) == 0 {
		return nil, nil
	}
	if w.d.People == nil {
		return nil, map[string]any{"status": "mentions_not_available"}
	}
	var (
		ids    []string
		seen   = map[string]bool{}
		report []map[string]any
	)
	for _, name := range names {
		contacts, err := w.d.People.List(ctx, workspaceID, "clickup", escapeLikeTerm(name), assistantMentionSearchLimit)
		if err != nil {
			return nil, map[string]any{"status": "mentions_unreadable"}
		}
		pick, certain := pickMentionContacts(contacts, name)
		switch {
		case len(pick) == 0:
			report = append(report, map[string]any{"name": oneLine(name, assistantMentionNameRunes), "result": "not_found"})
		case len(pick) == 1 && certain:
			id := clickUpAccountID(pick[0])
			if id == "" {
				// One person with several ClickUp accounts: never guess.
				report = append(report, map[string]any{"name": oneLine(name, assistantMentionNameRunes), "result": "ambiguous",
					"candidates": []string{oneLine(people.CleanDisplayName(pick[0].Name), 80)}})
				break
			}
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		default:
			cands := make([]string, 0, assistantMentionCandidates)
			for _, c := range pick {
				if len(cands) == assistantMentionCandidates {
					break
				}
				cands = append(cands, oneLine(people.CleanDisplayName(c.Name), 80))
			}
			report = append(report, map[string]any{"name": oneLine(name, assistantMentionNameRunes), "result": "ambiguous", "candidates": cands})
		}
	}
	if len(report) > 0 {
		return nil, map[string]any{"status": "mentions_unresolved", "posted": false, "names": report}
	}
	return ids, nil
}

// clickUpAccountIDs lists the numeric ClickUp user ids of a contact.
func clickUpAccountIDs(c people.Contact) []string {
	var out []string
	for _, a := range c.Accounts {
		if a.Platform == "clickup" && draftCommentIDRe.MatchString(a.ExternalID) && strings.TrimLeft(a.ExternalID, "0") != "" {
			out = append(out, a.ExternalID)
		}
	}
	return out
}

// clickUpAccountID is the single ClickUp user id of a contact.
func clickUpAccountID(c people.Contact) string {
	if ids := clickUpAccountIDs(c); len(ids) == 1 {
		return ids[0]
	}
	return ""
}

// clickUpContacts keeps the contacts that have a ClickUp user id.
func clickUpContacts(cs []people.Contact) []people.Contact {
	var out []people.Contact
	for _, c := range cs {
		if len(clickUpAccountIDs(c)) > 0 {
			out = append(out, c)
		}
	}
	return out
}

// pickMentionContacts returns the contacts a name may mean. certain is true
// only for an exact match or a whole-word match with exactly one contact,
// on a search that was not cut off; a substring-only match is never certain.
func pickMentionContacts(contacts []people.Contact, name string) ([]people.Contact, bool) {
	complete := len(contacts) < assistantMentionSearchLimit
	matches := clickUpContacts(contacts)
	if exact := exactContacts(matches, name); len(exact) > 0 {
		return exact, complete
	}
	if words := wholeWordContacts(matches, name); len(words) > 0 {
		return words, complete && len(words) == 1
	}
	return matches, false
}

// contactNames lists the names a contact is known by on ClickUp.
func contactNames(c people.Contact) []string {
	names := []string{c.Name, c.Alias}
	for _, a := range c.Accounts {
		if a.Platform == "clickup" {
			names = append(names, a.PlatformName, a.Username)
		}
	}
	return names
}

// exactContacts keeps the contacts whose full name, alias or username is
// name, ignoring case.
func exactContacts(cs []people.Contact, name string) []people.Contact {
	var out []people.Contact
	for _, c := range cs {
		for _, known := range contactNames(c) {
			if known != "" && strings.EqualFold(known, name) {
				out = append(out, c)
				break
			}
		}
	}
	return out
}

// wholeWordContacts keeps the contacts with a name that contains the words
// of name in order, as whole words ("Nam" is in "Nguyễn Văn Nam", "An" is
// not in "Hoàng Anh").
func wholeWordContacts(cs []people.Contact, name string) []people.Contact {
	want := nameWords(name)
	if len(want) == 0 {
		return nil
	}
	var out []people.Contact
	for _, c := range cs {
		for _, known := range contactNames(c) {
			if containsWords(nameWords(known), want) {
				out = append(out, c)
				break
			}
		}
	}
	return out
}

// nameWords splits a name into words of letters, digits and combining marks.
func nameWords(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && !unicode.IsMark(r)
	})
}

// containsWords reports whether want occurs in words as a contiguous run.
func containsWords(words, want []string) bool {
	for i := 0; i+len(want) <= len(words); i++ {
		match := true
		for j := range want {
			if !strings.EqualFold(words[i+j], want[j]) {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// likeTermEscaper escapes the LIKE wildcards of a search term.
var likeTermEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// escapeLikeTerm makes a name match literally in the directory's ILIKE.
func escapeLikeTerm(s string) string { return likeTermEscaper.Replace(s) }

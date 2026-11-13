package behavior

import (
	"encoding/json"
	"log"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/LDKhangg/nexus-hub/internal/domain"
)

// This file matches source events against the triggers of enabled behavior
// versions and turns each match into a run request. It knows trigger types
// and filter fields, never what a behavior does with them.

// SubjectTaskPrefix starts the subject of a run about one ClickUp task.
const SubjectTaskPrefix = "clickup:task:"

// TaskSubject returns the run subject of a ClickUp task.
func TaskSubject(taskID string) string { return SubjectTaskPrefix + taskID }

// taskIDRe accepts ClickUp task IDs, including custom IDs such as "AB-12".
var taskIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// Event is a source event reduced to the identifiers triggers look at. It
// carries no message text.
type Event struct {
	Type           string // a Trigger* constant
	Key            string // unique per source event; the run revision
	IntegrationID  string // the source integration
	TaskID         string
	ListID         string // empty when the source did not say
	StatusTo       string
	StatusFrom     string
	AssigneesAdded []string
	SenderID       string
	FromOwnBot     bool
	// Chat message fields (TriggerChatMessage), set by the chat poller from
	// trusted identifiers only. Key is ChatSubject(MessageID).
	MessageID string
	ChannelID string
	// ChannelType is the chat channel type ("dm"/"group_dm", or ClickUp's
	// "DM"/"GROUP_DM"); it is matched case-insensitively.
	ChannelType string
	// PRs are the pull requests linked from the message (ExtractPRRefs).
	PRs []PRRef
}

// EventFromNormalized maps a normalized ClickUp event to a trigger event.
// ok is false for an event no trigger type takes.
func EventFromNormalized(e domain.NormalizedEvent) (Event, bool) {
	if e.Source != domain.SourceClickup {
		return Event{}, false
	}
	ev := Event{
		Key: e.EventKey, IntegrationID: e.IntegrationID, TaskID: e.ExternalID, ListID: e.Metadata["list_id"],
		SenderID: e.SenderID, FromOwnBot: e.FromOwnBot,
	}
	switch e.EventType {
	case domain.EventStatusChanged:
		ev.Type = TriggerTaskStatusChanged
		ev.StatusTo, ev.StatusFrom = e.Metadata["status_to"], e.Metadata["status_from"]
		if ev.StatusTo == "" {
			return Event{}, false
		}
	case domain.EventAssigneeChanged:
		ev.Type = TriggerTaskAssigned
		for _, id := range strings.Split(e.Metadata["assignee_added"], ",") {
			if id = strings.TrimSpace(id); id != "" {
				ev.AssigneesAdded = append(ev.AssigneesAdded, id)
			}
		}
		if len(ev.AssigneesAdded) == 0 {
			return Event{}, false
		}
	case domain.EventCommandPosted:
		ev.Type = TriggerCommentAdded
	default:
		return Event{}, false
	}
	return ev, true
}

// TriggerData is what a run stores about what started it: trusted
// identifiers only, never message or context text.
type TriggerData struct {
	Type          string `json:"type"`
	EventKey      string `json:"event_key,omitempty"`
	IntegrationID string `json:"integration_id,omitempty"`
	TaskID        string `json:"task_id,omitempty"`
	ListID        string `json:"list_id,omitempty"`
	StatusTo      string `json:"status_to,omitempty"`
	StatusFrom    string `json:"status_from,omitempty"`
	Assignee      string `json:"assignee,omitempty"` // the added assignee that matched
	SenderID      string `json:"sender_id,omitempty"`
	// NeedsAssigneeCheck: the event did not carry the task's assignees.
	// The executor must confirm that AssigneeCheck is assigned to the task
	// (a read of the task) before any model work, and finish the run as
	// skipped when not.
	NeedsAssigneeCheck bool   `json:"needs_assignee_check,omitempty"`
	AssigneeCheck      string `json:"assignee_check,omitempty"`
	// NeedsListCheck: the event did not carry the task's list. The
	// executor must confirm the task is in one of ListCheck first.
	NeedsListCheck   bool     `json:"needs_list_check,omitempty"`
	ListCheck        []string `json:"list_check,omitempty"`
	Command          string   `json:"command,omitempty"`
	TelegramChatID   int64    `json:"telegram_chat_id,omitempty"`
	TelegramUpdateID int64    `json:"telegram_update_id,omitempty"`
	Actor            string   `json:"actor,omitempty"`
	// Chat message fields (TriggerChatMessage). SenderID above is the
	// message's ClickUp user. The message text is never stored here.
	MessageID   string  `json:"message_id,omitempty"`
	ChannelID   string  `json:"channel_id,omitempty"`
	ChannelType string  `json:"channel_type,omitempty"` // "dm" or "group_dm"
	PRs         []PRRef `json:"prs,omitempty"`
}

// DecodeTriggerData reads a run's stored trigger data.
func DecodeTriggerData(raw json.RawMessage) (TriggerData, error) {
	var t TriggerData
	if len(raw) == 0 {
		return t, nil
	}
	err := json.Unmarshal(raw, &t)
	return t, err
}

// Match is one behavior version an event starts.
type Match struct {
	Version Version
	Doc     ResolvedDoc
	Trigger TriggerData
}

// NewRun returns the run request for the match: subject is the task, the
// revision is the event key, so a repeated event never starts a second run
// of the same version.
func (m Match) NewRun(subject, revision string) (NewRun, error) {
	raw, err := json.Marshal(m.Trigger)
	if err != nil {
		return NewRun{}, err
	}
	return NewRun{
		BehaviorID: m.Version.BehaviorID, VersionID: m.Version.ID, Version: m.Version.Version,
		TriggerKind: m.Trigger.Type, Trigger: raw, Subject: subject, Revision: revision,
	}, nil
}

// RunFor returns the run request of a ClickUp event match, or ok=false
// when the event's identifiers cannot make a valid run. A chat message
// run's subject and revision are both ChatSubject(MessageID), so a message
// starts each version at most once.
func (m Match) RunFor(ev Event) (NewRun, bool) {
	if ev.Type == TriggerChatMessage {
		if !chatIDRe.MatchString(ev.MessageID) || ev.Key != ChatSubject(ev.MessageID) {
			return NewRun{}, false
		}
		r, err := m.NewRun(ChatSubject(ev.MessageID), ev.Key)
		if err != nil {
			return NewRun{}, false
		}
		return r, true
	}
	if !taskIDRe.MatchString(ev.TaskID) || strings.TrimSpace(ev.Key) == "" || len(ev.Key) > 512 {
		return NewRun{}, false
	}
	r, err := m.NewRun(TaskSubject(ev.TaskID), ev.Key)
	if err != nil {
		return NewRun{}, false
	}
	return r, true
}

// resolveEnabled resolves each version, skipping (and logging by ID) any
// that no longer resolves, so one broken behavior never blocks the others.
func resolveEnabled(versions []Version) []Match {
	out := make([]Match, 0, len(versions))
	for _, v := range versions {
		doc, err := ResolveVersion(v)
		if err != nil {
			log.Printf("behavior: version %d does not resolve; its triggers are skipped", v.ID)
			continue
		}
		out = append(out, Match{Version: v, Doc: doc})
	}
	return out
}

// MatchEvent returns the enabled versions the event starts, at most one
// match per version (its first matching trigger). ownerClickUpID is the
// owner's ClickUp user ID ("" when not set: filters that need it never
// match). Nexus's own comments never match.
func MatchEvent(ev Event, versions []Version, ownerClickUpID string) []Match {
	if ev.FromOwnBot || ev.Type == "" {
		return nil
	}
	var out []Match
	for _, m := range resolveEnabled(versions) {
		for _, t := range m.Doc.Triggers {
			if t.Type != ev.Type {
				continue
			}
			if data, ok := matchTrigger(t, ev, ownerClickUpID); ok {
				m.Trigger = data
				out = append(out, m)
				break
			}
		}
	}
	return out
}

// resolveAssignee maps an assignee filter to a user ID; ok is false when it
// names the owner and the owner is not set.
func resolveAssignee(filter, owner string) (string, bool) {
	if filter == AssigneeMe {
		return owner, owner != ""
	}
	return filter, true
}

func matchTrigger(t ResolvedTrigger, ev Event, owner string) (TriggerData, bool) {
	data := TriggerData{
		Type: ev.Type, EventKey: ev.Key, IntegrationID: ev.IntegrationID, TaskID: ev.TaskID, ListID: ev.ListID,
		SenderID: ev.SenderID,
	}
	if len(t.Lists) > 0 {
		if ev.ListID == "" {
			data.NeedsListCheck, data.ListCheck = true, append([]string(nil), t.Lists...)
		} else if !containsFold(t.Lists, ev.ListID, false) {
			return TriggerData{}, false
		}
	}
	switch ev.Type {
	case TriggerTaskStatusChanged:
		data.StatusTo, data.StatusFrom = ev.StatusTo, ev.StatusFrom
		if len(t.ToStatus) > 0 && !containsFold(t.ToStatus, ev.StatusTo, true) {
			return TriggerData{}, false
		}
		if t.Assignee != "" {
			id, ok := resolveAssignee(t.Assignee, owner)
			if !ok {
				return TriggerData{}, false
			}
			// A status callback does not carry the task's assignees.
			data.NeedsAssigneeCheck, data.AssigneeCheck = true, id
		}
	case TriggerTaskAssigned:
		want := ""
		if t.Assignee != "" {
			id, ok := resolveAssignee(t.Assignee, owner)
			if !ok {
				return TriggerData{}, false
			}
			want = id
		}
		for _, a := range ev.AssigneesAdded {
			if want == "" || a == want {
				data.Assignee = a
				break
			}
		}
		if data.Assignee == "" {
			return TriggerData{}, false
		}
	case TriggerCommentAdded:
		if t.From == FromOthers {
			// Without the owner's ID "others" cannot be told apart.
			if owner == "" || ev.SenderID == owner {
				return TriggerData{}, false
			}
		}
	case TriggerChatMessage:
		if !matchChat(t, ev, owner) {
			return TriggerData{}, false
		}
		data.TaskID, data.ListID = "", ""
		data.MessageID, data.ChannelID = ev.MessageID, ev.ChannelID
		data.ChannelType = strings.ToLower(ev.ChannelType)
		data.PRs = append([]PRRef(nil), ev.PRs...)
	default:
		return TriggerData{}, false
	}
	return data, true
}

func containsFold(list []string, v string, fold bool) bool {
	for _, s := range list {
		if s == v || (fold && strings.EqualFold(strings.TrimSpace(s), strings.TrimSpace(v))) {
			return true
		}
	}
	return false
}

// ParseTaskRef reads a ClickUp task ID or task URL
// (https://app.clickup.com/t/<id> or https://app.clickup.com/t/<team>/<id>).
func ParseTaskRef(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if taskIDRe.MatchString(s) {
		return s, true
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" || !strings.EqualFold(u.Hostname(), "app.clickup.com") || u.Port() != "" || u.User != nil {
		return "", false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	switch {
	case len(parts) == 2 && parts[0] == "t":
		return parts[1], taskIDRe.MatchString(parts[1])
	case len(parts) == 3 && parts[0] == "t":
		if _, err := strconv.ParseUint(parts[1], 10, 64); err != nil {
			return "", false
		}
		return parts[2], taskIDRe.MatchString(parts[2])
	}
	return "", false
}

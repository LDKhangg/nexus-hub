// Package chatpoll is the ClickUp Chat poller: a service worker that reads
// new direct messages of every ClickUp source whose workspace has an
// enabled clickup.chat_message behavior, turns them into behavior events
// and starts their runs. ClickUp sends no webhook for Chat, so it polls.
//
// Message content is untrusted: it is used only to extract pull request
// references (behavior.ExtractPRRefs) and never leaves the poller, never
// reaches a log, trigger data or an error. Logs carry codes and
// workspace/integration IDs only.
package chatpoll

import (
	"context"
	"errors"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/sources/clickupchat"
	"github.com/LDKhangg/nexus-hub/internal/behavior"
)

// Defaults and limits.
const (
	DefaultInterval     = 30 * time.Second
	DefaultMaxBackoff   = 10 * time.Minute
	DefaultChannelPages = 10
	DefaultMessagePages = 5
	DefaultTimeout      = 2 * time.Minute
	// DefaultCursorLag keeps the cursor this far behind the time the
	// channel list was read, so a message that becomes visible late, or
	// one in a channel that was not listed yet, is still read next poll.
	// Messages inside the lag are read again; run dedupe drops them.
	DefaultCursorLag = 10 * time.Second
	// UnauthorizedNoticeStreak is how many consecutive chat_unauthorized
	// polls send the owner one Telegram notice.
	UnauthorizedNoticeStreak = 3
)

// Poll error codes recorded and logged (REQ-5).
const (
	CodeUnauthorized = clickupchat.CodeUnauthorized
	CodeRateLimited  = clickupchat.CodeRateLimited
	CodeUnavailable  = clickupchat.CodeUnavailable
)

// Target is one ClickUp source integration the poller may read: its
// workspace, integration, ClickUp workspace (team) ID and the credential
// reference of the ClickUp token its agent config uses.
type Target struct {
	WorkspaceID   string
	IntegrationID string
	TeamID        string
	TokenRef      string
}

// Targets lists the enabled ClickUp source integrations, across all
// workspaces, that have a ClickUp team ID and a token reference.
type Targets interface {
	ChatPollTargets(ctx context.Context) ([]Target, error)
}

// Versions returns the latest version of each enabled behavior
// (behavior.DefinitionStore.EnabledVersions).
type Versions interface {
	EnabledVersions(ctx context.Context, workspaceID string) ([]behavior.Version, error)
}

// Secrets resolves a credential reference (secrets.Resolver).
type Secrets interface {
	Resolve(ctx context.Context, workspaceID, ref string) (string, error)
}

// ChatAPI is the part of clickupchat.Client the poller reads with.
type ChatAPI interface {
	DMChannelsSince(ctx context.Context, ws, token string, types []string, sinceMS int64, cursor string) (clickupchat.Page[clickupchat.Channel], error)
	Messages(ctx context.Context, ws, token, channelID, cursor string) (clickupchat.Page[clickupchat.Message], error)
}

// Sender sends one Telegram message (behavior.Messenger.SendMessage).
type Sender interface {
	SendMessage(ctx context.Context, to behavior.ChatRef, m behavior.OutMessage) (int64, error)
}

// Profiles reads the workspace profile for its language.
type Profiles interface {
	GetProfile(ctx context.Context, workspaceID string) (behavior.Profile, error)
}

// Deps are the poller's collaborators. Chats, Messenger and Profiles are
// only used for the unauthorized notice; with Chats or Messenger nil no
// notice is sent. Profiles nil means Vietnamese.
type Deps struct {
	Targets   Targets
	Versions  Versions
	State     behavior.ChatPollStore
	Runs      behavior.ChatRunStarter
	Chat      ChatAPI
	Secrets   Secrets
	Chats     behavior.ChatResolver
	Messenger Sender
	Profiles  Profiles
}

// Settings tune the poller. Zero values take the defaults.
type Settings struct {
	Interval time.Duration // clickup_chat_poll_interval
	// OwnerClickUpUserID drops the owner's own messages; empty drops none
	// here (the run starter applies the stored setting again).
	OwnerClickUpUserID string
	MaxBackoff         time.Duration
	ChannelPages       int           // page cap of the channel list
	MessagePages       int           // page cap of one channel's messages
	Timeout            time.Duration // one integration's poll
	CursorLag          time.Duration
	Now                func() time.Time
}

// Poller polls ClickUp Chat. Run polls once, then every Interval after
// the previous poll ended, so polls never overlap.
type Poller struct {
	d Deps
	s Settings
}

// New builds a poller.
func New(d Deps, s Settings) *Poller {
	if s.Interval <= 0 {
		s.Interval = DefaultInterval
	}
	if s.MaxBackoff <= 0 {
		s.MaxBackoff = DefaultMaxBackoff
	}
	if s.ChannelPages <= 0 {
		s.ChannelPages = DefaultChannelPages
	}
	if s.MessagePages <= 0 {
		s.MessagePages = DefaultMessagePages
	}
	if s.Timeout <= 0 {
		s.Timeout = DefaultTimeout
	}
	if s.CursorLag < 0 {
		s.CursorLag = 0
	} else if s.CursorLag == 0 {
		s.CursorLag = DefaultCursorLag
	}
	if s.Now == nil {
		s.Now = time.Now
	}
	return &Poller{d: d, s: s}
}

// Run polls until ctx ends; it returns nil then. A failed poll never stops
// it.
func (p *Poller) Run(ctx context.Context) error {
	for ctx.Err() == nil {
		p.PollOnce(ctx)
		t := time.NewTimer(p.s.Interval)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C:
		}
	}
	return nil
}

// PollOnce runs one poll of every eligible integration, one after the
// other. One integration's failure does not stop the others.
func (p *Poller) PollOnce(ctx context.Context) {
	targets, err := p.d.Targets.ChatPollTargets(ctx)
	if err != nil {
		if ctx.Err() == nil {
			log.Print("clickup chat poll: listing integrations failed; retrying next poll")
		}
		return
	}
	// Channel types per workspace, read once per poll; nil means the
	// workspace has no enabled chat behavior and is not polled.
	types := map[string][]string{}
	checked := map[string]bool{}
	for _, t := range targets {
		if ctx.Err() != nil {
			return
		}
		if !checked[t.WorkspaceID] {
			checked[t.WorkspaceID] = true
			ct, err := p.channelTypes(ctx, t.WorkspaceID)
			if err != nil {
				if ctx.Err() == nil {
					log.Printf("clickup chat poll: reading behaviors failed workspace=%s", t.WorkspaceID)
				}
				continue
			}
			types[t.WorkspaceID] = ct
		}
		ct := types[t.WorkspaceID]
		if len(ct) == 0 {
			continue
		}
		pctx, cancel := context.WithTimeout(ctx, p.s.Timeout)
		p.pollTarget(pctx, ctx, t, ct)
		cancel()
	}
}

// channelTypes returns the ClickUp channel types ("DM", "GROUP_DM") the
// workspace's enabled clickup.chat_message triggers ask for, sorted; empty
// when there is no such trigger. A version that does not resolve is
// skipped, as the run starter skips it.
func (p *Poller) channelTypes(ctx context.Context, workspaceID string) ([]string, error) {
	versions, err := p.d.Versions.EnabledVersions(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for _, v := range versions {
		doc, err := behavior.ResolveVersion(v)
		if err != nil {
			continue
		}
		for _, t := range doc.Triggers {
			if t.Type != behavior.TriggerChatMessage {
				continue
			}
			channels := t.Channels
			if len(channels) == 0 {
				channels = []string{behavior.ChannelDM}
			}
			for _, c := range channels {
				switch strings.ToLower(strings.TrimSpace(c)) {
				case behavior.ChannelDM:
					set[clickupchat.ChannelDM] = true
				case behavior.ChannelGroupDM:
					set[clickupchat.ChannelGroupDM] = true
				}
			}
		}
	}
	out := make([]string, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Strings(out)
	return out, nil
}

// pending is one new message kept for the run batch.
type pending struct {
	date int64
	ev   behavior.Event
}

// pollTarget polls one integration. ctx carries the per-integration
// timeout; parent is the service context, used to tell a shutdown from a
// timeout.
func (p *Poller) pollTarget(ctx, parent context.Context, t Target, types []string) {
	now := p.s.Now()
	state, fresh, err := p.d.State.InitChatPoll(ctx, t.WorkspaceID, t.IntegrationID, t.TeamID, now.UnixMilli())
	if err != nil {
		if parent.Err() == nil {
			p.logf("storage_failed", t)
		}
		return
	}
	if fresh {
		// First poll (or a new ClickUp workspace): the cursor starts now
		// and older messages are never processed (REQ-2).
		return
	}
	if state.BackoffUntil != nil && state.BackoffUntil.After(now) {
		return
	}
	token, err := p.d.Secrets.Resolve(ctx, t.WorkspaceID, t.TokenRef)
	if err != nil || token == "" {
		if parent.Err() != nil {
			return
		}
		// A missing or unreadable ClickUp credential needs the owner as
		// much as a rejected one does.
		p.fail(ctx, t, CodeUnauthorized)
		return
	}
	// The cursor never passes the time the channel list was read (minus
	// the lag): a channel that gets its first new message after the list
	// was read is found next poll.
	ceiling := now.Add(-p.s.CursorLag).UnixMilli()
	items, complete, code := p.collect(ctx, t, token, types, state.CursorMS)
	if parent.Err() != nil {
		return
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].date < items[j].date })
	events := make([]behavior.Event, 0, len(items))
	for _, it := range items {
		if it.ev.Key != "" {
			events = append(events, it.ev)
		}
	}
	if len(events) > 0 {
		if _, err := p.d.Runs.StartChatRuns(ctx, t.WorkspaceID, events); err != nil {
			// No run of the batch is stored: the cursor stays and the
			// messages are read again next poll.
			if parent.Err() == nil {
				p.logf("storage_failed", t)
			}
			return
		}
	}
	if code != "" {
		p.fail(ctx, t, code)
		return
	}
	if complete {
		maxDate := state.CursorMS
		for _, it := range items {
			if it.date > maxDate {
				maxDate = it.date
			}
		}
		if maxDate > ceiling {
			maxDate = ceiling
		}
		if maxDate > state.CursorMS {
			if _, err := p.d.State.AdvanceChatCursor(ctx, t.WorkspaceID, t.IntegrationID, t.TeamID, maxDate); err != nil && parent.Err() == nil {
				if errors.Is(err, behavior.ErrNotFound) {
					p.logf("cursor_restarted", t)
				} else {
					p.logf("storage_failed", t)
				}
				return
			}
		}
	}
	if state.ErrorStreak > 0 || state.UnauthorizedStreak > 0 || state.BackoffUntil != nil {
		if err := p.d.State.ResetChatPollErrors(ctx, t.WorkspaceID, t.IntegrationID); err != nil && parent.Err() == nil {
			p.logf("storage_failed", t)
		}
	}
}

// collect reads the channels of the given types with messages since the
// cursor and their messages newer than the cursor. It returns every new
// message (dropped ones carry an empty event Key but still count for the
// cursor), whether everything newer than the cursor was read, and the
// poll error code ("" when every call succeeded). Messages read before a
// failure are returned too: their runs are deduplicated if they are read
// again.
func (p *Poller) collect(ctx context.Context, t Target, token string, types []string, cursorMS int64) ([]pending, bool, string) {
	var out []pending
	complete := true
	var channels []clickupchat.Channel
	pageCursor := ""
	listed := false
	for page := 0; page < p.s.ChannelPages; page++ {
		pg, err := p.d.Chat.DMChannelsSince(ctx, t.TeamID, token, types, cursorMS, pageCursor)
		if err != nil {
			return out, false, errorCode(err)
		}
		channels = append(channels, pg.Items...)
		if pg.NextCursor == "" {
			listed = true
			break
		}
		if pg.NextCursor == pageCursor {
			break
		}
		pageCursor = pg.NextCursor
	}
	if !listed {
		// More channels than the page cap: the unlisted ones may hold
		// messages of any date after the cursor.
		p.logf("chat_channels_truncated", t)
		complete = false
	}
	want := map[string]bool{}
	for _, ty := range types {
		want[ty] = true
	}
	seen := map[string]bool{}
	for _, ch := range channels {
		if ctx.Err() != nil {
			return out, false, CodeUnavailable
		}
		if ch.ID == "" || seen[ch.ID] || !want[ch.Type] {
			continue
		}
		seen[ch.ID] = true
		msgs, chComplete, code := p.readChannel(ctx, t, token, ch, cursorMS)
		out = append(out, msgs...)
		if code != "" {
			return out, false, code
		}
		if !chComplete {
			complete = false
		}
	}
	return out, complete, ""
}

// readChannel reads one channel's messages newer than the cursor, paging
// up to the cap. ClickUp does not document the order, so every message is
// filtered by date; paging stops early only when the messages read so far
// are newest-first and a page already reached the cursor.
func (p *Poller) readChannel(ctx context.Context, t Target, token string, ch clickupchat.Channel, cursorMS int64) ([]pending, bool, string) {
	var out []pending
	pageCursor := ""
	descending := true
	var last int64 = -1
	for page := 0; page < p.s.MessagePages; page++ {
		pg, err := p.d.Chat.Messages(ctx, t.TeamID, token, ch.ID, pageCursor)
		if err != nil {
			switch clickupchat.ErrorCode(err) {
			case clickupchat.CodeNotFound, clickupchat.CodeInvalidInput:
				// The channel is gone or its ID is unusable: nothing in
				// it can be read, now or later.
				p.logf("chat_channel_skipped", t)
				return out, true, ""
			}
			return out, false, errorCode(err)
		}
		reachedCursor := false
		for _, m := range pg.Items {
			if last >= 0 && m.DateMS > last {
				descending = false
			}
			last = m.DateMS
			if m.DateMS <= cursorMS {
				reachedCursor = true
				continue
			}
			out = append(out, pending{date: m.DateMS, ev: p.event(t, ch, m)})
		}
		if pg.NextCursor == "" {
			return out, true, ""
		}
		if descending && reachedCursor {
			return out, true, ""
		}
		if pg.NextCursor == pageCursor {
			// ClickUp handed back the same cursor: stop instead of looping.
			p.logf("chat_messages_truncated", t)
			return out, false, ""
		}
		pageCursor = pg.NextCursor
	}
	if descending {
		// Newest first and the cap reached: what is left is older than
		// every message read. It is dropped so the cursor can move on.
		p.logf("chat_backlog_truncated", t)
		return out, true, ""
	}
	p.logf("chat_messages_truncated", t)
	return out, false, ""
}

// event builds the trigger event of one new message, or an event with an
// empty Key when the message is dropped: the owner's own, not a plain
// message, a thread reply, or one without an ID or sender.
func (p *Poller) event(t Target, ch clickupchat.Channel, m clickupchat.Message) behavior.Event {
	if m.ID == "" || m.UserID == "" || m.Type != "message" || m.ParentMessage != "" {
		return behavior.Event{}
	}
	if p.s.OwnerClickUpUserID != "" && m.UserID == p.s.OwnerClickUpUserID {
		return behavior.Event{}
	}
	return behavior.Event{
		Type:          behavior.TriggerChatMessage,
		Key:           behavior.ChatSubject(m.ID),
		IntegrationID: t.IntegrationID,
		MessageID:     m.ID,
		ChannelID:     ch.ID,
		ChannelType:   strings.ToLower(ch.Type),
		SenderID:      m.UserID,
		PRs:           behavior.ExtractPRRefs(m.Content),
	}
}

// errorCode maps a ClickUp Chat error to a poll error code.
func errorCode(err error) string {
	switch clickupchat.ErrorCode(err) {
	case clickupchat.CodeUnauthorized, clickupchat.CodeMissingCredentials:
		return CodeUnauthorized
	case clickupchat.CodeRateLimited:
		return CodeRateLimited
	default:
		return CodeUnavailable
	}
}

// fail records a failed poll, backs off and, after enough consecutive
// chat_unauthorized polls, tells the owner once.
func (p *Poller) fail(ctx context.Context, t Target, code string) {
	p.logf(code, t)
	if _, err := p.d.State.RecordChatPollError(ctx, t.WorkspaceID, t.IntegrationID, code, p.s.Now(), p.s.Interval, p.s.MaxBackoff); err != nil {
		p.logf("storage_failed", t)
		return
	}
	if code != CodeUnauthorized || p.d.Chats == nil || p.d.Messenger == nil {
		return
	}
	claimed, err := p.d.State.ClaimChatUnauthorizedNotice(ctx, t.WorkspaceID, t.IntegrationID, UnauthorizedNoticeStreak, p.s.Now())
	if err != nil {
		p.logf("storage_failed", t)
		return
	}
	if !claimed {
		return
	}
	chat, err := p.d.Chats.OwnerChat(ctx, t.WorkspaceID, behavior.TriggerData{})
	if err != nil {
		p.logf("notice_no_chat", t)
		return
	}
	lang := ""
	if p.d.Profiles != nil {
		if prof, err := p.d.Profiles.GetProfile(ctx, t.WorkspaceID); err == nil {
			lang = prof.Language
		}
	}
	if _, err := p.d.Messenger.SendMessage(ctx, chat, behavior.OutMessage{Text: unauthorizedNotice(lang)}); err != nil {
		p.logf("notice_send_failed", t)
	}
}

// unauthorizedNotice is the owner notice text. It names no secret and
// quotes no ClickUp response.
func unauthorizedNotice(language string) string {
	if strings.HasPrefix(language, "en") {
		return "⚠️ ClickUp Chat: the ClickUp token was rejected 3 times in a row, so new direct messages are not being read. Check the ClickUp token credential of the ClickUp source."
	}
	return "⚠️ ClickUp Chat: token ClickUp bị từ chối 3 lần liên tiếp nên tin nhắn trực tiếp mới không được đọc. Hãy kiểm tra credential token ClickUp của nguồn ClickUp."
}

// logf logs a code with the workspace and integration IDs only.
func (p *Poller) logf(code string, t Target) {
	log.Printf("clickup chat poll: %s workspace=%s integration=%s", code, t.WorkspaceID, t.IntegrationID)
}

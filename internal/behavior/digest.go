package behavior

import (
	"context"
	"errors"
	"log"
	"strings"
	"time"
)

// This file applies the profile's quiet hours and digest mode to the
// messages that only inform (the digest of notify items and the FYI of
// auto items). Messages the owner acts on (asks, drafts, confirmations)
// and run-failure notices are always sent at once.
//
//   - per_run: sent when the run is dispatched, unless that falls in quiet
//     hours: then queued until the quiet hours end;
//   - hourly: queued until the start of the next hour (local time);
//   - daily: queued until the next daily time: the end of the quiet hours
//     when they are set, otherwise DailyDigestMinute;
//   - a due time inside quiet hours moves to their end.
//
// Queued messages wait in the DigestStore. The engine's service loop
// sends every due entry of one chat as one message (a batch).

// DailyDigestMinute is the local minute of day of the daily digest when no
// quiet hours are set (08:00).
const DailyDigestMinute = 8 * 60

// Digest message keys (behavior_messages.msg_key of the run).
const (
	DigestKeyNotify = "digest"
	DigestKeyFYI    = "fyi"
)

// maxBatchEntries caps the entries (run messages) of one batch.
const maxBatchEntries = 50

// DigestEntry is a queued run message: its run and key.
type DigestEntry struct {
	RunID int64
	Key   string
}

// DigestBatch is a claimed set of due entries of one chat.
type DigestBatch struct {
	ID         int64
	ClaimToken string
	Chat       ChatRef
	Entries    []DigestEntry
}

// DigestStore queues digest and FYI messages and batches them.
type DigestStore interface {
	// QueueDigest claims the run's message key (MessageLog) and queues it
	// for a batch due at due, in one transaction. false when the key was
	// already claimed: the message was sent or queued before.
	QueueDigest(ctx context.Context, workspaceID string, runID int64, key string, chat ChatRef, due time.Time) (bool, error)
	// ClaimDigestBatch puts every due, unbatched entry of one chat (at most
	// limit) into a new batch; nil when none is due.
	ClaimDigestBatch(ctx context.Context, now time.Time, limit int) (*DigestBatch, error)
	// FinishDigestBatch records the sent message on the batch and on each
	// entry's run message key. ErrStaleClaim when the batch is not open.
	FinishDigestBatch(ctx context.Context, batchID int64, claimToken string, messageID int64) error
	// ReleaseDigestBatch returns the entries to the queue (due again at due
	// when it is not zero) and drops the batch.
	ReleaseDigestBatch(ctx context.Context, batchID int64, claimToken string, due time.Time) error
}

// profileLocation is the profile's time zone (the default zone when
// invalid).
func profileLocation(p Profile) *time.Location {
	for _, tz := range []string{p.Timezone, DefaultProfile().Timezone} {
		if tz == "" {
			continue
		}
		if loc, err := time.LoadLocation(tz); err == nil {
			return loc
		}
	}
	return time.UTC
}

func atMinute(day time.Time, minute int, loc *time.Location) time.Time {
	return time.Date(day.Year(), day.Month(), day.Day(), minute/60, minute%60, 0, 0, loc)
}

// QuietUntil reports whether t falls in the profile's quiet hours and, if
// so, when they end. Quiet hours run from QuietStart (inclusive) to
// QuietEnd (exclusive), local time, and may cross midnight; equal start
// and end mean no quiet hours.
func QuietUntil(p Profile, t time.Time) (time.Time, bool) {
	if p.QuietStart == nil || p.QuietEnd == nil || *p.QuietStart == *p.QuietEnd {
		return time.Time{}, false
	}
	loc := profileLocation(p)
	l := t.In(loc)
	m := l.Hour()*60 + l.Minute()
	s, e := *p.QuietStart, *p.QuietEnd
	switch {
	case s < e && m >= s && m < e:
		return atMinute(l, e, loc), true
	case s > e && m >= s:
		return atMinute(l.AddDate(0, 0, 1), e, loc), true
	case s > e && m < e:
		return atMinute(l, e, loc), true
	}
	return time.Time{}, false
}

// DigestDue returns when an informing message produced at now is sent:
// now (deferred=false) or a later time (deferred=true).
func DigestDue(p Profile, now time.Time) (due time.Time, deferred bool) {
	loc := profileLocation(p)
	l := now.In(loc)
	due = now
	switch p.DigestMode {
	case DigestHourly:
		due = time.Date(l.Year(), l.Month(), l.Day(), l.Hour()+1, 0, 0, 0, loc)
	case DigestDaily:
		minute := DailyDigestMinute
		if p.QuietStart != nil && p.QuietEnd != nil && *p.QuietStart != *p.QuietEnd {
			minute = *p.QuietEnd
		}
		due = atMinute(l, minute, loc)
		if !due.After(now) {
			due = atMinute(l.AddDate(0, 0, 1), minute, loc)
		}
	}
	if end, quiet := QuietUntil(p, due); quiet {
		due = end
	}
	return due, due.After(now)
}

// runProfile is the profile of a dispatch: the one loaded for the run, or
// a fresh read.
func (e *Engine) runProfile(ctx context.Context, rc RunContext) (Profile, error) {
	if rc.Profile.Timezone != "" && rc.Profile.DigestMode != "" {
		return rc.Profile, nil
	}
	return e.d.Profiles.GetProfile(ctx, rc.Run.WorkspaceID)
}

// inform sends a digest or FYI of a run now, or queues it when the profile
// defers it. Without a DigestStore everything is sent at once.
func (e *Engine) inform(ctx context.Context, rc RunContext, chat ChatRef, key, head string, items []Item, withStatus bool) error {
	if len(items) == 0 {
		return nil
	}
	if e.d.Digests != nil {
		p, err := e.runProfile(ctx, rc)
		if err != nil {
			return err
		}
		if due, deferred := DigestDue(p, e.now()); deferred {
			queued, err := e.d.Digests.QueueDigest(ctx, rc.Run.WorkspaceID, rc.Run.ID, key, chat, due)
			if err != nil {
				return err
			}
			if queued {
				log.Printf("behavior run %d corr=%s: message %s queued (%d items)", rc.Run.ID, rc.Run.CorrelationID, key, len(items))
			}
			return nil
		}
	}
	return e.summary(ctx, rc, chat, key, head, items, withStatus)
}

// FlushDigests sends every due batch, at most maxBatches per call, and
// returns how many were sent.
func (e *Engine) FlushDigests(ctx context.Context) (int, error) {
	if e.d.Digests == nil {
		return 0, nil
	}
	const maxBatches = 20
	sent := 0
	for i := 0; i < maxBatches && ctx.Err() == nil; i++ {
		b, err := e.d.Digests.ClaimDigestBatch(ctx, e.now(), maxBatchEntries)
		if err != nil || b == nil {
			return sent, err
		}
		ok, err := e.flushBatch(ctx, b)
		if err != nil {
			return sent, err
		}
		if ok {
			sent++
		}
	}
	return sent, nil
}

// batchSection is one run's part of a batch.
type batchSection struct {
	rc         RunContext
	key        string
	items      []Item
	withStatus bool
}

// flushBatch builds and sends one batch. ok is false when nothing was
// sent. A send failure returns the entries to the queue.
func (e *Engine) flushBatch(ctx context.Context, b *DigestBatch) (bool, error) {
	ws := b.Chat.WorkspaceID
	p, err := e.d.Profiles.GetProfile(ctx, ws)
	if err != nil {
		e.releaseBatch(ctx, b, time.Time{})
		return false, err
	}
	// Quiet hours set after the entries were queued still hold.
	if end, quiet := QuietUntil(p, e.now()); quiet {
		e.releaseBatch(ctx, b, end)
		return false, nil
	}
	var sections []batchSection
	total := 0
	for _, en := range b.Entries {
		rc, err := e.loadRun(ctx, ws, en.RunID)
		if err != nil {
			if errors.Is(err, ErrNotFound) || errors.Is(err, ErrInvalid) {
				log.Printf("behavior digest batch %d: run %d not loadable; left out", b.ID, en.RunID)
				continue
			}
			e.releaseBatch(ctx, b, time.Time{})
			return false, err
		}
		items, err := e.d.Store.ListItems(ctx, ws, en.RunID)
		if err != nil {
			e.releaseBatch(ctx, b, time.Time{})
			return false, err
		}
		sec := batchSection{rc: rc, key: en.Key}
		for _, it := range items {
			switch {
			case en.Key == DigestKeyNotify && it.Class == ClassNotify:
				sec.items = append(sec.items, it)
			case en.Key == DigestKeyFYI && it.Class == ClassAuto && !rc.Doc.DryRun && it.Status != ItemDrafted:
				sec.items = append(sec.items, it)
				sec.withStatus = true
			}
		}
		if len(sec.items) > 0 {
			sections = append(sections, sec)
			total += len(sec.items)
		}
	}
	if total == 0 {
		// Nothing left to say: the batch stays unsent and is never resent.
		log.Printf("behavior digest batch %d: no items; not sent", b.ID)
		return false, nil
	}
	t := textsFor(p.Language)
	var sb strings.Builder
	sb.WriteString(t.batchHead(total) + "\n")
	per := max(maxTelegramRunes/total-40, 60)
	for _, sec := range sections {
		head := t.digestHead()
		if sec.key == DigestKeyFYI {
			head = t.fyiHead()
		}
		sb.WriteString("\n" + head + " (" + t.runRef(sec.rc.Doc.Name, sec.rc.Run.ID) + ")\n")
		sb.WriteString(summaryLines(t, sec.items, sec.withStatus, per))
	}
	msg := OutMessage{Text: capRunes(strings.TrimSpace(sb.String()), maxTelegramRunes)}
	if total == 1 {
		sec := sections[0]
		fb, err := e.ensureApproval(ctx, ws, NewApproval{
			Kind: ApprovalFeedback, RunID: sec.rc.Run.ID, ItemID: sec.items[0].ID, Action: sec.key, Resource: sec.rc.Run.Subject,
			ExpiresAt: e.expiry(sec.rc.Doc),
		}, false)
		if err != nil {
			e.releaseBatch(ctx, b, time.Time{})
			return false, err
		}
		msg.Buttons = [][]Button{e.feedbackRow(t, fb.Nonce)}
	}
	sctx, cancel := context.WithTimeout(ctx, e.s.RequestTimeout)
	id, err := e.d.Messenger.SendMessage(sctx, b.Chat, msg)
	cancel()
	if err != nil {
		log.Printf("behavior digest batch %d: not sent (%s); queued again", b.ID, sendCode(err))
		e.releaseBatch(ctx, b, time.Time{})
		return false, nil
	}
	if err := e.d.Digests.FinishDigestBatch(ctx, b.ID, b.ClaimToken, id); err != nil {
		log.Printf("behavior digest batch %d: sent, id not stored", b.ID)
		return true, err
	}
	log.Printf("behavior digest batch %d: sent (%d runs, %d items)", b.ID, len(sections), total)
	return true, nil
}

func (e *Engine) releaseBatch(ctx context.Context, b *DigestBatch, due time.Time) {
	if err := e.d.Digests.ReleaseDigestBatch(context.WithoutCancel(ctx), b.ID, b.ClaimToken, due); err != nil {
		log.Printf("behavior digest batch %d: not released", b.ID)
	}
}

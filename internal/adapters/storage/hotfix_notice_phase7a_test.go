package storage

import (
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/domain"
)

func nmEvent(key, kind string, sender string, meta map[string]string) domain.NormalizedEvent {
	e := noticeEvent(key, "task-"+key)
	e.EventType = domain.EventType(kind)
	e.SenderID = sender
	m := map[string]string{"webhook_id": "w1"}
	for k, v := range meta {
		m[k] = v
	}
	e.Metadata = m
	return e
}

// TD-NM-1 (unit): the notice decision for each event shape.
func TestHotfixNM1_NoticeWantedTable(t *testing.T) {
	cmt := domain.EventCommandPosted
	cases := []struct {
		name    string
		e       domain.NormalizedEvent
		owner   string
		started int
		want    bool
	}{
		{"tags owner", domain.NormalizedEvent{EventType: cmt, SenderID: "5", Metadata: map[string]string{"tagged_user_ids": "9,77"}}, "77", 0, true},
		{"tags group", domain.NormalizedEvent{EventType: cmt, SenderID: "5", Metadata: map[string]string{"tagged_group": "true"}}, "77", 0, true},
		{"untagged", domain.NormalizedEvent{EventType: cmt, SenderID: "5", Metadata: map[string]string{}}, "77", 0, false},
		{"nil metadata", domain.NormalizedEvent{EventType: cmt, SenderID: "5"}, "77", 0, false},
		{"tags other", domain.NormalizedEvent{EventType: cmt, SenderID: "5", Metadata: map[string]string{"tagged_user_ids": "9,10"}}, "77", 0, false},
		{"prefix of owner id", domain.NormalizedEvent{EventType: cmt, SenderID: "5", Metadata: map[string]string{"tagged_user_ids": "770,7"}}, "77", 0, false},
		{"owner's own comment", domain.NormalizedEvent{EventType: cmt, SenderID: "77", Metadata: map[string]string{"tagged_user_ids": "77"}}, "77", 0, false},
		{"started a behavior run", domain.NormalizedEvent{EventType: cmt, SenderID: "5", Metadata: map[string]string{"tagged_user_ids": "77"}}, "77", 1, false},
		{"status change", domain.NormalizedEvent{EventType: domain.EventStatusChanged, SenderID: "5", Metadata: map[string]string{"tagged_group": "true", "tagged_user_ids": "77"}}, "77", 0, false},
		{"owner unknown", domain.NormalizedEvent{EventType: cmt, SenderID: "5", Metadata: map[string]string{"tagged_user_ids": "", "tagged_group": "true"}}, "", 0, false},
		{"empty tag list, empty owner", domain.NormalizedEvent{EventType: cmt, SenderID: "5", Metadata: map[string]string{"tagged_user_ids": ""}}, "", 0, false},
		{"group flag only exact true", domain.NormalizedEvent{EventType: cmt, SenderID: "5", Metadata: map[string]string{"tagged_group": "yes"}}, "77", 0, false},
	}
	for _, c := range cases {
		if got := noticeWanted(c.e, c.owner, c.started); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

// TD-NM-1 (DB): with notice_mentions_only on, only comments from someone else
// that tag the owner (or a group) make a notification_outbox row.
func TestHotfixNM1_MentionsOnlyOutbox(t *testing.T) {
	ctx, pool, r := newNoticeDB(t)
	mustExec(t, ctx, pool, `UPDATE integrations SET config = config || '{"notice_mentions_only":true}'::jsonb WHERE id=$1`, integrationA)
	mustExec(t, ctx, pool, `INSERT INTO app_settings(key,value) VALUES ('owner_clickup_user_id','"77"')`)
	cmt := string(domain.EventCommandPosted)
	send := func(e domain.NormalizedEvent) (accepted, ignored int) {
		res, err := r.Accept(ctx, []domain.NormalizedEvent{e})
		if err != nil {
			t.Fatalf("accept %s: %v", e.EventKey, err)
		}
		return res.Accepted, res.Ignored
	}
	rows := func() int { return countRows(t, ctx, pool, "notification_outbox") }

	if a, i := send(nmEvent("untagged", cmt, "5", nil)); a != 0 || i != 1 || rows() != 0 {
		t.Fatalf("untagged comment: accepted=%d ignored=%d rows=%d", a, i, rows())
	}
	status := nmEvent("status", string(domain.EventStatusChanged), "5", map[string]string{"status_to": "done"})
	if a, _ := send(status); a != 0 || rows() != 0 {
		t.Fatalf("status change made a notice: rows=%d", rows())
	}
	if a, _ := send(nmEvent("own", cmt, "77", map[string]string{"tagged_user_ids": "77"})); a != 0 || rows() != 0 {
		t.Fatalf("owner's own comment made a notice: rows=%d", rows())
	}
	if a, _ := send(nmEvent("other", cmt, "5", map[string]string{"tagged_user_ids": "9,10,770"})); a != 0 || rows() != 0 {
		t.Fatalf("comment tagging others made a notice: rows=%d", rows())
	}
	if a, _ := send(nmEvent("tagged", cmt, "5", map[string]string{"tagged_user_ids": "9,77"})); a != 1 || rows() != 1 {
		t.Fatalf("comment tagging the owner: accepted=%d rows=%d", a, rows())
	}
	if a, _ := send(nmEvent("group", cmt, "5", map[string]string{"tagged_group": "true"})); a != 1 || rows() != 2 {
		t.Fatalf("group mention: accepted=%d rows=%d", a, rows())
	}
	// the same callback delivered twice is still one notice
	res, err := r.Accept(ctx, []domain.NormalizedEvent{nmEvent("tagged", cmt, "5", map[string]string{"tagged_user_ids": "9,77"})})
	if err != nil || res.Duplicates != 1 || rows() != 2 {
		t.Fatalf("duplicate delivery: %+v %v rows=%d", res, err, rows())
	}
	// a mixed batch: only the tagged one passes
	res, err = r.Accept(ctx, []domain.NormalizedEvent{
		nmEvent("m1", cmt, "5", nil), nmEvent("m2", cmt, "5", map[string]string{"tagged_user_ids": "77"}), nmEvent("m3", cmt, "6", nil)})
	if err != nil || res.Accepted != 1 || res.Ignored != 2 || rows() != 3 {
		t.Fatalf("mixed batch: %+v %v rows=%d", res, err, rows())
	}
}

// TD-NM-1 (DB): without the key behaviour is unchanged (everything is noticed);
// with the key on but no owner configured nothing is noticed.
func TestHotfixNM1_KeyOffAndNoOwner(t *testing.T) {
	cmt := string(domain.EventCommandPosted)
	t.Run("key off", func(t *testing.T) {
		ctx, pool, r := newNoticeDB(t)
		mustExec(t, ctx, pool, `INSERT INTO app_settings(key,value) VALUES ('owner_clickup_user_id','"77"')`)
		evs := []domain.NormalizedEvent{
			nmEvent("a", cmt, "5", nil),
			nmEvent("b", string(domain.EventStatusChanged), "5", map[string]string{"status_to": "done"}),
			nmEvent("c", cmt, "77", map[string]string{"tagged_user_ids": "77"}),
		}
		res, err := r.Accept(ctx, evs)
		if err != nil || res.Accepted != 3 || countRows(t, ctx, pool, "notification_outbox") != 3 {
			t.Fatalf("key off changed behaviour: %+v %v rows=%d", res, err, countRows(t, ctx, pool, "notification_outbox"))
		}
	})
	t.Run("key not literal true", func(t *testing.T) {
		ctx, pool, r := newNoticeDB(t)
		mustExec(t, ctx, pool, `UPDATE integrations SET config = config || '{"notice_mentions_only":"true"}'::jsonb WHERE id=$1`, integrationA)
		res, err := r.Accept(ctx, []domain.NormalizedEvent{nmEvent("a", cmt, "5", nil)})
		if err != nil || res.Accepted != 1 {
			t.Fatalf(`"true" string must not switch the filter on: %+v %v`, res, err)
		}
	})
	t.Run("no owner", func(t *testing.T) {
		ctx, pool, r := newNoticeDB(t)
		mustExec(t, ctx, pool, `UPDATE integrations SET config = config || '{"notice_mentions_only":true}'::jsonb WHERE id=$1`, integrationA)
		res, err := r.Accept(ctx, []domain.NormalizedEvent{nmEvent("a", cmt, "5", map[string]string{"tagged_user_ids": "77", "tagged_group": "true"})})
		if err != nil || res.Accepted != 0 || countRows(t, ctx, pool, "notification_outbox") != 0 {
			t.Fatalf("no owner configured yet noticed: %+v %v", res, err)
		}
	})
}

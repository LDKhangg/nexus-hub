package clickup

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/domain"
)

// realComment follows the shape ClickUp documents and the VPS received on
// 2026-11-16: before is null, after is the comment id as a JSON string.
const realComment = `{"event":"taskCommentPosted","history_items":[{"id":"4415872","type":1,"date":"1763300000000","field":"comment","parent_id":"901100","data":{},"source":null,"user":{"id":183,"username":"Teammate","email":"t@example.com","color":"#4194f6","initials":"T","profilePicture":null},"before":null,"after":"90110012345","comment":{"id":"90110012345","comment":[{"text":"Please check "},{"type":"tag","user":{"id":77,"username":"Owner","email":"o@example.com","color":"#000","initials":"O","profilePicture":null}},{"text":" thanks\n"}],"text_content":"Please check @Owner thanks\n","user":{"id":183,"username":"Teammate","email":"t@example.com"},"date":"1763300000000","resolved":false,"assignee":null,"group_assignee":null}}],"task_id":"86abc","webhook_id":"de2df88e-1111-2222-3333-444444444444"}`

// TD-CA-1 (FR-018, FR-012): the real-shaped, signed taskCommentPosted yields one event with the tagged user.
func TestHotfixCA1_RealShapedCommentParsesToOneEvent(t *testing.T) {
	p := NewClickUpParser("s3cret", "")
	h := http.Header{}
	h.Set(signatureHeader, sign("s3cret", realComment))
	if err := p.Verify(h, []byte(realComment)); err != nil {
		t.Fatalf("verify: %v", err)
	}
	events, err := p.Parser([]byte(realComment))
	if err != nil {
		t.Fatalf("real payload rejected: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("%d events", len(events))
	}
	e := events[0]
	if e.EventType != domain.EventCommandPosted || e.Metadata["tagged_user_ids"] != "77" || e.SenderID != "183" ||
		e.ExternalID != "86abc" || e.ExternalMessageID != "comment:90110012345" || e.EventKey != "de2df88e-1111-2222-3333-444444444444:4415872" {
		t.Fatalf("event: %+v", e)
	}
	if e.Metadata["tagged_group"] != "" {
		t.Fatal("group tag invented")
	}
	if e.Metadata["list_id"] != "901100" {
		t.Fatalf("list from parent_id: %v", e.Metadata)
	}
}

// TD-CA-2: before/after values of every JSON kind never fail a comment callback.
func TestHotfixCA2_OddBeforeAfterNeverFailComments(t *testing.T) {
	values := []string{`null`, `"90110012345"`, `""`, `12345`, `1.5`, `true`, `false`, `[]`, `["a",1]`, `{}`, `{"status":5}`, `{"id":[1]}`, `[{"status":"x"}]`}
	for _, before := range values {
		for _, after := range values {
			body := fmt.Sprintf(`{"event":"taskCommentPosted","task_id":"t","webhook_id":"w","history_items":[{"id":"h1","field":"comment","before":%s,"after":%s,"comment":{"id":"c1","text_content":"hi","user":{"id":1}}}]}`, before, after)
			events, err := NewClickUpParser("", "").Parser([]byte(body))
			if err != nil || len(events) != 1 {
				t.Fatalf("before=%s after=%s: %v %d", before, after, err, len(events))
			}
		}
	}
}

// TD-CA-2: status callbacks with such values still error "missing new status".
func TestHotfixCA2_StatusStillNeedsNewStatus(t *testing.T) {
	status := func(before, after string) string {
		return fmt.Sprintf(`{"event":"taskStatusUpdated","task_id":"t","webhook_id":"w","history_items":[{"id":"h1","field":"status","before":%s,"after":%s}]}`, before, after)
	}
	for _, after := range []string{`null`, `"open"`, `12`, `true`, `[]`, `{}`, `{"status":""}`, `{"type":"open"}`, `["done"]`} {
		_, err := NewClickUpParser("", "").Parser([]byte(status(`{"status":"to do"}`, after)))
		if err == nil || !strings.Contains(err.Error(), "missing new status") {
			t.Fatalf("after=%s: %v", after, err)
		}
	}
	// a good after with odd before still works, and the old status reads Unknown
	for _, before := range []string{`null`, `"x"`, `7`, `[]`, `{}`, `false`} {
		events, err := NewClickUpParser("", "").Parser([]byte(status(before, `{"status":"done","type":"closed"}`)))
		if err != nil || len(events) != 1 || events[0].Content != "Unknown → done" || events[0].Metadata["status_to"] != "done" {
			t.Fatalf("before=%s: %v %+v", before, err, events)
		}
	}
	events, _ := NewClickUpParser("", "").Parser([]byte(status(`{"status":"to do"}`, `{"status":"done"}`)))
	if events[0].Content != "to do → done" || events[0].Metadata["status_from"] != "to do" {
		t.Fatalf("normal status change: %+v", events[0])
	}
}

// TD-CA-2: assignee additions without a user id are skipped; with an id they start behaviors.
func TestHotfixCA2_AssigneeWithoutUserIsSkipped(t *testing.T) {
	assignee := func(after string) string {
		return fmt.Sprintf(`{"event":"taskAssigneeUpdated","task_id":"t","webhook_id":"w","history_items":[{"id":"h1","field":"assignee_add","before":null,"after":%s}]}`, after)
	}
	for _, after := range []string{`null`, `{}`, `"7"`, `7`, `true`, `[]`, `{"id":null}`, `{"username":"x"}`} {
		if _, err := NewClickUpParser("", "").Parser([]byte(assignee(after))); !errors.Is(err, domain.ErrIgnoredEvent) {
			t.Fatalf("after=%s: %v", after, err)
		}
	}
	for after, want := range map[string]string{`{"id":5}`: "5", `{"id":"7"}`: "7"} {
		events, err := NewClickUpParser("", "").Parser([]byte(assignee(after)))
		if err != nil || len(events) != 1 || events[0].Metadata["assignee_added"] != want {
			t.Fatalf("after=%s: %v %+v", after, err, events)
		}
	}
	// removals only (assignee_rem) are ignored
	rem := `{"event":"taskAssigneeUpdated","task_id":"t","webhook_id":"w","history_items":[{"id":"h1","field":"assignee_rem","before":{"id":5},"after":null}]}`
	if _, err := NewClickUpParser("", "").Parser([]byte(rem)); !errors.Is(err, domain.ErrIgnoredEvent) {
		t.Fatalf("removal: %v", err)
	}
}

func commentWith(parts, text string) string {
	return fmt.Sprintf(`{"event":"taskCommentPosted","task_id":"t","webhook_id":"w","history_items":[{"id":"h1","field":"comment","before":null,"after":"c1","comment":{"id":"c1","text_content":%q,"user":{"id":9},"comment":%s}}]}`, text, parts)
}

// TD-NM-2: group mentions are detected from known part types and from the text,
// and ordinary words and e-mail addresses are not group mentions.
func TestHotfixNM2_TagsGroup(t *testing.T) {
	group := func(parts, text string) bool {
		events, err := NewClickUpParser("", "").Parser([]byte(commentWith(parts, text)))
		if err != nil || len(events) != 1 {
			t.Fatalf("parse: %v", err)
		}
		return events[0].Metadata["tagged_group"] == "true"
	}
	for _, part := range []string{"everyone", "group_tag", "group_mention", "team_tag", "followers", "followers_tag", "assignees", "assignees_tag", "Everyone"} {
		if !group(fmt.Sprintf(`[{"type":%q}]`, part), "x") {
			t.Errorf("part type %q not seen as a group tag", part)
		}
	}
	for _, text := range []string{"@everyone", "ping @all please", "@here", "@channel", "(@followers)", "hi\n@assignees", "@Everyone look", "@ALL", "@watchers"} {
		if !group(`[]`, text) {
			t.Errorf("text %q not seen as a group tag", text)
		}
	}
	for _, text := range []string{"a@all.com", "mail bob@everyone.org", "@allison look", "@alligator", "all of us", "everyone", "@@all", "user@here.example", "", "email: x@channel.io"} {
		if group(`[]`, text) {
			t.Errorf("text %q falsely seen as a group tag", text)
		}
	}
	// a single user tag is not a group tag
	if group(`[{"type":"tag","user":{"id":77}}]`, "Please check @Owner") {
		t.Error("user tag counted as a group")
	}
	// no parts at all, or parts of the wrong shape, never fail
	for _, parts := range []string{`null`, `{}`, `"x"`, `[1,2]`, `[{"type":5}]`} {
		if _, err := NewClickUpParser("", "").Parser([]byte(commentWith(parts, "hi"))); err != nil {
			t.Fatalf("parts %s: %v", parts, err)
		}
	}
}

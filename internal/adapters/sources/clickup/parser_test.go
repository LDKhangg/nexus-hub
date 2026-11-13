package clickup

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/domain"
)

const commentBody = `{"webhook_id":"w1","event":"taskCommentPosted","task_id":"t1","task":{"name":"T"},
"history_items":[{"id":"h1","field":"comment","date":"1700000000000","comment":{"id":"c1","text_content":"hi","user":{"id":7,"username":"u"}}}]}`

func sign(secret, body string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(body))
	return hex.EncodeToString(m.Sum(nil))
}

func TestVerify(t *testing.T) {
	p := NewClickUpParser("s3cret", "")
	h := http.Header{}
	if err := p.Verify(h, []byte(commentBody)); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("missing signature: got %v", err)
	}
	h.Set(signatureHeader, sign("wrong", commentBody))
	if err := p.Verify(h, []byte(commentBody)); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("bad signature: got %v", err)
	}
	h.Set(signatureHeader, sign("s3cret", commentBody))
	if err := p.Verify(h, []byte(commentBody)); err != nil {
		t.Fatalf("valid signature: got %v", err)
	}
}

func TestParserComment(t *testing.T) {
	events, err := NewClickUpParser("", "").Parser([]byte(commentBody))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("events: %v", events)
	}
	e := events[0]
	if e.EventType != domain.EventCommandPosted || e.Content != "hi" || e.SenderID != "7" || e.OccurredAt.UnixMilli() != 1700000000000 {
		t.Fatalf("unexpected event: %+v", e)
	}
}

func TestParserIgnored(t *testing.T) {
	for _, body := range []string{
		`{"event":"taskCreated","task_id":"t1"}`,
		`{"event":"taskUpdated","task_id":"t1"}`,
	} {
		if _, err := NewClickUpParser("", "").Parser([]byte(body)); !errors.Is(err, domain.ErrIgnoredEvent) {
			t.Fatalf("%s: want ErrIgnoredEvent, got %v", body, err)
		}
	}
}

func TestParserFromOwnBot(t *testing.T) {
	p := NewClickUpParser("", "7")
	cases := []struct {
		name    string
		userID  int
		content string
		want    bool
	}{
		{"bot của mình", 7, botMarker + " Deadline là thứ 6", true},
		{"mình tự comment", 7, "@AI check giúp", false},
		{"khách gõ marker", 9, botMarker + " hỏi chút", false},
		{"khách bình thường", 9, "deadline khi nào?", false},
		{"bot đồng nghiệp", 9, botMarker + " [Tin nhắn từ AI] sửa CI", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"webhook_id":"w1","event":"taskCommentPosted","task_id":"t1","task":{"name":"T"},
"history_items":[{"id":"h1","field":"comment","date":"1700000000000","comment":{"id":"c1","text_content":%q,"user":{"id":%d,"username":"u"}}}]}`,
				tc.content, tc.userID)

			events, err := p.Parser([]byte(body))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}

			if len(events) != 1 || events[0].FromOwnBot != tc.want {
				t.Errorf("events = %+v, want own bot %v", events, tc.want)
			}
		})
	}
}

func TestRealPayloadBatch(t *testing.T) {
	for _, kind := range []string{"comment", "status"} {
		t.Run(kind, func(t *testing.T) {
			body, err := os.ReadFile("testdata/" + kind + ".json")
			if err != nil {
				t.Fatal(err)
			}
			events, err := NewClickUpParser("", "").Parser(body)
			if err != nil {
				t.Fatal(err)
			}
			if len(events) != 2 {
				t.Fatalf("batch length %d", len(events))
			}
			if events[0].EventKey != "w1:h1" || events[1].EventKey != "w1:h2" || events[0].WorkspaceID != "" || events[0].IntegrationID != "" || events[0].Metadata["webhook_id"] != "w1" {
				t.Fatalf("identity: %+v", events)
			}
			if kind == "status" && (events[0].Content != "open → in progress" || events[0].SenderID != "7" || events[1].Content != "Unknown → closed") {
				t.Fatalf("status: %+v", events)
			}
			if kind == "comment" && (events[0].ExternalMessageID != "comment:c1" || events[1].SenderName != "Unknown user") {
				t.Fatalf("comment: %+v", events)
			}
		})
	}
}

func TestMalformedSupportedPayload(t *testing.T) {
	for _, body := range []string{"{", strings.Replace(commentBody, `"w1"`, `""`, 1), strings.Replace(commentBody, `"t1"`, `""`, 1), strings.Replace(commentBody, `"h1"`, `""`, 1), strings.Replace(commentBody, `"c1"`, `""`, 1), `{"webhook_id":"w1","event":"taskStatusUpdated","task_id":"t1","history_items":[]}`} {
		if _, err := NewClickUpParser("", "").Parser([]byte(body)); err == nil || errors.Is(err, domain.ErrIgnoredEvent) {
			t.Fatalf("malformed accepted: %s (%v)", body, err)
		}
	}
}

func TestVerifyFailsClosedAndIsolated(t *testing.T) {
	for _, secret := range []string{"", "other"} {
		h := http.Header{}
		h.Set(signatureHeader, sign("first", commentBody))
		if err := NewClickUpParser(secret, "").Verify(h, []byte(commentBody)); !errors.Is(err, ErrInvalidSignature) {
			t.Fatalf("secret isolation: %v", err)
		}
	}
	p := NewClickUpParser("first", "")
	h := http.Header{}
	for _, signature := range []string{"not-hex", sign("first", commentBody)} {
		h.Set(signatureHeader, signature)
		if p.Verify(h, []byte(commentBody+" ")) == nil {
			t.Fatal("altered body accepted")
		}
	}
}

func TestDuplicateSourceKeysStable(t *testing.T) {
	body := strings.Replace(commentBody, `}]}`, `},{"id":"h1","field":"comment","comment":{"id":"c1","text_content":"hi"}}]}`, 1)
	events, err := NewClickUpParser("", "").Parser([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].EventKey != events[1].EventKey {
		t.Fatalf("unstable duplicate keys: %+v", events)
	}
}

func TestPhase3ECommentMentions_AcceptsBothPartsAndFlexibleIDs(t *testing.T) {
	// TC-70, TC-71 (REQ-E7): this uses the expected taskCommentPosted
	// history_items[].comment.comment[] shape because no recorded webhook
	// payload is available. Malformed parts must not reject the webhook.
	body := `{"webhook_id":"w1","event":"taskCommentPosted","task_id":"t1","history_items":[{"id":"h1","field":"comment","comment":{"id":"c1","text_content":"PRIVATE COMMENT","user":{"id":9},"comment":[{"type":"tag","user":{"id":7}},{"type":"user_mention","user":{"id":"8"}},{"type":"tag","user":{"id":"7"}},{"type":"tag","user":{"id":{"bad":true}}},{"bad":true}]}}]}`
	events, err := NewClickUpParser("", "").Parser([]byte(body))
	if err != nil || len(events) != 1 { t.Fatalf("parse: %+v %v", events, err) }
	e := events[0]
	if got := e.Metadata["tagged_user_ids"]; got != "7,8" { t.Fatalf("tagged ids=%q", got) }
	if e.ExternalMessageID != "comment:c1" { t.Fatalf("comment identity=%q", e.ExternalMessageID) }
	if strings.Contains(fmt.Sprint(e.Metadata), "PRIVATE COMMENT") { t.Fatalf("comment leaked to trusted metadata: %v", e.Metadata) }
}

func TestPhase3ECommentMentions_BoundedAndTextDoesNotForgeTag(t *testing.T) {
	// TC-71 (REQ-E7).
	var parts strings.Builder
	for i := 0; i < maxTaggedUsers+10; i++ {
		if i > 0 { parts.WriteByte(',') }
		fmt.Fprintf(&parts, `{"type":"tag","user":{"id":"u%d"}}`, i)
	}
	body := fmt.Sprintf(`{"webhook_id":"w","event":"taskCommentPosted","task_id":"t","history_items":[{"id":"h","field":"comment","comment":{"id":"c","text_content":"@owner only text","user":{"id":2},"comment":[%s]}}]}`, parts.String())
	events, err := NewClickUpParser("", "").Parser([]byte(body))
	if err != nil { t.Fatal(err) }
	if got := strings.Split(events[0].Metadata["tagged_user_ids"], ","); len(got) != maxTaggedUsers { t.Fatalf("tag cap=%d", len(got)) }

	withoutParts := strings.Replace(body, `"comment":[`+parts.String()+`]`, `"comment":"malformed"`, 1)
	events, err = NewClickUpParser("", "").Parser([]byte(withoutParts))
	if err != nil { t.Fatal(err) }
	if events[0].Metadata["tagged_user_ids"] != "" { t.Fatalf("text forged mention: %v", events[0].Metadata) }
}

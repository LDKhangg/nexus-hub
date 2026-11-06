package clickup

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/domain"
)

const commentBody = `{"event":"taskCommentPosted","task_id":"t1","task":{"name":"T"},
"history_items":[{"field":"comment","date":"1700000000000","comment":{"text_content":"hi","user":{"id":7,"username":"u"}}}]}`

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
	e, err := NewClickUpParser("", "").Parser([]byte(commentBody))
	if err != nil {
		t.Fatal(err)
	}
	if e.EventType != domain.EventCommandPosted || e.Content != "hi" || e.SenderID != "7" || e.OccurredAt.UnixMilli() != 1700000000000 {
		t.Fatalf("unexpected event: %+v", e)
	}
}

func TestParserIgnored(t *testing.T) {
	for _, body := range []string{
		`{"event":"taskCreated","task_id":"t1"}`,
		`{"event":"taskStatusUpdated","task_id":"t1","history_items":[{"field":"status"}]}`,
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
			body := fmt.Sprintf(`{"event":"taskCommentPosted","task_id":"t1","task":{"name":"T"},
"history_items":[{"field":"comment","date":"1700000000000","comment":{"text_content":%q,"user":{"id":%d,"username":"u"}}}]}`,
				tc.content, tc.userID)

			e, err := p.Parser([]byte(body))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}

			if e.FromOwnBot != tc.want {
				t.Errorf("FromOwnbot = %v, want %v", e.FromOwnBot, tc.want)
			}
		})
	}
}

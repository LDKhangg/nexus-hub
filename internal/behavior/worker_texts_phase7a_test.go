package behavior

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/relay"
)

// T-B-95 (REQ-8, R-B-8): failure texts for the timeout and diff-limit codes are the generic ones; an unknown code is shown as a code, never as free text.
func TestWorkerFailureText_GenericReasons(t *testing.T) {
	cases := map[string]string{
		"executor_timeout": "quá thời gian cho phép",
		"diff_too_large":   "thay đổi vượt giới hạn số dòng, cần bạn xác nhận",
		"tests_failed":     "test không đạt",
		"cancelled":        "đã huỷ",
	}
	for code, want := range cases {
		got := workerFailureText(&WorkerResult{ReasonCode: code})
		if got != "Việc thất bại: "+want+"." {
			t.Errorf("%s: %q", code, got)
		}
	}
	if got := workerFailureText(nil); !strings.Contains(got, "không rõ nguyên nhân") {
		t.Errorf("nil result: %q", got)
	}
	if got := workerFailureText(&WorkerResult{ReasonCode: "weird_code"}); !strings.Contains(got, "mã lỗi weird_code") {
		t.Errorf("unknown code: %q", got)
	}
}

// T-B-96 (REQ-7): a long summary is clipped in the owner's failure text.
func TestWorkerFailureText_SummaryBounded(t *testing.T) {
	got := workerFailureText(&WorkerResult{ReasonCode: "executor_crash", Summary: strings.Repeat("x", 5000)})
	if n := len([]rune(got)); n > 1100 {
		t.Fatalf("failure text has %d runes", n)
	}
}

// T-B-97 (REQ-6, R-B-6): Telegram's "message is not modified" counts as a successful edit; other errors do not.
func TestNotModified(t *testing.T) {
	if !notModified(&relay.DeliveryError{Code: "message_not_modified"}) {
		t.Fatal("message_not_modified not treated as success")
	}
	if !notModified(fmt.Errorf("edit: %w", &relay.DeliveryError{Code: "message_not_modified"})) {
		t.Fatal("wrapped message_not_modified not treated as success")
	}
	if notModified(&relay.DeliveryError{Code: "chat_not_found"}) || notModified(errors.New("boom")) || notModified(nil) {
		t.Fatal("other errors treated as not modified")
	}
}

// T-B-98 (REQ-8, R-B-7): a retry on an ended execution is a conflict (so existing conflict handling still applies) with a Vietnamese owner text.
func TestWorkerExecutionEnded_IsConflictWithText(t *testing.T) {
	if !errors.Is(ErrWorkerExecutionEnded, ErrConflict) {
		t.Fatal("ErrWorkerExecutionEnded must wrap ErrConflict")
	}
	if msgWorkerExecutionEnded != "Quy trình của việc này đã kết thúc nên không thể thử lại. Hãy gửi /task mới." {
		t.Fatalf("text %q", msgWorkerExecutionEnded)
	}
}

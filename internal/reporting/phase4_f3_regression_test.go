package reporting

import (
	"strings"
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/people"
)

// TC-R6 (Phase 4 F3, R6): a failure, skipped-approval or
// verdict-unavailable report keeps its reason unless a kept line actually
// substituted a non-empty {reason}. Another value that contains the reason
// text no longer counts.
func TestPhase4F3ReasonLine(t *testing.T) {
	kinds := []Kind{KindFailure, KindPRSkipped, KindVerdictUnavailable}

	t.Run("TC-R6 outcome containing the reason text keeps the reason line", func(t *testing.T) {
		for _, kind := range kinds {
			out, err := Render("{outcome}", kind, Values{Outcome: "Behavior run failed", Reason: "failed"})
			if err != nil || out != "Behavior run failed\nLý do: failed" {
				t.Fatalf("%s: %q err=%v", kind, out, err)
			}
			out, err = RenderLanguage("{outcome}", kind, Values{Outcome: "Behavior run failed", Reason: "failed"}, "en")
			if err != nil || out != "Behavior run failed\nReason: failed" {
				t.Fatalf("%s en: %q err=%v", kind, out, err)
			}
		}
	})

	t.Run("TC-R6 contact name crafted to equal the reason keeps the reason line", func(t *testing.T) {
		const reason = "Lý do: policy denied"
		name := people.CleanDisplayName(reason)
		for _, kind := range kinds {
			out, err := Render("{contact}: {outcome}", kind, Values{Contact: name, Outcome: "Bỏ qua", Reason: reason})
			if err != nil || out != name+": Bỏ qua\nLý do: "+reason {
				t.Fatalf("%s: %q err=%v", kind, out, err)
			}
			out, err = Render("{contact}", kind, Values{Contact: "policy denied", Reason: "policy denied"})
			if err != nil || out != "policy denied\nLý do: policy denied" {
				t.Fatalf("%s plain: %q err=%v", kind, out, err)
			}
		}
	})

	t.Run("TC-R6 a kept {reason} line is shown once", func(t *testing.T) {
		for _, kind := range kinds {
			out, err := Render("{outcome}\nVì: {reason}", kind, Values{Outcome: "Behavior run failed", Reason: "failed"})
			if err != nil || out != "Behavior run failed\nVì: failed" || strings.Count(out, "Lý do") != 0 {
				t.Fatalf("%s: %q err=%v", kind, out, err)
			}
			// The reason inside a longer kept line still counts as shown.
			out, err = Render("{outcome} ({reason})", kind, Values{Outcome: "Lỗi", Reason: "hết giờ"})
			if err != nil || out != "Lỗi (hết giờ)" {
				t.Fatalf("%s inline: %q err=%v", kind, out, err)
			}
		}
	})

	t.Run("TC-R6 a dropped {reason} line does not count", func(t *testing.T) {
		for _, kind := range kinds {
			out, err := Render("{outcome}\n?{link}Chi tiết: {reason} {link}", kind, Values{Outcome: "Behavior run failed", Reason: "failed"})
			if err != nil || out != "Behavior run failed\nLý do: failed" {
				t.Fatalf("%s: %q err=%v", kind, out, err)
			}
		}
	})

	t.Run("TC-R6 other kinds never get a reason line", func(t *testing.T) {
		for _, kind := range []Kind{KindNotify, KindPRApprove, KindDigest} {
			out, err := Render("{outcome}", kind, Values{Outcome: "Xong", Reason: "failed"})
			if err != nil || out != "Xong" {
				t.Fatalf("%s: %q err=%v", kind, out, err)
			}
		}
	})
}

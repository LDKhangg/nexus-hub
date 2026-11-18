package orchestrator

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// Phase 10 Fix B regressions, R-B-2 (commit b317aa1): the worker's result
// summary reaches the stored task result, bounded on a rune boundary, so the
// /plan report draft can show it. Fakes only (the step harness).

func fixbTaskResult(t *testing.T, h *shH) map[string]string {
	t.Helper()
	var result map[string]string
	if err := json.Unmarshal(h.plans.task(1, "w").Result, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func fixbFinish(t *testing.T, summary, state string) map[string]string {
	t.Helper()
	h := startWorker(t)
	h.step("run_01")
	h.jobs.set("run_01", func(j *WorkerJob) {
		j.State, j.Branch, j.PRURL, j.TestOutcome, j.Summary = state, "feat/x", "https://github.com/o/r/pull/3", "passed", summary
	})
	if res, err := h.step("run_01"); err != nil || res.WaitState != "" {
		t.Fatalf("%q %v", res.WaitState, err)
	}
	return fixbTaskResult(t, h)
}

// R-B-2: the summary of a succeeded job is stored in extra["summary"] of the task result.
func TestP10FixB_WorkerSummaryReachesTaskResult(t *testing.T) {
	got := fixbFinish(t, "Đã thêm endpoint /v2/orders và test", JobSucceeded)
	if got["summary"] != "Đã thêm endpoint /v2/orders và test" || got["branch"] != "feat/x" {
		t.Fatalf("result %v", got)
	}
}

// R-B-2: a cancelled job keeps its summary too; no summary stores no key.
func TestP10FixB_WorkerSummaryCancelledAndEmpty(t *testing.T) {
	if got := fixbFinish(t, "làm được một nửa", JobCancelled); got["summary"] != "làm được một nửa" {
		t.Fatalf("cancelled result %v", got)
	}
	if got := fixbFinish(t, "   ", JobSucceeded); got["summary"] != "" {
		t.Fatalf("blank summary stored: %v", got)
	}
	if _, has := fixbFinish(t, "", JobSucceeded)["summary"]; has {
		t.Fatal("an empty summary stored a key")
	}
}

// R-B-2: a multibyte summary over 500 bytes is cut on a rune boundary: still valid UTF-8, within 500 bytes, kept.
func TestP10FixB_WorkerSummaryMultibyteStaysValid(t *testing.T) {
	for name, in := range map[string]string{
		"two byte letters":  strings.Repeat("é", 400),
		"vietnamese words":  strings.Repeat("Đã xong phần việc này. ", 60),
		"three byte runes":  strings.Repeat("việc ✓ ", 200),
		"four byte emoji":   strings.Repeat("🎉", 300),
		"cut inside a rune": strings.Repeat("a", 499) + "é",
	} {
		t.Run(name, func(t *testing.T) {
			if len(in) <= maxResultTextBytes {
				t.Fatalf("test input is only %d bytes", len(in))
			}
			got := fixbFinish(t, in, JobSucceeded)["summary"]
			if got == "" || !utf8.ValidString(got) || len(got) > maxResultTextBytes || strings.ContainsRune(got, utf8.RuneError) {
				t.Fatalf("summary %d bytes valid %v: %q", len(got), utf8.ValidString(got), got)
			}
			if !strings.HasPrefix(in, strings.TrimSpace(got)) && !strings.HasPrefix(strings.Join(strings.Fields(in), " "), got) {
				t.Fatalf("summary is not a prefix of the worker's text: %q", got)
			}
		})
	}
}

// R-B-2 (abuse): control characters and line breaks in the worker's text never reach the stored summary,
// and invalid UTF-8 is replaced.
func TestP10FixB_WorkerSummaryIsCleaned(t *testing.T) {
	got := fixbFinish(t, "dòng 1\n\n\tdòng 2\x00\x07 xong\xff\xfe", JobSucceeded)["summary"]
	if !utf8.ValidString(got) || strings.ContainsAny(got, "\n\t\x00\x07") || !strings.HasPrefix(got, "dòng 1 dòng 2") {
		t.Fatalf("summary %q", got)
	}
}

// R-B-2: boundResultText itself.
func TestP10FixB_BoundResultText(t *testing.T) {
	if got := boundResultText("  ngắn gọn  "); got != "ngắn gọn" {
		t.Fatalf("got %q", got)
	}
	if got := boundResultText(""); got != "" {
		t.Fatalf("got %q", got)
	}
	long := strings.Repeat("ế", 1000)
	got := boundResultText(long)
	if len(got) > maxResultTextBytes || !utf8.ValidString(got) || got == "" {
		t.Fatalf("%d bytes, valid %v", len(got), utf8.ValidString(got))
	}
}

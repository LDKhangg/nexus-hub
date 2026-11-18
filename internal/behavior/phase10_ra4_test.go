package behavior

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
)

// Phase 10 Batch B, regression for R-A-4 (commit d1a4f86): a stale press on a
// clickup_draft approval never wipes buttons that are live on the message.
// Fakes only: the in-memory draft store of phase10_drafts_test.go.

// p10GetFailsFrom is a draft store whose GetDraft fails from its n-th call on
// (1 = every call). It does not hold its mutex while calling the inner store.
type p10GetFailsFrom struct {
	*p10Drafts
	mu   sync.Mutex
	n    int
	from int
}

func (s *p10GetFailsFrom) GetDraft(ctx context.Context, ws string, id int64) (ClickUpDraft, error) {
	s.mu.Lock()
	s.n++
	fail := s.from > 0 && s.n >= s.from
	s.mu.Unlock()
	if fail {
		return ClickUpDraft{}, errP10DB
	}
	return s.p10Drafts.GetDraft(ctx, ws, id)
}

func (e *p10Env) removedCount() int {
	e.msg.mu.Lock()
	defer e.msg.mu.Unlock()
	return len(e.msg.removed)
}

// TC-1 (R-A-4, human): the owner presses "Đăng" twice. Press 1 fails with a
// retryable error and the preview shows "Thử lại / Huỷ"; press 2 on the old,
// decided approval is stale but RemoveButtons is not called, and the retry
// button still posts exactly once.
func TestP10RA4_DoublePressKeepsRetryButtons(t *testing.T) {
	e := newP10Env(t)
	id := e.create(t, "đăng hai lần")
	msgID, m := e.msg.lastSent(t)
	dang := e.approvalOf(t, m, "Đăng")
	e.tool.script[ToolPostComment] = p10ResRetry
	if _, code, _ := e.pressRaw(CallbackApprove, dang, msgID, "owner"); code != "clickup_failed" {
		t.Fatalf("press 1 code %q", code)
	}
	if d := e.draft(t, id); d.State != DraftFailed || d.Body == "" {
		t.Fatalf("a retryable failure must wait: %+v", d)
	}
	ed := e.msg.lastEdit(t)
	if len(ed.buttons) != 1 || len(ed.buttons[0]) != 2 || ed.buttons[0][0].Text != "Thử lại" {
		t.Fatalf("retry buttons %+v", ed.buttons)
	}
	toast, code, remove := e.pressRaw(CallbackApprove, e.reload(t, dang), msgID, "owner")
	if code != "stale" || toast != msgDraftStale || remove {
		t.Fatalf("press 2: toast %q code %q remove %v", toast, code, remove)
	}
	if e.removedCount() != 0 {
		t.Fatalf("RemoveButtons called %d times", e.removedCount())
	}
	// The retry buttons are still alive and post once.
	delete(e.tool.script, ToolPostComment)
	cd, err := ParseCallbackData(ed.buttons[0][0].Data)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := e.ap.GetApprovalByNonce(context.Background(), p10WS, cd.Nonce)
	if err != nil {
		t.Fatal(err)
	}
	if _, code, _ := e.pressRaw(CallbackApprove, retry, msgID, "owner"); code != "" {
		t.Fatalf("retry code %q", code)
	}
	if d := e.draft(t, id); d.State != DraftPosted || e.tool.realPosts() != 1 {
		t.Fatalf("state %s posts %d", d.State, e.tool.realPosts())
	}
}

// TC-2 (R-A-4, human): the same double press through the inbox worker. The
// replay of the decided press and a press by another actor are both stale and
// neither removes the live retry buttons.
func TestP10RA4_DoublePressThroughInboxKeepsButtons(t *testing.T) {
	e := newP10Env(t)
	id := e.create(t, "qua hộp thư hai lần")
	msgID, m := e.msg.lastSent(t)
	w := p10InboxWorker(e)
	e.tool.script[ToolPostComment] = p10ResRetry
	out := w.callback(context.Background(), p10CallbackUpdate("Đăng", m, msgID, 1), p10ChatTo(), "owner", "c")
	if out.code == "stale" || out.retry {
		t.Fatalf("first delivery %+v", out)
	}
	if e.draft(t, id).State != DraftFailed {
		t.Fatalf("state %s", e.draft(t, id).State)
	}
	for _, actor := range []string{"owner", "intruder"} {
		out = w.callback(context.Background(), p10CallbackUpdate("Đăng", m, msgID, 2), p10ChatTo(), actor, "c")
		if out.code != "stale" {
			t.Fatalf("%s: outcome %+v", actor, out)
		}
	}
	if e.removedCount() != 0 {
		t.Fatalf("RemoveButtons called %d times", e.removedCount())
	}
	if n := len(e.tool.callsOf(ToolPostComment)); n != 1 {
		t.Fatalf("post calls %d", n)
	}
}

// TC-3 (R-A-4, human): after "Sửa" the draft has a new preview with live
// buttons. A stale press of the old approval that reaches the new message keeps
// the new buttons, and the new "Đăng" still posts the new text once.
func TestP10RA4_StaleOldApprovalOnNewPreviewKeepsButtons(t *testing.T) {
	e := newP10Env(t)
	id := e.create(t, "bản đầu")
	oldMsg, m1 := e.msg.lastSent(t)
	oldPost := e.approvalOf(t, m1, "Đăng")
	oldEdit := e.approvalOf(t, m1, "Sửa")
	e.press(CallbackFreeText, oldEdit, oldMsg)
	reply, err := e.d.HandleDraftReply(context.Background(), DraftReply{WorkspaceID: p10WS, IntegrationID: p10Bot, ChatID: p10Chat,
		Approval: e.reload(t, oldEdit), Text: "bản mới", Actor: "owner"})
	if err != nil || reply != msgDraftRevised {
		t.Fatalf("reply %q err %v", reply, err)
	}
	newMsg, m2 := e.msg.lastSent(t)
	if newMsg == oldMsg {
		t.Fatal("no new preview")
	}
	removedBefore := e.removedCount()
	toast, code, remove := e.pressRaw(CallbackApprove, oldPost, newMsg, "owner")
	if code != "stale" || toast != msgDraftStale || remove {
		t.Fatalf("stale press on new message: toast %q code %q remove %v", toast, code, remove)
	}
	// Through the inbox: old buttons data on the new message id.
	w := p10InboxWorker(e)
	if out := w.callback(context.Background(), p10CallbackUpdate("Đăng", m1, newMsg, 1), p10ChatTo(), "owner", "c"); out.code != "stale" {
		t.Fatalf("inbox outcome %+v", out)
	}
	if e.removedCount() != removedBefore {
		t.Fatalf("new buttons removed: %d -> %d", removedBefore, e.removedCount())
	}
	if n := len(e.tool.callsOf(ToolPostComment)); n != 0 {
		t.Fatalf("posted by a stale press: %d", n)
	}
	e.press(CallbackApprove, e.approvalOf(t, m2, "Đăng"), newMsg)
	calls := e.tool.callsOf(ToolPostComment)
	if len(calls) != 1 || calls[0].input["text"] != "🤖 bản mới" || calls[0].draft != id {
		t.Fatalf("calls %+v", calls)
	}
}

// TC-4 (R-A-4, requirement): a stale press on a message that shows no live
// approval of the draft removes the buttons: terminal draft, posted draft,
// another message, missing draft.
func TestP10RA4_StalePressRemovesDeadButtons(t *testing.T) {
	t.Run("cancelled draft", func(t *testing.T) {
		e := newP10Env(t)
		e.create(t, "huỷ rồi bấm lại")
		msgID, m := e.msg.lastSent(t)
		dang := e.approvalOf(t, m, "Đăng")
		e.press(CallbackReject, e.approvalOf(t, m, "Huỷ"), msgID)
		if _, code, remove := e.pressRaw(CallbackApprove, dang, msgID, "owner"); code != "stale" || !remove {
			t.Fatalf("code %q remove %v", code, remove)
		}
	})
	t.Run("posted draft", func(t *testing.T) {
		e := newP10Env(t)
		e.create(t, "đã đăng rồi bấm lại")
		msgID, m := e.msg.lastSent(t)
		dang := e.approvalOf(t, m, "Đăng")
		e.press(CallbackApprove, dang, msgID)
		if _, code, remove := e.pressRaw(CallbackApprove, dang, msgID, "owner"); code != "stale" || !remove {
			t.Fatalf("code %q remove %v", code, remove)
		}
		if e.tool.realPosts() != 1 {
			t.Fatalf("posts %d", e.tool.realPosts())
		}
	})
	t.Run("another message", func(t *testing.T) {
		e := newP10Env(t)
		e.create(t, "tin nhắn khác")
		msgID, m := e.msg.lastSent(t)
		dang := e.approvalOf(t, m, "Đăng")
		if _, code, remove := e.pressRaw(CallbackApprove, dang, msgID+50, "owner"); code != "stale" || !remove {
			t.Fatalf("code %q remove %v", code, remove)
		}
		// The real preview keeps working.
		if _, code, _ := e.pressRaw(CallbackApprove, e.reload(t, dang), msgID, "owner"); code != "" {
			t.Fatalf("real press code %q", code)
		}
		if e.tool.realPosts() != 1 {
			t.Fatalf("posts %d", e.tool.realPosts())
		}
	})
	t.Run("missing draft", func(t *testing.T) {
		e := newP10Env(t)
		e.create(t, "bản nháp biến mất")
		msgID, m := e.msg.lastSent(t)
		a := e.approvalOf(t, m, "Đăng")
		a.Payload, _ = json.Marshal(draftPayload{DraftID: 9999, SHA: "x", ChatID: p10Chat, Purpose: "preview"})
		if _, code, remove := e.pressRaw(CallbackApprove, a, msgID, "owner"); code != "stale" || !remove {
			t.Fatalf("code %q remove %v", code, remove)
		}
	})
}

// TC-5 (R-A-4, abuse): a storage error while the hub decides whether the
// buttons are live counts as live. A GetDraft error before that is a retry and
// removes nothing.
func TestP10RA4_StorageErrorKeepsButtons(t *testing.T) {
	t.Run("error in the live check", func(t *testing.T) {
		e := newP10Env(t)
		e.create(t, "lỗi khi kiểm tra")
		msgID, m := e.msg.lastSent(t)
		dang := e.approvalOf(t, m, "Đăng")
		e.d.d.Store = &p10GetFailsFrom{p10Drafts: e.store, from: 2} // call 1 binds the press, call 2 is the live check
		_, code, remove := e.pressRaw(CallbackApprove, dang, msgID+50, "owner")
		if code != "stale" || remove {
			t.Fatalf("code %q remove %v", code, remove)
		}
	})
	t.Run("error before the live check", func(t *testing.T) {
		e := newP10Env(t)
		e.create(t, "lỗi ngay đầu")
		msgID, m := e.msg.lastSent(t)
		dang := e.approvalOf(t, m, "Đăng")
		e.d.d.Store = &p10GetFailsFrom{p10Drafts: e.store, from: 1}
		_, code, remove := e.pressRaw(CallbackApprove, dang, msgID, "owner")
		if code != "retry" || remove {
			t.Fatalf("code %q remove %v", code, remove)
		}
		if e.removedCount() != 0 || len(e.tool.calls) != 0 {
			t.Fatalf("removed %d posts %d", e.removedCount(), len(e.tool.calls))
		}
	})
	t.Run("HasLiveButtons on a storage error", func(t *testing.T) {
		e := newP10Env(t)
		e.create(t, "kiểm tra trực tiếp")
		msgID, m := e.msg.lastSent(t)
		dang := e.approvalOf(t, m, "Đăng")
		p := DraftPress{WorkspaceID: p10WS, IntegrationID: p10Bot, ChatID: p10Chat, MessageID: msgID, Kind: CallbackApprove, Approval: dang, Actor: "owner"}
		if !e.d.HasLiveButtons(context.Background(), p) {
			t.Fatal("a live preview was reported dead")
		}
		e.d.d.Store = &p10GetFailsFrom{p10Drafts: e.store, from: 1}
		if !e.d.HasLiveButtons(context.Background(), p) {
			t.Fatal("a storage error must count as live")
		}
	})
}

// TC-6 (R-A-4, requirement): other approval kinds are unchanged. A stale press
// on a worker_job approval still removes its buttons even when drafts are wired.
func TestP10RA4_OtherApprovalKindsStillRemoveButtons(t *testing.T) {
	e := b3Buttons(t, WorkerJobFailed)
	e.w.d.ClickUpDrafts = newP10Env(t).d
	if out := e.press(0, b1Chat); out.code != "" {
		t.Fatalf("first press %+v", out)
	}
	before := len(e.m.removed)
	out := e.press(0, b1Chat)
	if out.code != "stale" || e.lastToast() != msgStale {
		t.Fatalf("second press %+v toast %q", out, e.lastToast())
	}
	if len(e.m.removed) != before+1 {
		t.Fatalf("RemoveButtons calls %d -> %d, want one more", before, len(e.m.removed))
	}
	if len(e.jobs.retries) != 1 {
		t.Fatalf("retries %v", e.jobs.retries)
	}
}

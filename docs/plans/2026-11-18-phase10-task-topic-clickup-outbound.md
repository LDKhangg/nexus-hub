# Phase 10 — Task topic as the full context, ClickUp outbound (MVP 1)

> **For agentic workers:** REQUIRED SUB-SKILL: Use `executing-plans` for an assigned task. The role boundaries and sequential handoff process in `AGENTS.md` override generic implementation/TDD instructions. Steps use checkbox (`- [ ]`) syntax for tracking.

**Status:** DRAFT 2026-11-18, waiting for owner approval. Owner decision D7 (ROADMAP §5): Phase 10 runs right after 7b and ends MVP 1; Phase 9 (policy) comes after MVP 1.

**Goal:** A ClickUp task topic in Telegram becomes the single place for that task. Everything that happens on the task in ClickUp (comments, replies, tags, status and assignee changes) arrives in the topic directly, not in General. From the topic the owner can push results back to ClickUp: a `/task` or `/plan` result, a comment, a reply to a specific ClickUp comment, or a comment that tags a person. In this phase, every ClickUp write asks the owner first (preview + buttons). Phase 9 decides later which writes may skip the question.

**Architecture:** No new service. Three changes on top of the existing pieces:
1. Destination resolution: before a ClickUp event or task-scoped report goes to General, the hub looks up an existing `clickup_task` topic (`telegram_topics`) and delivers there instead.
2. A message link table maps the Telegram message that shows a ClickUp comment to that comment's id. When the owner replies to that Telegram message, the hub knows which ClickUp comment to answer.
3. A ClickUp draft-and-confirm flow on top of the existing `ApprovalConfirmAction` path and the existing `clickup_post_comment` tool. The tool is extended with mentions and threaded replies. The orchestrator's `behavior-actions` executor (7b) uses the same flow.

**Tech Stack:** Go (go.mod version), PostgreSQL, pgx v5, sqlc, goose. No new dependency.

**Spec:** `docs/product/PCC-001.md` EPIC 02 (FR-013), EPIC 12 (FR-131–FR-133, FR-138–FR-141), E2E-008. The scope comes from the owner's flow (chat 2026-11-18) and `docs/product/ROADMAP.md` §Phase 10. The contract is read-only.

## Global Constraints

- Code, documentation and commits are in English. Owner-facing discussion is in Vietnamese.
- Baseline: `develop` `9cb1e57`. Branch: `feature/phase10`. Next migration number: `00031`.
- One handoff at a time per working tree. The Implementer changes production code only. The Tester changes tests only. The Reviewer is read-only.
- Nobody runs `go test` locally. Allowed checks: `go build ./...`, `go vet ./...`, `gofmt -l cmd internal`, `make generate` (second run clean), `goose -dir db/migrations validate`. The suite runs only in CI.
- Never edit old migrations and never hand-edit sqlc output. New tables are workspace-scoped, with composite foreign keys and RLS, like `telegram_topics`.
- **Every ClickUp write asks first (owner, 2026-11-18).** No code path posts to ClickUp without the owner pressing "Đăng" on a preview that shows the exact text, the task, and any mentioned person or replied-to comment. This holds for the assistant, `/task` and `/plan` results, and the orchestrator. Existing behaviors keep their configured autonomy unchanged (for example `pr-auto-approve` and `mention-report`).
- Generic hub: no behavior name, person or task is hard-coded. Routing works on subject keys only (`clickup_task:<id>`).
- Privacy: never log or store message text, comment text, prompts or tokens beyond what the flow needs. A draft body is stored only until it is posted, cancelled or expired. After that it is cleared, and only metadata stays (ids, state, times, hash). Link rows hold ids only.
- Loop guard: a comment the hub posted (the existing `FromOwnBot` marker) never comes back into the topic as a new ClickUp activity.
- Tests use fakes only (fake ClickUp, Telegram, provider). DB tests use loopback `TEST_DATABASE_URL`, database `nexus_storage_test`.

## Review Focus

- No ClickUp write without an explicit owner confirm bound to the exact draft (text hash, task, parent comment, mention ids). A stale, edited, expired or double-pressed button never posts twice and never posts different content.
- Destination resolution never sends to a wrong workspace, chat or topic. An unknown destination falls back to General, never to a random topic (EPIC 12 AC 4).
- Exactly once: a redelivered webhook gives one topic message. A confirm press retried after a crash gives one ClickUp comment (idempotency key per draft).
- A failed ClickUp post never changes the result state of the `/task` or `/plan` execution (EPIC 12 AC 5).
- The 7b carry-ins: an orchestrated execution waiting for a worker can always end (wall-time timer). Retry/Cancel act only on their own job.

## 1. Scope and decisions

### 1.1 What the owner gets (flow agreed 2026-11-18)

```
ClickUp: owner assigned to FC-123
  └─► General: notice + "📌 Mở topic task"                      (exists)
        └─► Topic "FC-123 · <title>"
              ├─ /task, /plan, chat → reports in the topic      (exists)
              ├─ every ClickUp activity on FC-123 → the topic    (REQ-1..4)
              ├─ result → preview "Đăng lên ClickUp?" → comment  (REQ-7, 9)
              └─ "tag X hỏi …" / reply to a shown comment
                 → preview → comment with @X / threaded reply   (REQ-6, 8)
```

### 1.2 Owner decisions

| ID | Decision |
|---|---|
| T1 | Order: Phase 10 now (ends MVP 1); Phase 9 after MVP 1 (D7). |
| T2 | In this phase, every ClickUp write asks the owner first. Phase 9 policy may relax this later. |

### 1.3 Lead calls (the owner may change any of them when approving)

- L1: A task without a topic behaves as today: its notices go to General with the "📌 Mở topic task" button.
- L2: A closed task topic (task done, W4) still receives the task's events. The first such event reopens it through the existing reopen path.
- L3: Inside a task topic, `notice_mentions_only` does not filter. Every comment from anyone, including the owner's own comments made in ClickUp, is shown. Comments posted by the hub are not shown (loop guard).
- L4: A comment that started a behavior run (for example `mention-report`) gets no separate activity message, as today. If that run's report topic is `general` and the task has a topic, the report goes to the task topic.
- L5: Mentions use the people directory (Phase 4) to turn a name into a ClickUp user id. An ambiguous or unknown name never guesses: the assistant asks which person.
- L6: **Deferred out of Phase 10** to a later "Phase 10b", because they do not serve the owner's flow. The Lead updates ROADMAP after approval.
  - unified `outbox_messages`: FR-139 stays partial;
  - GitHub inbound webhooks: FR-017 GitHub part;
  - adapter registry and demo adapter: FR-010, FR-011, E2E-010;
  - fallback destination: FR-143;
  - aggregation: FR-142;
  - connection lifecycle and health: FR-014, FR-016;
  - configurable on-started, completed, blocked and failed policies: FR-134–FR-137.

### 1.4 Boundaries

- Out of scope:
  - multi-slot workers (Phase 8, deferred);
  - policy engine and approval rules (Phase 9);
  - GitHub writes beyond what 7a already does;
  - a web UI;
  - ClickUp writes other than comment, reply and status;
  - the items in L6.
- Not changed: `/task`, `/plan` and the worker protocol, except carry-ins REQ-11–13; and existing behaviors and their autonomy.

## 2. Requirements and acceptance

| ID | Observable behavior | PCC IDs | Task |
|---|---|---|---|
| REQ-1 | A ClickUp event (comment, threaded reply, status change, assignee change) for a task that has a `clickup_task` topic in the workspace's forum chat is delivered to that topic, not to General. A task without a topic behaves as today (L1). The message shows who, what kind of event, and the text or change. Comment text goes through the existing untrusted-text rendering. | FR-131, FR-133 | R1 |
| REQ-2 | Inside a task topic, `notice_mentions_only` does not filter (L3). Comments the hub posted (`FromOwnBot`) are never shown. A comment that started a behavior run gets no extra activity message (L4). A redelivered webhook gives no second message. | FR-138, FR-018 | R1 |
| REQ-3 | A behavior run whose report topic is `general` and whose subject is a ClickUp task with a topic reports into that task topic. Fixed topics (for example `PR approve`) and `workflow` topics are unchanged. | FR-131 | R1 |
| REQ-4 | An event for a task whose topic is closed reopens the topic through the existing lifecycle reopen path, then delivers (L2). If Telegram rejects the topic (deleted or not found), the message goes to General once, with the "📌 Mở topic task" button. It never goes to a different topic. | FR-131, EPIC 12 AC 4 | R1 |
| REQ-5 | Every Telegram message that shows a ClickUp comment stores a link row: workspace, integration, chat, Telegram message id → task id and comment id (migration `00031`, ids only). Rows older than a retention setting (default 30 days) are pruned by the existing cleanup loop. | FR-132 | R1 |
| REQ-6 | `clickup_post_comment` accepts optional `parent_comment_id` (threaded reply) and optional `mention_user_ids` (ClickUp user ids, max 5). Mentions are sent in ClickUp's structured comment format, so the person is notified. A reply goes to the reply endpoint. Inputs are validated strictly. The tool stays tier write, runs only through the executor (policy, audit, idempotency), and keeps the 🤖 bot marker. The exact ClickUp request shapes are confirmed from ClickUp API docs and written in the code comment (unconfirmed fact U1). | FR-013, FR-132 | W1 |
| REQ-7 | ClickUp draft and confirm. A draft (task, text ≤ 8,000 runes, optional parent comment, optional mention ids, optional new status) is shown in the task topic as an exact preview with the buttons "Đăng", "Sửa", "Huỷ". "Đăng" posts once (idempotency key = draft id), then edits the preview to "✅ Đã đăng" with a link to the task. "Sửa": the owner's next reply to the preview replaces the text and shows a new preview; the old buttons stop working. "Huỷ" or expiry (setting, default 24 h) posts nothing. A second press, a press on an old preview, or a press by anyone but the owner has no effect. The draft body is cleared after it is posted, cancelled or expired. A ClickUp error shows "❌ Không đăng được: <reason code>" with a "Thử lại" button. | FR-013, FR-139, FR-140, FR-141, FR-138 | W1 |
| REQ-8 | In a task topic, the assistant has a tool `draft_clickup_comment` that only makes a draft (REQ-7) and never posts. Examples it must handle: "comment lên task là …", "tag anh X hỏi deadline", "đổi status sang review". When the owner's message replies to a Telegram message linked to a ClickUp comment (REQ-5), the draft is a threaded reply to that comment. Names resolve through the people directory; an unknown or ambiguous name gets a question back, never a guess (L5). Outside a task topic, the tool needs an explicit task id. | FR-132, FR-133 | W2 |
| REQ-9 | When a `/task` or `/plan` execution on a ClickUp task ends (completed or failed), the final report in the topic has a button "📤 Đăng lên ClickUp". Pressing it makes a draft (REQ-7) with the summary and artifact links (PR URL, branch). A failed or cancelled post never changes the execution's state. | FR-133, FR-135, E2E-008 | W2 |
| REQ-10 | The orchestrator's `behavior-actions` executor is wired, so Plan may offer `clickup.write` and `telegram.notify` (7b carry-in). A `clickup.write` task never posts directly. It creates a draft (REQ-7), and the plan task waits for the decision: posted → success with the comment id as evidence; cancelled or expired → the task is blocked, and evaluate decides. `telegram.notify` sends to the execution's topic. | FR-013, FR-049, ARCH-002 | K1 |
| REQ-11 | (7b carry-in) An orchestrated execution waiting for a worker job is in `WAITING_FOR_WORKER`, the same state `local-task` uses, not `WAITING_FOR_USER`. It wakes on the job result, on job failure or interruption, and in any case no later than its wall-time budget. At that point it ends with the O4 outcome, never waiting for the 24 h deadline. `/status WF-n` shows the wait. | FR-097, FR-063, FR-056 | K1 |
| REQ-12 | (7b carry-in) A worker job started by an orchestrated execution shows the same in-place progress message and Retry/Cancel buttons as a 7a `/task` job, in the same topic. Retry/Cancel act only on that job, and the execution sees the outcome. | FR-066, FR-068, FR-064 | K1 |
| REQ-13 | E2E-008 with fakes. A ClickUp-sourced task with a topic runs `/plan` to completion. The owner presses "📤 Đăng lên ClickUp", then "Đăng". Expected: the fake ClickUp receives exactly one comment on the right task, with the PR link; the topic gets the summary with links; nothing goes to another workspace, chat or topic. | E2E-008 | W2 (journey test by the Tester) |
| REQ-14 | Runbook section "Task topics and ClickUp replies (Phase 10)": the flow, the always-ask rule, retention and settings. Architecture note for destination resolution and message links (NFR-034). `nh telegram topics` output unchanged. | NFR-034 | D1 |

### Unconfirmed facts (the Implementer confirms them from ClickUp API docs; the Tester proves the code against recorded or doc-shaped payloads; live checks become test debt)

- U1: Request shape for a mention (structured `comment` array with a tag part and user id) and for a threaded reply (`POST /api/v2/comment/{comment_id}/reply`). Whether `notify_all: false` still notifies the mentioned person.
- U2: Whether ClickUp sends a threaded reply as `taskCommentPosted`, and which field carries the parent comment id.
- U3: The task link format to show after posting. ClickUp may return no direct link to a comment, so the link goes to the task.

## 3. Domain and interface decisions

### 3.1 Migration `00031_task_topic_outbound.sql`

- `telegram_message_links`: workspace_id, integration_id, chat_id, message_id, subject_kind (`clickup_comment`), task_id ≤ 64, comment_id ≤ 64, created_at. Unique on (workspace_id, integration_id, chat_id, message_id). RLS. Ids only.
- `clickup_drafts`: id, workspace_id, chat binding (integration, chat, thread), task_id, parent_comment_id, mention_user_ids, new_status, body (nullable; cleared when terminal), body_sha256, state (`pending`, `posted`, `cancelled`, `expired`, `failed`), source (`assistant`, `execution_report`, `orchestrator`), execution_id nullable, plan task key nullable, preview_message_id, posted_comment_id, expires_at, timestamps. RLS. A Lead call: if the existing approval rows can carry this cleanly, the Implementer may reuse them instead and must explain why in a Ruling.

### 3.2 Interfaces

- `behavior.DestinationResolver.ForClickUpTask(ctx, workspace, taskID) (thread, ok)`. The notice delivery and the behavior report path both call it. It reuses `topic_targets.go` and `topic_lifecycle.go`.
- `behavior.ClickUpDrafts` with `Create`, `Confirm(draftID, actor, previewMsgID)`, `Edit`, `Cancel` and `Expire`. Confirm re-checks state, hash and owner, then runs `clickup_post_comment` through the tool executor with idempotency key `draft:<id>`.
- `orchestrator.TaskExecutor` for `behavior-actions`: it creates a draft and returns "waiting". The engine wakes the execution through the existing workflow signal when the draft reaches a terminal state.

## 4. File ownership map

| Area | Files |
|---|---|
| Destination + links (R1) | `internal/adapters/storage/notice_repository.go`, `notice_delivery.go`, `internal/relay/` notice worker, `internal/behavior/topic_targets.go`, `topic_lifecycle.go`, `topic_notice.go`, migration `00031`, `db/query/telegram_topic.sql` (or new `outbound.sql`) |
| ClickUp tool + drafts (W1) | `internal/adapters/tools/clickup/write.go`, new `internal/behavior/clickup_drafts.go`, `approvals.go`, `inbox_worker.go` callbacks, storage store |
| Telegram entry (W2) | `internal/behavior/assistant_tools.go`, `assistant.go`, `local_task.go` / `orchestrated_task.go` final report button, `texts.go` |
| Carry-ins (K1) | `internal/orchestrator/steps.go`, `capability.go`, `internal/behavior/orchestrated_task.go`, `worker_notify.go`, `worker_jobs.go`, `internal/workflow/vocabulary.go`, `cmd/server/wiring.go` |
| Docs (D1) | `docs/runbooks/self-host-home.md`, `docs/architecture.md` |

## 5. Task packets

- **K1 — 7b carry-ins (Opus).** REQ 10–12. `behavior-actions` wired, using the draft flow from W1 through an interface stub until W1 lands (K1 runs after W1 if that is simpler). `WAITING_FOR_WORKER` with a wall-time wake. Progress and Retry/Cancel for orchestrated jobs. Commit `feat(orchestrator): Phase 7b carry-ins (actions executor, worker wait, job controls)`.
- **R1 — Destination resolution and message links (Opus).** REQ 1–5, migration `00031` (links part). Commit `feat(topics): ClickUp task events go to the task topic`.
- **W1 — ClickUp tool and drafts (Opus).** REQ 6, 7, migration `00031` (drafts part, same file if R1 has not merged it yet; otherwise `00032`). Commit `feat(clickup): drafted comments, replies and mentions with owner confirm`.
- **W2 — Telegram entry points (Sonnet).** REQ 8, 9, 13 (production side). Commit `feat(assistant): draft ClickUp comments from the task topic`.
- **D1 — Docs (Sonnet).** REQ 14. Commit `docs: task topics and ClickUp replies`.

Order: R1 → W1 → K1 → Batch A (test, review, fixes) → W2 → D1 → Batch B (test, review, fixes).

## 6. Agent dispatch and evidence gates

```text
R1 → W1 → K1 → Batch A Tester/CI (open test debt first; REQ-1..7, 10..12) → Batch A Reviewer → fixes
W2 → D1 → Batch B Tester/CI (REQ-8, 9, 13 journeys, E2E-008) → Batch B Reviewer → fixes
same-head PASS + APPROVE → completion report → Telegram report → merge on the owner's /merge → deploy
```

- Agents: Claude subagents per role, with the same role limits. Sonnet: W2, D1, tests and small fixes. Opus: R1, W1, K1 and every review. One handoff at a time.
- Test debt paid first in Batch A:
  - `docs/product/test-debt/hotfix-notice-topics.md` (TD-NT-1..3, written with this plan);
  - open review items TD-6, TD3-2 and TD3d-2 stay open unless a review covers them.
  - TD-7A-1, TD3-3 and TD3d-3 are live checks and stay with the owner.
- Live checks after deploy, owner and Lead together (become test debt if not done):
  - a comment in a task with a topic arrives in the topic;
  - a reply from the topic lands as a threaded reply in ClickUp;
  - a mention notifies the person (U1–U3).

## 7. Phase completion gate

- [ ] REQ-1–14 have CI evidence on the final head. No skip, race or panic. E2E-008 has a named journey test with fakes.
- [ ] Reviewer APPROVE on the same head. Critical and Important findings are resolved.
- [ ] The migration validates, generation is reproducible, and no text is stored after a draft ends or logged.
- [ ] The Lead updates `TRACEABILITY.md` and ROADMAP (L6 → Phase 10b), writes `docs/product/reports/<date>-phase10-report.md`, and reports on Telegram. Merging into `develop` (which deploys) needs the owner's `/merge`.

# Phase Completion Report

Phase: 10 — Task topics and ClickUp outbound (ends MVP 1)

Status: Implemented, tested and reviewed on `feature/phase10` at code head `a507263` (tests `658559f`). Not merged into `develop` yet (owner merges with `/merge` in Telegram). Base `develop` 9cb1e57.

Implemented requirements: REQ-1 … REQ-14 of `docs/plans/2026-11-18-phase10-task-topic-clickup-outbound.md` (PCC FR-013, FR-131–FR-133, FR-135, FR-138–FR-141, E2E-008, NFR-034; 7b carry-ins FR-049, FR-056, FR-063, FR-064, FR-066, FR-068, FR-097, ARCH-002). Owner decisions L1, L3, L5, L6; R-B-3 option A (`/comment`). FR-134, FR-136, FR-137 and the rest of PCC Phase 10 moved to Phase 10b (ROADMAP).

Delivered components:
- Destination resolution: ClickUp comment, threaded reply, status and assignee events for a task with a forum topic go to that topic (no mentions-only filter there); tasks without a topic keep General, now with the 📌 Mở topic task button. General behavior reports about a task go to its topic. A closed topic is reopened; a deleted topic falls back to General once. Own-bot comments and redeliveries never show twice.
- Message links (migration 00031): every Telegram message that shows a ClickUp comment stores ids only (task, comment); pruned after `message_link_retention_days` (30).
- `clickup_post_comment`: optional `parent_comment_id` (reply endpoint) and up to 5 structured mentions; executor only (policy, audit, idempotency, 🤖 marker). Draft-owned tool runs store only a hash and length of the text.
- ClickUp drafts (migrations 00032, 00034): exact preview with Đăng / Sửa / Huỷ; Đăng posts once (key = draft id); Sửa replaces the text from the owner's reply; Huỷ or 24 h expiry posts nothing; stale, double and foreign presses do nothing; failures show the reason with Thử lại; body cleared at the end; stuck `posting` replayed by a sweep; `failed_final` for errors that must not retry.
- Assistant: `draft_clickup_comment` (draft only, never posts). A reply to a shown ClickUp comment always reaches the assistant and drafts a threaded reply, even while a job runs. Names resolve only on an exact or a single whole-word match; anything else asks back. New command `/comment <text>` in a task topic always starts a draft, never a worker note.
- 📤 Đăng lên ClickUp on finished, failed or cancelled `/task` and `/plan` reports: a draft with the worker summary, PR link and an outcome line. The button stays after Cancel and after Retry/Cancel expire. A failed post never changes the execution.
- Orchestrator (7b carry-ins): `behavior-actions` executor wired (`clickup.write` → draft and wait for the decision; `telegram.notify` → the execution's topic); orchestrated worker waits use `WAITING_FOR_WORKER` (migration 00033) and wake on the job result or the wall-time budget; orchestrated worker jobs show the 7a progress message and Retry/Cancel.
- Docs: runbook "Task topics and ClickUp replies (Phase 10)" incl. `/comment`; architecture note on destination resolution and message links.

Tests executed: full suite with `-race` against a throwaway PostgreSQL in CI only, fakes only (no real ClickUp, Telegram, GitHub or Anthropic). Batch A (52 approved cases: routing, links, post tool, drafts, orchestrator executor, waits, test debt from the notice-topics hotfix), Batch B (TC-1..34, TC-21 dropped by the owner: draft tool, report button, E2E-008 journey), Fix B regressions (57 tests) and fix-B3/T-1 regressions.

Test results: CI run 44332870926 on `658559f`: success, 2803 tests, no fail, skip, race or panic.

Evidence:
- Batch A: CI 44262610317 red on test defects only (fixed by the Tester). Review A CHANGES: R-A-1 Important (no-retry failure stranded the orchestrator task; replan could double a comment), R-A-2, R-A-3 Minor → 0b49311; re-review APPROVE; R-A-4 Minor (double tap wiped buttons) → d1a4f86. Test Report A PASS (CI 44264907138), Fix A regressions green (CI 44280586752).
- Batch B: Review B CHANGES on 90393c8: R-B-1 Critical (reply to a shown ClickUp comment never reached the assistant), R-B-2 Important (/plan report lacked the worker summary), R-B-3 Important (draft request became a worker note during a job), R-B-4 Important (substring name match could tag the wrong person), R-B-5..8 Minor; CI defect D-B-1 Minor (wrong-message press closed the approval) → b317aa1; R-B-3 → `/comment` 7c49cfc (owner option A). Re-review APPROVE on 7c49cfc; R-B-9 Minor (expiry removed 📤) and T-1 (test gap) → a507263 + f89e2c9, review APPROVE. CI 44330666740 red on 3 test defects → 658559f; CI 44332870926 green.
- Agents: Claude subagents per role (Opus: R1, W1, K1, fixes, reviews; Sonnet: W2, D1, tests). Ledger `.superpowers/sdd/phase10/progress.md` (local).

Known limitations:
- Live ClickUp checks pending after deploy: U1 (mention and threaded-reply request shapes, whether `notify_all: false` notifies a mentioned person on a reply), U2 (threaded-reply webhook shape and parent field), U3 (task link format after posting).
- Plain text in a task topic while exactly one job runs still goes to that job as a note; use `/comment` to draft a ClickUp comment then.
- `/retry WF-n` looks up step `run`, not orchestrated `run_NN` (the Retry button works).
- A done-status notice may reopen a just-closed topic; the lifecycle sweep closes it again (about 30 min).
- Two truly concurrent 📤 presses are not atomic (the single inbox worker serialises them in practice).
- Live checks still open from earlier phases: TD-7A-1 (laptop), 7b `/plan` on a real task and route-eval.

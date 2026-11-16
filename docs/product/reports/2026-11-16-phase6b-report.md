# Phase Completion Report

Phase: 6b — Telegram topics per task

Status: Implemented, tested and reviewed on `feature/phase6b` at `6b514c8`. Not merged into `develop` yet (owner decision).

Implemented requirements: REQ-1 … REQ-10 and REQ-13 of `docs/plans/2026-11-16-phase6b-telegram-topics.md` (PCC FR-131 and FR-132 Telegram part, FR-012 Telegram part, FR-033/034/035/041 per topic, E2E-003 Telegram part). REQ-11 (router retest with route-eval) runs on the VPS after the merge deploys the `route-eval` command; its result is appended below.

Delivered components:
- Migration `00025_telegram_topics`: `telegram_topics` (one topic per subject: ClickUp task, workflow or fixed name; reservation lease; open/closed), conversation per (chat, thread), inbound thread id. Down folds topic conversations into the chat's General conversation without losing instructions, messages or bindings.
- One owner group per workspace (`owner_chat_id`, else `chat_id`, else the global setting), used by inbound, behavior messages, relay agent reports and ClickUp notices (`tenant.OwnerTargetConfig`).
- Topic routing: `report.topic` (`task` default, `workflow`, `general` or a fixed name such as "PR approve"); lazy topic creation; at most one resend to General on a definite rejection; unclear creates keep the reservation (no duplicate topics); non-forum chats remembered for 10 minutes.
- Topic-aware resolution: inside a task topic, plain text, questions and commands without an id resolve to that task's active workflow; several → buttons for that task only; none → assistant with the task as context. Explicit `WF-n` and replies keep priority. History per topic.
- Jump links: a lone ClickUp task link or `/topic <id>` in General answers with an "Mở topic" button (forum supergroups only); the same button on executions that fell back to General and on `/list`.
- Lifecycle: a task in a done/closed status type or a status named in `topic_close_statuses` closes its topic when no execution is active (status event + 30 s sweep); a later send reopens it; never deletes.
- CLI: `nh telegram topics`, `nh telegram group set|show`, `nh assistant route-eval`. Runbook section "Telegram topics (Phase 6b)".

Tests executed: full suite with `-race` against throwaway PostgreSQL in CI, fakes only. Batch A TC A1.1–A15.1, Batch B TC-B1-01…96, fix regressions TC-B1-R1a…R6.

Test results: CI run 43823427615 on `6b514c8`: all jobs success; no skip, race or panic.

Evidence:
- Batch A Test Report (7fedc96): D1 00025 Down failed with dependents → fixed a254393; CI 43816226869 green.
- Review #A1 on a254393: TD-RS-1/2 (hotfix router schema debt) closed; R1 Important (non-topic reply chains treated as topics), R2 Important (relay used chat_id, not the owner chat), R3 Minor (duplicate topic after unclear create), R4 Minor (Down deleted instructions) → fixed d7d3706, f4b920e; regressions 4e41a64; CI 43817611023 green; Review #A2 APPROVE (R5 notices → P4).
- Batch B Test Report (360ee6d): D1 owner-chat-only config rejected by notice ingest → fixed f58055c; CI 43821587252 green.
- Review #B1 APPROVE on f58055c with Minors R1–R6 (reopen re-closed by the sweep, status names ignored, stale thread id, owner-chat-only paths, bare link in non-forum chats, missing owner user id warning) → fixed 8382730; regressions 6b514c8; CI 43823427615 green; Review #B2 APPROVE on 6b514c8.
- Agents: all tasks, tests and reviews by Claude subagents under the role limits (Sonnet for P1, P2, P4, fixes and tests; Opus for P3 and reviews).
- Ledger: `.superpowers/sdd/2026-11-16-phase6b-telegram-topics/progress.md` (local).

Known limitations:
- R7 (Minor, accepted): when a send hits "topic closed" and `Recover` reopens the topic, the lifecycle is not told; if the task is remembered as done, the sweep may close it again within 30 s. Narrow path (owner closed the topic in the app while the DB said open). Carried into Phase 7.
- A timed-out topic create that Telegram did perform can still leave a second topic after the 60 s lease (the Bot API cannot list topics). No message is lost.
- Agent comment-pipeline reports and digests have no jump button (P4 ruling Q2).
- A topic closes up to ~30 s after the last execution ends (sweep, P4 ruling Q1).

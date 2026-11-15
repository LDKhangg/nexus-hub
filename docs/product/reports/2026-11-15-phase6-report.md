# Phase Completion Report

Phase: 6 — Conversations, workflow references and control commands

Status: Implemented, tested and reviewed on `feature/phase6` at `52af5e0`. Not merged into `develop` yet (owner decision). PCC IDs stay `IN_PROGRESS` in `docs/product/TRACEABILITY.md` until the merge.

Implemented requirements: REQ-1 … REQ-11 of `docs/plans/2026-11-15-phase6-conversations-control.md` (PCC EPIC 04 FR-030–FR-042; E2E-003 without worker delivery; NFR-034 runbook part). Owner decisions: Q1a `WF-<execution id>`, Q2b summaries only (no raw chat text stored), Q3a conversations bound to workspace + owner chat, Q4a instructions read by the behavior model step and the agent investigate step.

Delivered components:
- Migrations 00023 (conversations, metadata-only conversation messages, conversation↔workflow bindings, resource bindings, versioned workflow instructions; composite workspace FKs, RLS, Down) and 00024 (Telegram message/reply ids on the durable inbox).
- `WF-<id>` references: `workflow.FormatRef/ParseRef/FindRefs`; shown in behavior and agent reports (a `WF-n` line in short/default styles, `/trace n · WF-n` in detailed), approval prompts, CLI and assistant tools; `{workflow_ref}` template placeholder.
- Resource bindings at execution start (ClickUp task, comment, chat message, GitHub PR) and after every execution-owned Telegram send (`<integration>:<chat>:<message>`), stored after the message receipt with a bounded timeout; a binding failure never fails or repeats a send.
- Persistent conversation per (workspace, bot, chat): metadata for every inbound/outbound message; a sanitized summary (≤ 2,000 bytes, `triage_model`, one updater per conversation) seeds the assistant after a restart.
- Target resolution: exactly one explicit `WF-n` → reply to a bound message → clarification with up to 5 buttons (chat- and workspace-bound, expiring, request kept only in memory, decided only after the request succeeds). Multiple refs always ask. Questions (ending in `?`/`？`) go to the assistant with the resolved workflow as context; replies to finished workflows go to the assistant.
- Versioned instruction updates (`WF-n <text>`, `/note`, reply), idempotent per Telegram update, refused for finished executions; delivered as an escaped, 8 KiB-bounded `<owner_instructions>` block at the next model/investigate step, outside untrusted context; the used version is checkpointed. Instructions cannot change actions, targets, budgets, dry-run or output schema.
- Commands `/status`, `/list`, `/cancel`, `/resume`, `/note`; cancel/resume through the Phase 5 Làm/Huỷ path. Assistant tool `workflow_note` writes only to one unambiguous owner target.
- CLI `nh workflow show` adds resource kinds/counts and instruction metadata (no text); runbook section "Conversations and control (Phase 6)".

Tests executed: full suite with `-race` against throwaway PostgreSQL in CI, fakes only. Batch A (storage, refs, bindings, summaries) and Batch B (resolver, intake, clarification, commands, delivery, CLI, privacy) plus regression tests for every fixed defect and finding.

Test results: CI run 43670776642 on `52af5e0`: all jobs success; 1258 tests passed, 0 failed, 0 skipped; no data race or panic.

Evidence:
- Review: #1 on `c99d6d8` (R1 Critical raw text in approval payload, R2–R4 Important) → fixed `d43dee0`; #2 on `8833585` (R1 fixed `84b8486`; R2 concurrent presses rejected: sequential inbox worker, idempotent effects); #3 on `4d40a60` (R1 Important explicit ref lost to reply target, R2 Minor terminal replies) → fixed `1db162b`; #4 APPROVE on `52af5e0`.
- Defects found by tests and fixed: D1 instruction replay after terminal (`3b7a422`), F1 receipt before binding (`3b7a422`), D-A2 ParseRef (`ffed251`), R-A3 report layout (`acef5a4`), D1–D4 /list paging, metadata-only /status, clarification hint, intake sanitising (`c99d6d8`), callback workspace check (`18ac0f1`), WF-prefixed questions (`0a45234`).
- Ledger with rulings: `.superpowers/sdd/2026-11-15-phase6-conversations-control/progress.md` (local).
- The OpenAI agent quota ran out near the end; the last test, fix and review rounds were done by Claude agents under the same role limits.

Known limitations:
- `/note` arguments are capped at 512 characters by the durable inbox (`telegram_updates.args` CHECK); longer instructions use `WF-n <text>` (up to 2,000 characters, kept only in memory until stored).
- Pending clarification text lives in process memory: after a restart the button asks the owner to resend.
- Conversation-to-user binding is the owner chat only (FR-031 partial until the users table, Phase 4b).
- Delivery into worker-side execution (E2E-003 worker part) is Phase 8; live Telegram/Claude evidence is Phase 13.

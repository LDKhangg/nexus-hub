# Phase Completion Report

Phase: 5 — Durable workflow engine (plus REQ-17/18: open Telegram assistant with Claude as orchestrator)

Status: Implemented, tested and reviewed on `feature/phase5` at `2ac779b`; deployed to the local home hub on 2026-11-15 and the LEAP workspace cut over to the durable queue. Not merged into `develop` yet (owner decision). PCC IDs stay `IN_PROGRESS` in `docs/product/TRACEABILITY.md` until the merge.

Implemented requirements: REQ-1 … REQ-18 of `docs/plans/2026-11-13-phase5-durable-workflow-engine.md` (PCC FR-073–096, FR-098–100, FR-106, FR-108, NFR-002–004 as scoped by the plan; E2E-005 fake-provider part only).

Delivered components:
- Workflow definitions on the behavior import/validate/publish/enable flow; schema 2 orchestration (step graph, retry, timeouts, worker constraints, notification/approval references); restore into a new version.
- Execution state machine with append-only history, steps, bounded checkpoints, attempts and cumulative usage (migrations 00016–00022).
- One persistent job queue: due times, leases, fenced claim tokens, retry backoff, dead letters, execution deadlines, reconciliation at startup and periodically.
- Durable approval/user-input waits and signals; resume pinned to a wait generation; manual retry as a linked child execution.
- Behavior runs and the Phase 2 comment pipeline run as durable executions with legacy-equivalent reports, budgets, trusted targets, dry-run and undo; unknown external outcomes are reported, never resent.
- Idempotent, fenced, offline cutover (`nh workflow cutover [--apply]`).
- Operator CLI `nh workflow list|show|steps|history|jobs|dead-letter|cancel|resume|retry`, metadata only (reference text as SHA-256 digests; every defined reason code shown, unknown values as `unknown`).
- Telegram assistant: open conversation through `assistant_model` (default `claude-opus-5-5`), bounded read tools, Làm/Huỷ proposals for alias/prefs/workflow commands with crash-safe replay, chat-bound confirmations, per workspace+chat in-memory history (20 exchanges, 64 KiB), ordered long replies with per-part delivery budgets, silence after cancellation.
- Fix of a Phase 4 defect (D-P1): claimed behavior runs now carry the behavior name, so per-behavior report prefs/templates apply.

Tests executed: full suite with `-race` against throwaway PostgreSQL in CI, fakes only (no real GitHub/ClickUp/Telegram/Anthropic). Approved Test Plans: Batch A TC-1..28, Batch B TC-B01..B47, Batch C TC-C01..C24, Batch D TC-D01..D24, plus P7 TC-P701..P707 and regression cases for every fixed defect.

Test results: CI run 43630791438 on `2ac779b`: all jobs success; 1120 tests passed, 0 failed, 0 skipped; no data race or panic.

Evidence:
- Review: Final Review #1 on `a0d6320` (R1 Important: CLI hid valid reason codes) → fixed `8db64c8` → APPROVE on `d5d9050`, confirmed on `2ac779b` (test-only delta). Earlier: Batch A/B reviews (R1–R7 fixed), P9 source review R1–R4 fixed and approved.
- Defects found by tests and fixed in production: Batch A D1–D4, Batch B D1, CLI payload disclosure (`5a8402e`), D-P1 (`0bbaf77`), P9 hardening M1/M2 (`42374d1`).
- Ledger with rulings: `.superpowers/sdd/2026-11-13-phase5-durable-workflow-engine/progress.md` (local).

Known limitations:
- Crash/restart/reclaim safety is proven with fake providers only; live ClickUp/Telegram/Claude behavior after a crash is open evidence (E2E-005 live part: Phase 12/13).
- Assistant history is in memory only and is lost on restart (persistent conversations: Phase 6).
- Workflow controls are CLI and Telegram Làm/Huỷ only; no UI. External worker pool and capability scheduling: Phase 8.
- Rollback after cutover is only by restoring the pre-upgrade database backup.

Open blockers: none for the phase. Merge of `feature/phase5` into `develop` awaits the owner.

Architecture decisions: one generic execution-job queue reusing behavior definitions (no second engine); schema 1 kept and normalized; terminal states immutable; manual retry creates a pinned child execution; unknown external outcome is a visible failure, never an automatic resend; offline fenced cutover with no old-binary rollback; reason-code allowlists owned by each package; assistant confirmations bound to their chat.

Next phase dependencies: Phase 6 (conversational workflow controls, persistent conversation state) builds on the durable waits/signals and the assistant tool layer; Phase 8 on worker constraints and the job queue; open test debt files in `docs/product/test-debt/` remain for the next Phase Brief.

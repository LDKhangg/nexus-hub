# Phase Completion Report

Phase: 7a — Local worker bridge

Status: Implemented, tested and reviewed on `feature/phase7a` at `c85e9ca`. Not merged into `develop` yet (owner merges with `/merge` in Telegram). The live check with the owner's laptop runs after the merge deploys the hub.

Implemented requirements: REQ-1 … REQ-19 of `docs/plans/2026-11-16-phase7a-local-worker.md` (PCC FR-057–FR-061, FR-063, FR-064, FR-066–FR-068, FR-070–FR-072, FR-097, FR-103, FR-107, FR-109, FR-110, NFR-019, NFR-030, ARCH-003, E2E-007 with a fake executor). Traceability rows moved to IN_PROGRESS (Phase 8 keeps the multi-device remainder; live check pending).

Delivered components:
- Migration `00026_worker_bridge`: `worker_devices`, `worker_enroll_codes`, `worker_jobs`, `worker_events`, telemetry latest/hourly rollups, `worker_task_routes`; `approvals_kind_check` now also allows `worker_job` and `workflow_target` (the latter was missing), `local_task` internal behavior kind. RLS like earlier tables.
- Enrollment: `/worker enroll` one-time code (10 min, single use, hashed); `nh worker enroll --hub <url>` stores the device token (0600) on the laptop; `/worker list|revoke`, `nh worker devices|revoke|jobs`.
- Hub protocol `/worker/v1/*`: token shape check before any DB call, global bucket for failed lookups (known devices bypass it), per-device rate limit, 64 KiB bodies; long-poll ≤ 55 s; lease 10 min, heartbeat 20 s; lost poll response reissues the lease only to a poll started after it; exactly-once result with idempotency key; progress seq dedupe; cancel via heartbeat; sweeper (INTERRUPTED, offline after 90 s, resume within 24 h); polls end on server shutdown; telemetry intake and threshold-aware assignment (Claude limit, disk, RAM).
- Laptop worker `nh worker run|status|clean`: config `~/.config/nexus-hub/worker.yaml`; first heartbeat at once; clone/fetch registered repo, per-job worktree and branch `nexus/<task>-<short>`, repo+branch lock; Claude Code headless with file tools only (no Bash, web or MCP), `--settings` blocking reads and writes outside the worktree, sensitive-path denies, claude ≥ 2.1.314 required; repo test command with a sandbox HOME and no gh/git/SSH-agent credentials; diff limit (binary files weigh 100 lines); push to the nexus branch only, never force; draft PR via the owner's `gh`; durable fsync journal outbox; restart recovery (`worker_restarted`); sleep inhibitor while busy; worktree retention. `executor: opencode` is refused until it can be sandboxed.
- Hub UX: `/task <text>` in a ClickUp task topic (or `/task <TASK-ID> <text>`) starts built-in workflow `local-task` (step `worker.run`); topic messages: waiting for the laptop, threshold wait with reset time, device that took it, one progress message edited in place, result with PR link, failures with Retry/Cancel buttons (chat-bound, 24 h); follow-up text delivered on the next heartbeat until acknowledged; `/retry WF-n`; `/worker` card per device (state, CPU, RAM, disk, Claude limits, tokens, jobs, failures, hours, last seen).
- Runbook section "Local worker (Phase 7a)"; README command list.
- Also carried: hotfix test debt TD-CA-1..3 and TD-NM-1..3 paid (R-4: a redelivered mention stays silent).

Tests executed: full suite with `-race` against throwaway PostgreSQL in CI only (no local runs), fakes only (fake hub/executor/runner/messenger; real git against local bare remotes). Batch A T-1…T-61 + fix regressions; Batch B T-B-1…T-B-98, T-B2-1…5, TestB3* hub/storage cases.

Test results: CI run 44009727911 on `c85e9ca`: all jobs success, 1960 tests, no skip, race or panic.

Evidence:
- Batch A: Test Report (c35ab9d) D-1 payload secret keys → b2be605. Review A: R-1 Important (lost poll response stranded the lease) → 7258408; R-2 shutdown → 514017d; R-3 bad-token flood → aa01e2d, 935eb75; R-4 redelivered mention → 0128986; re-review R-5 Important (stale poll rotated a delivered lease) → 316dbf8; APPROVE at cf7e6ac; CI 43893022683 green on aae9fbb.
- Batch B: Review B CHANGES on 0dbf931: R-B-1 Important (Bash prefix rules escaped the worktree) → b3f6761, 5efdb04; R-B-2 Important (opencode unrestricted) → af344f0; R-B-3 Important (test command with owner credentials) → 4a6deeb; R-B-4..8 → 3484cb5; re-review R-B-9 Important (writes outside the worktree) + R-B-10 → 6f1dfc5, R-B-11 docs → b2cdc75; APPROVE at b2cdc75. Tester defects: B-CI-1 git add with `.nexus` ignored → ff1188b; D-B-1..3 + binary diff → 1a42d82; D-B-5 `/task` word as id → ef7df99; D-B-6 Retry/Cancel buttons never encoded → c85e9ca. Final review APPROVE at c85e9ca.
- Test dispute B-CI-2 (sender retry test failed every op): Lead ruling, test fixed (9cf3f1e); production unchanged.
- Agents: Claude subagents under role limits (Sonnet W-1, W-4, small fixes, tests; Opus W-2, W-3, main fixes, reviews). Ledger `.superpowers/sdd/phase7a/progress.md` (local).

Known limitations:
- Live check (laptop enroll, `/task` → draft PR, offline wait, lid close mid-job) not done yet; runs after deploy. Deploy the hub before running a laptop worker (heartbeat field `ack_instruction_ids`).
- Claude limits left are reported as unknown (the CLI does not expose them); threshold checks then never block.
- Confinement is permission-based (Claude Code settings + environment scrubbing), not an OS sandbox. OpenSSH can still use unencrypted `~/.ssh` keys from code run by the test command; the runbook advises passphrase-protected keys or a read-only deploy key.
- opencode executor refused; FAILED jobs wait for Retry without a deadline; no periodic re-notice while a job waits; one device at a time (Phase 8).
- Test debt: `docs/product/test-debt/phase7a-local-worker.md`.

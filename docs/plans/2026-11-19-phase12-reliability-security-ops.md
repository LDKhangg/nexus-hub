# Phase 12 — Reliability, security and operations hardening

> **For agentic workers:** REQUIRED SUB-SKILL: Use `executing-plans` for an assigned task. The role boundaries and sequential handoff process in `AGENTS.md` override generic implementation/TDD instructions. Steps use checkbox (`- [ ]`) syntax for tracking.

**Status:** DRAFT 2026-11-19. Plan only. The owner asked to record the plan and **not** start implementation. No task is dispatched until the owner says go. The owner's three decisions (§1.2) are fixed. Every Lead call in §1.3 can be changed by the owner before approval.

**Goal:** Prove that the hub survives failure and attack on the real deployment (Oracle VPS, Supabase PostgreSQL, Cloudflare tunnel). A restart, a crash, a duplicate webhook or a dead worker never loses acknowledged work and never runs a side effect twice. The owner can see queue depth, worker status, errors and LLM cost, and gets a Telegram alert when a workflow is stuck or failed. Old data is cleaned up by rule. The database can be restored from a Supabase backup by following a tested runbook. Git, the one binary the hub runs, cannot reach the network except GitHub, cannot fill the disk and runs inside an OS sandbox.

**Architecture:** No new subsystem. The work hardens what exists: the workflow queue and reconciler (`internal/workflow/reconcile.go`, Phase 5), the worker lease sweeper (`internal/behavior/worker_jobs.go:520`), the approval and gate sweeps (`internal/behavior/approvals.go:586`, `policy_gates.go:305`), the HTTP server and its shutdown (`cmd/server/main.go:257-295`), and the git runner (`internal/adapters/tools/execrunner/runner.go`). New pieces are small: a `/metrics` endpoint, a stuck/failed workflow watcher that sends Telegram alerts, a retention sweeper driven by settings, and an OS sandbox wrapper for git.

**Tech Stack:** Go (go.mod version), PostgreSQL (Supabase), pgx v5, sqlc, goose, Echo. One possible new dependency: `github.com/prometheus/client_golang` (Lead call L2). Sandbox: `bubblewrap` (`bwrap`) on the VPS (Lead call L5).

**Spec:** `docs/product/PCC-001.md` NFR-001–NFR-007, NFR-010, NFR-012, NFR-013, NFR-017, NFR-024, NFR-027–NFR-030, NFR-032, NFR-040, the DB criteria for backup/restore and retention, Gate B, Gate C. `docs/product/ROADMAP.md` §Phase 12. The contract is read-only.

## Global Constraints

- Code, documentation and commits are in English. Owner-facing discussion is in Vietnamese.
- Baseline: `develop` `37b16fa` (Phase 9 merged). Branch: `feature/phase12`. Next migration number: `00038`.
- One handoff at a time per working tree. The Implementer changes production code only. The Tester changes tests only. The Reviewer is read-only.
- Nobody runs `go test` locally. Allowed checks: `go build ./...`, `go vet ./...`, `gofmt -l cmd internal`, `make generate` (second run clean), `goose -dir db/migrations validate`. The suite runs only in CI. **One exception, owner decision O2:** the load test (REQ-12) runs on the owner's local machine, started by the Lead in the owner's chat session.
- Never edit old migrations and never hand-edit sqlc output. New tables are workspace-scoped, with composite foreign keys and RLS.
- **No behaviour change for the owner's flows.** Hardening never changes what a behavior, plan or approval does on the happy path.
- **Safe by default.** Retention deletes only what its rule names, never rows that a live execution, open approval or open wait still points to. A retention or alert error never deletes more or alerts in a loop.
- Privacy: metrics, alerts and logs carry ids, counts, states, durations and error codes. Never message text, prompts, tool inputs or outputs, tokens or Authorization headers. Metric labels never contain user text or unbounded values.
- `/metrics` is not public: it is bound to loopback or protected (Lead call L3). The Cloudflare tunnel never exposes it.
- Tests use fakes only (fake GitHub, ClickUp, Telegram, provider). DB tests use loopback `TEST_DATABASE_URL`, database `nexus_storage_test`. No test touches Supabase or the VPS.
- Generic hub: no repo, person or behavior name is hard-coded.

## Review Focus

- Acknowledged means durable: every inbound event the hub answered 2xx for is in the DB before the answer, and is processed after any crash.
- Exactly-once side effects across restart: leases expire and work resumes, but idempotency keys stop a second send, post, merge or approval use.
- Graceful shutdown drains in-flight work within a bounded time and leaves nothing half-committed.
- Retention can never delete referenced or in-flight rows.
- Metrics and alerts leak no private data and cannot be reached from the internet.
- Git sandbox: no network except `github.com` over HTTPS, no write outside its workspace, disk use capped. A sandbox start failure fails closed (git does not run unsandboxed).

## 1. Scope and decisions

### 1.1 What the owner gets

- Confidence that a VPS reboot, a `systemctl restart`, a crash or a Telegram/ClickUp retry storm loses nothing and doubles nothing, backed by tests for every Gate B item.
- `/metrics` (loopback) and `nh status`: queue depth per state, worker online/offline and last heartbeat, error counts by kind, LLM tokens and cost per day.
- `nh execution timeline <id>`: the ordered steps, waits, approvals and errors of one execution.
- A Telegram alert in the General topic when a workflow is stuck (no progress past its threshold) or failed, at most once per execution per state.
- Retention: old message logs, tool runs, policy decisions, finished executions and their events are cleaned on a schedule set in `nh settings`, with `nh retention show|run --dry-run`.
- A recovery runbook: restore Supabase from its built-in backup, rotate the master key, recover a stuck execution, re-register webhooks.
- Git that cannot leak data to arbitrary hosts or fill the disk.

### 1.2 Owner decisions

- O1 (2026-11-19): backups use **Supabase's built-in backups**. No own `pg_dump` cron. Phase 12 documents and tests the restore path from a Supabase backup (into a scratch project or local PostgreSQL), plus the master key backup that Supabase does not hold.
- O2 (2026-11-19): the **load test runs on the owner's local machine**, driven by the Lead from the owner's current chat session, against a locally started hub and a local PostgreSQL. Not on CI, not against the VPS.
- O3 (2026-11-19): the **OS-level sandbox for git is in scope** (12d). Recorded in this plan only; no implementation until the owner says go.

### 1.3 Lead calls (the owner may change any of them)

- L1: Split into four batches 12a–12d (§5), each with its own Test and Review round, to keep handoffs small.
- L2: Metrics use `prometheus/client_golang` text format on `/metrics`. Alternative if the owner prefers no new dependency: a hand-written text exposition of the same few gauges and counters.
- L3: `/metrics` listens on a second listener bound to `127.0.0.1` (setting `metrics_addr`, default `127.0.0.1:9090`), not on the public Echo server. No auth needed because it is loopback only.
- L4: Stuck thresholds per state live in settings: `RUNNING` without a heartbeat for 15 min, `WAITING_FOR_APPROVAL` past its expiry, queued for more than 30 min. The alert dedup key is (execution id, state). Alerts stop after 20 per hour (counter in the log).
- L5: The git sandbox uses `bwrap` with a read-only root, a writable bind of the repo workspace only, a private `/tmp`, no `/home`, and network allowed only through the existing egress guard (HTTPS proxy or allow-list to `github.com`). Disk quota per workspace via a size check before and after each git call plus a per-repo cap (`git_workspace_max_mb`). If `bwrap` is missing, git does not run and the tool returns `sandbox_unavailable`.
- L6: Retention defaults (days): message logs 30, tool runs 90, policy decisions 180, finished workflow executions and events 90, delivered outbound records 30. `0` means keep forever. Rows referenced by an open execution, wait or approval are always kept.
- L7: `/healthz` and `/readyz` stay in Phase F as the roadmap says. Phase 12 only uses them if Phase F lands first.

### 1.4 Boundaries

- Out of scope: multi-slot workers and the scheduler (Phase 8, deferred by D7), the admin UI (Phase 11), own backup tooling (O1), external monitoring (Grafana, uptime services), health endpoints (Phase F), deploy automation changes (Phase 13).

## 2. Requirements and acceptance

| ID | Observable behavior | PCC IDs | Task |
|---|---|---|---|
| REQ-1 | Every inbound webhook (Telegram, ClickUp, GitHub) is stored durably before the handler returns 2xx. A crash after the 2xx and before processing still processes the event once after restart. | NFR-001, NFR-003 | A1 |
| REQ-2 | A duplicate delivery of the same webhook (same provider id or same body hash in the dedup window) is answered 2xx and produces no second execution, message or side effect. | NFR-004, Gate B | A1 |
| REQ-3 | After a crash, the reconciler and sweepers at startup return expired leases to the queue, re-arm missing wakeups and resume waits. Each side effect that already ran (Telegram send, ClickUp post, GitHub merge, approval use) is not repeated, proven by its idempotency key. | NFR-002, NFR-004, Gate B | A1 |
| REQ-4 | SIGTERM: the hub stops accepting new webhooks, lets in-flight HTTP requests and job steps finish up to `shutdown_timeout` (default 25 s, below systemd's 30 s), releases its leases, and exits 0. Work that did not finish is resumed by the next start. | NFR-005 | A1 |
| REQ-5 | Concurrent workflows: N executions running at the same time on one worker do not share state, deadlock or lose updates. A worker that dies mid-job has its lease expire and the job retried up to its retry limit, then marked failed. | NFR-012, Gate B | A2 |
| REQ-6 | Approvals and policy gates survive restart: an open approval stays open, a decision taken before the crash is applied once after it, and an expired one never runs. | Gate B | A2 |
| REQ-7 | Backpressure: when queued executions exceed `queue_max_pending` (default 500) new non-urgent work is refused with a clear log and status, and the webhook still answers 2xx after storing the event. Resource quotas (per-workspace concurrent executions, per-day LLM budget) are settings with safe defaults. | NFR-012 | A2 |
| REQ-8 | `/metrics` on the loopback listener exposes: executions by state, queue depth, oldest queued age, worker online and last heartbeat age, errors by kind, webhook latency histogram, LLM tokens and cost by day. No label carries user text. | NFR-028, NFR-029, NFR-030 | B1 |
| REQ-9 | `nh status` prints the same numbers. `nh execution timeline <id>` prints steps, waits, approvals, retries and errors in order, with times and error codes only. | NFR-027, NFR-028 | B1 |
| REQ-10 | A stuck or failed execution sends one Telegram alert to the General topic (id, behavior name, state, age, error code). Same execution and state never alerts twice. Alert rate is capped (L4). | NFR-032 | B2 |
| REQ-11 | Migration `00038` adds retention settings if needed. The retention sweeper deletes rows older than their rule, in batches, skipping rows referenced by open executions, waits or approvals. `nh retention show` and `nh retention run --dry-run` print counts only. | NFR-024, DB criteria | C1 |
| REQ-12 | Load test (O2): a local driver sends webhooks to a local hub and creates at least 100 workflow executions. All 100 finish, the DB queries them back correctly, and the webhook handler p95 is under 500 ms on a healthy local DB. The script and its result live in the repo (`scripts/loadtest/`, `docs/product/reports/`). | NFR-007, NFR-010 | C2 |
| REQ-13 | Restore drill (O1): runbook steps restore a Supabase backup into a scratch target, run migrations check, start the hub against it with the master key, and confirm data. The drill is done once and its date and result are recorded. | NFR-006, NFR-040, DB criteria | C3 |
| REQ-14 | Git runs inside `bwrap` (L5). It can reach only `github.com` over HTTPS through the egress guard, writes only inside its workspace, and is stopped when the workspace exceeds `git_workspace_max_mb`. Missing sandbox fails closed. | NFR-017, Gate C | D1 |
| REQ-15 | Security pass against Gate C: no plaintext credentials in repo or logs (scan in CI), every admin/worker endpoint authenticated, privileged actions go through the policy engine, worker tool boundaries tested, external input cannot change policy. HTTPS for every public endpoint (Cloudflare tunnel) documented. Findings fixed or recorded. | NFR-013, NFR-017, Gate C | D2 |
| REQ-16 | Recovery runbook section "Recovery (Phase 12)": restore from Supabase, master key backup and rotation, stuck execution recovery, re-register webhooks, read `/metrics` and alerts. | NFR-040 | D3 |

## 3. Domain and interface decisions

- Inbound durability: confirm or add a store-then-ack path in `internal/delivery/http` handlers (`cmd/server/main.go:255-256`). Dedup by provider delivery id where one exists, else body SHA-256 within a window.
- Shutdown: one `shutdown_timeout` setting; the workflow worker, behavior sweepers and outbound senders take a context that `main.go` cancels after HTTP shutdown, and each releases its leases.
- Metrics: package `internal/metrics` with a small registry interface so domain packages do not import Prometheus directly. Wiring in `cmd/server/wiring.go`.
- Alerts: `internal/behavior/workflow_alerts.go` (watcher) using the existing Telegram notice path (`internal/relay/notice_worker.go`); dedup table or column decided in A/B tasks.
- Retention: `internal/behavior/retention.go` sweeper + `db/query/retention.sql`; settings keys in `internal/settings/settings.go`.
- Git sandbox: `execrunner` gains a `Sandbox` option that wraps the fixed git binary with `bwrap` args; the binary and arguments stay fixed, no shell.

## 4. File ownership map

| Area | Files |
|---|---|
| Reliability (A1, A2) | `cmd/server/main.go`, `internal/delivery/http/*`, `internal/workflow/{reconcile,worker}.go`, `internal/behavior/worker_jobs.go`, storage queue queries, `internal/settings/settings.go` |
| Observability (B1, B2) | new `internal/metrics/`, `cmd/server/wiring.go`, `cmd/setup-notices` (`nh status`, `nh execution timeline`), new `internal/behavior/workflow_alerts.go`, `texts.go` |
| Data ops (C1–C3) | migration `00038`, `db/query/retention.sql`, new `internal/behavior/retention.go`, `cmd/setup-notices` (`nh retention`), new `scripts/loadtest/`, runbook |
| Security (D1–D3) | `internal/adapters/tools/execrunner/runner.go`, `internal/adapters/tools/code/workspace.go`, CI secret scan step, `docs/runbooks/self-host-home.md`, `docs/architecture.md` |

## 5. Task packets

- **12a — Reliability.** A1: REQ 1–4, commit `feat(reliability): durable ack, crash resume and graceful shutdown`. A2: REQ 5–7, commit `feat(reliability): worker failure retry, approval persistence and backpressure`.
- **12b — Observability.** B1: REQ 8–9, commit `feat(ops): loopback metrics, status and execution timeline`. B2: REQ 10, commit `feat(ops): Telegram alerts for stuck and failed workflows`.
- **12c — Data ops.** C1: REQ 11, commit `feat(ops): retention sweeper and controls`. C2: REQ 12 (owner-local run, O2), commit `test(load): 100-execution load test and webhook p95`. C3: REQ 13, commit `docs: Supabase restore drill`.
- **12d — Security.** D1: REQ 14 (O3), commit `feat(security): sandbox git with bwrap, egress and disk caps`. D2: REQ 15, commit `chore(security): Gate C pass`. D3: REQ 16, commit `docs: recovery runbook`.

Order: 12a → test/review/fix → 12b → test/review/fix → 12c → test/review/fix → 12d → test/review/fix.

## 6. Agent dispatch and evidence gates

```text
A1 → A2 → Batch 12a Tester/CI (REQ-1..7, Gate B) → Reviewer → fixes
B1 → B2 → Batch 12b Tester/CI (REQ-8..10) → Reviewer → fixes
C1 → C2 (local load run, O2) → C3 (restore drill) → Batch 12c Tester/CI (REQ-11) → Reviewer → fixes
D1 → D2 → D3 → Batch 12d Tester/CI (REQ-14..15, Gate C) → Reviewer → fixes
same-head PASS + APPROVE → completion report → owner merges
```

- Agent backend (opencode or Claude subagents) is chosen at dispatch time, by quota, with the same role limits.
- Nothing is dispatched until the owner approves this plan (§Status).
- Live check after deploy, with the owner: `/metrics` answers on the VPS loopback only; one forced stuck execution alerts once; `nh retention run --dry-run` prints counts; git sandbox active (`bwrap` present).

## 7. Phase completion gate

- Test Report PASS and Review Report APPROVE on the same code head; CI green with no skip, race or panic.
- Gate B and Gate C items each point to a test or evidence.
- Load test result (REQ-12) and restore drill (REQ-13) recorded in the Phase Completion Report.
- `docs/product/TRACEABILITY.md` updated for the IDs above; Phase Completion Report in `docs/product/reports/`.
- Merge into `develop` only on the owner's word.

# Phase 7a — Local Worker Bridge Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `executing-plans` for an assigned task. The role boundaries and sequential handoff process in `AGENTS.md` override generic implementation/TDD instructions. Steps use checkbox (`- [ ]`) syntax for tracking.

**Status:** DRAFT 2026-11-16 — written for the owner's review. Not approved; nothing is implemented. Open decisions in §1.3.

**Goal:** The VPS hub stays the orchestrator (Telegram, ClickUp, GitHub events, approvals, state). Heavy work — clone, edit, build, test, run Claude Code — runs on the owner's laptop when it is online, using the owner's own Claude subscription login and GitHub credentials. From a task topic the owner sends `/task <what to do>`; the laptop does the work in an isolated git worktree, pushes a branch, opens a draft PR from the owner's account, and the result appears in the same topic. When the laptop is offline the job waits and the topic says so.

**Architecture:** A new `nh worker` command runs on the laptop. It connects **out** to the hub over HTTPS through the existing Cloudflare tunnel (no inbound port, no fixed IP), authenticates with a per-device token, long-polls for work, holds each job with a lease and heartbeats, runs it with a local executor (Claude Code headless, opencode fallback) in a per-job git worktree, and posts progress and an idempotent result back. On the hub, worker jobs are Phase 5 durable workflow steps of a new kind `worker.run`; the hub never sends secrets to the worker.

**Tech Stack:** Go (go.mod version), PostgreSQL, pgx v5, sqlc, goose. New migration `00026`. No new external service. The laptop needs `git`, `gh` (logged in as the owner), and `claude` (logged in with the subscription) or `opencode`.

**Spec:** `docs/product/PCC-001.md` FR-057–FR-061, FR-063, FR-064, FR-066–FR-068, FR-070, FR-071, FR-072, FR-097, FR-103, FR-107, FR-110, NFR-019, ARCH-003, E2E-007 (worker failure, fake executor); roadmap decision D1 (workers run project tests) is resolved by running them on the laptop. The contract is read-only.

## Global Constraints

- Code, documentation and commits are English; owner-facing discussion is Vietnamese.
- Baseline: `develop` after hotfix 6b.1 (`98febd7`). Branch `feature/phase7a`. Migration `00026` only.
- One handoff at a time per shared working tree. Implementer changes production only; Tester changes tests only; Reviewer is read-only. Lead/Implementer never run `go test`; the suite runs only in CI.
- **The connection is always laptop → hub.** The hub never connects to the laptop, never needs its IP, and authenticates by device token only (rotatable, revocable, stored hashed). Changing networks, NAT, sleep and reconnects are normal, not errors.
- **No secret travels to the worker.** GitHub and Claude credentials stay on the laptop (`gh`, `claude` logins). The job payload holds only ids, repo coordinates, branch names, the owner's instruction and bounded context; the hub stores the instruction text only as the Phase 6 instruction rows already allow, never in logs.
- **No fallback execution.** When no worker is online the job waits (`WAITING_FOR_WORKER`) and the task topic is told; it never runs on the VPS, on GitHub Actions or with the API key (owner decision 2026-11-16).
- **Output is a branch and a draft PR opened with the owner's own account.** The worker never merges, never approves, never pushes to the default or a protected branch, never force-pushes.
- Every job runs in its own git worktree under a configured `repos_dir` on the laptop; one write lock per repository + branch (FR-072).
- Privacy unchanged: no prompts, instructions, code, diffs or model output in hub logs; the result stored on the hub is the structured contract (status, summary ≤ 2,000 runes, PR link, artifact refs, usage), shown only to the owner.
- Tests use fakes only: a fake worker client against the real HTTP handlers, a fake executor; never real Claude, GitHub, ClickUp or Telegram.

## Review Focus

- A stolen or revoked token can do nothing; one device cannot read another workspace's jobs.
- Exactly-once result: lease expiry, reconnects and retried uploads never run a job twice concurrently or record two results.
- Offline, sleep and network changes leave every job in a clear state the owner can see in the topic.
- The executor cannot escape its worktree, cannot push to protected branches, and receives no hub secret.

## 1. Scope and decisions

### 1.1 In scope

- Device enrollment, worker presence, job lease protocol, local executor, result and progress, `/task` from a task topic, Telegram/CLI visibility.

### 1.2 Out of scope (later phases)

- Plan/evaluate orchestration and DELEGATE decisions (Phase 7b, the former Phase 7 plan).
- Several laptops, scheduling priorities, per-user concurrency, backpressure (Phase 8 remainder).
- Policy engine and auto-approval rules (Phase 9); GitHub/ClickUp comment replies (Phase 10).

### 1.3 Decisions

Owner decisions taken 2026-11-16:

| ID | Decision |
|---|---|
| W1 | Split: Phase 7a worker bridge first, then Phase 7b orchestrator. |
| W2 | The laptop connects out over HTTPS long-poll through the Cloudflare tunnel; works with changing IPs and networks. |
| W3 | Executor on the laptop: Claude Code headless with the owner's subscription; opencode as fallback. |
| W4 | Laptop offline → job waits and the task topic is told; no fallback to the VPS, GitHub Actions or the API key. |
| W5 | Output: branch + draft PR from the owner's personal account (that account cannot approve its own PR). |

Open, for the owner (recommended option first):

| ID | Question | Options |
|---|---|---|
| W6 | Long-poll vs WebSocket | **a)** HTTPS long-poll, each request ≤ 55 s (under Cloudflare's ~100 s limit), reconnect with backoff · b) WebSocket through the tunnel (fewer requests, more moving parts) |
| W7 | How long a disconnected job keeps its lease | **a)** heartbeat every 20 s, lease 10 min; after expiry the job becomes `INTERRUPTED`, the topic is told, and the same device may resume it from its worktree within 24 h; afterwards the owner chooses Retry/Cancel · b) lease expiry requeues the job automatically from scratch |
| W8 | Job size limits | **a)** per job: 60 min wall time, executor turn limit 50, diff ≤ 2,000 changed lines (larger → stop and ask) · b) other numbers (settings, changeable later) |
| W9 | Which repositories a job may touch | **a)** only repositories registered on the hub (`nh github-repo`) and present under the laptop's `repos_dir` · b) any GitHub repo the owner's `gh` can see |
| W10 | Where the task's instruction comes from | **a)** the `/task` text plus the ClickUp task (title, description, recent comments) read on the laptop with a read-only ClickUp token stored on the laptop · b) only the `/task` text and the task id (the executor reads ClickUp itself if it has access) |

## 2. Requirements and acceptance

| ID | Observable behavior | PCC IDs | Task |
|---|---|---|---|
| REQ-1 | `nh worker enroll --hub <url>` on the laptop asks for a one-time code; the owner gets the code with `/worker enroll` in Telegram (expires in 10 min, single use). Enrollment stores a device token on the laptop (file mode 0600) and only its hash on the hub, with a device name and capability profile. `/worker list` and `nh worker devices` show devices, last seen, state; `/worker revoke <name>` revokes at once. | FR-057, FR-058 | W-1 |
| REQ-2 | Presence: the worker polls `POST /worker/v1/poll` (long-poll ≤ 55 s) and heartbeats; the hub records last seen and marks the device offline after 90 s without contact. Changing IP or network needs no action. | FR-059, FR-107 | W-2 |
| REQ-3 | Lease protocol: a poll may return one job with a lease token; the worker heartbeats `POST /worker/v1/jobs/{id}/heartbeat` (W7 interval); a missed lease moves the job per W7; a stale lease token is rejected (409) for heartbeats, progress and results. A job is never leased to two devices at once. | FR-060, FR-106, FR-107 | W-2 |
| REQ-4 | Capability match: a job lists required capabilities (`repo.write`, `build.<lang>`, `executor.claude` …); only a device whose profile covers them may lease it. No online match → `WAITING_FOR_WORKER` and one topic message ("Đang chờ máy local online"); when a matching device comes online the job starts and the topic is told. | FR-103, FR-097 | W-2, W-4 |
| REQ-5 | Executor: the worker creates a worktree for the job's repo on a new branch `nexus/<task-id>-<short>` from the default branch, runs Claude Code headless (`claude -p`, subscription login) — or opencode when configured — inside it with the job instruction and limits (W8), runs the repository's test command when the profile has one, commits, pushes the branch with the owner's `gh` credentials, and opens a draft PR. It never pushes to the default/protected branch, never force-pushes, never merges or approves. | FR-061, FR-063, NFR-019, ARCH-003 | W-3 |
| REQ-6 | Progress: the worker posts bounded progress events (phase: cloning, editing, testing, pushing; counts; no code or prompts); the topic shows one message that is edited in place, not a stream of messages. | FR-066 | W-3, W-4 |
| REQ-7 | Result contract: `POST /worker/v1/jobs/{id}/result` with an idempotency key; status `succeeded | failed | needs_input | cancelled`, summary ≤ 2,000 runes, branch, PR URL, test outcome, artifact refs, executor usage. A repeated upload with the same key returns the first result; a different result for a finished job is rejected. | FR-067, FR-070 | W-2, W-3 |
| REQ-8 | Failure reporting: executor crash, test failure, limit hit, push/PR failure and lost lease each produce a fixed reason code in the result and a topic message with Retry/Cancel buttons (chat-bound, expiring). | FR-068, E2E-007 | W-3, W-4 |
| REQ-9 | Cancellation: `/cancel` in the task topic (or the button) marks the job cancelling; the worker sees it on the next heartbeat, stops the executor, keeps the worktree, and reports `cancelled`. | FR-064 | W-2, W-3 |
| REQ-10 | Workspace hygiene on the laptop: one write lock per repo + branch; worktrees of finished jobs are removed after a retention period (setting, default 7 days); `nh worker clean` removes them now. | FR-071, FR-072 | W-3 |
| REQ-11 | `/task <instruction>` inside a ClickUp task topic starts a built-in workflow `local-task` (Phase 5 engine) with one `worker.run` step for that task; outside a task topic it asks which task (or accepts `/task FC-6583 <instruction>`). The topic shows `WF-n`, the device that took it, progress, and the result with the PR link. Follow-up text in the topic while the job runs is stored as a Phase 6 instruction and delivered to the worker with the next heartbeat. | FR-060, FR-066 | W-4 |
| REQ-12 | Audit: every lease, heartbeat loss, result and revoke is recorded (device id, job id, times, reason code) and visible in `nh worker jobs` / `nh workflow show WF-n`; no text. | FR-110 | W-1, W-2 |
| REQ-13 | Security: worker endpoints accept only a valid device token (constant-time hash compare), are rate-limited per device, reject bodies over 64 KiB, and only return jobs of the device's workspace. The job payload never contains a secret; tests prove a revoked token gets 401 on every endpoint. | NFR-019, ARCH-003 | W-1, W-2 |
| REQ-14 | Runbook section "Local worker (Phase 7a)": install, enroll, run as a user service on Linux/macOS/Windows, `repos_dir`, executor choice, what happens offline, revoke a lost laptop. | — | W-4 |

## 3. Interfaces (summary)

- Hub HTTP (behind the existing tunnel): `POST /worker/v1/enroll`, `POST /worker/v1/poll`, `POST /worker/v1/jobs/{id}/heartbeat`, `POST /worker/v1/jobs/{id}/progress`, `POST /worker/v1/jobs/{id}/result`. JSON, versioned, device token in the `Authorization` header.
- Storage (`00026`): `worker_devices` (workspace, name, token hash, capabilities, state, last_seen, revoked_at), `worker_jobs` (execution id, step id, required capabilities, state, device id, lease token hash, lease expiry, attempt, result JSON, idempotency key), `worker_events` (audit). RLS like existing tables.
- Workflow: step kind `worker.run`, built-in definition `local-task`; states add `WAITING_FOR_WORKER`, `INTERRUPTED`.
- Laptop: `nh worker enroll|run|status|clean|devices`; config file `~/.config/nexus-hub/worker.yaml` (hub URL, repos_dir, executor, test commands per repo).

## 4. Task split

| Task | Difficulty | Agent | Content |
|---|---|---|---|
| W-1 enrollment, devices, auth, audit, migration 00026 | Medium | Claude Sonnet (implementer limits) | REQ-1, REQ-12, REQ-13 |
| W-2 lease protocol, presence, capability match, cancellation | Hard | Claude Opus (implementer limits) | REQ-2, REQ-3, REQ-4, REQ-7, REQ-9 |
| W-3 laptop worker: executor, worktree, push, draft PR, progress, hygiene | Hard | Claude Opus (implementer limits) | REQ-5, REQ-6, REQ-8, REQ-10 |
| W-4 `/task`, `local-task` workflow, topic messages, runbook | Medium | Claude Sonnet (implementer limits) | REQ-4, REQ-8, REQ-11, REQ-14 |
| Batch A tests (W-1, W-2) / Batch B tests (W-3, W-4) | — | Claude Sonnet (tester limits) | fake worker client, fake executor, real handlers + throwaway PostgreSQL |
| Reviews | — | Claude Opus (reviewer limits) | read-only |

Dispatch: W-1 → W-2 → Batch A test → review → fixes → W-3 → W-4 → Batch B test → review → fixes → report on Telegram → merge after the owner's `/merge` → live check with the owner's laptop.

## 5. Roadmap change (applied when this plan is approved)

- Phase 7 becomes **7a Local worker bridge** (this plan) and **7b AI orchestrator** (`docs/plans/2026-11-16-phase7-ai-orchestrator.md`, renamed; its O3 "DELEGATE" now targets the local worker).
- Phase 8 keeps the remainder: several devices/slots, scheduler priority, per-user concurrency, backpressure, NFR-009, E2E-002.
- Roadmap decision D1 (worker tests) is resolved: project tests run on the laptop.
- Carry-ins from Phase 6b and hotfix 6b.1 go to 7b: router tuning (route-eval 13/15, 12/15), R7 (Recover reopen skips the lifecycle hook), lowercase custom IDs, command behaviors with custom IDs; the route-eval result is appended to the 6b report.

## 6. Phase completion gate

- [ ] All REQ-1–14 cases have CI evidence on the final head; no skip, race or panic.
- [ ] Reviewer APPROVE on the same head; Critical/Important findings resolved.
- [ ] Live check: the owner's laptop enrolls, takes a `/task` from a task topic, opens a draft PR; offline → waiting message; lid closed mid-job → interrupted/resume path.
- [ ] Lead updates `docs/product/TRACEABILITY.md`, writes `docs/product/reports/<date>-phase7a-report.md`, sends the report to Telegram; merge after the owner's `/merge`.

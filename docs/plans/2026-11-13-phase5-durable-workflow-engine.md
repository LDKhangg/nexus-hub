# Phase 5 — Durable Workflow Engine Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `executing-plans` for an assigned task. The role boundaries and sequential handoff process in `AGENTS.md` override generic implementation/TDD instructions. Steps use checkbox (`- [ ]`) syntax for tracking.

**Status:** APPROVED by the owner on 2026-11-14 (execute with the opencode agents; local release and Telegram report after completion; merge into `develop` still needs OK). REQ-17–18 / P9 added at the owner's request.

**Goal:** Turn the existing behavior and Phase 2 agent pipelines into durable, inspectable executions that survive restart without silently repeating known side effects.

**Architecture:** Evolve the current engine, not a second product or an external workflow platform. Reuse `behaviors` / immutable `behavior_versions` for definitions; introduce a shared execution/step/history model and one authoritative execution-job queue. Existing agent, behavior, approval and outbound code become adapters around that runtime, preserving their current policies and user-facing reports.

**Tech Stack:** Go 1.27.0, PostgreSQL, pgx v5, sqlc, goose, existing YAML/JSON and CLI infrastructure. No new service or dependency is planned.

**Spec:** `docs/product/PCC-001.md` EPIC 07–09, §4, §5, E2E-005 and NFR-002–004; phase scope in `docs/product/ROADMAP.md` §Phase 5. The contract is read-only.

## Global Constraints

- Code, documentation and commits are English; owner-facing discussion is Vietnamese.
- Baseline inspected: `feature/phase4`, `7b813413e4e3a6a78463d046e0a9a67d4be25628`. Phase 4 is working; do not reopen its completion or require Phase 4b before this phase.
- Before execution, the Lead records the actual approved branch/base/head. If Phase 4 is merged first, rebase the *plan's references*, not the owner's working tree, against that approved base.
- One handoff at a time per shared working tree. Implementer changes production only; Tester changes tests only; Reviewer is read-only. No agent is dispatched by preparing this plan.
- Lead/Implementer never run `go test`. Their checks are `go build ./...`, `go vet ./...`, `gofmt -l cmd internal`, `make generate` (second run clean), and `goose -dir db/migrations validate`. The full suite runs only in CI under the tester guide.
- Never edit old migrations or hand-edit generated sqlc files. Allocate new migration numbers from the actual head (currently the next is `00016`).
- Never print secrets, Authorization headers, prompts, tool input/output or payloads. New lifecycle tables store allowlisted IDs, states, reason codes, counters and references, not copied message bodies or model transcripts.
- Preserve workspace scoping in every query and composite relationship; apply the existing RLS pattern. This is not a substitute for Phase 4b identity/authentication work.
- Existing action policy, approvals, nonce/actor validation, dry-run, trusted-target selection, undo, report preferences/templates and unknown-outcome safeguards remain enforced.
- Tests use fakes, never real GitHub/ClickUp/Telegram/Anthropic. DB tests require loopback `TEST_DATABASE_URL`, database `nexus_storage_test`.
- Local commits only when executing an approved brief; no push, PR, merge, destructive reset or deployment without owner permission.

## Review Focus

1. Crash between an external request and its receipt: recovery must inspect existing evidence or expose uncertainty, never blindly resend (P3/P4/P6/P7).
2. An old worker returns after lease expiry or cancellation: no checkpoint, status change or new external action may be authorized by its stale token (P2/P3/P4).
3. Approval decision races with restart, expiry or cancellation: no lost wakeup, bypass or association with an unrelated alias/feedback approval (P5).
4. Cutover with debounced comments, an active agent claim and a waiting behavior: no lost cursor, duplicate report, dual claimer or workspace crossover (P6/P7).
5. Edit, restore or retry a workflow while it has live executions: immutable version pins and original side-effect identity remain intact (P1/P2/P8).

## 1. Scope and decisions

### 1.1 What the owner gets

- Workflow configurations still use the familiar behavior import/validate/publish/enable/disable flow, with versioned orchestration, dependency, retry and timeout settings.
- An execution has a persisted state, timeline, steps, attempts and actionable failure reason. Restart resumes safe work or reports a recoverable/uncertain failure.
- CLI operators can inspect executions and the queue, cancel work, resume an eligible wait, and request a safe retry of a failed execution.
- Current ClickUp comment → triage/investigate → Telegram report and current behaviors continue to work, with existing destinations, budgets, approvals, reports and dry-run semantics.

### 1.2 Approaches considered

1. **Extend the current PostgreSQL engine — selected proposal.** Reuses immutable behavior versions, tool/action receipts and outbound dedupe; migration effort is explicit and current operation remains recognizable.
2. **Introduce a separate workflow engine/platform.** Adds infrastructure and a second definition/authorization model before the product needs it; rejected for this phase.
3. **Only add status columns to current queues.** Smaller patch, but keeps two schedulers and cannot reliably express checkpoints, dependency history or approval recovery; rejected.

These are planning decisions proposed by the Lead, not a claim that the owner has already approved this detailed design.

### 1.3 Boundaries

- Phase 4b's generic inbound inbox/replay, identity linking and adapter registry are **not dependencies**. Start at today's authenticated/trusted trigger boundaries and retain their existing dedupe keys. Do not claim generic inbound replay or full FR-identity coverage.
- Phase 6 owns conversational workflow controls and persistent conversation state; this phase's new controls are CLI-only. Existing Telegram behavior/approval UX stays functional.
- Phase 7 owns AI planning/evaluation. `PLANNING` and `EVALUATING` here are lifecycle stages for deterministic registered steps, not new LLM autonomy.
- Phase 8 owns the external worker pool, resource scheduling, heartbeats and capabilities. Phase 5 validates/pins worker-constraint configuration, but accepts only capabilities the current in-process executor actually supports; unsupported combinations fail validation. No shell execution or new tool permission.
- Phase 9's generalized approval policy and Phase 10's response router are not implemented. Reuse current action/approval/outbound adapters.
- Execution jobs absorb agent/behavior scheduling only. Notification outbox, digest, chat polling and Telegram assistant queues keep their existing purposes.
- No UI, deployment, live-service acceptance or opportunistic fixes to Phase 4 assistant queue minors. Real E2E-005 remains Phase 13 evidence; fake-provider crash tests are Phase 5 evidence only.

## 2. Requirements and acceptance

Every requirement includes the Global Constraints. Evidence must identify the code head and test commit/CI run, not merely mark a checkbox.

| ID | Observable behavior | PCC IDs | Owner tasks |
|---|---|---|---|
| REQ-1 | Create/update drafts; validate; publish an immutable version; enable only a published configuration; export/import JSON and YAML. Schema 1 inputs remain valid and keep their meaning. Invalid drafts may be saved but cannot publish. | FR-073–078, FR-084, FR-086 | P1 |
| REQ-2 | Pin validated orchestration, step dependencies, registered worker constraints, notification/approval rules and retry/timeout settings. Unknown step kinds, cycles, missing dependencies and unsupported capabilities fail with field errors before publish. | FR-079–083, FR-096 | P1 |
| REQ-3 | Editing or restoring a definition cannot change an existing execution. Restore copies a selected published configuration into a draft and publishes it as a **new** version; it never mutates historical rows. Disabled/unpublished definitions do not start new work. | FR-074–077, FR-084–085; EPIC 07 acceptance | P1/P2/P8 |
| REQ-4 | Persist the PCC execution states and enforce legal transitions atomically with append-only history. A terminal execution never returns to an active state, including `COMPLETED` → `RUNNING`. | FR-087–090; NFR-002 | P2 |
| REQ-5 | Persist step dependencies, bounded checkpoints, attempts and cumulative usage. Completed steps are not repeated after restart; one failed execution cannot stop unrelated work. | FR-089–091, FR-096; NFR-002–004 | P2/P3/P4/P6 |
| REQ-6 | One persistent job queue uses due times, expiring leases and fresh claim tokens. Claim, retry, checkpoint and completion writes are fenced; two workers cannot authorize the same active step. | FR-098, FR-100, FR-106; NFR-003–004 | P3 |
| REQ-7 | A retryable step failure schedules bounded backoff without resetting completed steps or usage. Exhausted/non-retryable work becomes a visible failed execution with dead-letter reason and attempt history. | FR-091, FR-094–095 | P3/P8 |
| REQ-8 | Cancellation is durable and rejects future work even from an old claimant; it does not pretend to roll back an already-issued external request. Deadline expiry is recorded and follows the published timeout/retry policy. | FR-092, FR-094; NFR-004 | P2/P3/P4 |
| REQ-9 | Startup and periodic reconciliation recover due/expired jobs and discover missing wakeups or inconsistent active records. They neither steal unexpired leases nor rerun terminal work; unresolved side-effect uncertainty is visible instead of guessed. | FR-093, FR-099; NFR-002–004 | P3/P5/P7 |
| REQ-10 | Only a blocking execution-linked approval/user-input wait changes execution state. A persisted decision wakes the correct step idempotently; restart preserves the wait. Alias, preferences and feedback approvals do not block unrelated executions. | FR-082, FR-087, FR-093; EPIC 08 acceptance | P5 |
| REQ-11 | Resume requires an eligible wait with its authorized decision/input already recorded. Manual retry of a failed execution creates a linked **new execution**, pinned to the same version; known side effects retain their operation identity and unsafe/uncertain retries are rejected. | FR-085, FR-091, FR-093; NFR-004 | P2/P5/P8 |
| REQ-12 | Existing behaviors preserve trigger dedupe, model/tool budgets, trusted action targets, per-item actions, run actions, verdicts, reports, undo and dry-run. They use durable checkpoints rather than a second independent runner after cutover. | FR-085, FR-089, FR-096, FR-100; NFR-004 | P4/P5/P7 |
| REQ-13 | The Phase 2 comment pipeline preserves debounce/max-wait, per-thread exclusion, cumulative usage, oldest-untriaged batching, through-message cursor, owner-comment behavior, atomic report/outbox commit and follow-up enqueue. Restart cannot report the same logical batch twice when its receipt is known. | FR-089, FR-093, FR-098–100; NFR-002–004 | P6/P7 |
| REQ-14 | Cutover imports existing queued/running/terminal records and blocking waits idempotently, preserves IDs/receipts/references and leaves only the generic queue authoritative. Historical `agent_jobs`, `behavior_runs`, approvals and tool receipts remain inspectable. | FR-088–090, FR-099–100; NFR-002–004 | P7 |
| REQ-15 | Workspace-scoped CLI lists executions/jobs/dead letters and shows status, timeline, step attempts, version, wait/timeout/uncertainty reason and legacy links. Cancel/resume/retry provide explicit outcomes without leaking stored payloads. | FR-090–095, FR-108 | P8 |
| REQ-17 | Owner free text on Telegram gets an open, conversational answer from Claude: any question (general knowledge, code, ideas, hub state), not a fixed intent menu. No 300-character reply cap: long answers are split into Telegram-sized messages (4096 limit). Recent turns of the owner chat are kept as bounded context (in memory, last ~20 turns / token-bounded, summary when trimmed) so follow-ups work. The model may call read-only hub tools (contacts, runs/trace, prefs/templates, workflow list/show/history from P2/P8) and answers from their results. Side-effecting tools (alias, prefs style, workflow cancel/resume/retry) keep the existing Làm/Huỷ confirmation. The rate guard only stops runaway loops (default 30/min, a setting). Non-owner senders stay ignored; prompts, tool I/O and replies are never logged. | FR-035, FR-037, FR-040, FR-042, FR-043, FR-053, FR-055 (owner chat only; persistent FR-030 stays Phase 6) | P9 |
| REQ-18 | Claude is the hub's orchestrator: a new setting `assistant_model` (default `claude-opus-5-5`, Claude model id validation) selects the model for the Telegram assistant through the configured provider (`claude_cli` on the home hub), with its own timeout setting (default 180s). The local release sets it to `claude-opus-5-5`. | FR-055, FR-056 | P9 + release |
| REQ-16 | Crash/restart, stale claimant, duplicate signal and cutover cases have approved independent tests and review; runbook and phase report distinguish proven fake-provider behavior from open live-service evidence. | NFR-002–004; EPIC 07/08 acceptance; E2E-005 (partial) | All + QA |

FR-097's external worker-slot implementation remains Phase 8. In Phase 5, a waiting execution must nevertheless hold **no runnable job lease**. FR-080 is configuration plus rejection of unsupported constraints, not proof of worker-pool scheduling. These scope limits must appear in traceability and the phase completion report.

## 3. Domain and interface decisions

### 3.1 Storage mapping and authority

PCC §4 permits logical entities to share physical implementation. Use this mapping:

| Logical entity | Physical implementation / invariant |
|---|---|
| Workflow definition / version | Existing `behaviors` / `behavior_versions`; workspace-scoped, immutable published document and version ID. Built-in agent pipeline gets an internal immutable definition/version, not a mutable code-version-only pin. |
| Execution | New `workflow_executions`; workspace, version ID, trigger dedupe identity, correlation ID, state/reason, cancellation/deadline, optional parent retry ID and legacy run/job reference. |
| Step / history | New `execution_steps` and `execution_events`; stable step key, dependencies, status, attempts, usage, bounded checkpoint references; unique event sequence per execution. |
| Execution job | New `jobs`; execution/step reference, due time, attempts, token, lease expiry, error/dead-letter disposition. At most one active job per execution; deterministic ready-step order, not parallel DAG execution in Phase 5. |
| Durable signal | New `workflow_signals`; unique workspace + signal ID, execution/wait reference, decision/input reference, consumed marker. Never store arbitrary free-text input here. |
| Compatibility state | Existing `agent_jobs` / `behavior_runs` remain identifiers, receipts and read projections after cutover, never independent scheduling authority. |

New tables use explicit workspace predicates and workspace-consistent FKs. Checkpoint JSON is an allowlisted typed record, maximum 64 KiB per step; references point to existing domain result/receipt records. Existing redacted behavior context retains its existing separate size limit. Do not copy raw model output into events or the generic queue.

For built-in agent versions, snapshot non-secret effective orchestration/budget/model settings at enqueue; credential references are not credential values. A changed source configuration produces a new internal published version for future batches. Internal definitions cannot be edited or triggered through the public behavior CLI.

### 3.2 Configuration compatibility

- Keep behavior schema 1 and all current action/read validation intact.
- Add schema 2 with an optional typed `workflow` block: registered step graph, retry policy, step/execution timeout, worker constraints, notification rules and approval references. Put domain settings in `internal/workflow/definition.go`; `behavior.Doc` may embed them, but `workflow` must not import `behavior` or `agent`.
- Schema 1 is normalized into a deterministic built-in graph without rewriting its stored published document. Schema 2 graph nodes reference registered internal step kinds, not arbitrary tools, scripts, URLs or model-provided action targets.
- At most 64 steps; unique nonempty step keys; every dependency exists; graph is acyclic. Initial graph execution is serial in declaration order among ready steps. A dependency must succeed before a dependent becomes runnable; failed/cancelled dependencies never implicitly satisfy it.
- Defaults for new runtime policy: 5 attempts total, retry delays 5s/15s/30s/60s, 10-minute step timeout, 15-minute lease. Validate positive timeouts, attempts 1–5, and lease greater than step timeout by at least 30 seconds. Execution deadline is optional, immutable once started and separate from step timeout. Existing smaller action/provider budgets remain effective.
- Schema 1/agent adapters additionally retain their current execution-wide retry and per-attempt timeout budgets; splitting one pipeline into steps must not multiply its retry count, wall-clock allowance or token allowance. Persist the current attempt's deadline across step boundaries. Legacy-specific lease/settings overrides are normalized explicitly rather than silently replaced by new defaults.
- Notification/approval settings reference existing registered behavior handlers and current policy checks. Worker constraints are persisted and validated against the in-process capability registry; no ignored unsupported setting.
- Restore uses `behavior restore <name> --version <n>` to copy the old document/vars to the draft. Normal validate/publish then creates the next immutable version. `GetVersion(..., 0)` and enabled-version selection retain their current latest-published meaning.

### 3.3 Execution state and retry semantics

Persist exactly the PCC execution state vocabulary:

`RECEIVED`, `TRIAGING`, `PLANNING`, `QUEUED`, `RUNNING`, `EVALUATING`, `WAITING_FOR_USER`, `WAITING_FOR_APPROVAL`, `COMPLETED`, `FAILED`, `CANCELLED`.

- Start creates `RECEIVED`; validated acceptance queues work. Registered triage/planning/evaluation step kinds expose their named active stage; other work exposes `RUNNING`. A step completion either queues a ready successor, records a durable wait, or completes the execution.
- Legal edges: `RECEIVED` → `QUEUED`/`FAILED`/`CANCELLED`; `QUEUED` → a named active stage/`FAILED`/`CANCELLED`; an active stage (`TRIAGING`, `PLANNING`, `RUNNING`, `EVALUATING`) → `QUEUED`/either waiting state/any terminal state; either waiting state → `QUEUED`/`FAILED`/`CANCELLED`. All other edges are forbidden. A transition to the same state is an idempotent no-op, not a history event or permission to re-run a step.
- Automatic retry is nonterminal: active stage → `QUEUED` with a due time. A waiting execution → `QUEUED` only after a matching persisted authorized signal. Waiting records keep the blocked step key; no lease is retained while waiting.
- Any nonterminal state may become `CANCELLED`; timeout/exhaustion/non-retryable error may become `FAILED`. Terminal states are `COMPLETED`, `FAILED`, `CANCELLED` and have no outgoing transitions. Repeated identical terminal commands return an idempotent result, not a new transition.
- Manual retry accepts only `FAILED` with a proven safe checkpoint/operation plan. It creates a child execution with the same version and logical operation keys, copies only safe completed-checkpoint references, and schedules only unfinished safe work. `COMPLETED`, `CANCELLED`, unknown-outcome and invalid-checkpoint retries fail explicitly. Do not offer force-resend in Phase 5.
- At most one direct retry child exists per failed parent; concurrent/repeated retry requests return that child. If that child later fails, retry the child, not the original parent. Child trigger identity is distinct; external operation identity is inherited. Cancel keys derive from workspace/execution/action; resume keys also include the persisted wait/signal generation, so later legitimate waits are not suppressed by an earlier resume.
- Step states: `PENDING`, `RUNNING`, `WAITING`, `SUCCEEDED`, `FAILED`, `CANCELLED`. Attempt history lives in events; retry does not erase prior attempts or usage.
- Legacy behavior `done` means dispatch/handoff finished today; it is **not** proof the new execution is completed if a blocking approval is outstanding. Preserve the legacy read projection for existing UX and expose the precise execution state in new CLI/trace output.

### 3.4 Public seam and transactional ownership

Create `internal/workflow` as an SDK-free domain/runtime package. Freeze these public names in P2; record any necessary adjustment in the ledger before dependent briefs are sent:

```go
type Engine interface {
    Start(context.Context, StartRequest) (Execution, error)
    HandleSignal(context.Context, Signal) error
    Cancel(context.Context, Command) (Execution, error)
    Resume(context.Context, Command) (Execution, error)
    Retry(context.Context, Command) (Execution, error)
}

type StepExecutor interface {
    Execute(context.Context, StepContext) (StepResult, error)
}
```

`StartRequest` contains workspace ID, published version ID, deterministic trigger key, correlation ID and trusted domain references. `Command` contains workspace ID, execution ID and idempotency key. `Signal` adds signal ID, blocked step key and an already-authorized decision/input reference. IDs follow the existing storage types: workspace UUID string, execution/version IDs int64, step/idempotency keys strings.

`StepContext` contains the execution, version/config snapshot, step, current claim token and checkpoint references. `StepResult` contains a typed result/checkpoint reference and either success or blocking wait references (a step may await multiple item decisions). Errors distinguish invalid input, not found, conflict, stale claim, unsafe retry and provider uncertainty; persist sanitized codes, not arbitrary error strings.

Storage adapters own transactions. Export `StartExecutionTx(ctx, pgx.Tx, StartRequest) (Execution, error)` on the workflow storage adapter so existing webhook/debounce transactions can insert/resolve an execution without a second commit. Domain interfaces must not import pgx.

Claiming returns a fresh token. Every checkpoint/usage/finish/retry transaction checks token, active job, execution state, deadline and cancellation. State, event, step and next-job changes commit atomically. Action authorization checks the same fence immediately before the existing action executor; no DB fence can retract an HTTP request already in flight.

### 3.5 Side effects and approval bridge

- Retain existing `tool_runs`, action receipts, `behavior_messages`, outbound dedupe and approval identities. A step retry or child execution must not derive a new operation key merely because its execution ID changed; preserve the original logical batch/run/action identity.
- A confirmed success is reused. A confirmed failure may retry under policy. A started action without a reliable receipt is reconciled through the existing provider-specific evidence path when available; otherwise fail with `side_effect_uncertain`. In particular, do not automatically resend an uncertain Telegram send.
- Approval creation and the execution wait must be committed together, or use a persisted bridge marker that reconciliation can finish. Decision and durable signal recording likewise share a transaction. A crash after `Decide` but before external action cannot lose the decision or cause an unrecorded replay.
- After cutover, callbacks for execution-linked approvals record the decision/signal but do not also execute the business action inline: the fenced durable continuation is its only executor. Legacy callbacks without a migrated execution retain their existing route until cutover; never run both routes for one decision.
- Keep current authorization/nonce checks before accepting signals. Only explicit blocking approvals for the linked execution/step count as waits. A decision on an alias, preferences or feedback prompt must never wake a business execution.
- Approval or validated user input queues the appropriate continuation. For legacy item approvals, rejection/expiry records the existing per-item denied/expired outcome and suppresses that item's action; it must not fail unrelated items or change current aggregate behavior. Explicit fail-on-rejection workflows may fail their blocked step with a specific reason. A step with multiple blocking waits completes only when its configured join is satisfied; duplicate decisions cannot authorize another item's action. Resume never creates an approval, manufactures input or bypasses a rejection. Cancellation does not delete approval audit history.

## 4. File ownership map

Paths below are intended production boundaries, not permission to restructure neighboring packages. Each brief narrows edits; Tester owns every new or existing `_test.go` and `testdata` file.

| Task | Production files | Tester-owned files |
|---|---|---|
| P1 | New `internal/workflow/definition.go`; existing `internal/behavior/schema.go`, `resolve.go`; new `internal/behavior/workflow_definition.go`; `cmd/setup-notices/behavior.go` | `internal/workflow/definition_test.go`, `internal/behavior/workflow_definition_test.go`, `cmd/setup-notices/behavior_workflow_test.go` |
| P2 | New `internal/workflow/types.go`, `state.go`, `engine.go`; `internal/adapters/storage/workflow_store.go`; new migrations and `db/query/workflow.sql` | `internal/workflow/state_test.go`, `internal/adapters/storage/workflow_store_test.go` |
| P3 | New `internal/workflow/worker.go`, `reconcile.go`; `internal/adapters/storage/workflow_jobs.go`; `db/query/workflow.sql` | `internal/workflow/worker_test.go`, `internal/adapters/storage/workflow_jobs_test.go` |
| P4 | New `internal/behavior/workflow_adapter.go`; existing `run_worker.go`, `run_actions.go`; `internal/adapters/storage/behavior_store.go`, `db/query/behavior.sql` | `internal/behavior/workflow_adapter_test.go`, `internal/adapters/storage/workflow_behavior_test.go` |
| P5 | New `internal/behavior/workflow_signals.go`; existing `approvals.go`; new `internal/adapters/storage/workflow_signals.go`; approval query file identified from the actual tree | `internal/behavior/workflow_signals_test.go`, `internal/adapters/storage/workflow_signals_test.go` |
| P6 | New `internal/agent/workflow_adapter.go`; existing `orchestrator.go`; `internal/adapters/storage/agent_store.go`, `notice_repository.go`, `db/query/agent.sql` | `internal/agent/workflow_adapter_test.go`, `internal/adapters/storage/workflow_agent_test.go` |
| P7 | New `internal/adapters/storage/workflow_cutover.go`; `cmd/server/wiring.go`, worker startup in `cmd/server/main.go`; legacy claim queries; additive migration for cutover fence if needed | `internal/adapters/storage/workflow_cutover_test.go`, `cmd/server/workflow_recovery_test.go` |
| P8 | New `cmd/setup-notices/workflow.go`; command dispatch in `main.go`; existing `inspect.go`, `trace.go`; `docs/runbooks/self-host-home.md` | `cmd/setup-notices/workflow_test.go`, fake-provider recovery scenarios under the owning packages |

Generated files under `internal/adapters/storage/sqlc/` change only through `make generate`. A named existing file that is absent at execution must be resolved from the actual tree and reported, not guessed into existence.

## 5. Task packets

All P tasks belong to EPIC 07/08/09 with the PCC mapping in §2. Each numbered step is a separate checkable action. Tests named below are acceptance targets for the independent Tester, **not** permission for Implementer to write tests. Each task ends with a local production commit whose body ends in `Tests not run.` followed by the brief's attribution line. Compile success is not acceptance proof.

### P1 — Versioned workflow configuration and restore

**Requirements:** REQ-1–3. **Dependencies:** owner-approved plan and actual base. **Deliverables:** schema compatibility, typed settings/graph validation, restore-to-draft CLI. **Interfaces:** `workflow.Definition`, `workflow.StepDefinition`, `workflow.RetryPolicy`; `workflow.ValidateDefinition(Definition, []string) error` (supported capability names); `behavior.NormalizeWorkflow(ResolvedDoc) (workflow.Definition, error)`; existing definition-store APIs stay compatible.

- [ ] Read existing schema/resolve/definition-store/CLI validation and preserve Schema 1 fixture semantics.
- [ ] Implement Schema 2 settings and bounded graph validation from §3.2, keeping action permissions in existing validators.
- [ ] Implement normalization and `behavior restore` by copying selected immutable version into a draft; publish remains a separate operation.
- [ ] Run permitted compile/generation checks; commit `feat(workflow): add versioned workflow configuration` and report exact interfaces.

**Acceptance tests / DoD:** `TestWorkflowSchema1Compatibility`, `TestWorkflowDefinitionRejectsInvalidGraph` (cycle/missing dependency/65 nodes/unknown kind), `TestWorkflowConstraintsRejectUnsupportedCapability`, `TestWorkflowPublishPinsSettings`, `TestBehaviorRestoreCreatesNewVersion` (draft edit/restore never changes a live version), JSON/YAML round-trip, unpublished/disabled trigger negatives. All expected results trace REQ-1–3; Tester owns their implementation.

### P2 — Execution state, steps and transactional history

**Requirements:** REQ-3–5, REQ-8, REQ-11. **Dependencies:** P1 interface report. **Deliverables:** logical tables, engine commands, transition rules, atomic start/history/step persistence. **Interfaces:** §3.4; `workflow.CanTransition(from, to State) bool`; `storage.StartExecutionTx`.

- [ ] Add domain types, legal-transition checks and typed error/reason codes; define all request/result fields from §3 before storage consumers are written.
- [ ] Add additive migrations and sqlc queries with workspace-consistent FKs, immutable version pin, unique trigger key, retry lineage and ordered events.
- [ ] Implement atomic/idempotent start, guarded cancel, safe child-retry planning and checkpoint/history transactions. A child inherits original side-effect identity; terminal parent is untouched.
- [ ] Run permitted checks (generate twice clean and migration validation included); commit `feat(workflow): persist execution lifecycle and checkpoints`.

**Acceptance tests / DoD:** `TestExecutionTransitionMatrix` tests every state pair, especially terminal exits; `TestExecutionStartDeduplicatesTrigger`; `TestExecutionWorkspaceIsolation`; `TestExecutionCheckpointAndHistoryAtomic`; `TestExecutionCancelWinsStaleFinish`; `TestExecutionRetryCreatesPinnedChild` and unsafe/completed/cancelled negatives. Atomic retry-child creation dedupes repeated command keys. Steps/events cannot reference another workspace.

### P3 — Durable jobs, leases, retry, timeout and reconciliation

**Requirements:** REQ-5–9. **Dependencies:** P2. **Deliverables:** generic job worker/storage, registered executor registry, startup/periodic reconciler and dead-letter visibility queries. **Interfaces:** `workflow.StepExecutor` from P2; `workflow.Worker.Run(context.Context)` and `workflow.Reconciler.Reconcile(context.Context, time.Time) error`; injected clock for deterministic deadlines/backoff.

- [ ] Implement transactional due-job claim with `FOR UPDATE SKIP LOCKED`, a fresh token, one active job per execution and token-fenced writes.
- [ ] Implement serial dependency scheduling, bounded attempt/backoff/deadline policy, cancellation-aware action gate and failure/dead-letter disposition.
- [ ] Implement startup/periodic reconciliation for expired claims, nonterminal records lacking a job/wait, and pending signals; retain unexpired claims and terminal records unchanged.
- [ ] Run permitted checks; commit `feat(workflow): add leased jobs and restart reconciliation`.

**Acceptance tests / DoD:** `TestJobConcurrentClaimsAreExclusive`, `TestJobOldTokenCannotWriteOrAuthorize`, `TestJobRetryPreservesUsageAndCompletedSteps`, `TestJobTimeoutAndExhaustionDeadLetter`, `TestReconcilePreservesLiveLeaseAndTerminalState`, `TestReconcileRepairsMissingWakeup`, `TestFailedExecutionDoesNotStopOtherJobs`. Fake clock asserts exact 5/15/30/60-second defaults and five-attempt cap; waiting owns no job lease.

### P4 — Existing behavior execution as durable steps

**Requirements:** REQ-5, REQ-8, REQ-12. **Dependencies:** P1–P3. **Deliverables:** behavior adapter for context/read, model result, item dispatch and run actions; compatibility projections and operation keys. **Interfaces:** register `behavior.context`, `behavior.model`, `behavior.dispatch`, `behavior.run_actions` executors via the P3 registry; keep public `RunWorker` callers compiling until P7 switches startup.

- [ ] Extract reusable operations behind the adapter without duplicating the current policy/tool executor or enabling a new production claimer yet.
- [ ] Checkpoint persisted context/result/item/verdict/action references at the existing safe boundaries; preserve cumulative budgets and skip known completed work.
- [ ] Bind existing run/action/message keys to logical operation identity and apply the P3 fence before writes; classify missing receipts as uncertainty, not an automatic resend.
- [ ] Run permitted checks; commit `refactor(behavior): execute behaviors through durable steps`.

**Acceptance tests / DoD:** `TestBehaviorWorkflowMatchesLegacyReports`, `TestBehaviorRecoveryDoesNotRepeatModelOrAction`, `TestBehaviorUncertainTelegramIsNotResent`, `TestBehaviorCancelPreventsNextAction`, `TestBehaviorDryRunAndTrustedTargetsPreserved`. Cover item/run-action verdict gates, undo, detailed/short templates and model/tool budgets. Existing tests remain untouched by Implementer.

### P5 — Durable approval and user-input waits

**Requirements:** REQ-9–12. **Dependencies:** P2–P4. **Deliverables:** blocking-wait mapping, transactional decision/signal bridge, recovery-safe approval continuation and expiry/rejection outcomes. **Interfaces:** P2 `HandleSignal` / `Resume`; existing callback/nonce/actor authorization remains upstream; non-execution approvals retain their current behavior.

- [ ] Persist explicit execution/step wait bindings and bridge markers atomically with approval creation, without treating every open approval as blocking.
- [ ] Record a durable authorized signal with approval decision; process it idempotently through the generic queue, retaining existing action receipt checks.
- [ ] Reconcile decisions/expiry across restart; implement resume eligibility and cancellation races without bypassing the original approval or retaining a job lease.
- [ ] Run permitted checks; commit `feat(workflow): persist approval waits and signals`.

**Acceptance tests / DoD:** `TestApprovalWaitSurvivesRestart`, `TestApprovalDecisionCrashHasNoLostWakeup`, `TestApprovalSignalDuplicateIsNoOp`, `TestResumeCannotBypassApproval`, `TestApprovalRejectExpireCancelRaces`, `TestApprovalMultipleItemsPreserveLegacyOutcomes`, `TestAliasPrefsFeedbackDoNotBlockExecution`, `TestForeignWorkspaceSignalRejected`. Crash immediately after decision and immediately before external action; retain the existing nonce/actor abuse cases. Rejecting one item does not abort unrelated items or execute its denied action.

### P6 — Built-in Phase 2 comment pipeline adapter

**Requirements:** REQ-5, REQ-13. **Dependencies:** P1–P3; P4's receipt pattern available. **Deliverables:** immutable built-in definition/version, `agent.triage`, `agent.investigate` and `agent.report` adapters, transactional batch/cursor/report/follow-up integration. **Interfaces:** P2 `StartExecutionTx`, P3 `StepExecutor`; existing `AgentStore` API remains compatible until P7.

- [ ] Register and snapshot the internal definition/settings; keep trigger identity tied to the existing logical agent batch and thread.
- [ ] Split orchestration at persisted triage/investigation/report boundaries, retaining current routing, tool permissions, batching and cumulative usage. Persist validated structured results in agent-domain records and reference them from checkpoints; never put raw model transcripts in generic lifecycle JSON.
- [ ] Integrate atomic cursor/report/outbox/follow-up persistence, preserving debounce/max-wait and owner-comment semantics; do not switch live claimers yet.
- [ ] Run permitted checks; commit `refactor(agent): adapt comment pipeline to durable executions`.

**Acceptance tests / DoD:** `TestAgentWorkflowLegacyEquivalence`, `TestAgentWorkflowDebounceAndThreadExclusion`, `TestAgentWorkflowCursorOutboxAndFollowupAtomic`, `TestAgentWorkflowRestartAfterReportCommit`, `TestAgentWorkflowOldestBatchAndOwnerComments`, `TestAgentWorkflowBudgetAndFallback`, `TestInternalAgentDefinitionCannotBePubliclyTriggered`. Include >100 untriaged comments, new arrivals during processing, disabled/invalid source configuration and duplicate webhook IDs. No duplicate known report, skipped cursor or premature compression; version snapshots never bypass current security policy or revocation checks.

### P7 — Safe cutover, compatibility and recovery

**Requirements:** REQ-9, REQ-12–14. **Dependencies:** P1–P6 and approved Batch A/B evidence below. **Deliverables:** idempotent import/backfill, runtime-mode fence, startup switch and preserved historical inspection. **Interfaces:** `func (s *WorkflowStore) Cutover(ctx context.Context, workspaceID string, apply bool) (CutoverReport, error)` in storage; `apply=false` is a read-only preflight, `apply=true` locks the workspace and atomically validates/imports/fences it. Report counts, blocked IDs and reason codes only. The generic queue becomes the sole agent/behavior scheduling authority after the fence.

- [ ] Add a persisted workspace runtime-mode fence checked by both current-compatible legacy claim paths and new enqueue paths; dormant adapters must not create competing jobs before cutover.
- [ ] Implement idempotent mapping of legacy IDs, pinned versions, pending/running jobs, completed checkpoints, receipts and blocking waits. Imported unknown in-flight actions remain uncertain, not assumed successful or resent; imported unexpired leases remain unavailable until safely reconciled.
- [ ] Switch server startup to the generic worker and reconciler, leaving outbound/digest/poll workers unchanged. Legacy tables become read/receipt projections; there is no dual-authoritative write mode.
- [ ] Run permitted checks; commit `feat(workflow): cut over legacy execution queues safely`.

**Acceptance tests / DoD:** `TestWorkflowCutoverIdempotent`, `TestWorkflowCutoverMixedLegacyStates`, `TestWorkflowCutoverNoDualClaimers`, `TestWorkflowCutoverPreservesReceiptKeys`, `TestWorkflowRestartRunningAndWaitingApproval`, `TestWorkflowCutoverWorkspaceIsolation`. Test a second cutover, historical tool-run FK access, concurrent enqueue at fence, unexpired claim and crash after external success before local receipt. Invalid or unmappable records block cutover with IDs/codes, not silent deletion.

**Operational boundary:** owner-approved maintenance window, stop producers/workers, drain or wait for lease expiry, preflight mappings and backups, then run cutover and restart the new binary. Do not start an old pre-fence binary against the cutover database. Disabling the new worker is not a safe rollback to the old scheduler; reverse migration/restore requires a separately approved recovery procedure. No historical rows or migrations are dropped in this phase.

### P8 — Operator CLI and runbook

**Requirements:** REQ-3, REQ-7, REQ-11, REQ-15–16. **Dependencies:** P2/P3 APIs; final verification after P5/P7. **Deliverables:** commands below, legacy trace/inspect compatibility, runbook examples and recovery limits. **Interfaces:** P2 engine commands; workspace-scoped read queries from P2/P3, no CLI-only state mutation SQL.

- [ ] Add `workflow list`, `workflow show <id>`, `workflow steps <id>`, `workflow history <id>`, `workflow jobs` and `workflow dead-letter`, all requiring `--workspace-id <uuid>`. Lists use `--limit` (default 50, range 1–200) and an opaque `--cursor`; output provides the next cursor without dumping all rows.
- [ ] Add `workflow cancel <id>`, `workflow resume <id>` and `workflow retry <id>` using idempotent engine commands and §3.3 key/lineage rules; print resulting state/new child ID or a sanitized conflict/unsafe-retry reason with nonzero exit status. Add `workflow cutover --workspace-id <uuid>` for read-only preflight and require explicit `--apply` for the P7 maintenance operation; it invokes the storage cutover seam, never ad-hoc mutation SQL.
- [ ] Add execution links to trace/inspect without changing their historical IDs; document restore/publish, waits, deadlines, leases, uncertainty, dead letters, cutover and restart/shutdown in `docs/runbooks/self-host-home.md`.
- [ ] Run permitted compile checks and `make docs-check`; commit `feat(cli): inspect and recover durable workflow executions`.

**Acceptance tests / DoD:** `TestWorkflowCLIWorkspaceAndPagination`, `TestWorkflowCLITimelineAndDeadLetter`, `TestWorkflowCLIIdempotentCommands`, `TestWorkflowCLIResumeRetrySafety`, `TestWorkflowCLICutoverDefaultsReadOnly`, `TestWorkflowCLINoPayloadLeak`, `TestLegacyInspectTraceLinks`. Every state-changing command rejects wrong-workspace access; exit/output distinguishes already cancelled, not resumable and uncertain action. A CLI retry never promises a resend or changes the failed parent; repeated/concurrent retries return one direct child. Cutover without `--apply` changes no rows.

### P9 — Open Telegram assistant with Claude as orchestrator

**Requirements:** REQ-17–18. **Dependencies:** P8 read APIs (workflow tools); can start after P8. **Deliverables:** replace the fixed-intent router in `internal/behavior/assistant.go` with a tool-calling conversation loop; owner-chat bounded history; message splitting; `assistant_model`, `assistant_timeout`, `assistant_rate_per_minute` settings; runbook section. **Interfaces:** reuse the provider's tool-calling path (`internal/adapters/llm` anthropic tools and CLI tool emulation); existing handlers become tools; existing confirmation buttons stay the only path to side effects.

- [ ] Read `internal/behavior/assistant.go`, `inbox_worker.go`, `internal/delivery/http/telegram_handler.go`, `internal/adapters/llm/{anthropic_tools,cli_prompt,claude_cli}.go`, `internal/settings/settings.go`.
- [ ] Replace the intent JSON router with a system prompt that lets Claude answer anything in Vietnamese by default, plus read-only tools mapped to existing handlers and P8 workflow reads; side-effect tools return a pending confirmation instead of acting.
- [ ] Add bounded in-memory owner-chat history (turn and size caps, oldest summarized or dropped), cleared on restart; never persisted or logged.
- [ ] Remove the 300/1000-rune reply caps; split replies at paragraph/line boundaries under 4096 characters, keep the existing sanitising of mentions and control characters.
- [ ] Add the three settings with validation and defaults; the assistant uses `assistant_model`, not `triage_model`.
- [ ] Document in `docs/runbooks/self-host-home.md`; run permitted checks; commit `feat(assistant): open Claude conversation with hub tools on Telegram`.

**Acceptance tests / DoD:** free question with no tool gets a full answer; long answer split into ordered messages; follow-up uses earlier turn; read tool result appears in answer; side-effect tool requires Làm and Huỷ does nothing; non-owner ignored; rate guard at setting; history cap and restart clear; `assistant_model` used and validated; no prompt/tool I/O/reply in logs (fake provider).

### Release — local hub (owner pre-approved 2026-11-14)

After the completion gate: back up the DB, stop the service, run migrations and the P7 cutover (`workflow cutover --apply`), install the new binaries, set `assistant_model claude-opus-5-5`, start the service, check logs and `workflow list`, then send the owner a Telegram message listing what Phase 5 lets them do. Merge into `develop` still needs the owner's OK.

## 6. Agent dispatch and evidence gates

### 6.1 Order and batches

```text
Owner approves plan + actual branch/base
  P1 → P2 → P3 → Batch A Tester/CI → Batch A Reviewer → fixes
  P4 → P5 → P6 → Batch B Tester/CI → Batch B Reviewer → fixes
  P7 → P8      → Batch C Tester/CI → Batch C Reviewer → fixes
  P9           → Batch D Tester/CI → Batch D Reviewer → fixes
  same-head PASS + APPROVE → completion report → local release (owner pre-approved 2026-11-14) → Telegram report
```

- Each P task gets its own Implementer session/commit and a concise brief. Do not send this entire plan as a pasted prompt; point to its sections/files.
- Tester first returns a Test Plan; owner approves it before tests are written. Owner then authorizes/pushes to trigger CI; Lead supplies the run ID to that Tester session. Reviewer receives the actual brief head, including test commits, after the test stage.
- Batch A covers REQ-1–9/11 foundations; Batch B covers REQ-5/8–13 adapters/waits; Batch C proves REQ-1–16 end-to-end and cutover; Batch D covers REQ-17–18 (P9). Batch A/B PASS is only for their approved cases, not Phase 5 completion.
- Every defect/finding gets a verified ruling; fixes return through Implementer/Test/Review. Do not edit tests to conceal a regression. Owner decides test disputes and any accepted test debt.
- Batch scope does not authorize old debt to expand indefinitely: read the targeted debt and state which evidence is paid or still open. Tests may need expectations updated for newly added tables; only Tester does that, never by removing invariants.

### 6.2 Brief packet

Local packet: `.superpowers/sdd/2026-11-13-phase5-durable-workflow-engine/`.

- `README.md`: dispatch instructions, baseline pin, role/commit/report rules.
- `task-P1.md` … `task-P8.md`: Implementer briefs linked to §5, requirements, exact commit subjects and read/edit boundaries.
- `phase-brief-template.md`: fill actual base/head, selected batch/cases, open debt and test environment before sending to Tester and Reviewer sequentially.
- `progress.md`: approval status, task/evidence slots and decisions; no fabricated completion or CI result.

Briefs are prepared drafts, **not runnable handoffs until the Lead fills the actual head and the owner approves the plan**. Use `scripts/agent-handoff.sh` according to `AGENTS.md`; never launch concurrent writers on the same branch. Short fix follow-ups may reuse a warm session; otherwise use a new session with a carry-over note under 40 lines.

### 6.3 Carry-in debt

The first Phase Brief must explicitly carry:

- TD6: independent review of the Phase 2 agent/tool changes (`docs/product/test-debt/phase2-agent-core-tools.md`).
- TD3-2 and TD3-3: behavior independent review and real ClickUp payload evidence (`docs/product/test-debt/phase3-behavior-engine.md`).
- TD3d-2 and TD3d-3: chat/PR independent review and live chat/reaction evidence (`docs/product/test-debt/phase3d-chat-pr-auto-approve.md`).
- Phase 4 ledger minors T1/T2/R8 and BotFingerprint retry observation: compatibility risks, not silently approved production-fix scope.

Read the debt files for exact ranges and already-closed items. Fake fixtures cannot close the remaining live-service items. Keep any unproven item open in traceability/reporting; request owner authorization for new real-service checks.

## 7. Phase completion gate

- [ ] All owner-approved REQ-1–18 cases have CI evidence on the final code head plus identified test commits; no approved case skipped, no race or panic.
- [ ] Reviewer returns APPROVE on that same final head; Critical/Important findings and test defects are resolved or owner explicitly accepts recorded debt.
- [ ] Fake-provider recovery demonstrates one running execution plus one blocking approval across restart, no lost records, no terminal rerun and no repeat of known side effects; uncertainty is surfaced.
- [ ] Generic queue is the sole agent/behavior claimer; legacy receipts, trace and report/approval UX remain compatible.
- [ ] Additive migrations validate and generation is reproducible; runbook covers maintenance/cutover and prohibits unsafe rollback/resend.
- [ ] Lead updates `docs/product/TRACEABILITY.md` from evidence (including FR-080/097/E2E-005 boundaries), updates roadmap decisions/status without rewriting the PCC, and writes `docs/product/reports/2026-11-13-phase5-report.md` (use actual completion date if different).
- [ ] Owner separately approves any merge, release or deployment. No claim that a draft plan, compile-only check or fake-provider result completes the product contract.

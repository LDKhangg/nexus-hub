# Phase 7b — AI Orchestrator Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `executing-plans` for an assigned task. The role boundaries and sequential handoff process in `AGENTS.md` override generic implementation/TDD instructions. Steps use checkbox (`- [ ]`) syntax for tracking.

**Status:** DRAFT 2026-11-16 — waiting for the owner's decisions O1–O5 (§1.2). Renamed from Phase 7 to Phase 7b on 2026-11-16 (owner decision W1): Phase 7a (local worker bridge, `docs/plans/2026-11-16-phase7a-local-worker.md`) goes first. Before approval, re-check O1, O3 and §1.3 against the merged 7a: `/task` already starts the 7a `local-task` workflow, and `DELEGATE` now targets the 7a local worker instead of waiting for Phase 8.

**Goal:** A request is no longer one-shot triage. The hub turns it into a structured, validated plan (intent, type, priority, complexity, capabilities, tasks with dependencies, budget), runs the tasks through the Phase 5 durable engine, evaluates each result, and decides one of seven next actions. It completes only with evidence, asks when it is unsure, waits for the owner's OK on risky actions, and stops (never loops) when a budget is spent.

**Architecture:** A new provider-neutral `internal/orchestrator` package (`Plan`, `Evaluate`) on top of the existing `llm.StructuredProvider` + JSON-schema validation, with a deterministic fake provider for tests. Two new workflow step kinds, `orchestrator.plan` and `orchestrator.evaluate`, plus a built-in workflow definition `orchestrated-task` that loops plan → run tasks → evaluate under budget. Go — not the model — enforces schema, capability matching, evidence for `COMPLETE`, budgets and the existing action authorization (policy). Existing behaviors and the comment pipeline keep working unchanged; only their triage classification moves onto the orchestrator's classifier (O2).

**Tech Stack:** Go (go.mod version), PostgreSQL, pgx v5, sqlc, goose. No new service or dependency.

**Spec:** `docs/product/PCC-001.md` EPIC 05 (FR-043–FR-056, the seven decisions, the acceptance criteria), ARCH-002, ARCH-004; scope in `docs/product/ROADMAP.md` §Phase 7b and carry-over §6 (batch budget hiding older comments). The contract is read-only.

## Global Constraints

- Code, documentation and commits are English; owner-facing discussion is Vietnamese.
- Baseline: `develop` after the Phase 7a merge (local worker bridge). Orchestrated-task messages use `report.topic: task` (or `workflow` without a ClickUp task). Branch `feature/phase7b`. Next migration number is `00027` (Phase 7a uses `00026`).
- Carry-ins from Phase 6b and hotfix 6b.1 (ROADMAP §Phase 7b): router tuning (route-eval 13/15 and 12/15); 6b R7 (`Recover` reopening a closed topic skips the lifecycle hook); lowercase custom task IDs (`fc-6583`); command behaviors addressed with a custom ID (`FC-…`); append the route-eval result to `docs/product/reports/2026-11-16-phase6b-report.md`.
- One handoff at a time per shared working tree. Implementer changes production only; Tester changes tests only; Reviewer is read-only.
- Lead/Implementer never run `go test`. Checks: `go build ./...`, `go vet ./...`, `gofmt -l cmd internal`, `make generate` (second run clean), `goose -dir db/migrations validate`. The suite runs only in CI.
- Never edit old migrations or hand-edit sqlc output. New tables are workspace-scoped with composite foreign keys and RLS like their precedents.
- **ARCH-004:** domain code (`internal/orchestrator`, `internal/workflow`, `internal/behavior`, `internal/agent`) never imports a provider package or names a model vendor; providers are chosen in `cmd/server/wiring.go` from settings.
- **ARCH-002 / policy:** the orchestrator only *proposes*. Every external side effect still goes through the existing action authorizer, tool policy and Làm/Huỷ approval path. A plan or evaluation can never widen permissions, trusted targets, budgets, dry-run or output schemas (same rule as Phase 6 owner instructions).
- **Privacy (Phase 6 Q2b):** no raw prompts, chat text or untrusted source text in the DB or logs. Plans store bounded, sanitized structured fields (enums, ids, short titles ≤ 200 runes, counters, usage). CLI output stays metadata-only.
- Tests use fakes only (fake provider, fake ClickUp/GitHub/Telegram); DB tests use loopback `TEST_DATABASE_URL`, database `nexus_storage_test`. The VPS has no Anthropic key yet; nothing in this phase needs one.

## Review Focus

- The orchestrator cannot bypass policy: no code path from a plan/evaluation to a side effect except through the existing authorizer and approval step.
- Invalid structured output is rejected or repaired within the bound; never partially applied.
- Capability matching and the `COMPLETE`-needs-evidence rule are enforced in Go, not trusted from the model.
- Budgets: every loop (repair, plan iterations, replans, tokens, wall time) is bounded and ends in `FAILED` or a blocked wait — prove no unbounded loop.
- Durable engine invariants (fencing, idempotent checkpoints, terminal immutability, restart reconciliation) hold for the new step kinds.
- Provider neutrality (ARCH-004) and privacy (no raw text stored or logged).

## 1. Scope and decisions

### 1.1 What the owner gets

- In the owner chat: "WF làm giúp: kiểm tra US task 86xyz, liệt kê OQ" (or `/task <text>`) starts an orchestrated workflow `WF-n`. The hub shows the plan (tasks, dependencies, budget), runs it, and reports the result with evidence — primary use case 1 (ROADMAP §7).
- If the request is unclear the hub asks a clarification question (Phase 6 reply/`WF-n` path) instead of guessing; risky steps wait for Làm/Huỷ.
- `/status WF-n` shows the plan version, task states, decisions and budget used.
- Existing behaviors (spec-review, pr-auto-approve, mention-report) and the ClickUp comment pipeline behave as today.

### 1.2 Owner decisions needed

| ID | Question | Options (Lead recommendation first) |
|---|---|---|
| O1 | Where does the orchestrator run in Phase 7? | **a)** new built-in `orchestrated-task` workflow started from Telegram (`/task`, assistant tool); behaviors untouched · b) also replace `behavior.model` in every behavior · c) only the ClickUp comment pipeline |
| O2 | Does the comment pipeline's Tier-1 triage switch to the orchestrator classifier (FR-043–046)? | **a)** yes, same output contract, same routing, a fake-provider equivalence test · b) no, keep Phase 2 triage until Phase 8 |
| O3 | `DELEGATE` before real workers (Phase 8) — revisit: Phase 7a now provides a local worker | **a)** a static capability registry of in-process executors (read-only investigate loop = `clickup.read`, `github.read`, `repo.read`; behavior actions = `clickup.write`, `telegram.notify` under policy); Phase 8 swaps in worker slots · b) `DELEGATE` always blocks with "needs worker" until Phase 8 |
| O4 | Budget exhausted | **a)** block: `WAITING_FOR_USER` with buttons "Thêm 1 vòng" / "Huỷ" (expires → `FAILED`) · b) always `FAILED` with reason `planning_budget_exceeded` |
| O5 | Models and default budgets (setting keys, changeable later) | **a)** plan + evaluate = `actor_model` (Sonnet class), classifier = `triage_model`; max 3 plan iterations, 2 replans, 2 repair attempts, 80k tokens, 20 min per execution · b) plan on an Opus-class model, others as a) |

### 1.3 Boundaries

- No worker slots, scheduler, isolated workspaces or parallel execution (Phase 8). Tasks run sequentially through in-process executors.
- No new policy engine or rule sets (Phase 9); the orchestrator uses the current authorizer and approval step.
- No new outbound adapters (Phase 10); no ClickUp writes beyond what behaviors already allow.
- Real-provider evidence is Phase 13 (or a smoke check the owner allows once the VPS key is set).

## 2. Requirements and acceptance

| ID | Observable behavior | PCC IDs | Tasks |
|---|---|---|---|
| REQ-1 | An `AIRuntime` interface (structured call with schema, usage, cost) is the only way orchestrator/agent/behavior code reaches a model. Anthropic, Claude CLI and opencode CLI adapters implement it; a deterministic fake provider (scripted responses keyed by step) exists for tests. Domain packages import no provider package. | ARCH-004, FR-055 | P1 |
| REQ-2 | Every orchestrator output is validated against a versioned JSON schema. Invalid output gets at most N repair attempts (N from settings, default 2) with only the validation error fed back; still invalid → the step fails with reason `invalid_output`, nothing from the bad output is applied or stored. | FR-055 | P1, P2 |
| REQ-3 | `Classify` returns intent, task type, priority proposal and complexity estimate from a fixed vocabulary, with a confidence value; low confidence (below a setting) yields `REQUEST_CLARIFICATION`. | FR-043, FR-044, FR-045, FR-046, FR-053 | P2 |
| REQ-4 | `Plan` returns a plan: tasks (key, title ≤ 200 runes, required capabilities, depends_on, priority, complexity, acceptance hint), an initial decision and a budget request. Go checks: unique keys, known capabilities, acyclic dependencies, task count ≤ limit, budget ≤ configured caps (clamped, never widened). A plan that fails checks counts as invalid output (REQ-2). | FR-047, FR-048, FR-050, FR-056 | P2 |
| REQ-5 | Worker selection: for each task the orchestrator recommends an executor; Go assigns only an executor whose capability profile covers all required capabilities. No match → the task is blocked and the decision becomes `REQUEST_CLARIFICATION` or `FAIL` (never assigned to an executor lacking the capability). | FR-049, EPIC 05 AC 4 | P3 |
| REQ-6 | `Evaluate` reads a task result (structured, with evidence references: step output ids, resource bindings, artifact refs) and returns one of `EXECUTE`, `DELEGATE`, `REQUEST_CLARIFICATION`, `REQUEST_APPROVAL`, `REPLAN`, `COMPLETE`, `FAIL` plus a next-action recommendation. | FR-051, FR-052 | P2, P4 |
| REQ-7 | `COMPLETE` is accepted only when every required task has a successful result with at least one valid evidence reference that belongs to this execution; otherwise Go converts it to `REPLAN` (if budget remains) or `FAIL` with reason `missing_evidence`. | EPIC 05 AC 5 | P4 |
| REQ-8 | `REPLAN` creates plan version v+1 (previous versions kept, immutable), keeps finished task results, and counts against the replan budget. | FR-054 | P3, P4 |
| REQ-9 | `REQUEST_APPROVAL` and any task whose action needs approval go through the existing approval step (Làm/Huỷ, chat-bound, idempotent); `REQUEST_CLARIFICATION` sends a question bound to `WF-n` and waits (`WAITING_FOR_USER`); the owner's reply (Phase 6 resolution) resumes with the answer as a versioned instruction. No orchestrator path performs a side effect directly. | EPIC 05 AC 1, FR-053, ARCH-002 | P4 |
| REQ-10 | Budgets per execution (plan iterations, replans, repair attempts, tokens, wall time) are checkpointed; any limit reached ends in the O4 outcome, never another model call. Restart resumes with the same counters. | FR-056, EPIC 05 AC 6 | P3, P4 |
| REQ-11 | Built-in workflow `orchestrated-task` starts from `/task <text>` or the assistant tool `start_task`, shows the plan in Telegram with `WF-n`, runs tasks in dependency order through the durable engine, and reports the result with evidence links. `/status WF-n` adds plan version, task states, last decision and budget used (metadata only). | FR-048, FR-050, FR-051 | P5 |
| REQ-12 | (O2a) The comment pipeline's Tier-1 triage uses `Classify`; routing, report and dedupe behave as before (equivalence test with the fake provider). Carry-over: when the batch budget drops older comments the plan/report says so. | FR-043, FR-045 | P5 |
| REQ-13 | Persistence: plans, plan tasks and decisions are stored as sanitized structured rows (migration `00027`), workspace-scoped, RLS; CLI `nh workflow show WF-n` lists plan versions, tasks, decisions, usage (no text beyond titles); runbook section "Orchestrated tasks (Phase 7b)". | FR-055, NFR-034 | P1, P6 |

## 3. Domain and interface decisions

### 3.1 Tables (migration `00027_orchestrator.sql`)

- `orchestrator_plans` (workspace_id, execution_id, version, schema_version, classification enums, confidence, budget json, status, usage tokens/cost, created_at); unique (execution_id, version); immutable after insert except `status`.
- `orchestrator_plan_tasks` (plan id, key, title ≤ 200, capabilities text[], depends_on text[], priority, complexity, executor, state, result_step_id, evidence refs jsonb bounded).
- `orchestrator_decisions` (execution_id, plan version, task key nullable, decision enum, reason code, converted_from enum nullable, created_at) — audit of every decision, including Go conversions (REQ-7).

### 3.2 Interfaces

- `orchestrator.AIRuntime` — `Generate(ctx, StructuredRequest) (StructuredResponse, error)`; wraps today's `llm.StructuredProvider`.
- `orchestrator.Orchestrator` — `Classify`, `Plan`, `Evaluate`; pure: input structs in, validated output structs out; no DB, no side effects.
- `orchestrator.CapabilityRegistry` — static executor profiles (O3); `Match(caps) (executor, ok)`.
- `orchestrator.Budget` — counters + caps; `Charge(kind, n) error` returns `ErrBudgetExceeded`.
- Step kinds `orchestrator.plan`, `orchestrator.evaluate`, `orchestrator.run_task` registered in `internal/workflow/definition.go`.

### 3.3 Loop (inside the durable engine)

`plan` → for each ready task in dependency order: `run_task` (executor via existing adapters, policy, approval) → `evaluate` → decision. `EXECUTE`/`DELEGATE` → next task; `REPLAN` → `plan` (v+1); `REQUEST_*` → wait; `COMPLETE` → evidence check → report; `FAIL` → report. Each transition is a checkpoint; budgets charged before every model call.

## 4. File ownership map

| Area | Files |
|---|---|
| Runtime + fake | `internal/orchestrator/runtime.go`, `fake.go` (new); `internal/adapters/llm` adapters implement it |
| Orchestrator | `internal/orchestrator/{classify,plan,evaluate,schema,budget,capability}.go` (new) |
| Migration + storage | `db/migrations/00027_orchestrator.sql`, `db/query/orchestrator.sql`, sqlc output, `internal/adapters/storage/orchestrator_store.go` (new) |
| Workflow wiring | `internal/workflow/definition.go`, `internal/workflow/vocabulary.go` (reasons), new executor in `internal/orchestrator/steps.go`, `cmd/server/wiring.go` |
| Telegram + assistant | `internal/behavior/commands.go` (`/task`, `/status` extras), `assistant_tools.go` (`start_task`) |
| Comment pipeline | `internal/agent/tier1_router.go`, `orchestrator.go` (O2a) |
| CLI + docs | `cmd/setup-notices/workflow.go`, `docs/runbooks/self-host-home.md` |

## 5. Task packets

### P1 — AI runtime, fake provider, schemas and storage
**REQ:** 1, 2, 13 (storage). `AIRuntime` + adapters, scripted fake provider, schema registry with repair loop, migration 00027 + store. Commit `feat(orchestrator): provider-neutral AI runtime, schemas and plan storage`.

### P2 — Classify, Plan, Evaluate
**REQ:** 2, 3, 4, 6. Pure orchestrator functions, fixed prompts, Go plan checks (keys, capabilities, DAG, limits, clamp). Commit `feat(orchestrator): classify, plan and evaluate with validated outputs`.

### P3 — Capabilities, budgets, plan versions
**REQ:** 5, 8, 10. Capability registry (O3), deterministic matching, budget counters checkpointed, plan versioning. Commit `feat(orchestrator): capability matching, budgets and plan revisions`.

### P4 — Durable step kinds and decision handling
**REQ:** 6, 7, 9, 10. `orchestrator.plan/run_task/evaluate` step executors, evidence check, decision conversions audited, approval/clarification waits through existing paths, O4 outcome. Commit `feat(workflow): run orchestrated plans as durable steps`.

### P5 — Telegram entry, status, comment-pipeline triage
**REQ:** 11, 12. `orchestrated-task` built-in definition, `/task`, `start_task`, plan message, `/status` extras, Tier-1 → `Classify` (O2a), batch-budget note. Commit `feat(behavior): start orchestrated tasks from Telegram`.

### P6 — CLI metadata and runbook
**REQ:** 13. `nh workflow show` plan/task/decision metadata; runbook section; architecture note (NFR-034). Commit `docs: orchestrated tasks`.

## 6. Agent dispatch and evidence gates

```text
P1 → P2 → P3 → Batch A Tester/CI (runtime, schemas, plan checks, budgets) → Batch A Reviewer → fixes
P4 → P5 → P6 → Batch B Tester/CI (EPIC 05 AC journeys with the fake provider) → Batch B Reviewer → fixes
same-head PASS + APPROVE → completion report → release (VPS deploy after green CI on develop) → Telegram report
```

- Each task gets its own Implementer session and commit; briefs under 80 lines point to this plan's sections.
- Agents: opencode GPT agents by default; if the quota runs out, follow the owner's standing rule (wait job, or Claude subagents with the same role limits).
- Carry-in debt: open items in `docs/product/test-debt/` stay open unless a test in this phase proves them.

## 7. Phase completion gate

- [ ] All REQ-1–13 cases have CI evidence on the final head; no skip, race or panic; each EPIC 05 acceptance criterion has a named journey test with the fake provider (policy bypass impossible, invalid output repaired/rejected, decomposition with dependencies, capability mismatch refused, no `COMPLETE` without evidence, over-budget ends blocked/failed).
- [ ] Reviewer APPROVE on the same head; Critical/Important findings resolved.
- [ ] Migration validates, generation reproducible, no provider import in domain packages, no raw text stored or logged.
- [ ] Lead updates `docs/product/TRACEABILITY.md`, writes `docs/product/reports/<date>-phase7-report.md`, reports on Telegram. Merge into `develop` (which deploys to the VPS) needs the owner's OK.

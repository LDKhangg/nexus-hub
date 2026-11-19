# Phase 9 — Policy engine and approvals

> **For agentic workers:** REQUIRED SUB-SKILL: Use `executing-plans` for an assigned task. The role boundaries and sequential handoff process in `AGENTS.md` override generic implementation/TDD instructions. Steps use checkbox (`- [ ]`) syntax for tracking.

**Status:** APPROVED 2026-11-18 by the owner's standing instruction ("phase tiếp theo luôn … bạn lên plan và làm đi"), given before the owner went offline. The owner did not see this text. Every Lead call in §1.3 can be changed by the owner later; changes after merge go into a follow-up.

**Goal:** The owner's rules, not code switches, decide what the hub may do. Every privileged action is checked by one policy engine before it runs. Anything not allowed is denied. Every decision is recorded. Risky actions, starting with merging a PR, wait for the owner's OK from the phone. The OK is bound to that exact action, so it can never authorize another one.

**Architecture:** A new package `internal/policy` holds a versioned rule set and a pure `Decide` function. The existing tool executor (Phase 2) calls it before every non-read tool, in place of the global `allow_dangerous` switch. The orchestrator's `behavior-actions` executor (Phase 10) calls it before every action, and gains one new capability, `github.merge`. When a rule says `approval_required`, the hub creates an approval request on the existing `approvals` table, bound to user, execution, action, resource and an input hash. It shows Duyệt / Từ chối buttons in the execution's topic and waits through the existing workflow wait path. After approval it checks policy and the resource again, then runs the action once.

**Tech Stack:** Go (go.mod version), PostgreSQL, pgx v5, sqlc, goose. No new dependency.

**Spec:** `docs/product/PCC-001.md` EPIC 10 (FR-111–FR-116, FR-118; FR-117 UI is Phase 11), EPIC 11 (FR-121–FR-130), FR-009, NFR-023, E2E-004, E2E-009. `docs/product/ROADMAP.md` §Phase 9. The contract is read-only.

## Global Constraints

- Code, documentation and commits are in English. Owner-facing discussion is in Vietnamese.
- Baseline: `develop` `3cb8928` (Phase 10 merged). Branch: `feature/phase9`. Next migration number: `00035`.
- One handoff at a time per working tree. The Implementer changes production code only. The Tester changes tests only. The Reviewer is read-only.
- Nobody runs `go test` locally. Allowed checks: `go build ./...`, `go vet ./...`, `gofmt -l cmd internal`, `make generate` (second run clean), `goose -dir db/migrations validate`. The suite runs only in CI.
- Never edit old migrations and never hand-edit sqlc output. New tables are workspace-scoped, with composite foreign keys and RLS.
- **Fail safe.** A missing, unreadable or invalid rule set never allows more. The engine falls back to the built-in safe set: read allow, write `approval_required`, dangerous deny. A storage error while deciding is a deny.
- **No behaviour change on upgrade.** The migration seeds each workspace with rule set v1, which reproduces today's behaviour: read allow, write allow, dangerous `approval_required`. The existing tool grants (`RepoGrant`, `Enabled`, budgets, `head_sha` pin) stay as an inner check. Policy can only narrow them, never widen them.
- **Only the owner, through the CLI, changes policy.** No assistant tool, behavior action, workflow step, orchestrator capability, webhook or chat message can create, change or activate a rule set (FR-118, NFR-023).
- Every ClickUp write still asks first (Phase 10 rule). Existing behaviors keep their autonomy classes (auto/notify/ask).
- Generic hub: no repo, person or behavior name is hard-coded. Rules name tools and capabilities only.
- Privacy: policy decisions store ids, action names, the resource key, effect, rule reference and an input hash. Never inputs, text, prompts or tokens.
- Tests use fakes only (fake GitHub, ClickUp, Telegram, provider). DB tests use loopback `TEST_DATABASE_URL`, database `nexus_storage_test`.

## Review Focus

- Deny by default: an unknown action, an unknown effect, a missing rule set or a store error never yields allow.
- An approval authorizes exactly one action: same action, same resource, same input hash, same execution, not expired, not already used. A stale or reused approval never authorizes a different or a second action.
- Policy is checked again right before the side effect after approval (FR-130). A rule changed to `deny` in the meantime, or a PR whose head moved, stops the action.
- Nothing merges before approval. A rejection, expiry or restart never merges.
- No path lets the model or untrusted text change policy or decide an approval.

## 1. Scope and decisions

### 1.1 What the owner gets

- `nh policy show | validate <file> | apply <file> | history | rollback <version>`: the rule set as a YAML file with a version number. `apply` validates, stores a new version and activates it. Old versions stay for rollback.
- The hub checks every write or dangerous action against the active rule set and records each decision (`nh policy decisions [--limit N]`).
- `/plan` can now finish with "merge the PR". With the default rule `github_pr_merge: approval_required`, the topic shows "🔐 Cần duyệt: merge PR owner/repo#12 (head abc1234)" with Duyệt / Từ chối. Duyệt merges once after a fresh check. Từ chối or expiry (24 h) never merges, and the plan task is blocked so evaluate decides.
- The `allow_dangerous` setting is retired. Its job moves into the rule set.

### 1.2 Owner decisions

- Owner, 2026-11-18: run Phase 9 now, plan and implement without waiting.

### 1.3 Lead calls (the owner may change any of them)

- P1: Rule set file schema `policy/v1`, YAML:
  ```yaml
  schema: policy/v1
  defaults: {read: allow, write: allow, dangerous: approval_required}
  actions:
    github_pr_merge: approval_required
    github_pr_approve: allow
  approval_ttl_hours: 24
  ```
  Keys under `actions` must be known tool names or orchestrator capabilities. Effects are `allow`, `approval_required` and `deny`. Anything else is invalid, and `apply` refuses it. Other EPIC 10 rule kinds (repos, integrations, hours, concurrency, notifications) stay in their current config and move into later schema versions (Phase 10b/11).
- P2: Approvals reuse the existing `approvals` table with a new kind `policy_gate` and new columns, not a new table. That keeps one decision path (buttons, sweep, waits, actor).
- P3: Approval requests are created only where the hub can wait: orchestrated executions (`behavior-actions`). A direct agent tool call (Phase 2 agent, behavior run actions) that hits `approval_required` is denied with status `approval_required` and a clear tool result. It never waits and never runs.
- P4: The approver must be the workspace owner. The owner is the Telegram owner identity (existing `isOwner`) mapped to the workspace member with role `owner`. The decision stores both the Telegram user id and the internal user id.
- P5: `allow_dangerous` stays a known settings key for one phase, but it has no effect. At startup the hub logs once if it is set to true. `nh settings` reports it as deprecated.

### 1.4 Boundaries

- Out of scope: rule management UI (Phase 11, FR-117), multi-user approvers, per-repo or per-time rules, prompt-injection work beyond NFR-023 (FR-119/120 stay as they are), unified outbox (10b).

## 2. Requirements and acceptance

| ID | Observable behavior | PCC IDs | Task |
|---|---|---|---|
| REQ-1 | Migration `00035`: `policy_rule_sets` (workspace, version, schema, body jsonb, body_sha256, created_by, created_at, activated_at; one active per workspace) and `policy_decisions` (workspace, action, tier, resource_key, effect, reason, rule_set_version, rule_key, actor_kind, execution_id, tool_run_id, approval_id, input_sha256, created_at). RLS, composite FKs. Each existing workspace gets v1 equal to today's behaviour (§Global Constraints). Down drops both. | FR-111, FR-116 | P1 |
| REQ-2 | `policy.Parse`/`Validate`: schema `policy/v1` only; known keys only; effects in the closed set; `approval_ttl_hours` 1–168; unknown schema, duplicate key, unknown action or effect → error naming the path (no values echoed). | FR-111, FR-112 | P1 |
| REQ-3 | `policy.Engine.Decide(ctx, Request{workspace, actor kind, action, tier, resource key, execution id, input hash})` → `Decision{effect, reason, rule ref}`. Order: exact action rule, then tier default. No active set, invalid stored set or store error → built-in safe set (read allow, write approval_required, dangerous deny), reason says which. Every call writes one `policy_decisions` row; if that write fails the decision becomes deny. | FR-113, FR-114, FR-116 | P1 |
| REQ-4 | CLI `nh policy show/validate/apply/history/rollback/decisions`. `apply` stores a new version and activates it in one transaction; `rollback N` re-activates an old version as a new version. Output never prints secrets. No other writer of `policy_rule_sets` exists (grep-provable; no assistant tool, behavior action or capability). | FR-111, FR-112, FR-118, NFR-023 | P1 |
| REQ-5 | The tool executor asks the engine before every non-read tool, after its existing grant checks. `deny` → `tool_runs` status `denied`, reason `policy_denied`. `approval_required` without a matching grant → `denied`, reason `approval_required`, tool result tells the model the owner must approve and nothing was done. `allow` → runs as today. `allow_dangerous` no longer gates anything (P5). Budgets and the `head_sha` pin stay. | FR-009, FR-113, FR-114, FR-115 | P2 |
| REQ-6 | Migration `00036`: `approvals` gains `policy_action`, `resource_key`, `input_sha256`, `execution_id`, `requested_user_id`, `decided_user_id`, `consumed_at`; kind `policy_gate` added to the CHECK. A unique partial index allows one open `policy_gate` per (workspace, policy_action, resource_key, input_sha256). | FR-121, FR-122, FR-127, FR-129 | P2 |
| REQ-7 | An approval grant: the executor accepts a call under `approval_required` only with an approval that is `approved`, kind `policy_gate`, same workspace, action, resource key, input hash and execution, not expired, and not consumed. It marks it consumed in the same transaction as `store.Begin` of the tool run, so a retry with the same idempotency key replays and a different call is refused. | FR-122, FR-127, FR-130 | P2 |
| REQ-8 | Before running an approved action the engine decides again. If the rule is now `deny`, or the grant check fails, nothing runs, and the decision is recorded with reason `revalidate_denied`. | FR-130, FR-116 | P2 |
| REQ-9 | Orchestrator capability `github.merge` (executor `behavior-actions`, input: repo, PR number, expected head SHA from earlier task evidence). Plan may offer it only when the merge tool is wired. The action calls the engine. `allow` merges through the executor. `deny` blocks the task with `policy_denied` and no side effect (E2E-009). `approval_required` creates a `policy_gate` approval and the execution waits in `WAITING_FOR_APPROVAL`. | FR-115, FR-121, FR-128, E2E-004, E2E-009 | P3 |
| REQ-10 | The approval message goes to the execution's topic (General fallback as Phase 10). It shows the action, the resource (repo#PR, short head), the rule that asked, and the expiry, with buttons Duyệt / Từ chối. Only the owner's press counts. Others are ignored with a toast. A second press, a press on an old message or a press after expiry does nothing. The decision records the actor (Telegram and internal user id) and time. | FR-123, FR-124, FR-125, FR-126, FR-129 | P3 |
| REQ-11 | Approve → the execution resumes, re-checks policy (REQ-8) and that the PR head still equals the bound SHA and the checks are green, then merges once (idempotency key from the approval id). Reject or expiry → task blocked (`approval_rejected` / `approval_expired`), evaluate decides, nothing merged. A restart while waiting keeps the wait. A restart after approval never merges twice. | FR-128, FR-130, E2E-004 | P3 |
| REQ-12 | Runbook section "Policy and approvals (Phase 9)": rule file, CLI, default rules, the approval message, retirement of `allow_dangerous`. Architecture note: where `Decide` is called. | NFR-034 | D1 |

## 3. Domain and interface decisions

- `internal/policy`: `type Effect string`; `RuleSet{Schema, Defaults map[Tier]Effect, Actions map[string]Effect, ApprovalTTL}`; `Parse([]byte) (RuleSet, error)`; `Validate(RuleSet, KnownActions) error`; `Engine{Store, Known, Now}` with `Decide(ctx, Request) (Decision, error)` (always returns a usable decision; err only for logging). `Store`: `ActiveRuleSet`, `ApplyRuleSet`, `RuleSetHistory`, `RecordDecision`. Storage in `internal/adapters/storage/policy_store.go`, queries `db/query/policy.sql`.
- Executor: a `PolicyGate` interface injected through `NewExecutor` options (nil only in tests that predate it; production wiring always sets it). The `allowDangerous` constructor parameter is removed.
- Grants: `ApprovalGrant{ApprovalID}` passed through the call context by the orchestrator action only. The executor verifies the grant against the store. The context value alone never authorizes.
- Orchestrator: `github.merge` in `capability.go`; `OrchestratorActions` gains the merge path and the gate creation; the wait reuses `workflow_waits` with the approval.

## 4. File ownership map

| Area | Files |
|---|---|
| Engine + CLI (P1) | new `internal/policy/`, migration `00035`, `db/query/policy.sql`, `internal/adapters/storage/policy_store.go`, `cmd/setup-notices/policy.go` (`nh policy`), wiring |
| Executor + grants (P2) | `internal/agent/tools/executor.go`, `internal/agent/orchestrator.go`, `internal/agent/workflow_adapter.go`, `internal/settings/settings.go`, migration `00036`, approval store, `cmd/server/wiring.go` |
| Merge gate (P3) | `internal/orchestrator/capability.go`, `steps*.go`, `internal/behavior/orchestrator_actions.go`, `approvals.go`, `inbox_worker.go` callbacks, `texts.go` |
| Docs (D1) | `docs/runbooks/self-host-home.md`, `docs/architecture.md` |

## 5. Task packets

- **P1 — Policy engine, rule sets and CLI (Opus).** REQ 1–4. Commit `feat(policy): versioned rule sets and a deny-by-default engine`.
- **P2 — Executor gate and approval grants (Opus).** REQ 5–8. Commit `feat(policy): tool executor asks the policy engine; approval grants`.
- **P3 — PR merge behind owner approval (Opus).** REQ 9–11. Commit `feat(orchestrator): merge a PR only after the owner approves`.
- **D1 — Docs (Sonnet).** REQ 12. Commit `docs: policy and approvals`.

Order: P1 → P2 → Batch A (test, review, fixes) → P3 → D1 → Batch B (test, review, fixes, E2E-004 and E2E-009 journeys).

## 6. Agent dispatch and evidence gates

```text
P1 → P2 → Batch A Tester/CI (REQ-1..8) → Batch A Reviewer → fixes
P3 → D1 → Batch B Tester/CI (REQ-9..12, E2E-004, E2E-009) → Batch B Reviewer → fixes
same-head PASS + APPROVE → completion report → owner merges
```

- Agents: Claude subagents per role, with the same role limits. Sonnet: D1, tests and small fixes. Opus: P1–P3 and every review. One handoff at a time.
- Test Plans are approved by the Lead under the owner's standing instruction and recorded in the ledger.
- Live check after deploy, with the owner: `nh policy show` on the VPS shows v1; one real `/plan` that ends in a merge asks first.

## 7. Phase completion gate

- Test Report PASS and Review Report APPROVE on the same code head; CI green with no skip, race or panic.
- `docs/product/TRACEABILITY.md` updated for the IDs above; Phase Completion Report in `docs/product/reports/`.
- Merge into `develop` only on the owner's word.

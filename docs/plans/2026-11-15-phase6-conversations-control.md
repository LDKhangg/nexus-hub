# Phase 6 — Conversations and Control Commands Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `executing-plans` for an assigned task. The role boundaries and sequential handoff process in `AGENTS.md` override generic implementation/TDD instructions. Steps use checkbox (`- [ ]`) syntax for tracking.

**Status:** APPROVED by the owner on 2026-11-15 with decisions Q1a, Q2b, Q3a, Q4a (§1.2). Execute with the opencode agents; the Lead self-approves Test Plans under the owner's standing authorization; local release and a Telegram report after completion; merge into `develop` still needs the owner's OK.

**Goal:** The owner drives several workflows from Telegram without mixing them up: every execution has a `WF-<id>` reference, follow-ups reach the right workflow (or the hub asks), running workflows accept versioned instruction updates at a safe checkpoint, and `status`/`list`/`cancel`/`resume` work from the chat.

**Architecture:** Extend the Phase 5 durable engine and the P9 Telegram assistant. New workspace-scoped tables hold conversations, metadata-only message records, conversation↔workflow bindings, workflow↔resource bindings and versioned instruction updates. The assistant/inbox worker gains a target resolver; the behavior model step and the comment-pipeline investigate step read the instructions of their own execution at step start.

**Tech Stack:** Go (go.mod version), PostgreSQL, pgx v5, sqlc, goose. No new service or dependency.

**Spec:** `docs/product/PCC-001.md` EPIC 04 (FR-030–FR-042), E2E-003 (without worker delivery), NFR-002/004 privacy rules; scope in `docs/product/ROADMAP.md` §Phase 6. The contract is read-only.

## Global Constraints

- Code, documentation and commits are English; owner-facing discussion is Vietnamese.
- Baseline: `develop` at merge `65be4ac` (Phases 4 and 5). Branch `feature/phase6`. Next migration number is `00023`.
- One handoff at a time per shared working tree. Implementer changes production only; Tester changes tests only; Reviewer is read-only.
- Lead/Implementer never run `go test`. Checks: `go build ./...`, `go vet ./...`, `gofmt -l cmd internal`, `make generate` (second run clean), `goose -dir db/migrations validate`. The suite runs only in CI.
- Never edit old migrations or hand-edit sqlc output. Every new table is workspace-scoped with composite foreign keys and RLS enabled like its precedents.
- **Q2b privacy rule:** the database never stores raw owner or assistant chat text. Allowed: identifiers, kinds, timestamps, counters, a bounded sanitized conversation summary, and the instruction-update text the owner explicitly gave to a workflow (that text is the requested data). Nothing of this is logged; CLI output stays metadata-only (Phase 5 rule).
- Owner-only and workspace isolation stay as today. Side effects (cancel/resume/retry, alias, prefs) still need Làm/Huỷ through the P9 chat-bound confirmation path. Owner instructions can never widen action permissions, trusted targets, budgets or dry-run.
- Tests use fakes only; DB tests use loopback `TEST_DATABASE_URL`, database `nexus_storage_test`.

## Review Focus

- Cross-workflow and cross-workspace isolation: no instruction, summary, binding or resolution leaks between executions, conversations or workspaces.
- Resolution never guesses: ambiguous or unknown targets ask and execute nothing.
- Q2b: no raw chat text persisted or logged; summary bounded and sanitized.
- Instruction delivery is idempotent at checkpoints, versioned, cannot cancel running work, and stays inside existing policy.
- Durable engine invariants from Phase 5 (fencing, lock order, terminal immutability) hold for the new writes.

## 1. Scope and decisions

### 1.1 What the owner gets

- Reports, approvals and assistant answers name workflows as `WF-101`.
- "WF-101 thêm integration test nữa" stores instruction v1 for WF-101 only and confirms; WF-102 is untouched. The next model/investigate step of WF-101 reads it.
- "thêm test nữa" with no clear target gets a question with one button per candidate workflow; nothing happens until the owner picks one.
- Replying to a hub report targets that report's workflow.
- `/status WF-101`, `/list`, `/cancel WF-101`, `/resume WF-101` (and the same in plain words). Cancel/resume ask Làm/Huỷ.
- The assistant remembers the conversation across restarts as a short summary.

### 1.2 Owner decisions (2026-11-15)

| ID | Decision |
|---|---|
| Q1a | Reference ID is `WF-<execution id>`; parse `WF-101`/`wf-101` (and bare `101` where a command expects an execution). |
| Q2b | Persist only conversation summaries and metadata; raw chat text stays in the bounded in-memory history of P9. |
| Q3a | A conversation belongs to (workspace, Telegram integration, chat). The `users`/identity tables come with Phase 4b; keep a nullable seam, no fake user rows. |
| Q4a | Instruction updates are read by the behavior model step and the comment-pipeline investigate step at their next start (a safe checkpoint). Worker delivery is Phase 8. |

### 1.3 Boundaries

- Not in scope: Phase 7 planning/evaluation, Phase 8 workers, Phase 4b identity/inbox, Phase 9 policy, UI.
- `message_logs` (Phase 1 ClickUp threads) is not migrated; the new metadata table is the conversation record for Telegram. Note the overlap in the report.
- E2E-003 "worker receives the request at a safe checkpoint" is proven only for the model/investigate steps.

## 2. Requirements and acceptance

| ID | Observable behavior | PCC IDs | Tasks |
|---|---|---|---|
| REQ-1 | A conversation row exists per (workspace, Telegram integration, chat) and survives restart. Each owner message and hub reply in it gets a metadata-only record (direction, Telegram message id, reply-to id, kind, bound execution id if any, time). No raw text column exists. | FR-030, FR-031, FR-033 | P1, P3 |
| REQ-2 | After each assistant exchange a sanitized summary of at most 2,000 bytes (previous summary + exchange, produced by `triage_model` with a fixed prompt) is stored on the conversation. On restart the assistant starts from the stored summary. A failed or oversized summary keeps the previous one. Never logged. | FR-030, FR-042 | P3 |
| REQ-3 | Every execution is addressable as `WF-<id>`. Behavior/agent Telegram reports and approval prompts show the reference; CLI `nh workflow show/cancel/resume/retry/history` accept `WF-<id>` or the number; assistant tools print it. A template placeholder `{workflow_ref}` is available. | FR-036 | P2 |
| REQ-4 | A conversation can bind many executions. A binding is created when the hub sends an execution's report/approval into the chat, or when the owner names a `WF-<id>` that belongs to the workspace. Bindings are listable and workspace-scoped. | FR-032, FR-033 | P1, P2, P4 |
| REQ-5 | An execution can bind many external resources (ClickUp task, ClickUp comment, ClickUp chat message, GitHub PR, Telegram message). Bindings come from execution references at start and from sent Telegram messages. Lookup by resource returns the execution(s) in the same workspace only. | FR-034 | P1, P2 |
| REQ-6 | Target resolution for an owner message, in order: explicit `WF-<id>` (exactly one, in this workspace) → reply to a message bound to one execution → otherwise ambiguous. Ambiguous, multiple or unknown/foreign targets get a clarification (up to 5 buttons of the conversation's active bound executions, or a "not found" reply) and nothing executes. A button press continues the original request once. | FR-035, FR-037, FR-041 | P4 |
| REQ-7 | An instruction update for a resolved, non-terminal execution is stored as the next version (v1, v2, …) with sanitized text (≤ 2,000 runes), source Telegram message id, conversation id and time; the same Telegram update is stored once. The owner gets "WF-101: đã nhận yêu cầu bổ sung (v2)". Terminal executions refuse. Running work is not cancelled. | FR-038, FR-039, E2E-003 | P1, P4 |
| REQ-8 | The behavior model step and the agent investigate step, at step start, read all instruction versions of their own execution and add them as a bounded owner-instructions block, separate from untrusted context. The step records the highest version it used (history/checkpoint). Updates after a step started apply at the next model/investigate step or retry. They never change action permissions, trusted targets, budgets or dry-run. | FR-038, FR-041, E2E-003 | P5 |
| REQ-9 | `/status WF-<id>` (state, current step, waits, latest instruction version, bound resources), `/list` (active executions of the workspace, newest first, max 20), `/cancel WF-<id>` and `/resume WF-<id>` work in the owner chat; without an id they use REQ-6. Cancel/resume go through Làm/Huỷ (P9 path, chat-bound, frozen idempotency key). The assistant offers the same through its tools. | FR-040 | P4 |
| REQ-10 | Isolation: a prompt, instruction block, summary, status or binding of one execution/conversation/workspace never contains another's data; the EPIC 04 example passes exactly as written (WF-101 updated, WF-102 unaffected; "thêm test nữa" asks first). | FR-041, EPIC 04 acceptance | P4, P5 |
| REQ-11 | Runbook and report document commands, reference IDs, resolution rules, summary privacy and limits; CLI shows bindings and instruction versions as metadata only. | FR-040, NFR-034 | P6 |

## 3. Domain and interface decisions

### 3.1 Tables (migration `00023_conversations.sql`)

- `conversations(workspace_id, id, integration_id, chat_id, summary text ≤ 2000 bytes, summary_updated_at, created_at, updated_at)`, unique (workspace_id, integration_id, chat_id); `user_id uuid NULL` reserved for Phase 4b.
- `conversation_messages(workspace_id, id, conversation_id, direction in|out, kind, telegram_message_id, reply_to_message_id, execution_id NULL, created_at)` — no text.
- `conversation_workflows(workspace_id, conversation_id, execution_id, source, created_at)`, primary key (workspace_id, conversation_id, execution_id).
- `resource_bindings(workspace_id, execution_id, kind, external_id ≤ 512, created_at)`, unique (workspace_id, kind, external_id, execution_id); index (workspace_id, kind, external_id).
- `workflow_instructions(workspace_id, execution_id, version, text ≤ 2000 runes, conversation_id, telegram_update_id, created_at)`, primary key (workspace_id, execution_id, version); unique (workspace_id, telegram_update_id) for idempotency.
- All with composite FKs to `workflow_executions(workspace_id, id)` / `conversations`, RLS enabled, comments stating what is never stored.

### 3.2 Interfaces

- `workflow.FormatRef(id int64) string`, `workflow.ParseRef(string) (int64, bool)`; `workflow.FindRefs(text) []int64`.
- `storage.ConversationStore`: `EnsureConversation`, `RecordMessage`, `BindWorkflow`, `ListBoundWorkflows`, `SetSummary/GetSummary`, `BindResource`, `ExecutionsByResource`, `AppendInstruction` (returns version, idempotent), `ListInstructions(ws, execution)`.
- Behavior/agent steps receive instructions through a small reader interface injected at wiring; `workflow` must not import `behavior`/`agent`.

### 3.3 Resolution and clarification

The resolver runs before the assistant model for owner text. A message with an explicit control command or a resolved target and an instruction intent is handled deterministically; everything else goes to the P9 assistant with the resolved target (if any) in its context. Intent detection for "instruction update" vs "question": explicit forms (`WF-101 <text>`, reply to a workflow message with text, `/note WF-101 <text>`) are instruction updates; plain questions stay assistant questions. The assistant may also propose an instruction update through a tool, which stores immediately (no external side effect) and is acknowledged. Clarification uses a short-lived approval-style callback (existing nonce/expiry/chat binding) that stores the chosen execution and replays the request once.

## 4. File ownership map

| Area | Files |
|---|---|
| Migration + queries | `db/migrations/00023_conversations.sql`, `db/query/conversation.sql`, generated sqlc |
| Storage | `internal/adapters/storage/conversation_store.go` (new), small hooks in `workflow_store.go` (bindings at start) |
| References | `internal/workflow/ref.go` (new), `cmd/setup-notices/workflow.go`, report rendering (`internal/reporting`), behavior/agent report call sites |
| Conversation + summary | `internal/behavior/assistant.go`, `assistant_history.go`, new `conversation.go` |
| Resolver + commands | new `internal/behavior/workflow_targets.go`, `inbox_worker.go`, `assistant_tools.go`, `commands.go` |
| Instruction delivery | `internal/behavior/output.go`/`workflow_adapter.go`, `internal/agent/workflow_adapter.go`/`orchestrator.go`, `cmd/server/wiring.go` |
| Docs | `docs/runbooks/self-host-home.md` |

## 5. Task packets

### P1 — Conversation, binding and instruction storage
**REQ:** 1, 4, 5, 7 (storage). Migration 00023, queries, `ConversationStore` with the §3.2 API, workspace checks, idempotent `AppendInstruction` (version = max+1 under a lock on the execution row; terminal execution → `ErrNotEligible`). Commit `feat(storage): persist conversations, bindings and workflow instructions`.

### P2 — Workflow references and resource bindings
**REQ:** 3, 4, 5. `workflow` ref helpers; CLI accepts refs; reports/approval prompts show `WF-<id>` and `{workflow_ref}`; bind resources from execution references at start and Telegram messages when sent (behavior engine and agent report paths); bind conversation↔execution when a report/approval is sent to a chat. Commit `feat(workflow): reference executions as WF ids and bind their resources`.

### P3 — Persistent conversation and summary
**REQ:** 1, 2. Ensure conversation per chat; record message metadata for inbound owner text/commands and outbound hub messages; after each successful assistant exchange compute the summary with `triage_model` (bounded, sanitized, failure keeps old); load summary on start into the assistant context; keep P9 in-memory raw history. Commit `feat(assistant): remember conversations as persisted summaries`.

### P4 — Target resolution, instruction intake and control commands
**REQ:** 4, 6, 7, 9, 10. Resolver (§3.3), clarification buttons and replay, instruction intake + acknowledgement, `/status` `/list` `/cancel` `/resume` `/note`, assistant tools `workflow_note` and resolution-aware status. Commit `feat(behavior): resolve workflow targets and accept instruction updates from Telegram`.

### P5 — Instruction delivery at safe checkpoints
**REQ:** 8, 10. Inject an instruction reader into the behavior model step and the agent investigate step; bounded owner-instructions block in the prompt (separate from untrusted `<context>`), record the highest version used; never touch validators/targets/budgets. Commit `feat(workflow): deliver instruction updates at the next model step`.

### P6 — CLI metadata and runbook
**REQ:** 11. `nh workflow show` adds bound resources and instruction versions (count, latest version, times; no text); runbook section "Conversations and control (Phase 6)". Commit `docs: conversations, WF references and control commands`.

## 6. Agent dispatch and evidence gates

```text
P1 → P2 → P3 → Batch A Tester/CI → Batch A Reviewer → fixes
P4 → P5 → P6 → Batch B Tester/CI (incl. EPIC 04 + E2E-003 journeys) → Batch B Reviewer → fixes
same-head PASS + APPROVE → completion report → local release → Telegram report
```

- Each task gets its own Implementer session and commit; briefs under 80 lines point to this plan's sections.
- Briefs say "PRE-APPROVED, implement directly" for Test Plans the Lead approves; Tester still reports a TC map.
- If the OpenAI quota runs out, the Lead waits for the reset with a background job and resumes the same brief in a fresh session.
- Carry-in debt: open items in `docs/product/test-debt/` stay open unless a test in this phase proves them; live-service items need owner approval.

## 7. Phase completion gate

- [ ] All REQ-1–11 cases have CI evidence on the final head; no skip, race or panic; EPIC 04 example and E2E-003 (minus worker delivery) pass as journeys with fakes.
- [ ] Reviewer APPROVE on the same head; Critical/Important findings resolved.
- [ ] Migration validates, generation reproducible, Q2b verified (no raw chat text columns or logs).
- [ ] Lead updates `docs/product/TRACEABILITY.md`, writes `docs/product/reports/<date>-phase6-report.md`, releases locally and reports on Telegram. Merge needs the owner's OK.

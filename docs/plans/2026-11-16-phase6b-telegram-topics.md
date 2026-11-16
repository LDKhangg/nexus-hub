# Phase 6b — Telegram Topics per Task Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `executing-plans` for an assigned task. The role boundaries and sequential handoff process in `AGENTS.md` override generic implementation/TDD instructions. Steps use checkbox (`- [ ]`) syntax for tracking.

**Status:** DRAFT 2026-11-16. Owner decisions W1–W4 approved (§1.2); waiting for the owner's approval of this plan.

**Goal:** The owner's Telegram stops being one mixed stream. Each workspace has its own forum group; inside it every ClickUp task the owner is involved in gets its own topic, a fixed topic holds PR-approve notices, and General holds free chat. Whatever the owner writes inside a topic reaches that task's workflows without typing a workspace or `WF-n` code, and every notice carries a button that jumps to the right topic.

**Architecture:** Extend the Phase 6 conversation layer. A topic is a conversation keyed by (workspace, integration, chat, thread). A new `telegram_topics` table binds a thread to a subject (`clickup_task:<id>`, `workflow:<id>`, or a fixed name). A `TopicRouter` resolves the destination of every outbound owner message (behavior reports, agent reports, approvals, questions) from the behavior's `report.topic` setting and the execution's resource bindings, creating or reopening the topic on demand through the Bot API. Inbound updates carry `message_thread_id`; the Phase 6 target resolver gets the topic as its first, scoped signal.

**Tech Stack:** Go (go.mod version), PostgreSQL, pgx v5, sqlc, goose. Telegram Bot API forum methods (`createForumTopic`, `closeForumTopic`, `reopenForumTopic`, `message_thread_id`). No new service or dependency.

**Spec:** `docs/product/PCC-001.md` FR-131 (destination resolution), FR-132 (original thread replies), FR-033/034/035/041 (conversations, bindings, follow-up resolution, isolation), FR-012 (inbound normalization, Telegram part); E2E-003 Telegram part. Pulls FR-131/132 forward from Phase 10 for Telegram only. The contract is read-only.

## Global Constraints

- Code, documentation and commits are English; owner-facing discussion is Vietnamese.
- Baseline: `develop` after the Phase 6a merge. Branch `feature/phase6b`. Carry-in from Phase 6a: R-4 (a fast router reply ending with a question pushes the next owner message to the work model) — fix in P3 by making the router end its own replies without a question or by recording which tier answered each exchange. Next migration number is `00025` (Phase 7 then starts at `00026`).
- One handoff at a time per shared working tree. Implementer changes production only; Tester changes tests only; Reviewer is read-only.
- Lead/Implementer never run `go test`. Checks: `go build ./...`, `go vet ./...`, `gofmt -l cmd internal`, `make generate` (second run clean), `goose -dir db/migrations validate`. The suite runs only in CI.
- Never edit old migrations or hand-edit sqlc output. New tables are workspace-scoped with composite foreign keys and RLS like their precedents.
- **Privacy (Q2b):** the DB stores topic ids, subject keys (`clickup_task:<id>`), state and times only. Topic names are built from the task id and the ClickUp task name at send time, sanitized and ≤ 128 bytes, and are never stored or logged.
- **Hub stays generic:** where a behavior posts is data (`report.topic` in behavior YAML), never a hard-coded behavior name. The engine knows the three modes and fixed names, nothing about "PR approve".
- **Fallback never loses a message:** if the chat is not a forum, the bot lacks "Manage topics", or a topic call fails, the message goes to the chat's main thread (General) with the same content, and the failure is counted (metadata only). A topic failure never fails or repeats a send.
- Owner-only and workspace isolation stay as today: an update is accepted only from the workspace's own group and the owner's user id; a topic of workspace A can never resolve to data of workspace B.
- Tests use fakes only (fake Telegram Bot API with forum methods); DB tests use loopback `TEST_DATABASE_URL`, database `nexus_storage_test`.

## Review Focus

- Isolation: topic → workspace → task → executions is strictly scoped; another chat, another workspace's group, or a foreign thread id resolves to nothing.
- Exactly one topic per subject under concurrency (two events of a new task at once create one topic, never two).
- Fallback to General on every topic failure; no lost, duplicated or misrouted message.
- Resolver order inside a topic never guesses across tasks; buttons stay within the topic's subject.
- No topic name, task name or chat text stored or logged.

## 1. Scope and decisions

### 1.1 What the owner gets

```text
Group "LEAP · Nexus"            (one group per workspace)
├─ General                      free chat with the assistant, /list, system alerts
├─ PR approve                   fixed topic: every pr-auto-approve report
├─ 86c1x2 · Fix login timeout   Lead tagged me on a bug task → its own topic
├─ 86c1y7 · Spec: export report spec-review, open questions, answers
└─ WF-131 · <title>             work not tied to a ClickUp task
```

- Every event tied to a ClickUp task (mention/tag, assignment, comment pipeline, spec-review, a PR whose branch or body names the task) goes to that task's topic, created on first use.
- Inside a topic: one active workflow → the message goes to it (instruction or answer); several → buttons limited to this topic; none → the assistant answers with this task as context.
- A ClickUp task link pasted in General → reply with an "Mở topic" button (deep link), creating the topic if needed. Every report carries an "Mở topic" button when it is posted outside its topic; `/list` groups active work by topic with links.
- A topic closes when its task reaches a done/closed status on ClickUp and no workflow of it is active; it reopens on the next event. History stays.

### 1.2 Owner decisions (2026-11-16)

| ID | Decision |
|---|---|
| W1 | One Telegram forum group per workspace. The workspace's Telegram control integration holds its group chat id; `owner_telegram_chat_id` stays only as the fallback for a workspace without one. |
| W2 | Every event tied to a ClickUp task opens/uses that task's topic, except behaviors that name a fixed topic (pr-auto-approve → "PR approve"). |
| W3 | Notices that need no action (a tag just to inform) also go to the task's topic. |
| W4 | Close the topic when the task is done/closed and no workflow of it is active; keep history; reopen on a new event. |

### 1.3 Boundaries

- Telegram only. Cross-platform routing, notification policies and the unified outbox stay in Phase 10.
- Forum supergroups only; topics in private bot chats are out of scope (reported broken since Bot API 10.0).
- No topic deletion, renaming on task rename, or icons beyond a fixed emoji per subject kind.
- The owner turns on Topics in the group and gives the bot the "Manage topics" admin right; the Lead updates the chat id after the group becomes a supergroup.

## 2. Requirements and acceptance

| ID | Observable behavior | PCC IDs | Tasks |
|---|---|---|---|
| REQ-1 | Each workspace's Telegram control integration can hold its owner group chat id (`nh integration` / setup flag). Owner messages of a workspace go to that group; inbound updates are accepted only from that group (and the owner user), and map to that workspace. Without it, today's `owner_telegram_chat_id` behavior is unchanged. | FR-131, FR-041 | P1 |
| REQ-2 | `telegram_topics` binds (workspace, integration, chat, thread id) to one subject: `clickup_task:<id>`, `workflow:<id>` or `fixed:<name>`; state open/closed; unique per subject and per thread. Creation is race-safe: concurrent requests for one subject create exactly one topic. | FR-034, FR-131 | P1 |
| REQ-3 | Behavior YAML accepts `report.topic: task | workflow | general | "<fixed name>"` (default `task`; `task` falls back to `workflow` when the execution has no ClickUp task binding). Validation rejects unknown modes and fixed names > 64 bytes. `pr-auto-approve.yaml` uses `"PR approve"`. Agent (comment pipeline) reports use `task`. | FR-131 | P2 |
| REQ-4 | Every owner-bound send of an execution (report, approval prompt, clarification, instruction ack, failure notice) resolves its topic through the `TopicRouter`, creating or reopening it on demand, and is sent with `message_thread_id`. The sent message is bound to the execution as in Phase 6, now with its thread id. Topic name: `<task id> · <task name>` / `WF-<id> · <title>` / the fixed name, sanitized, ≤ 128 bytes, not stored. | FR-131, FR-132 | P2 |
| REQ-5 | Fallback: non-forum chat, missing right, or any topic API failure → the same message goes to General, the topic stays unbound (retried on the next send), and a metadata counter/log code records it. A send is never duplicated or lost because of a topic step. | FR-131, NFR-002 | P2 |
| REQ-6 | Inbound: updates carry the thread id into the durable inbox. A conversation is per (workspace, integration, chat, thread); each topic has its own summary (Phase 6 rules), General keeps the chat-level one. | FR-012, FR-030, FR-041 | P1, P3 |
| REQ-7 | Target resolution inside a topic, in order: explicit `WF-n` of this workspace → reply to a bound message → the topic's subject: exactly one active execution bound to it → that one; several → clarification buttons listing only those; none → the assistant with the subject (task id) as context. In General, Phase 6 rules apply unchanged. Commands (`/status`, `/cancel`, `/resume`, `/note`) without an id use the same order. | FR-035, FR-037, FR-041 | P3 |
| REQ-8 | A ClickUp task URL (or bare task id with `/topic <id>`) in General replies with an "Mở topic" URL button to that task's topic (created if needed, the task must be readable by the workspace's ClickUp source). Reports posted outside their topic (fallback) and `/list` entries carry the same button. | FR-131, FR-040 | P4 |
| REQ-9 | Lifecycle: a ClickUp status event moving a task to a done/closed status type closes its topic when no execution bound to it is active; when the last active execution ends and the task is already done/closed, the topic closes then. Any new send to a closed topic reopens it first. Closing never deletes. | FR-131 | P4 |
| REQ-10 | Runbook: enabling Topics, bot rights, setting the group per workspace, `report.topic` values, fallback behavior; CLI `nh telegram topics --workspace-id` lists subject keys, thread ids and states (no names). | NFR-034 | P4 |

## 3. Domain and interface decisions

### 3.1 Migration `00025_telegram_topics.sql`

- `telegram_topics` (id, workspace_id, integration_id, chat_id, thread_id, subject_kind enum-like text `clickup_task|workflow|fixed`, subject_key ≤ 128, state `open|closed`, created_at, updated_at, closed_at); unique (workspace_id, integration_id, chat_id, subject_kind, subject_key); unique (workspace_id, integration_id, chat_id, thread_id); composite FK to integrations; RLS.
- `conversations`: add `thread_id bigint NOT NULL DEFAULT 0` and replace the unique key with (workspace_id, integration_id, chat_id, thread_id) — new constraint in the new migration, old one dropped there (old migration untouched).
- `telegram_updates`: add `message_thread_id bigint`.
- Telegram control integration config: optional `owner_chat_id` (validated like `message_thread_id` in `internal/tenant/notification_config.go`).

### 3.2 Interfaces

- `telegram.Client`: `CreateForumTopic(name) (threadID)`, `CloseForumTopic`, `ReopenForumTopic`; `MessageOptions.ThreadID` already exists (`internal/adapters/control/telegram/bot.go:31`).
- `behavior.TopicRouter`: `Destination(ctx, ws, execution, mode) (ChatRef{…, ThreadID}, error)`; uses resource bindings to find the ClickUp task; race-safe ensure via insert-or-get under a row lock on the subject.
- `behavior.ChatRef` gains `ThreadID int64`; `ownerChats` (`cmd/server/wiring.go:250`) reads the per-workspace group first.
- Resolver (`internal/behavior/workflow_targets.go`) gets the topic subject as an input before the Phase 6 fallback.

## 4. File ownership map

| Area | Files |
|---|---|
| Migration + storage | `db/migrations/00025_telegram_topics.sql`, `db/query/telegram_topic.sql`, sqlc output, `internal/adapters/storage/telegram_topic_store.go` (new), `conversation_store.go` |
| Telegram adapter | `internal/adapters/control/telegram/bot.go` (forum methods), `internal/delivery/http/telegram_handler.go` (per-workspace group, thread id) |
| Routing | new `internal/behavior/topics.go`, `output.go`, `workflow_adapter.go`, `approvals.go`, `internal/agent/workflow_adapter.go` (report path), `cmd/server/wiring.go` |
| Behavior schema | `internal/behavior/schema.go`, `validate.go`, `examples/behaviors/pr-auto-approve.yaml`, other examples |
| Resolver + commands | `internal/behavior/workflow_targets.go`, `inbox_worker.go`, `commands.go`, `assistant.go` |
| Lifecycle | ClickUp status handling (`internal/adapters/sources/clickup/parser.go` consumer side), workflow terminal hook |
| CLI + docs | `cmd/setup-notices/` (group flag, `telegram topics`), `docs/runbooks/self-host-home.md` |

## 5. Task packets

### P1 — Topic storage, per-workspace group, inbound thread id
**REQ:** 1, 2, 6 (storage + inbound). Migration 00025, store with race-safe ensure, integration `owner_chat_id`, handler accepts the workspace group and records thread id, conversation per thread. Commit `feat(telegram): per-workspace groups and topic storage`.

### P2 — Outbound topic routing and fallback
**REQ:** 3, 4, 5. Forum methods in the client, `report.topic` schema + validation + example updates, `TopicRouter`, all owner-bound execution sends routed, bindings with thread id, General fallback. Commit `feat(behavior): route execution messages to task topics`.

### P3 — Topic-aware resolution
**REQ:** 6, 7. Resolver order with topic subject, scoped buttons, assistant context per topic, commands without id. Commit `feat(behavior): resolve follow-ups from the topic they are written in`.

### P4 — Jump links, lifecycle, CLI and runbook
**REQ:** 8, 9, 10. Task link in General → "Mở topic", buttons on fallback reports and `/list`, close/reopen rules, `nh telegram topics`, runbook. Commit `feat(telegram): topic links and lifecycle`.

## 6. Agent dispatch and evidence gates

```text
P1 → P2 → Batch A Tester/CI (storage, races, routing, fallback) → Batch A Reviewer → fixes
P3 → P4 → Batch B Tester/CI (journeys below) → Batch B Reviewer → fixes
same-head PASS + APPROVE → completion report → owner turns on Topics → merge (deploys to VPS) after OK → Telegram report
```

Journeys for Batch B (fake Bot API): Lead tags the owner on task T → topic "T · …" created, report inside; a second event on T lands in the same topic; owner writes "thêm test" in T's topic with one active WF → instruction v1 to it, nothing else touched; two active WFs on T → buttons with only those two; PR-approve DM → "PR approve" topic; task T closed → topic closed; new tag on T → reopened; bot loses the right → message in General with an "Mở topic" fallback, nothing lost; a message from another workspace's group or another user → ignored.

## 7. Phase completion gate

- [ ] All REQ-1–10 cases have CI evidence on the final head; no skip, race or panic; the journeys above pass.
- [ ] Reviewer APPROVE on the same head; Critical/Important findings resolved.
- [ ] Migration validates, generation reproducible, no topic/task names stored or logged.
- [ ] Lead updates `docs/product/TRACEABILITY.md`, writes `docs/product/reports/<date>-phase6b-report.md`. Merge into `develop` (deploys to the VPS) needs the owner's OK; Topics must be on in the group before the merge.

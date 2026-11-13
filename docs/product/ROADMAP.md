# Nexus-Hub roadmap to PCC-001

This file turns the Product Completion Contract (`docs/product/PCC-001.md`)
into an ordered list of phases for this repository. The PCC is the
destination. This roadmap is the route. `docs/product/TRACEABILITY.md` holds
the status of every PCC requirement.

When documents conflict, the PCC wins over this roadmap, and this roadmap
wins over `docs/architecture.md`. That file predates the PCC: it mentions Gemini
Flash, an Oracle micro VM and 5 milestones, and is superseded on those
points.

## 1. How this roadmap is used

- **One phase at a time.** Each phase is a vertical slice that runs and can be
  tested. A phase starts only after the phases it depends on have finished
  (PCC §10.1.5).
- **Phase plan.** For each phase, the Lead writes
  `docs/plans/<date>-phase<N>-<topic>.md`, following `AGENTS.md`.
  The plan:
  - lists the PCC IDs it covers (`FR-…`, `UI-…`, `NFR-…`, `DEP-…`, `E2E-…`);
  - turns them into numbered, observable `REQ-n` items, each tagged with its
    PCC IDs;
  - writes tasks in the PCC §10.2 format.
- **Evidence.** A PCC ID moves to `VERIFIED` only when a Test Report (`PASS`)
  and a Review Report (`APPROVE`) on the same commit cover it, and the phase is
  merged. The Lead updates `TRACEABILITY.md`; nobody else marks `VERIFIED`.
  Partial delivery stays `IN_PROGRESS`, with a note saying what is missing.
- **Phase end.** The phase is done when every item in "Exit criteria" holds
  and the PCC §10.3 checklist is met. The Lead then writes a Phase Completion
  Report (PCC §10.4) to `docs/product/reports/phase-<N>.md` and asks the owner
  before merging.
- **No silent scope cuts.** An acceptance criterion is never dropped to make a
  phase pass. If something cannot be done, it becomes a written blocker:
  cause, impact and options (PCC §10.1.9–10). It needs the owner's decision,
  or an approved exception (Gate A).
- **No fake success.** Integrations, API calls and E2E scenarios count as done
  only when they have actually run (PCC §10.1.11). Phases 1–12 are verified
  with fakes. Real-service acceptance happens in Phase 13, and in smoke checks
  the owner explicitly allows.

## 2. Where we are (2026-11-12)

| Repo phase | Status | What exists |
|---|---|---|
| 1 | Merged (`54aa048`) | ClickUp webhook (HMAC-verified) → durable `notification_outbox` → Telegram notice; idempotent ingest; setup CLI; runbook |
| 2 | Merged with test debt (`69fcb5e`; see `test-debt/phase2-agent-core-tools.md`) | Durable agent jobs with debounce, Tier-1 triage, Tier-2 tool loop, tiered tool policy with audit (`tool_runs`), hardened git runner, GitHub/ClickUp tools, netguard, AES-GCM credentials, DB settings, `outbound_actions`, CLI, systemd unit |

PCC milestones and repo phases do not map one to one:

| PCC §11 milestone | Repo phase(s) |
|---|---|
| 0 Project Foundation | F, before 13 (CI already done) |
| 1 Identity + Event Gateway | 4 |
| 2 Integrations + Conversations | 1, 2 (inbound and tools), 4, 6 |
| 3 Workflow Engine | 5 |
| 4 AI Orchestrator | 2 (triage), 7 |
| 5 Worker Runtime | 2 (tool loop, runner), 8 |
| 6 Policies + Approvals | 2 (tool policy), 9 |
| 7 Response Router | 1 (outbox), 10 |
| 8 Admin UI | 11 |
| 9 Reliability + Security | 12 |
| 10 E2E + Deployment | 13 |

The earlier plan held three items for "Phase 3". They now go here:
- Telegram owner commands → Phases 4 and 6.
- Live ClickUp acceptance → Phase 13 (and an earlier smoke check if the owner
  allows it).
- Independent review → every phase, through the Reviewer role.

## 3. Dependency graph

```text
P1 notices ─► P2 agent core ─► P3 behavior engine ─► P4 identity + event gateway
(Phase F foundation runs right before P13; P11 Admin UI is replaced by the CLI, D2)
                                                   │
                                                   ▼
                                    P5 workflow engine ─► P6 conversations + commands
                                                   │                 │
                                                   ▼                 │
                                    P7 orchestrator ◄────────────────┘
                                                   │
                                                   ▼
                                    P8 workers + scheduler
                                                   │
                                                   ▼
                                    P9 policy + approvals ─► P10 response router + outbound
                                                                     │
                                                                     ▼
                                    P11 admin UI + API (needs P5–P10 data)
                                                                     │
                                                                     ▼
                                    P12 reliability + security ─► P13 deployment + acceptance
```

P11 can start its read-only screens (executions, health) after P5. It only
finishes after P10.

## 4. Phases

Each phase lists its goal, the PCC IDs it delivers (a "partial" ID stays
`IN_PROGRESS` and is finished in a later phase), its main deliverables, its
exit criteria, and the decisions it needs.

### Phase 2: Agent core, tools and DB secrets (finish)

- **Goal:** finish and merge what is on `feature/phase2`.
- **PCC (partial):**
  - FR-015, FR-019, FR-029, FR-042
  - FR-043/045 (triage only), FR-055, FR-056
  - FR-062, FR-063, FR-065, FR-069, FR-098
  - FR-113, FR-114, FR-116 (tool audit), FR-119, FR-120
  - NFR-005, NFR-016, NFR-019 (git only), NFR-020, NFR-021, NFR-022, NFR-030
- **Exit criteria:**
  - a Test Report `PASS` and a Review Report `APPROVE` on the same head;
  - merged after the owner's OK;
  - Phase Completion Report written.

### Phase 3: Behavior engine with graded autonomy

- **Goal (owner, 2026-11-12):** the hub stays generic; what a user wants done
  on an event is a versioned **behavior** stored as data (trigger, reads,
  instructions, autonomy rules, allowed actions). The engine classes every
  result item `auto` / `notify` / `ask`, acts within policy, asks the owner on
  Telegram only when needed, and traces everything.
- **First behavior:** the owner's tech-spec review (company AI's spec PR + US +
  OQ, answers into the task description, dry-run first) as a YAML file, not
  code.
- **Plan:** `docs/plans/2026-11-12-phase3-behavior-engine.md`.
- **PCC:** FR-073–078, 081, 082, 084–086 (partial), FR-003, 012, 017, 018,
  029, FR-037, 053, 055, 056, FR-009, 113, 115, 116, 121–127, 129, 130, FR-026,
  NFR-021–023, 025, 026; E2E-004 partial.

### Phase F: Foundation and operability (moved, runs right before Phase 13)

Moved by the owner on 2026-11-12: it gives no user value before the system is
hosted. CI and correlation IDs are already done or built into each phase.


- **Goal:** the project can be built, tested and observed by anyone, which
  every later phase depends on.
- **PCC:**
  - NFR-025, NFR-026 (logging context), NFR-031, NFR-033, NFR-035, NFR-037,
    NFR-038
  - DEP-002, DEP-003, DEP-004, DEP-005
  - Go code criteria: structured errors, structured logging, composition root
- **Deliverables:**
  - `README.md`;
  - CI pipeline: `go build`, `go vet`, `gofmt`, `go test -race ./...` with a
    PostgreSQL service, `make generate` drift check, `goose validate`;
  - `log/slog` JSON logging with a correlation-ID field and the existing
    secret rules;
  - `/healthz` (liveness) and `/readyz` (DB, migrations, master key, workers);
  - migrations embedded and applied at startup, or a documented deploy step;
  - an environment and configuration document;
  - seed/demo data for local runs.
- **Exit criteria:**
  - CI is green on main;
  - health endpoints are tested;
  - a fresh clone runs following the README with fakes only.

### Phase 4: People directory and report preferences (re-scoped 2026-11-13)

- **Re-scoped by the owner:** identify the other people on connected
  platforms (contacts with aliases across ClickUp, GitHub, Telegram), and let
  the owner set report style, toggles and templates. Plan:
  `docs/plans/2026-11-13-phase4-people-directory-report-prefs.md`.
- The original Phase 4 scope below becomes **Phase 4b: Identity and event
  gateway** (inbox, event contract, adapter registry, `/link` for other hub
  users).

### Phase 4b: Identity and event gateway (was Phase 4)

- **Goal:** the system knows who is talking to it, and every inbound event
  becomes one versioned, deduplicated, traceable event.
- **PCC:**
  - FR-001–FR-009
  - FR-010, FR-011 (registry and capability discovery, inbound side)
  - FR-012, FR-017 (Telegram, ClickUp; GitHub in P10), FR-018
  - FR-021–FR-028
  - NFR-001, NFR-015, NFR-026
  - E2E-006
- **Deliverables:**
  - `users` and `external_identities` tables;
  - identity link flow: a one-time code from the CLI (later the UI), then
    `/link <code>` sent from Telegram;
  - an allowlist, revoke, and deny for unknown senders;
  - Telegram inbound (webhook verified with the secret-token header);
  - the event contract v1 (the PCC §3 EPIC 03 fields) and an `inbound_events`
    inbox: raw metadata, dedupe, safe handling of unknown types, and a replay
    command that never repeats side effects;
  - Phase 1/2 ClickUp ingest moved onto the same contract;
  - a correlation ID carried from the event to jobs, audit and outbox;
  - a `PlatformAdapter` registry (inbound methods);
  - `/help` and `/whoami` answered from Telegram.
- **Exit criteria:**
  - the EPIC 01 and EPIC 03 acceptance criteria are tested;
  - E2E-006 passes with fakes;
  - an unlinked Telegram account can reach nothing.
- **Decisions:** D3 (Telegram inbound mode), D4 (user ↔ workspace model).

### Phase 5: Durable workflow engine

- **Goal:** work is a versioned workflow execution that survives restarts.
- **PCC:**
  - FR-073–FR-086
  - FR-087–FR-096, FR-098, FR-099
  - FR-100, FR-106, FR-108
  - NFR-002, NFR-003, NFR-004
  - DB criteria: immutable versions, idempotency constraints, concurrency
    strategy
- **Deliverables:**
  - `workflow_definitions` and `workflow_versions` (validate, publish,
    immutable, enable/disable, triggers, JSON/YAML import and export);
  - `workflow_executions` with the PCC state set and an enforced
    transition table;
  - `execution_steps` checkpoints and event history;
  - a generic `jobs` queue with leases, which absorbs `agent_jobs`;
  - retry policy, timeouts, cancellation, restart reconciliation, and
    dead-letter inspection in the CLI;
  - the Phase 2 "ClickUp comment → triage → report" flow re-expressed as a
    built-in workflow definition, with no behaviour change.
- **Exit criteria:**
  - the EPIC 07 and EPIC 08 acceptance criteria are tested;
  - a kill-and-restart test loses nothing;
  - `COMPLETED → RUNNING` is impossible.

### Phase 6: Conversations and control commands

- **Goal:** the user drives several workflows from Telegram without mixing
  them up.
- **PCC:**
  - FR-030–FR-041 (FR-042 is finished here)
  - FR-034 (`resource_bindings`)
  - E2E-003, without worker delivery, which comes in P8
- **Deliverables:**
  - `conversations`, `messages` (unify with `message_logs`) and
    `resource_bindings`;
  - workflow reference IDs (`WF-101`);
  - follow-up resolution, with a clarification question when the target is
    ambiguous;
  - versioned instruction updates;
  - `status`, `list`, `cancel` and `resume` from Telegram;
  - context isolation between workflows.
- **Exit criteria:**
  - the EPIC 04 acceptance example passes exactly as written in the PCC, with
    WF-101 and WF-102.

### Phase 7: AI orchestrator

- **Goal:** structured plans and evaluations replace one-shot triage.
- **PCC:** FR-043–FR-056, ARCH-002, ARCH-004.
- **Deliverables:**
  - an `Orchestrator` with `Plan` and `Evaluate`;
  - the seven decisions (`EXECUTE`, `DELEGATE`, `REQUEST_CLARIFICATION`,
    `REQUEST_APPROVAL`, `REPLAN`, `COMPLETE`, `FAIL`);
  - JSON-schema validated outputs with bounded repair;
  - decomposition with dependencies, capability requirements,
    priority/complexity, and iteration and token budgets;
  - a provider-neutral AI runtime interface, with Anthropic as one
    implementation and a deterministic fake provider for tests;
  - `COMPLETE` only when result evidence exists.
- **Exit criteria:**
  - the EPIC 05 acceptance criteria are tested with the fake provider;
  - an over-budget plan ends in `FAILED` or blocked, never in a loop.

### Phase 8: Worker runtime and scheduler

- **Goal:** two isolated worker slots execute tasks in parallel under
  scheduler control.
- **PCC:**
  - FR-057–FR-072, FR-097
  - FR-101–FR-105, FR-107, FR-109, FR-110
  - NFR-009, NFR-012, NFR-019
  - ARCH-003
  - E2E-002, E2E-007
- **Deliverables:**
  - `worker_definitions` and `worker_runs`, with capability profiles,
    availability, heartbeat and cancellation;
  - one isolated workspace per worker run (a git worktree or a separate clone
    under `repos_dir`);
  - the Phase 2 Tier-2 loop refactored into `AgentRunner.Execute`;
  - the worker result contract (PCC JSON) and artifact references;
  - repo/branch write locks, workspace cleanup and retention;
  - the scheduler: priority, capability matching, `max_parallel_workers`,
    per-user concurrency, backpressure, allocation audit;
  - waiting states release the slot;
  - **test execution for workers**, according to D1.
- **Exit criteria:**
  - the EPIC 06 and EPIC 09 acceptance criteria pass (three jobs, two slots);
  - E2E-002 and E2E-007 pass with fakes.
- **Decisions:** D1 (sandbox for running project tests). This changes the
  Phase 2 constraint "git is the only binary".

### Phase 9: Policy engine and approvals

- **Goal:** the user's rules decide, and risky actions wait for the user's OK
  from the phone.
- **PCC:**
  - FR-009 (finished), FR-111–FR-118 (FR-117's UI part is in P11)
  - FR-121–FR-130
  - NFR-023
  - E2E-004, E2E-009
- **Deliverables:**
  - `user_rule_sets`, with a versioned schema and validation;
  - a `PolicyEngine` that unifies the Phase 2 tool policy with workflow
    actions;
  - deny by default, and every decision audited;
  - approval-gated actions (`merge_pr: approval_required`), replacing the
    global `allow_dangerous` switch;
  - `approval_requests`, delivered as Telegram inline buttons: binding to
    user, execution, action and resource; expiry; duplicate prevention; policy
    re-checked before execution; actor and time recorded;
  - the AI can never change policy.
- **Exit criteria:**
  - the EPIC 10 and EPIC 11 acceptance criteria are tested;
  - E2E-004 and E2E-009 pass with fakes;
  - a stale approval cannot authorize a different action.

### Phase 10: Response router and outbound actions

- **Goal:** results go to the right place on every platform, exactly once.
- **PCC:**
  - FR-013, FR-014, FR-016, FR-019 (finished)
  - FR-131–FR-143
  - FR-017 (GitHub webhooks)
  - E2E-008, E2E-010
  - E2E-001 path with fakes
- **Deliverables:**
  - a `ResponseRouter` with destination resolution and replies in the original
    thread;
  - cross-platform notifications with on-started, completed, blocked and
    failed policies, dedupe, aggregation, rate limits and a fallback
    destination;
  - one transactional outbox (merging `notification_outbox` and
    `outbound_actions` into `outbox_messages`);
  - outbound adapters: ClickUp comment and update; GitHub branch, PR, comment
    and status; Telegram reply. Each is capability-checked and goes through
    the policy;
  - GitHub inbound events;
  - a demo adapter with contract tests.
  - This phase ends the Phase 2 rule "post nothing to ClickUp".
- **Exit criteria:**
  - the EPIC 02 and EPIC 12 acceptance criteria are tested;
  - E2E-008 and E2E-010 pass;
  - E2E-001 passes end to end with fake ClickUp, GitHub and LLM.

### Phase 11: Admin UI and API

- **Goal:** daily operation without SSH or manual config edits.
- **PCC:**
  - UI-001–UI-030
  - FR-020, FR-117
  - NFR-011, NFR-014, NFR-018, NFR-036
- **Deliverables:**
  - admin authentication and sessions, CSRF protection, protected write
    operations;
  - dashboard, integrations (write-only credential entry, test connection,
    health), workflows (form, JSON/YAML preview, validate, publish,
    enable/disable), executions (list, timeline, step outputs, errors and
    retries, cancel/retry, artifact links), and policies and settings;
  - a mobile layout for status and approvals;
  - an OpenAPI document for the JSON API.
- **Exit criteria:** the EPIC 13 acceptance criteria are tested, and the UI is
  served only over HTTPS with authentication.
- **Decisions:** D2 (UI technology).

### Phase 12: Reliability, security and operations hardening

- **Goal:** prove that the system survives failure and attack on the target
  hardware.
- **PCC:**
  - NFR-001–NFR-007, NFR-010, NFR-012, NFR-013, NFR-017, NFR-024
  - NFR-027–NFR-030, NFR-032, NFR-040
  - DB criteria: backup and restore, retention and cleanup
  - Gate B, Gate C
- **Deliverables:**
  - a restart and crash test suite;
  - a load test: 100 executions, and webhook p95 under 500 ms;
  - a tested backup and restore procedure;
  - retention and deletion controls;
  - metrics (`/metrics`), queue depth and worker status;
  - alerts to Telegram for stuck or failed workflows;
  - carry-overs from Phase 2: git network traffic behind the egress guard, OS
    disk quotas, and an OS sandbox for git;
  - a security review against the PCC security NFRs;
  - the recovery runbook.
- **Exit criteria:** Gates B and C are met with evidence.

### Phase 13: Deployment and final acceptance

- **Goal:** a fresh install on the target server passes every E2E scenario
  against real services.
- **PCC:**
  - DEP-001–DEP-012
  - NFR-039
  - E2E-001–E2E-010 on real integrations
  - Gates A, D, E and F
  - §13 final success scenario
- **Deliverables:**
  - Docker Compose and/or systemd units, a reverse proxy with TLS, persistent
    volumes, backup scheduling and log rotation;
  - secure remote access (D5);
  - an upgrade and rollback guide;
  - a fresh-install test on Ubuntu 24.04 (x86-64, i5-8500T class);
  - E2E runs on real Telegram, ClickUp and GitHub accounts (D6);
  - the final test report, the traceability matrix at 100%, and the known
    limitations.
- **Exit criteria:** every release gate in PCC §12 is met, and the owner signs
  off.

## 5. Owner decisions

| ID | Topic | Status | Decision (2026-11-12) |
|---|---|---|---|
| D1 | How workers run project tests (E2E-001 "Worker chạy tests phù hợp") | Decided, deferred | The VPS is too small to run project tests. Until the system runs on a local machine, workers do **not** run tests. They push a PR; testing is delegated to an external tester (the ClickUp `@AI` agent, or CI checks), and Nexus-Hub reads and reports the result. A worker test sandbox returns when local hosting exists. This is a PCC exception to record at Gate A. |
| D2 | Admin UI | Decided | A CLI only for now (setup and administration). Daily control goes through Telegram commands. EPIC 13 (UI-001–UI-030), FR-020, FR-117 (UI part) and Gate D "configure workflows through the UI" become a PCC exception, unless the owner later asks for a UI. |
| D3 | Telegram inbound | Decided | Webhook, verified with the secret-token header, on the same public HTTPS endpoint as the ClickUp webhooks. Rationale: no idle polling connection, instant delivery; the VPS already has a public domain (D5). Long polling only wins behind NAT. |
| D4 | Users ↔ workspaces | Decided (revised 2026-11-13) | One user can belong to many workspaces (owner, in chat: "1 user nhiều ws được"), through a user ↔ workspace membership table with a role per workspace. Only the owner uses the system for now; `users` and `external_identities` let other people be added later, when the owner opens the next stage of the product. Superseded: "one user owns one workspace". |
| D5 | Hosting and remote access | Decided | Host on the VPS, with a public domain and port for HTTPS (reverse proxy + TLS). Webhooks are public; administration uses the CLI over SSH. |
| D6 | Real-service acceptance | Decided | Use the real ClickUp tasks assigned to the owner (no test workspace needed). The owner's primary use cases are listed in §7. |

## 6. Carry-over items already known

- **From the Phase 2 review fixes:**
  - git network traffic is not behind netguard (P12);
  - no OS disk quota (P12);
  - the batch budget can hide older comments from the AI, and the report says
    so (revisit in P7);
  - a stale `origin/HEAD` may mis-tier a push (P8, when workspaces are
    rebuilt).
- **Telegram-specific outputs:** Phase 2 reports are one-way. P6 adds replies,
  and P9 adds approvals.
- **Ownership:** `docs/architecture.md` stays as background. When a phase changes
  the architecture, that phase updates the doc (NFR-034).

## 7. Owner's primary use cases (2026-11-12)

These drive priority. Every phase plan should move at least one of them
forward.

1. **ClickUp task assistant.** For a task assigned to the owner, the agent:
   - verifies the user story: gaps, contradictions, missing acceptance
     criteria;
   - drafts a tech spec when asked;
   - lists open questions, split into "the owner can decide", "needs the
     PO/OP" and "needs someone else", with who to ask and how;
   - reports to Telegram.
2. **ClickUp status actions on request.** When the owner asks, Nexus-Hub moves
   the task status (for example from "tech spec review" to "development") and
   removes an assignee. These are ClickUp writes, so they go through the
   policy and an explicit owner confirmation.
3. **Code → PR → external test → notification.** A worker implements, then
   pushes a branch and opens a PR. When all the code for the task is done,
   Nexus-Hub itself calls the ClickUp `@AI` agent to test it (a comment on the
   task mentioning `@AI`, which is a ClickUp write under the policy). CI checks
   count too.
   Nexus-Hub picks up the result (a ClickUp comment or GitHub checks) and
   notifies the owner on Telegram.

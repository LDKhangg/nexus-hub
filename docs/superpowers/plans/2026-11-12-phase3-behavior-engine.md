# Phase 3: Behavior engine with graded autonomy

- Date: 2026-11-12. Status: **draft v3 (personalization added), waiting for the owner's approval**.
- Roadmap: `docs/product/ROADMAP.md` Phase 3. Contract: `docs/product/PCC-001.md`.
- Branch (when approved): `feature/phase3` from `main`.

## Goal

The hub stays generic. What a user wants done on an event is a **behavior**:
a versioned definition stored as data (YAML/JSON through the CLI), not code.
A behavior says:

- **when** it runs (trigger);
- **what** it may read (read tools);
- **what** to do (the user's own instructions);
- **how much** it may decide alone (autonomy rules);
- **where** it may write (allowed actions).

The engine runs a behavior and sorts every result item into one of three
classes:

- `auto`: decided alone, with evidence, and acted on;
- `notify`: sent as information;
- `ask`: sent to the user with options; the user's answer is acted on.

Everything is traceable.

The owner's own use (review the company AI's tech spec PR together with the
US and OQ, answer what can be answered, write answers into the task
description) is **the first behavior file**, used as the acceptance example.
No code knows about tech specs, US or OQ.

Change from draft v1: the owner rejected a hard-coded spec-review feature
("only the hub is shared; this is my personal behavior").

## Owner decisions that shape the engine (2026-11-12)

| Topic | Decision | Where it lives |
|---|---|---|
| Sources | ClickUp task and linked GitHub PRs | behavior: read tools |
| Write target for answers | ClickUp task description | behavior: allowed actions (`clickup.description_block`) |
| Trigger | Automatic on a ClickUp event, and on demand from Telegram | behavior: triggers |
| Auto posting | Dry-run first | behavior: `dry_run: true` by default |
| Users | Only the owner (D4) | engine |
| Telegram inbound | Webhook with the secret-token header (D3) | engine |
| Admin | CLI only (D2) | engine |

## PCC coverage

| PCC ID | Covered here |
|---|---|
| FR-073–FR-078, FR-081, FR-082, FR-084–FR-086 | Behavior definitions: create/update a draft, validate the schema, publish immutable versions, enable/disable, triggers, notification and approval rules, YAML/JSON import/export, every run pinned to a version (partial: no full state machine) |
| FR-003, FR-012, FR-017, FR-018, FR-029 | Telegram inbound for the owner: normalized updates, secret-token check, update_id dedupe, fast acknowledgement |
| FR-037, FR-053, FR-055, FR-056 | Clarification as `ask`; structured output validated by a schema; budgets per run |
| FR-009, FR-113, FR-115, FR-116, FR-121–FR-127, FR-129, FR-130 | Policy before every write; approvals bound to run, item, action and resource; expiry; no duplicates; actor and time; revalidation before the write |
| FR-026, NFR-021–NFR-023, NFR-025, NFR-026 | Correlation ID end to end, audited writes, untrusted content cannot change rules or targets, structured logs |
| E2E-004 | Partial: approve from the phone |

## Requirements

**Behavior definitions**

- REQ-1: The CLI can:
  - `behavior import <file>` creates or updates a draft;
  - `validate` reports every schema error with its path;
  - `publish` freezes a new immutable version;
  - `enable` / `disable`;
  - `list`;
  - `export <name> [version]` gives back the same YAML.
  An invalid draft cannot be published. (FR-073–FR-077, FR-084, FR-086)
- REQ-2: A behavior holds:
  - `triggers`: event types with filters;
  - `reads`: Phase 2 read-tool names and size caps;
  - `instructions`: free text;
  - `autonomy`:
    - `confidence_threshold`;
    - `always_ask_topics`;
    - `require_citations`;
    - `notify_when_informational`;
  - `actions`: per class, from a closed list;
  - `dry_run`;
  - `ask_expiry_hours`;
  - budgets: tokens, items, steps.
  Unknown fields, unknown tools and unknown actions are validation errors.
  (FR-075, FR-078, FR-081, FR-082)
- REQ-3: Every run stores the behavior name and version it used. Publishing a
  new version never changes a run already started. (FR-085)

**Triggers and inbound**

- REQ-4: Trigger types in this phase:
  - `clickup.task_status_changed`, with filters `to_status`, `assignee`
    (`me` = the owner's ClickUp user) and `list`;
  - `clickup.task_assigned`;
  - `clickup.comment_added`;
  - `telegram.command`, the command named in the behavior, for example
    `/review <task>`.
  An event can start several behaviors; each starts at most one run per
  (behavior version, subject, subject revision). (FR-078, FR-018)
- REQ-5: `POST /webhook/telegram/:integrationID` accepts an update only with the right
  `X-Telegram-Bot-Api-Secret-Token` (constant time). Otherwise it returns 401
  and stores nothing. Updates from other chats than the owner's have no effect
  and get no reply. A repeated `update_id` has one effect. The handler answers
  within 2 s and the work runs from storage. (FR-017, FR-018, FR-029,
  NFR-015)
- REQ-6: Built-in Telegram commands: `/help` (also lists the enabled
  behaviors' commands), `/trace <run or subject> [item]`, `/undo <run>`. A bad
  argument gets a one-line Vietnamese error that says what to send.
- REQ-7: The CLI registers the Telegram webhook (public URL from settings;
  the secret is generated, stored encrypted and never printed) and shows its
  status.

**Running a behavior**

- REQ-8: A run collects context only through the behavior's `reads`, via the
  Phase 2 executor (policy, audit, caps). It gives each source fragment a
  stable locator, for example `clickup:task/abc#desc:L12` or
  `github:o/r#42:docs/x.md:L30`.
- REQ-9: The model gets the fixed engine prompt, the behavior's
  `instructions` and the sanitized context. It must return items that
  validate against one generic schema, with these fields:
  - `key`: stable;
  - `label`: free text, for example "OQ" or "finding";
  - `text`;
  - `proposal`: the answer or action text;
  - `citations`: a list of locator plus quote;
  - `topic`;
  - `confidence`;
  - `informational`: true or false.
  Invalid output is retried once, then the run fails with a Telegram notice.
  (FR-055, FR-056)
- REQ-10: The class is decided by the engine, not by the model. An item is
  `auto` only if all of these hold:
  - each citation quote appears verbatim at its locator (when
    `require_citations`);
  - its topic is not in `always_ask_topics`;
  - its confidence is ≥ `confidence_threshold`.
  An informational item with no proposal is `notify`. Everything else is
  `ask`. Confidence can only move an item towards `ask`. The rule that decided
  each class is stored.
- REQ-11: Context text cannot change the behavior, the class or the write
  target. Text that reads like instructions to the AI is flagged on the item.
  Such an item can never be `auto`. (NFR-022, NFR-023)

**Actions**

- REQ-12: The action library (closed list) for this phase:
  - `telegram.message`;
  - `clickup.comment`;
  - `clickup.description_block`: writes only inside a block with start and
    end markers that carry the behavior name;
  - `clickup.set_status`;
  - `clickup.unassign`.
  Each action is allowed per class in the behavior. Every ClickUp write goes
  through the Phase 2 tool policy and audit, and is idempotent per (run, item,
  action). (FR-113, FR-116, NFR-021)
- REQ-13: With `dry_run`, `auto` actions are sent to Telegram as drafts with
  a "Đăng" button, and nothing is written to ClickUp until it is pressed.
  Without `dry_run`, they run, and the owner gets one FYI per run.
- REQ-14: `ask` items go to Telegram with the text, the suggestion, the
  citations, and these buttons:
  - the suggested options;
  - "Bỏ qua";
  - "Trả lời khác", for a free-text reply.
  The chosen answer runs through the behavior's `ask` actions. The answer
  expires after `ask_expiry_hours`. A second press, or a press after expiry,
  has no effect. The decision, its actor and time are stored. (FR-121–FR-127,
  FR-129)
- REQ-15: Before any ClickUp write, the engine re-reads the target. If the
  target changed since the run read it, the write is skipped and the item is
  marked `conflict`, then the owner is told. For `description_block`, only the
  text outside the block counts as a change. `clickup.set_status` and
  `clickup.unassign` always need a confirmation that shows the exact change.
  (FR-115, FR-130)
- REQ-16: `description_block` never changes text outside its block. With
  missing, duplicated or broken markers, it does not write; it marks the item
  `conflict`. The previous description is stored, and `/undo <run>` restores
  it when nothing changed since.
- REQ-17: `notify` items arrive as one digest per run.

**Personalization**

- REQ-22: A user profile in the DB (CLI `profile set/show`) holds:
  - language and tone for every message the hub writes;
  - quiet hours, during which `notify` and FYI messages wait and `ask`
    messages still arrive;
  - digest mode: per run, or batched hourly or daily;
  - the default model and budgets.
  Every behavior uses the profile unless it overrides a field.
- REQ-23: **Decision memory.** Every answer the owner gives to an `ask`, and
  every `auto` answer the owner posts or undoes, is stored with:
  - its topic;
  - its question;
  - its answer;
  - its scope: the behavior, the ClickUp list, the repo.
  A read tool `memory_search` (allowed through `reads` like any other) returns
  matching past decisions with a locator (`memory:decision/<id>`), so they can
  be cited.
  - Over time, items the owner already decided become `auto`, with the
    earlier decision as evidence.
  - A past decision never overrides `always_ask_topics`.
  - `memory list/forget` in the CLI manages them.
- REQ-24: Behaviors can declare `vars` (for example status names, list IDs,
  headings) used as `{{vars.x}}` inside `triggers`, `instructions` and
  `actions`. `behavior import` can take a second file of values. The same
  behavior file can then be reused with different settings, or shared.
- REQ-25: Feedback buttons on every message from a behavior: "👍" and
  "👎 + lý do". The rating is stored on the item and shown in `/trace`. CLI
  `behavior stats <name>` shows, per version, the counts of
  auto/notify/ask, undos, conflicts and 👎. The owner can then tune the
  thresholds and instructions with data.

**Trace**

- REQ-18: `/trace` and the CLI `trace` show a run and its items. For the run:
  behavior and version, trigger, subject and revision, times, correlation ID.
  For each item: class, rule, confidence, status (drafted, done, asked,
  answered, expired, skipped, conflict, failed), citations and the action
  result. No secret appears. (FR-026, NFR-026)
- REQ-19: Logs, tool runs and outbound actions of a run carry its
  correlation ID. Logs never contain context, item or prompt text.

**Acceptance example and debt**

- REQ-20: `examples/behaviors/spec-review.yaml` (the owner's behavior) passes
  `validate`. With fakes, it shows the full loop:
  1. a status change, then a review;
  2. one `auto` item drafted, and posted on "Đăng" into the description block;
  3. one `ask` item answered from Telegram and written;
  4. one `notify` digest;
  5. a trace of all of it.
- REQ-21: Phase 2 test debt TD-1 to TD-6 is proven or fixed.

## Design

### Engine flow

```text
ClickUp webhook ┐                          ┌ auto ── dry_run? draft+[Đăng] : action, FYI
Telegram cmd    ┼► match behaviors ─► run ─┤
                ┘  (version pinned,        ├ notify ─ digest
                   dedupe per revision)    └ ask ──── buttons ─► answer ─► action
   run = reads (executor) → LLM(instructions, generic schema) → verify citations → classify (rules)
```

### Data (migration `00004`, then `make generate`)

| Table | Holds |
|---|---|
| `behaviors` | name, status (draft/enabled/disabled), draft document |
| `behavior_versions` | name, version, document (immutable), published_at |
| `behavior_runs` | behavior, version, trigger, subject, revision (unique per behavior version), status, correlation_id, usage, snapshot for undo |
| `run_items` | run, key, label, text, proposal, citations, topic, confidence, flags, class, rule, status |
| `approvals` | run/item/action binding, options, nonce, expires_at, decision, actor, decided_at |
| `telegram_updates` | update_id (unique), chat, kind, processed_at |

Settings: `owner_telegram_chat_id`, `owner_clickup_user_id`,
`public_base_url`. The Telegram webhook secret is a credential.

### Reuse from Phase 2

Leased durable jobs, the `outbound_actions` outbox, the executor (policy,
audit, idempotency), the ClickUp and GitHub read tools, the sanitizer, the
secret resolver, netguard and the LLM client. Later, the Phase 2 comment
triage can itself become a behavior; this phase does not move it.

## Tasks (one Implementer each, in order)

| Task | Scope | REQ |
|---|---|---|
| T1 | Migration 00004, sqlc, stores, settings | data |
| T2 | Behavior schema, validator, YAML/JSON import and export, versions, CLI | 1–3 |
| T3 | Telegram inbound (webhook, owner gate, dedupe, commands, callbacks), CLI webhook setup | 5–7 |
| T4 | Trigger matching from ClickUp events and Telegram commands, run creation and dedupe | 4 |
| T5 | Run executor: reads, locators, the LLM with generic schema, citation verifier, classifier, injection flag | 8–11 |
| T6 | Action library, dry-run drafts, approvals and ask flow, conflict checks, description block and undo, digest | 12–17 |
| T7 | Trace CLI and `/trace`, correlation IDs, `examples/behaviors/spec-review.yaml` | 18–20 |
| T8 | Profile, decision memory and `memory_search`, behavior `vars`, feedback and `behavior stats` | 22–25 |

## Global constraints

- **Phase 2:** all Phase 2 Global Constraints still hold.
- **Generic code:** engine code holds no behavior-specific words or logic
  (no "spec", "OQ", "US", status names or headings). Those live only in
  behavior files.
- **Untrusted content:** behavior files come from the owner through the CLI
  and are trusted configuration. Context (task, comments, PRs) is untrusted
  and never chooses tools, classes or targets.
- **Writes:** writes happen only through the action library, the policy and
  the audit.
- **Telegram text:** short Vietnamese. Never secrets or raw provider errors.
- **Logs:** IDs, codes and counts only.
- **Migrations:** new migrations only; never hand-edit generated sqlc code.

## Review focus

1. Is any behavior-specific logic hidden in the engine?
2. Can context content push an item to `auto`, or change an action or a
   target?
3. Can a write happen twice, after expiry, after rejection, or over a newer
   edit?
4. Can the description block writer damage text outside its block?
5. Can a chat other than the owner's trigger anything?
6. Is every run reproducible from its pinned version, and is the trace free
   of secrets?

## Out of scope

- Multi-user (D4), Admin UI (D2), running project tests (D1).
- GitHub writes; the worker → PR → `@AI` flow.
- A full workflow state machine (Phase 5).
- Foundation items (Phase F).

# Phase 3: Tech-spec review assistant with graded autonomy

- Date: 2026-11-12. Status: **draft, waiting for the owner's approval**.
- Roadmap: `docs/product/ROADMAP.md` Phase 3. Contract: `docs/product/PCC-001.md`.
- Branch (when approved): `feature/phase3` from `main`.

## Goal

The company's AI already writes the tech spec for a task. For a ClickUp task
assigned to the owner, Nexus-Hub reads that spec (in a GitHub PR and/or in
ClickUp), the user story (US) and the open questions (OQ). It reviews them and
sorts every item into one of three classes:

- **auto**: Nexus can answer it from evidence (US, spec, code, earlier
  answers), with citations. The answer is written into the task description.
- **notify**: information only. The owner gets it in a digest; nothing is
  waiting for them.
- **ask**: needs the owner's opinion. It goes to Telegram with options; the
  owner's answer is written into the task description.

Every item can be traced to its sources, the rule that classified it, and what
was posted. Auto answers start in dry-run: drafts go to Telegram until the
owner turns real posting on.

## Owner decisions (2026-11-12)

| Topic | Decision |
|---|---|
| Where the spec is | Both: spec file(s) in a GitHub PR; US, OQ and discussion in ClickUp |
| Where answers go | Into the ClickUp task **description**, via the ClickUp API key |
| Trigger | Both: automatic (task assigned to the owner reaches a configured status, or its spec PR changes) and the Telegram command `/review <task>` |
| Auto posting | Dry-run first; a setting turns real posting on |
| Users | Only the owner (D4) |
| Telegram inbound | Webhook with the secret-token header (D3) |
| Admin | CLI only (D2) |

Assumptions to confirm at approval:

- A1. "Description" means the ClickUp task description, not the PR
  description.
- A2. The OQ list lives in the task description under a heading containing
  "Open Question" or "OQ" (any level), one question per bullet or numbered
  line; if there is no such heading, OQs in the spec file are used.
- A3. Linked spec PRs are the GitHub PR URLs in the task description, its
  comments, or its custom fields, in repos already granted in Phase 2.

## PCC coverage

| PCC ID | Covered here | Note |
|---|---|---|
| FR-003, FR-012, FR-017, FR-018 | Telegram inbound: owner identity by chat ID, normalized updates, secret-token verification, update_id dedupe | multi-user identity stays for later (D4) |
| FR-009, FR-113, FR-115, FR-116 | ClickUp writes go through policy; status change and unassign need approval | partial |
| FR-121–FR-127, FR-129, FR-130 | `ask` items and action confirmations: bound, delivered, approve/reject, expiry, no duplicates, actor and time recorded, revalidated before writing | partial (no workflow resume engine) |
| FR-026, NFR-025, NFR-026 | One correlation ID from the trigger to every item, write and log line | |
| FR-037, FR-053 | Clarification: unclear items become `ask` | |
| FR-055, FR-056 | Structured LLM output validated against a schema; token and item budgets | |
| NFR-015, NFR-021, NFR-022, NFR-023 | Webhook authenticity; audit of writes; spec/US/comments treated as untrusted; content cannot change rules | |
| E2E-004 | Partial: approving an action from the phone | full E2E later |

## Requirements

Each REQ is an observable behaviour.

**Telegram inbound**

- REQ-1: `POST /webhooks/telegram` accepts an update only when the
  `X-Telegram-Bot-Api-Secret-Token` header equals the stored secret (constant
  time). Otherwise it returns 401 and stores nothing. (FR-017, NFR-015)
- REQ-2: Updates from any chat other than the configured owner chat ID get no
  reply and no effect; only a counter and the chat ID are logged. (FR-003,
  FR-009)
- REQ-3: The same `update_id` delivered twice has an effect once. The
  webhook answers within 2 s, and work runs after the update is stored.
  (FR-018, FR-029)
- REQ-4: Commands: `/help`, `/review <task id or URL>`, `/trace <task id or
  URL> [item]`, `/status <task> <status>`, `/unassign <task> [user]`. A bad
  argument gets a one-line Vietnamese error that says what to send.
- REQ-5: A CLI command registers the webhook with Telegram (public URL from
  settings, secret generated and stored encrypted, never printed) and shows
  Telegram's webhook status.

**Trigger and sources**

- REQ-6: A review starts automatically when a ClickUp task assigned to the
  owner's ClickUp user moves into one of the configured trigger statuses, or
  when the head SHA of its linked spec PR changes while it is in such a status.
  `/review` starts one on demand.
- REQ-7: One review per (task, spec revision). The revision is the linked PR
  head SHAs plus a hash of the task description without the Nexus-managed
  block. Repeated triggers on the same revision do nothing; `/review` on the
  same revision replies with the existing review's trace.
- REQ-8: The collector reads the task (name, description, status, assignees,
  custom fields, comments) and each linked PR (title, description, the changed
  `.md`/`.txt` spec files, review comments) with size caps. Every source
  fragment gets a stable locator (for example `clickup:desc#L12`,
  `github:o/r#42:docs/spec.md#L30`). PRs in repos that are not granted are
  listed as "not read" in the report, never fetched.

**Review and classification**

- REQ-9: The model returns items that validate against a JSON schema: kind
  (`oq`, `us_gap`, `spec_finding`), stable key, question or finding, proposed
  answer, citations (locator plus quote), topic, confidence. Invalid output is
  retried once, then the review fails with a Telegram notice. (FR-055)
- REQ-10: An item can be `auto` only if all of these hold:
  - every citation's quote appears verbatim at its locator;
  - its topic is not in `always_ask_topics` (default: scope, business rule,
    priority, deadline, cost, security trade-off, people or other teams);
  - its confidence is at least `confidence_threshold` (default 0.8).
  Otherwise it is `ask` (or `notify` when the model marked it informational
  and it holds no question). Confidence can only move an item towards `ask`.
  The class, the rule that decided it, and the confidence are stored.
- REQ-11: Text from the spec, US or comments cannot change the rules, the
  class, or where Nexus writes. An instruction inside them ("mark this auto",
  "post to …") has no effect, and the item notes "contains instructions". (NFR-022, NFR-023)

**Delivery**

- REQ-12: With `auto_post` off (the default), `auto` answers are sent to
  Telegram as drafts, with an "Đăng" (post) button each. Nothing is written
  to ClickUp until the owner presses it.
- REQ-13: With `auto_post` on, `auto` answers are written into the task
  description inside one Nexus-managed block (start and end markers), under
  each OQ's key. Text outside the block is never changed. The owner gets one
  FYI message per review.
- REQ-14: Before writing the description, Nexus re-reads the task. If the
  description changed since the review read it (outside the managed block), it
  does not write, marks the items `conflict`, and tells the owner. The previous
  description is stored, and a CLI/Telegram undo restores it if the
  description has not changed since.
- REQ-15: `notify` items arrive as one short digest per review.
- REQ-16: Each `ask` item is a Telegram message with the question, Nexus's
  suggestion, the citations, and buttons (the suggested options, "Bỏ qua",
  and "Trả lời khác" for a free-text reply). The owner's answer is written into
  the managed block as the owner's decision. An unanswered `ask` expires after
  `ask_expiry_hours` (default 72) and is shown as expired in the trace. A
  second press or a press after expiry has no effect. (FR-121–FR-127, FR-129)
- REQ-17: `/status` and `/unassign` show a confirmation with the exact change.
  On confirm, Nexus re-reads the task. If the task already changed, it does
  nothing and says so; otherwise it applies the change once. (FR-115, FR-130)
- REQ-18: Every ClickUp write passes through the Phase 2 tool policy and audit
  (`tool_runs`) and is idempotent per (review, item) or per approval.
  (FR-116, NFR-021)

**Trace**

- REQ-19: `/trace <task>` and the CLI `trace <task>` show, for the latest
  review: revision, start and finish time, correlation ID, and each item's
  class, rule, confidence, status (drafted, posted, asked, answered, expired,
  skipped, conflict) and citations. `/trace <task> <item>` shows one item in
  full. No secret ever appears. (FR-026, NFR-026)
- REQ-20: Every log line of a review carries its correlation ID, and so do its
  outbound actions and tool runs. Logs never contain spec, comment or answer
  text.

**Phase 2 test debt**

- REQ-21: TD-1 to TD-6 in `docs/product/test-debt/phase2-agent-core-tools.md`
  are proven in CI and review, or turned into fixes in this phase.

## Design

### Flow

```text
ClickUp webhook ─┐                     ┌─ auto ── dry-run: Telegram draft + [Đăng]
(status/assignee)├─► spec_reviews job ─┤          posting on: managed block in description
Telegram /review ┘   (lease, dedupe)   ├─ notify ─ Telegram digest
                     collect → LLM →   └─ ask ──── Telegram buttons → owner answer → managed block
                     verify citations
                     → rules classify
```

### Data (migration `00004`, then `make generate`)

- `telegram_updates`: update_id (unique), chat_id, kind, received_at,
  processed_at. No message text is stored beyond the command and its
  arguments.
- `spec_reviews`: id, workspace, task_id, revision (unique with task_id),
  status, correlation_id, trigger, started_at, finished_at, error_code,
  description_before (for undo), usage.
- `review_items`: id, review_id, item_key (unique with review_id), kind,
  question, answer, class, rule, confidence, citations jsonb, topic, status,
  posted_text, telegram_message_id, decided_by, decided_at.
- `approvals`: id, kind (`item_answer`, `post_draft`, `status_change`,
  `unassign`), item_id or action payload, task_id, expires_at, decision,
  decided_at, nonce (in the button callback data), unique open approval per
  target.
- New settings under `spec_review.*`: `enabled`, `trigger_statuses`,
  `owner_clickup_user_id`, `owner_telegram_chat_id`, `auto_post` (false),
  `confidence_threshold` (0.8), `always_ask_topics`, `ask_expiry_hours`
  (72), `max_items` (30), `public_base_url`. The Telegram webhook secret is a
  credential.

### Reuse from Phase 2

Leased durable jobs and the claim pattern, the `outbound_actions` outbox, the
tool executor (policy, audit, idempotency), `clickup_get_task`, the GitHub read
tools, the sanitizer (`CleanInput`, `RedactSecrets`, mention neutralization),
the secret resolver, and netguard.

## Tasks (one Implementer each, in order)

| Task | Scope | REQ |
|---|---|---|
| T1 | Migration 00004, sqlc queries, settings keys, store interfaces | data for all |
| T2 | Telegram inbound: webhook handler, owner gate, dedupe, command parser, callback handling, CLI `telegram webhook set/status` | 1–5 |
| T3 | Triggers: ClickUp status/assignee and PR-head change → enqueue review; `/review`; revision dedupe | 6, 7 |
| T4 | Source collector: ClickUp task and comments, linked PR spec files, locators, caps, OQ section extraction | 8 |
| T5 | Reviewer: prompt, JSON schema validation, citation verifier, rule classifier, injection marking | 9–11 |
| T6 | Delivery: drafts and the post button, managed-block writer with conflict check and undo, digest, ask buttons, free-text answers, expiry | 12–16, 18 |
| T7 | ClickUp status and unassign tools with confirmation; trace CLI and `/trace`; correlation ID through logs, outbox and tool runs | 17–20 |

REQ-21 (test debt) runs through the Tester and the Reviewer from the first
Phase Brief.

## Global constraints

- All Phase 2 Global Constraints still hold: secrets, network allowlist, no
  new binaries, tool policy, DB-first configuration, two env variables.
- Spec, US, comments and PR text are untrusted input: they are sanitized and
  redacted before the LLM, never used as instructions, and never decide where
  Nexus writes.
- Nexus only writes to ClickUp: the managed block in the description, task
  status and assignees on approval. No other field, and nothing on GitHub in
  this phase.
- Telegram text is short Vietnamese, with no secret, token or raw provider
  error.
- Logs carry IDs, codes and counts only: never spec, comment, answer or
  prompt text.
- New migrations only; generated sqlc code is never edited by hand.

## Review focus

1. Can a malicious spec or comment push an item to `auto` or change what gets
   written?
2. Can a write happen twice, after expiry, after rejection, or over a
   description someone else just edited?
3. Can anyone other than the owner's chat trigger anything?
4. Can the managed-block writer damage text outside the block (markers
   missing, duplicated, nested, edited by a person)?
5. Is every item traceable, and is the trace free of secrets?

## Out of scope

Writing the tech spec; running project tests (D1); multi-user identity (D4);
Admin UI (D2); writing to GitHub; the worker → PR → `@AI` flow (next phase);
the foundation items (Phase F).

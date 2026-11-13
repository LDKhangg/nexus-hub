# Phase 3d: ClickUp Chat DM → auto-approve the GitHub PR → ✅ reaction

- Date: 2026-11-13. Status: approved by the owner in chat ("đúng rồi có thể
  approve pr của ai dm cũng được"), with D4.
- Roadmap: `docs/product/ROADMAP.md` (pulled forward from Phases 4, 9 and 10
  for one owner use case). Contract: `docs/product/PCC-001.md`.
- Branch (when approved): `feature/phase3d` from `main`. Independent of
  Phase 3c.
- PCC IDs: FR-012, FR-013, FR-018, FR-024, FR-025, FR-027, FR-029, FR-082,
  FR-090, FR-133, FR-135, FR-137, NFR-001, NFR-004.

## Goal

Someone sends the owner a ClickUp Chat direct message that contains a GitHub
pull request link and asks for an approval. Without asking the owner, the hub:

1. approves that PR on GitHub with the owner's GitHub account;
2. adds a ✅ reaction to the DM in ClickUp with the owner's ClickUp account;
3. tells the owner on Telegram what it did.

The hub stays generic: the engine gains a Chat source, one trigger type and
two action types. The owner's flow is a behavior file
(`examples/behaviors/pr-auto-approve.yaml`), not code.

## Owner decisions recorded (2026-11-13, chat)

- D1: **No confirmation step.** Approving and reacting run in the `auto`
  path with no Telegram button ("tự approve luôn").
- D2: **No safety rule on content.** The owner accepts that anyone who can DM
  them in ClickUp can get a PR approved ("điểm 4 skip đi"). The engine does not
  require green CI, a sender allowlist or a size limit. The behavior file may
  still add such filters later; they are optional, off by default.
- D4: Approve the PR from **any** DM sender on **any** repo the owner's GitHub
  account can access; no per-repo registration ("approve pr của ai dm cũng
  được").
- D3: Only **direct messages** to the owner (`DM`, and `GROUP_DM` only if the
  behavior asks for it). Channel messages are out of scope.

## Facts checked by the Lead (ClickUp API v3 docs, 2026-11-13)

- ClickUp webhooks have **no Chat events** (the event list is task, list,
  folder, space, goal and key result events only). Chat must be **polled**.
- `GET /api/v3/workspaces/{ws}/chat/channels?channel_types=DM&with_message_since=<ms>`
  lists DMs with new messages (cursor paging).
- `GET /api/v3/workspaces/{ws}/chat/channels/{channel_id}/messages` returns
  messages with `id`, `user_id`, `content` (text/md), `date` (ms),
  `parent_channel`, `type` (`message`|`post`), cursor paging.
- `POST /api/v3/workspaces/{ws}/chat/messages/{message_id}/reactions`
  body `{"reaction":"<lower-case emoji name>"}` → 201; 400 when the reaction
  is unsupported **or already exists**; 404 when the message is gone.
- GitHub: `POST /repos/{owner}/{repo}/pulls/{n}/reviews` with
  `{"event":"APPROVE"}` approves; GitHub refuses (422) to let an author
  approve their own PR.
- To probe live before T4 is merged: the exact emoji name ClickUp accepts for
  ✅ (`white_check_mark` expected). The default is a var, so a wrong guess is a
  values-file change.

## Requirements

### Chat source

- REQ-1: A ClickUp Chat poller runs inside the server for each workspace that
  has a ClickUp source with a `clickup_token` credential and at least one
  enabled behavior with a `clickup.chat_message` trigger. It polls every
  `clickup_chat_poll_interval` (setting, default `30s`, min `10s`, max `10m`).
  With no such behavior it makes no ClickUp call.
- REQ-2: The poller lists channels of the types the triggers ask for (default
  `DM`) with messages since its stored cursor, then reads their new messages.
  It stores, per workspace, the cursor (the newest message `date` it fully
  processed) in the database (new migration). On the very first poll it sets
  the cursor to "now" and does not process older messages.
- REQ-3: Each new message becomes one normalized event
  `clickup.chat_message` with key `clickup:chat:<message_id>`. The event key
  is deduplicated in the existing inbox so that a message is handled at most
  once, across polls and restarts (FR-018, FR-025, NFR-001). A message whose
  `user_id` is the owner's own ClickUp user (`owner_clickup_user_id`) is
  dropped. Message types other than `message` are dropped.
- REQ-4: The poller extracts GitHub PR URLs from the message content
  **deterministically** (regex
  `https://github.com/<owner>/<repo>/pull/<n>`, owner/repo per GitHub's name
  rules, n > 0). Trusted trigger data holds: message id, channel id, channel
  type, sender user id, and the PR references (repo, number), at most 5,
  deduplicated, in order. The message text is untrusted content: it may go to
  the model as context, never into trigger data.
- REQ-5: Poll errors are logged as codes only (`chat_unauthorized`,
  `chat_rate_limited`, `chat_unavailable`), back off (doubling, capped at
  10 minutes), never advance the cursor past an unprocessed message, and never
  log message content, tokens or headers. Three consecutive
  `chat_unauthorized` results send one Telegram notice to the owner.

### Trigger

- REQ-6: New trigger type `clickup.chat_message` with filters:
  - `channel`: `dm` (default) or `group_dm`, or both as a list;
  - `from`: `others` (default) or `any` (the owner is still dropped, REQ-3);
  - `has_pr_link`: `true` → matches only when trigger data has at least one
    PR reference;
  - `senders`: optional list of ClickUp user IDs; empty means anyone (D2).
  Validation rejects unknown filter fields for this type. The run subject is
  `clickup:chat:<message_id>`.

### Run-level actions

- REQ-7: A behavior may declare `actions.run`: actions that run **once per
  run**, after the model step, on trusted trigger data only. They do not depend
  on items, classes or confidence. Only these action types are allowed in
  `actions.run`: `github.pr_approve`, `clickup.chat_reaction`,
  `telegram.message`. The model's output cannot add, remove or change the
  target of a run-level action. A behavior whose trigger is
  `clickup.chat_message` may still have items (for example a PR summary for
  the Telegram notice); it may also have none.
- REQ-8: Run-level actions run in the declared order. An action with
  `on: success_of_previous` runs only if the action before it ended OK.
  `telegram.message` in `actions.run` always runs last and reports every
  outcome.
- REQ-9: `dry_run: true` runs no write: each write action is recorded as
  `dry_run` with what it would do, and the Telegram message says "would
  approve … / would react …".

### Actions

- REQ-10: `github.pr_approve` approves each PR reference in trigger data with
  the owner's own GitHub identity, **on any repo that identity can access**
  (D4); no `nh github-repo` registration is needed:
  - the action param `account` names a hub GitHub account (`nh
    github-account`) of type `pat`; validation rejects a missing account or a
    GitHub App account (an App would approve as a bot, not as the owner);
  - if the token cannot see the repo or PR → `pr_not_found`, no retry;
  - before approving it reads the PR: if it is closed or merged → `pr_closed`;
    if the owner's latest review on it is already `APPROVED` → `already_approved`
    (counts as OK, no new review);
  - it posts `{"event":"APPROVE"}` with an empty body (no comment text);
  - 422 for the author's own PR → `own_pr`; 404 → `pr_not_found`; 401/403 →
    `github_unauthorized`; other errors map to the existing tool codes;
  - the outcome for each PR records repo, number and the review id; nothing
    from the PR body or the DM.
  The action is OK when every PR ended OK or `already_approved`.
- REQ-11: `clickup.chat_reaction` adds the reaction named by its `reaction`
  param (default from the behavior var, e.g. `white_check_mark`) to the
  triggering message with the owner's ClickUp token. A 400 whose reason is
  "already exists" counts as OK. 404 → `message_not_found`. With
  `on: success_of_previous` after `github.pr_approve`, no reaction is added
  when the approval did not succeed.
- REQ-12: Both actions are idempotent per run: a retried run does not create
  a second review or a second reaction (scope key per run and action index, as
  for existing writes), and a crash between the API call and recording the
  outcome is resolved by the reads in REQ-10/REQ-11 on retry.
- REQ-13: The Telegram message to the owner states, in the profile language:
  the sender (ClickUp user id or name when the poller knows it), each PR
  (`owner/repo#n`) and its outcome, and whether ✅ was added. It never quotes the
  DM text.

### Operability

- REQ-14: `nh runs` and `nh trace` show chat-triggered runs with subject,
  trigger kind, the PR references and each run-level outcome. `/undo` on such
  a run removes the reaction and **dismisses** nothing on GitHub: it replies
  that a GitHub approval must be dismissed by hand, with the PR link.
- REQ-15: `examples/behaviors/pr-auto-approve.yaml` and a values example
  implement the owner's flow: trigger `clickup.chat_message`
  (`channel: dm`, `has_pr_link: true`), `actions.run`:
  `github.pr_approve` → `clickup.chat_reaction` (`on: success_of_previous`) →
  `telegram.message`; `dry_run: true` in the example. The runbook gains a
  section: add a `pat` GitHub account for the owner (classic PAT with `repo`,
  or a fine-grained PAT with Pull requests: write on the repos), set the poll
  interval, probe the
  reaction name, then switch `dry_run` off.

## Tasks

1. **T1 Migration + store**: chat poll cursor per workspace;
   `make generate`.
2. **T2 ClickUp Chat client**: list DM channels since, list messages, add
   reaction (v3), with error code mapping (REQ-5, REQ-11). No logging of
   content.
3. **T3 Poller**: scheduling, cursor, owner drop, PR extraction, normalized
   event into the inbox with dedup (REQ-1–REQ-5).
4. **T4 Trigger + schema**: `clickup.chat_message`, filters, validation,
   subject, `actions.run` schema and validation (REQ-6, REQ-7).
5. **T5 Run-level action executor**: order, `on: success_of_previous`,
   dry run, idempotency, Telegram summary (REQ-8, REQ-9, REQ-12, REQ-13).
6. **T6 `github.pr_approve` tool** (REQ-10) and **`clickup.chat_reaction`
   tool** (REQ-11).
7. **T7 Trace/undo + CLI** (REQ-14), example behavior + runbook (REQ-15).

## Interfaces (sketch)

```go
// internal/adapters/sources/clickupchat
type Client interface {
    DMChannelsSince(ctx, ws, token string, types []string, sinceMS int64, cursor string) (Page[Channel], error)
    Messages(ctx, ws, token, channelID, cursor string) (Page[Message], error)
    AddReaction(ctx, ws, token, messageID, reaction string) error
}

// internal/behavior
const (
    TriggerChatMessage   = "clickup.chat_message"
    ActionPRApprove      = "github.pr_approve"
    ActionChatReaction   = "clickup.chat_reaction"
    SubjectChatPrefix    = "clickup:chat:"
)
type PRRef struct { Repo string; Number int }
// TriggerData gains: MessageID, ChannelID, ChannelType, SenderID string; PRs []PRRef
```

## Global constraints

- All repo constraints in `AGENTS.md` (no logging of tokens, headers,
  prompts, tool I/O or payloads; tests never call real ClickUp/GitHub/
  Telegram/Anthropic; never edit existing migrations).
- Targets of write actions come only from trusted trigger data (REQ-4,
  REQ-7). The model never chooses which PR is approved or which message gets
  the reaction.
- Existing behaviors and triggers keep working unchanged; `actions.run` is
  optional.
- No new binary is executed.

## Review focus

- Exactly-once handling of a DM across polls, restarts and retries (no double
  approval, no missed message after an error).
- The model cannot steer the approval target; DM text never reaches logs,
  trigger data or Telegram.
- Error mapping and backoff when the ClickUp token is wrong or rate limited.
- `dry_run` really writes nothing.

## Out of scope

- Telegram/Slack/Zalo personal messages (needs a userbot; not in PCC).
- Merging the PR, commenting on it, or checking CI (possible later as
  optional filters).
- Channel (non-DM) messages, replies in threads, edited messages.

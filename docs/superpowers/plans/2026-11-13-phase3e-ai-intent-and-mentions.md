# Phase 3e: AI intent gate for run actions, and "someone tagged me" reports

- Date: 2026-11-13. Status: **draft, waiting for the owner's approval**.
- Depends on: Phase 3c (an LLM provider the owner can use without an API key:
  Claude Code CLI or opencode with OpenAI models).
- Branch (when approved): `feature/phase3e` from the Phase 3c head.
- Roadmap: pulled forward from Phases 4 and 7 for the owner's daily use.
  Contract: `docs/product/PCC-001.md`.
- PCC IDs: FR-012, FR-018, FR-024, FR-025, FR-027, FR-029, FR-052, FR-081,
  FR-133, FR-135, NFR-001, NFR-004.
- Open debt carried: Phase 2, Phase 3 and Phase 3d test debt files; TD3d-4 is
  fixed here (REQ-E9).

## Goal

1. **Approve only when the DM really asks for it.** Phase 3d approves every
   DM from someone else that contains a PR link: "PR này đang lỗi, đừng
   approve nhé <link>" would be approved. Now a model reads the DM first and
   the approval runs only when it says the message asks the owner to approve
   that PR. Anything else becomes a short Telegram note.
2. **"Someone tagged me" reports.** When someone tags the owner in a ClickUp
   task comment or in a ClickUp Chat channel message, the owner gets a short
   Telegram report: who, where (task or channel, with a link), and what they
   want, summarised by the model.

Both are behavior files; the engine gains generic pieces only: a model
verdict that can gate run-level actions, a `mentions: me` filter, and channel
polling.

## Owner decisions recorded (2026-11-13, chat)

- E-D1: Phase 3c and 3e are both wanted; the model runs through the Claude
  Code CLI or opencode with an OpenAI model ("cần làm 3c và 3e dùng claude
  cli hoặc opencode nhưng mà model của openAI").
- E-D2: the Phase 3d approval stays automatic (no button), but must be decided
  by AI reading the message ("mốt vẫn cần AI để biết coi này nhờ approve hay
  gì").
- 3d decisions D1–D5 still hold.

## Requirements

### A. Model verdict gating run-level actions

- REQ-E1: A behavior may declare `verdict`: a closed list of labels (2..8
  short snake_case words, for example `approve_request`, `other`) and a
  `min_confidence` (default 0.8). When it does, the model step returns, in
  addition to items, one structured verdict `{label, confidence, summary}`.
  `summary` is at most 280 characters, in the profile language.
- REQ-E2: A run-level action may declare `when: {verdict: <label>}`. It runs
  only when the run's verdict has that label **and** its confidence is at
  least `min_confidence`. Otherwise it is recorded as `skipped_verdict` with
  the label and confidence. An action without `when` runs as in Phase 3d.
  `on: success_of_previous` still applies after `when`.
- REQ-E3: The verdict can only **narrow** what runs. Targets (PR refs,
  message id, team id) still come only from trusted trigger data (3d
  REQ-4/REQ-7). The model cannot name a PR, add one, or change the
  reaction. A model failure, a timeout, a schema error or a missing verdict
  counts as "no verdict": every action with `when` is skipped and the owner
  is told (`verdict_unavailable`).
- REQ-E4: The prompt-injection classifier that runs on untrusted text must not
  turn an ordinary request ("approve giúp em") into a refusal of the verdict
  step: the DM is passed as quoted untrusted content, the verdict is still
  produced, and the classifier's flags are recorded on the run (trace)
  instead of blocking it. A flagged message never gets confidence above the
  flag's cap (0.5 by default), so it cannot pass a gate at the default
  `min_confidence`.
- REQ-E5: The Telegram report (3d REQ-13) adds one line with the verdict label,
  confidence and `summary` (redacted, mentions neutralised). It still never
  quotes the DM verbatim. When the gate skipped the approval, the report says
  so: "Không approve: AI đánh giá tin này không phải nhờ approve (other,
  0.92) – <summary>".
- REQ-E6: `examples/behaviors/pr-auto-approve.yaml` gains `instructions`
  (classify the DM: is the sender asking the owner to approve the linked
  PR?), `verdict: {labels: [approve_request, other], min_confidence: 0.8}`,
  and `when: {verdict: approve_request}` on `github.pr_approve`. It stays
  `dry_run: true` in the example.

### B. Mentions

- REQ-E7: Trigger `clickup.comment_added` gains the filter `mentions: me`. It
  matches only when the comment tags the owner (`owner_clickup_user_id`). The
  tagged user ids are read from the webhook payload into trusted trigger data
  (parser change; the exact payload field is probed live first, see Facts to
  check). Comments by the owner never match.
- REQ-E8: Trigger `clickup.chat_message` gains `channel: channel` (ClickUp Chat
  channels, not only DMs) and the filter `mentions: me`. A channel message
  matches only with `mentions: me` (validation refuses a channel trigger
  without it, so the hub never processes every channel message). The poller
  queries `CHANNEL` only when such a trigger exists, reads tagged users of
  each new channel message (from the message or its `tagged_users` link, as
  the probe shows), and puts the channel id and name and whether the owner is
  tagged into trusted trigger data. The cursor, dedup and error rules of 3d
  REQ-2..REQ-5 apply unchanged.
- REQ-E9 (TD3d-4): A ClickUp source that is only polled can be enabled from
  the CLI: `nh source enable --source-id <id>` (and `disable`). It refuses
  when the source has no team id. Webhooks on a source without a webhook
  secret stay refused (fail closed), as today.
- REQ-E10: `examples/behaviors/mention-report.yaml`: triggers
  `clickup.comment_added` with `mentions: me` and `clickup.chat_message` with
  `channel: channel`, `mentions: me`; reads `clickup_get_task` (for task
  comments) and the triggering message text as quoted untrusted context; the
  model writes one `notify` item: who, where, what they ask the owner, and a
  suggested next step (FR-052). Actions: `telegram.message` only (no ClickUp
  write). The Telegram message links the task or channel.
- REQ-E11: Runbook: register the ClickUp webhook for `taskCommentPosted` on
  the owner's team through the tunnel (Cloudflare quick tunnel or Tailscale
  Funnel), enable the source (REQ-E9), import the two behaviors, check
  `/trace` from the owner's Telegram group.

## Facts to check (Lead probes before the tasks that need them)

- ClickUp `taskCommentPosted` payload: where tagged users appear (comment
  parts of type `tag` with `user.id`, or another field).
- ClickUp Chat v3 channel messages: whether the message body carries tag
  markup with user ids, or only the `links.tagged_users` URL (one extra call
  per message).
- The 3c provider's latency and token cost for one verdict call
  (`nh llm check`).

## Tasks

| Task | Scope | REQ |
|---|---|---|
| E1 | Schema + validation: `verdict`, `when`, `mentions: me`, `channel: channel` | E1, E2, E7, E8 (schema) |
| E2 | Verdict in the model step and the run-level executor gate; injection cap; Telegram lines | E1–E5 |
| E3 | ClickUp comment parser: tagged users; matcher for `mentions: me` | E7 |
| E4 | Poller: channels with mentions; tagged users lookup | E8 |
| E5 | `nh source enable/disable` | E9 |
| E6 | Example behaviors, README, runbook | E6, E10, E11 |

## Global constraints

- Phase 3d global constraints hold. The model never chooses write targets.
- No message, comment or prompt text in logs, trigger data or `run_actions`;
  the verdict `summary` is model output, redacted, and is the only text
  derived from the message that reaches Telegram.
- Existing behaviors without `verdict` behave exactly as before.
- Works with every provider (`anthropic`, `claude_cli`, `opencode_cli`).

## Review focus

- Can a crafted DM make the gate pass for a message that is not an approval
  request, beyond what the Phase 3d rule would already have approved?
  (It cannot widen: the gate only narrows.)
- Model failure paths: does every one of them skip the gated actions?
- Channel polling never processes a channel message that does not tag the
  owner.

## Out of scope

- Approving with a button (owner chose automatic).
- Replying in ClickUp on the owner's behalf.
- Mentions in docs, whiteboards or task descriptions.

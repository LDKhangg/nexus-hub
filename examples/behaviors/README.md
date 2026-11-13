# Behavior files

A behavior is a versioned definition, stored as data, that tells the engine
what to do on an event. Write it in YAML (`.yaml`/`.yml`) or JSON (`.json`)
and manage it with the CLI:

Report text follows the owner's report preferences and templates; it is not
fixed by the behavior file.

```sh
setup-notices behavior import my-behavior.yaml --vars my-values.yaml --workspace-id <uuid>
setup-notices behavior validate my-behavior --workspace-id <uuid>
setup-notices behavior publish my-behavior --workspace-id <uuid>
setup-notices behavior enable my-behavior --workspace-id <uuid>
setup-notices behavior list --workspace-id <uuid>
setup-notices behavior export my-behavior [--version N] --workspace-id <uuid>
```

`import` saves a draft (creating the behavior if it is new) and reports any
validation errors; without `--vars` it keeps the values already stored.
`publish` refuses an invalid draft and otherwise freezes it as the next
immutable version. Every run is pinned to one version, so publishing again
never changes a run already started. `export` prints the exact imported text:
the draft by default, or a published version.

Unknown fields, unknown trigger types, read tools and actions are errors.
Every error is reported with its path, for example
`actions.auto[1].type: unknown action "x"`.

## Fields

| Field | Required | Meaning |
|---|---|---|
| `schema` | yes | Always `1`. |
| `name` | yes | 1-63 of `a-z 0-9 _ -`. |
| `description` | no | Up to 500 characters. |
| `triggers` | yes | One or more events that start a run (below). |
| `reads` | no | `{tool, max_bytes?}` read tools the run may use. Read-tier tools of the hub plus `memory_search`. |
| `instructions` | yes | Your own instructions to the model, up to 20,000 characters. It may be left out only when every trigger is `clickup.chat_message` and the only actions are `actions.run`: then no model is called. |
| `autonomy` | no | `confidence_threshold` (0..1, default 0.8), `always_ask_topics` (list), `require_citations` (default true), `notify_when_informational` (default true). |
| `actions` | no | Per class (`auto`, `notify`, `ask`), a list of `{type, params}`; and `run`, the run-level actions (below). |
| `dry_run` | no | Default `true`: `auto` actions are sent as drafts to confirm first. |
| `ask_expiry_hours` | no | 1..720, default 72. |
| `budgets` | no | `max_tokens` (default: profile/settings), `max_items` (1..100, default 30), `max_steps` (1..20, default 8). |
| `model` | no | A model ID; default from the profile or settings. |
| `profile` | no | `language`, `tone`: override the profile for this behavior. |
| `vars` | no | `name: {description?, default?}` (below). |
| `verdict` | no | `labels` (2-8 snake_case words), `min_confidence` (0..1, default 0.8): the model also returns one verdict per run (below). Needs `instructions`. |

### Triggers

| `type` | Fields |
|---|---|
| `clickup.task_status_changed` | `filter.to_status` (list), `filter.assignee` (`me` or a ClickUp user ID), `filter.list` (list IDs) |
| `clickup.task_assigned` | `filter.assignee`, `filter.list` |
| `clickup.comment_added` | `filter.list`, `filter.from` (`any` or `others`), `filter.mentions` (`me`: only comments that tag you) |
| `telegram.command` | `command` (for example `/name`; not a built-in command), `args` (`[task]`: a ClickUp task ID or URL) |
| `clickup.chat_message` | `filter.channel` (`dm`, default, `group_dm` and/or `channel`), `filter.from` (`others`, default, or `any`; your own messages never match), `filter.has_pr_link` (`true`: only messages with a GitHub PR link), `filter.senders` (ClickUp user IDs; empty means anyone), `filter.mentions` (`me`: only messages that tag you) |

`clickup.chat_message` is found by polling ClickUp Chat (setting
`clickup_chat_poll_interval`, default `30s`) and needs the setting
`owner_clickup_user_id`. The run's subject is `clickup:chat:<message id>`.
The message text is never stored; the trigger keeps only the message,
channel and sender IDs and up to 5 PR links (`owner/repo#n`). When the
behavior has `instructions`, the engine fetches the message text for the
model step as untrusted, quoted context (source `clickup:chat/<id>#text`);
it is kept only in the run's context snapshot, never in the trigger, the
trace or the logs.

#### Mentions

`mentions: me` matches only a comment or chat message that tags you
(`owner_clickup_user_id`); your own comments and messages never match, and
nothing matches while `owner_clickup_user_id` is unset. Task comments read
the tags from the ClickUp webhook; chat messages read
`[@name](#user_mention#<id>)` tags from the message (group mentions do not
count).

`channel: channel` (ClickUp Chat channels, not DMs) requires
`mentions: me`, so an untagged channel message never starts a run. Channel
messages are found by the same poller as DMs; every channel with new
messages costs at least one ClickUp call per poll.

`mentions` applies to every channel type listed in the same trigger. To get
every DM and only the channel messages that tag you, write two triggers:

```yaml
triggers:
  - type: "clickup.chat_message"
    filter: {channel: ["dm"]}
  - type: "clickup.chat_message"
    filter: {channel: ["channel"], mentions: "me"}
```

### Classes and actions

The engine, not the model, sorts every result item into a class:

- `auto`: decided alone, with evidence, and acted on;
- `notify`: sent as information;
- `ask`: sent to you with options; your answer is acted on.

Actions (closed list): `telegram.message`, `clickup.comment`,
`clickup.description_block` (param `block`, default the behavior name),
`clickup.set_status` (param `status`), `clickup.unassign` (param `user`:
`me` or a ClickUp user ID). `clickup.set_status` and `clickup.unassign`
always need your confirmation, whatever their class.

### Run-level actions (`actions.run`)

`actions.run` lists actions that run once per run, in order, on the trusted
trigger data only (the model cannot change their targets). They are allowed
only when every trigger is `clickup.chat_message`:

- `github.pr_approve` (param `account`: a hub GitHub account of type `pat`)
  approves every PR of the message as that account; needs
  `has_pr_link: true`;
- `clickup.chat_reaction` (param `reaction`: a lower-case emoji name) adds
  the reaction to the message with your ClickUp token;
- `telegram.message`, always last, reports every outcome to you.

`on: success_of_previous` runs an action only when the one before it ended
OK. `when: {verdict: <label>}` runs an action only when the run's verdict
has that label with at least `verdict.min_confidence` (below); it is not
allowed on `telegram.message`, which always runs and reports. With `dry_run: true` nothing is written and the report says "would
approve / would react". `/undo <run id>` removes a reaction the run added;
GitHub approvals are never dismissed by Nexus (the reply lists each PR to
dismiss by hand).

### Verdict and `when`

A behavior with `instructions` may declare a closed list of verdict labels.
The model step then returns one verdict for the run besides its items:
`{label, confidence, summary}`. The label is one of `verdict.labels`; the
summary (at most 280 characters, in the behavior's language) says in the
model's words who wants what. Your instructions define what each label
means.

```yaml
verdict:
  labels: ["approve_request", "other"]
  min_confidence: 0.8
actions:
  run:
    - type: "github.pr_approve"
      when: {verdict: "approve_request"}
      params: {account: "{{vars.github_account}}"}
    - type: "telegram.message"
```

- A run-level action with `when` runs only when the label matches and the
  confidence is at least `min_confidence`; otherwise it is skipped
  (`verdict_mismatch` or `verdict_low_confidence`), and an action with
  `on: success_of_previous` after it is skipped too.
- When the triggering content reads like instructions to the AI, the
  confidence is capped at 0.5, so a `when` gate with the default threshold
  does not pass.
- When the model gives no valid verdict (provider down, bad output after one
  retry), every gated action is skipped (`verdict_unavailable`); the other
  actions and the report still run.
- The Telegram report and `/trace` show `AI: <label> (<confidence>)` and the
  summary. A verdict without any `when` is only reported.
- Labels and `when.verdict` are literal words, not vars.

## Vars

Declare vars to keep IDs, status names or headings out of the behavior, so
the same file can be reused or shared:

```yaml
vars:
  review_status: {description: "Status that starts a run"}
  board: {default: "901234"}
```

Use them as `{{vars.name}}` inside `triggers`, `instructions` and action
`params`. Substitution is plain text, done once; there are no functions.
A values file is one object of `name: "value"` strings (quote numbers and
IDs). Referencing an undeclared var, or a var with neither a value nor a
default, is an error.

Vars are plain settings, never secrets. A value that looks like a token is
rejected; store credentials with `setup-notices secret set`.

## Try it: `spec-review`

[`spec-review.yaml`](spec-review.yaml) reviews the tech-spec PR of a ClickUp
task assigned to you against the task's user story, answers the open
questions the evidence answers, and asks you only the real decisions
(scope, business rules, priority, deadline, cost, security trade-offs,
people). It starts on a status change to `{{vars.review_status}}`, when a
task is assigned to you, or with `/review <task>` on Telegram. It runs with
`dry_run: true`: answers it could give alone come to you as drafts with a
"Đăng" button before anything is written to the task.

```sh
cp examples/behaviors/spec-review.vars.example.yaml spec-review.vars.yaml   # edit the values
setup-notices behavior import examples/behaviors/spec-review.yaml --vars spec-review.vars.yaml --workspace-id <uuid>
setup-notices behavior validate spec-review --workspace-id <uuid>
setup-notices behavior publish spec-review --workspace-id <uuid>
setup-notices behavior enable spec-review --workspace-id <uuid>
```

Then, in your private chat with the bot:

```text
/review https://app.clickup.com/t/<task id>
/trace <task id>            # the latest run of the task: one line per item
/trace <run id> oq-1        # one item in full: text, proposal, citations, actions, decisions
```

On the command line, `setup-notices runs --workspace-id <uuid> [--behavior spec-review]`
lists recent runs, and `setup-notices trace --workspace-id <uuid> <run id | task> [--item <key>] [--json]`
shows the full trace (also when `/trace` was cut to fit Telegram). A trace
reference is a run ID, a ClickUp task ID or URL, or a correlation ID; every
log line, tool run and engine message of a run carries that correlation ID
(`corr=<id>` in the logs).

## Try it: `pr-auto-approve`

[`pr-auto-approve.yaml`](pr-auto-approve.yaml) approves the GitHub pull
requests linked in a ClickUp Chat direct message sent to you when the sender
asks you to approve them, adds a reaction (default `white_check_mark`) to the
message and reports on Telegram. One model call per DM classifies the message
as `approve_request` or `other` (a question, an FYI, "đừng approve", a review
request without approval); only `approve_request` with confidence 0.8 or more
approves. It needs an LLM provider (`anthropic`, `claude_cli` or
`opencode_cli`). Set `github_account` to your `pat` account in the values
file:

```sh
cp examples/behaviors/pr-auto-approve.vars.example.yaml pr-auto-approve.vars.yaml   # edit the values
setup-notices behavior import examples/behaviors/pr-auto-approve.yaml --vars pr-auto-approve.vars.yaml --workspace-id <uuid>
setup-notices behavior publish pr-auto-approve --workspace-id <uuid>
setup-notices behavior enable pr-auto-approve --workspace-id <uuid>
```

It starts with `dry_run: true`. The full setup (GitHub token, settings,
reaction probe, going live) is in `docs/runbooks/self-host-home.md` §9. A
chat run is traced with `/trace #<run id>` or `/trace clickup:chat:<message
id>`, and `setup-notices runs` shows its PRs, the verdict and the run-level
outcomes.

## Try it: `mention-report`

[`mention-report.yaml`](mention-report.yaml) sends you one short Telegram
report when someone tags you in a ClickUp task comment or a ClickUp Chat
channel message: who, where, what they ask you and a suggested next step. It
writes nothing to ClickUp, so it runs with `dry_run: false`; the report is a
`notify` item and follows your quiet hours and digest mode. It needs no
values file:

```sh
setup-notices behavior import examples/behaviors/mention-report.yaml --workspace-id <uuid>
setup-notices behavior validate mention-report --workspace-id <uuid>
setup-notices behavior publish mention-report --workspace-id <uuid>
setup-notices behavior enable mention-report --workspace-id <uuid>
```

Channel mentions need only the chat poller (`owner_clickup_user_id` and an
enabled source); task comment mentions need the ClickUp webhook. The full
setup is in `docs/runbooks/self-host-home.md` §10.

## Profile

One profile per workspace sets defaults for every behavior; a behavior's
`profile`, `model` and `budgets` fields override it.

```sh
setup-notices profile show --workspace-id <uuid>
setup-notices profile set --workspace-id <uuid> --language vi --tone "ngắn gọn" \
  --quiet-hours 22:00-07:00 --timezone Asia/Ho_Chi_Minh --digest hourly \
  --model <model id> --max-tokens 80000
```

- `--language`: the model writes in this language; Telegram texts are in
  Vietnamese, or English when the language starts with `en`.
- `--quiet-hours` (local time, may cross midnight; `off` clears): `notify`
  digests and "done on its own" messages wait until the quiet hours end.
  Questions, drafts, confirmations and failure notices still arrive at once.
- `--digest`: `per_run` (default) sends each run's messages at once;
  `hourly` batches them into one message at the start of the next hour;
  `daily` into one message at the end of the quiet hours (08:00 when none).
- `--model`, `--max-tokens` (`0` clears): defaults when a behavior sets none;
  the server settings apply when neither does.

## Decision memory

Every answer you give to a question, every draft you post and every run you
undo is remembered with its topic, question, answer and scope (behavior,
ClickUp list, repos read). Add `memory_search` to a behavior's `reads` and
each run searches past decisions with words from what it read; matches are
shown to the model as sources `memory:decision/<id>` that it may cite, so a
decision you already made can become `auto` the next time. A past decision
never overrides `always_ask_topics`. Decisions from an undone run are kept
but never offered as evidence.

```sh
setup-notices memory list --workspace-id <uuid> [--behavior <name>] [--all]
setup-notices memory show --workspace-id <uuid> --id <n>
setup-notices memory forget --workspace-id <uuid> --id <n>   # hide it from memory_search
```

## Feedback and stats

Every behavior message carries 👍 and "👎 + lý do"; the rating (and the
optional reason you reply with) is stored on the item and shown in `/trace`.
To tune thresholds and instructions with data:

```sh
setup-notices behavior stats my-behavior --workspace-id <uuid> [--version N]
```

prints, per published version, the runs (failed, skipped, undone), items by
class and by status, conflicts, 👍/👎 counts and the average confidence of
each class.

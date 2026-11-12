# Behavior files

A behavior is a versioned definition, stored as data, that tells the engine
what to do on an event. Write it in YAML (`.yaml`/`.yml`) or JSON (`.json`)
and manage it with the CLI:

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
| `instructions` | yes | Your own instructions to the model, up to 20,000 characters. |
| `autonomy` | no | `confidence_threshold` (0..1, default 0.8), `always_ask_topics` (list), `require_citations` (default true), `notify_when_informational` (default true). |
| `actions` | no | Per class (`auto`, `notify`, `ask`), a list of `{type, params}` (below). |
| `dry_run` | no | Default `true`: `auto` actions are sent as drafts to confirm first. |
| `ask_expiry_hours` | no | 1..720, default 72. |
| `budgets` | no | `max_tokens` (default: profile/settings), `max_items` (1..100, default 30), `max_steps` (1..20, default 8). |
| `model` | no | A model ID; default from the profile or settings. |
| `profile` | no | `language`, `tone`: override the profile for this behavior. |
| `vars` | no | `name: {description?, default?}` (below). |

### Triggers

| `type` | Fields |
|---|---|
| `clickup.task_status_changed` | `filter.to_status` (list), `filter.assignee` (`me` or a ClickUp user ID), `filter.list` (list IDs) |
| `clickup.task_assigned` | `filter.assignee`, `filter.list` |
| `clickup.comment_added` | `filter.list`, `filter.from` (`any` or `others`) |
| `telegram.command` | `command` (for example `/name`; not a built-in command), `args` (`[task]`: a ClickUp task ID or URL) |

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

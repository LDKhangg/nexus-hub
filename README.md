# Nexus-Hub

A personal AI switchboard. It sits between you and your work channels
(ClickUp, GitHub, Telegram), reads what happens there, acts within rules you
set, and asks you on Telegram only when it needs to.

This page is the entry point: what the hub does, how it is put together, how
to run and use it, and where to read more.

## What it does today

| Feature | What you get | Since |
|---|---|---|
| Notices | ClickUp events (via webhook) arrive as short Telegram messages | Phase 1 |
| Agent | an LLM reads the task, GitHub and ClickUp with audited tools | Phase 2 |
| Behaviors | your automations as versioned YAML; every result is `auto`, `notify` or `ask` | Phase 3 |
| AI through a CLI | the hub calls the Claude CLI or opencode with your signed-in account | Phase 3c |
| PR auto-approve | someone DMs you a PR link in ClickUp Chat → the hub approves it with your GitHub account, reacts ✅, reports on Telegram | Phase 3d |
| Intent check and mentions | the AI reads the message before approving ("review giúp em" yes, "đừng merge" no); you get a Telegram summary when someone tags you in ClickUp | Phase 3e |
| People and report style | names instead of ids ("anh Định", not `clickup 1234567`), short reports, your own templates | Phase 4 (in progress) |

The full product goal is `docs/product/PCC-001.md`; the phase plan is
`docs/product/ROADMAP.md`.

## How it works

```
ClickUp webhook / Chat poll ─┐
GitHub (tools)               ├─► behavior engine ─► actions (approve, comment, react…)
Telegram (your commands)    ─┘        │
                                      ├─► LLM (Claude CLI, opencode, or API)
                                      └─► Telegram report, ask buttons, /trace, /undo
                         PostgreSQL: events, runs, traces, secrets (encrypted)
```

- **Generic engine.** The hub has no personal flow built in. What should
  happen on an event is a **behavior**: trigger, what to read, instructions
  for the AI, autonomy rules and allowed actions. Examples are in
  `examples/behaviors/`.
- **Graded autonomy.** Each result item is `auto` (done), `notify` (done, then
  told) or `ask` (waits for your button on Telegram). `dry_run: true` writes
  nothing and reports what it would do.
- **Everything is traced.** Every run keeps its trigger ids, tool calls,
  decisions and outcome. Message text and secrets are never stored or logged.
- **Stack.** Go, Echo, pgx, sqlc, goose, PostgreSQL. Two binaries:
  `nexus-server` (the service) and `nh` (the admin CLI, `cmd/setup-notices`).

## Run it

Follow `docs/runbooks/self-host-home.md`. In short:

```sh
go build -o ~/.local/bin/nexus-server ./cmd/server
go build -o ~/.local/bin/nh ./cmd/setup-notices
goose -dir db/migrations postgres "$DATABASE_URL" up
systemctl --user restart nexus-hub        # the service unit is in the runbook
```

Secrets go in through `nh secret set` on stdin, never as arguments, never in
Git. Updating later: pull, rebuild both binaries, `goose … up`, restart.

## Use it

### CLI (`nh`)

Most commands take `--workspace-id <uuid>`. Run `nh` without arguments for
the list, or `nh <command>` for its usage.

| Area | Commands |
|---|---|
| Setup | `init`, `master-key`, `secret`, `migrate-secrets`, `settings get/set/list` |
| ClickUp source | `source`, `bind-webhook`, `inspect`, `retry-failed` |
| Agent and AI | `enable-agent`, `disable-agent`, `llm check`, `profile`, `memory` |
| GitHub | `github-account`, `github-repo`, `github-check` |
| Telegram | `telegram` |
| Behaviors | `behavior import / validate / publish / enable / disable / list / export / stats` |
| Watching | `runs`, `trace` |
| People and reports (Phase 4) | `contact`, `report-prefs`, `report-template` |

A typical behavior change:

```sh
nh behavior import examples/behaviors/pr-auto-approve.yaml --vars my.vars.yaml --workspace-id $WS
nh behavior validate pr-auto-approve --workspace-id $WS
nh behavior publish  pr-auto-approve --workspace-id $WS
nh behavior enable   pr-auto-approve --workspace-id $WS
nh runs --workspace-id $WS
```

### Telegram

Only the owner's chat is obeyed.

| Command | Does |
|---|---|
| `/help` | list commands |
| `/trace [run]` | what a run read, decided and did |
| `/undo` | revert the last undoable action |
| 👍 / 👎 under a report | feedback, counted in `behavior stats` |
| `/contacts`, `/alias`, "Đặt tên" button | name the people around you (Phase 4) |
| `/prefs`, `/template <kind>` | report style and your own templates (Phase 4) |

## Repository map

| Path | Holds |
|---|---|
| `cmd/server`, `cmd/setup-notices` | the service and the admin CLI |
| `internal/behavior` | behavior engine, runs, traces, Telegram inbox |
| `internal/agent`, `internal/adapters` | LLM providers, tools, storage, ClickUp/GitHub/Telegram clients |
| `internal/people`, `internal/reporting` | contacts and report rendering (Phase 4) |
| `db/migrations`, `db/query` | goose migrations and sqlc queries (never edit old migrations or generated code) |
| `examples/behaviors` | behavior YAML you can import |
| `docs/product` | contract, roadmap, traceability, test debt |
| `docs/runbooks` | how to run and operate |
| `docs/plans` | one plan per phase with numbered requirements |
| `docs/agents` | the Tester and Reviewer guides |
| `docs/architecture.md` | the original MVP design (background; the PCC and roadmap win) |

## Contributing

The repo is built by several agents with fixed roles (Lead, Implementer,
Tester, Reviewer); read `AGENTS.md` first, then `docs/agents/tester.md` or `docs/agents/reviewer.md`
for those roles. CI (`.github/workflows/ci.yml`) runs gofmt, vet, build and
the full test suite with `-race` against a throwaway PostgreSQL. Tests use
fakes only and never call the real GitHub, ClickUp, Telegram or any LLM.

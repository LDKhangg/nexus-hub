# Phase 2 review fixes

Fixes for Review Report phase2-agent-core-tools #1 (head 793c048, 19 findings,
R1–R19). Branch `feature/phase2`. The owner approved this fix pass on
2026-11-12.

Workflow: `AGENTS.md`. Implementers write production code only. They do not
write or edit tests and do not run `go test`. Compile checks only:
`go build ./...`, `go vet ./...`, `gofmt -l cmd internal`, `make generate`
(clean on a second run), and `goose -dir db/migrations validate`. Existing
tests must still compile, so keep exported signatures and interfaces stable.
When a new capability is needed, add an optional interface checked with a
type assertion.

## Requirements (added to Phase 2)

- REQ-F1: A git subprocess never runs a program chosen by repository or
  clone config: no filters, no signing programs, no hooks, no external diff
  or textconv. Before every mutating or network git command, the managed
  clone's local config holds only allowlisted keys, and `remote.origin.url`
  equals the expected GitHub URL.
- REQ-F2: Push and fetch reach only the granted repository's URL. Remote
  URLs, push URLs, URL rewrites, proxies or redirects from config cannot
  change that.
- REQ-F3: Mutations run only when every path component from `repos_dir` to
  the clone, and its `.git`, is a real directory, not a symlink.
- REQ-F4: A commit never contains a secret-path file, whatever the pathspec
  or the earlier index state.
- REQ-F5: Every text sent to an LLM is redacted first: context packet,
  persona, summaries, compressor input and output. A stored rolling summary
  contains no known secret.
- REQ-F6: Secret-path files are withheld from every tool output, including
  GitHub PR patches.
- REQ-F7: Audit rows store only trusted or validated identifiers. An invalid
  tool name or repo is stored as a placeholder, never verbatim.
- REQ-F8: Policy denials raised inside a tool are audited as `denied`, with
  `is_error=true`.
- REQ-F9: A write or dangerous call whose effect is uncertain (transport
  error after sending, 5xx, invalid response, remote failure during push) is
  recorded as `unknown_outcome` and is never repeated.
- REQ-F10: Write and dangerous budgets count per durable job, across
  attempts.
- REQ-F11: Two workers never process two jobs of the same thread at the same
  time.
- REQ-F12: Every untriaged external comment is triaged or included in a
  fallback, however many other messages follow it.
- REQ-F13: Once a job's token budget is used up, no further LLM call is made;
  the job ends with a fallback.
- REQ-F14: A push to the remote default branch is dangerous. If the default
  branch is unknown, the push is treated as dangerous (fail closed).
- REQ-F15: A clone, fetch or pull that grows past `max_clone_bytes` is
  stopped while it runs. A clone does not start when free disk space is below
  `max_clone_bytes` plus a 1 GiB reserve.
- REQ-F16: A merge with `require_checks_green` is allowed only when every
  check run and status has been read and is green. Unread pages block the
  merge.
- REQ-F17: A cached PAT verification is valid only for the same expected
  login.
- REQ-F18: `migrate-secrets` refuses, writing nothing, when two env names map
  to one credential name or the name already exists.
- REQ-F19: For a source without `agent.enabled`, the agent config is never
  validated at ingest. For an enabled source whose agent config is invalid,
  comments go through the Phase 1 notice path and a warning is logged. The
  callback never fails because of agent config.

## Tasks

| Task | Findings | REQs | Main files |
|---|---|---|---|
| F1 Git hardening | R1, R2, R3, R10, R11 | F1, F2, F3, F14, F15 | `internal/adapters/tools/code/{git,workspace}.go` |
| F2 Secret paths and audit | R4, R6, R7, R19 | F4, F6, F7, F8 | `tools/code/workspace.go`, `tools/github/read.go`, `agent/tools/{tool,executor}.go` |
| F3 Durable execution | R8, R15, R16 | F9, F10, F11 | `agent/tools/executor.go`, `tools/github/common.go`, `storage/{tool_run_store,agent_store}.go`, `db/query/{tool_run,agent}.sql` |
| F4 LLM inputs and budget | R5, R9, R14 | F5, F12, F13 | `agent/orchestrator.go`, `storage/agent_store.go`, `db/query/agent.sql`, `memory/compressor.go`, `cmd/server/wiring.go` |
| F5 Accounts, checks, CLI, Phase 1 | R12, R13, R17, R18 | F16, F17, F18, F19 | `adapters/github/auth.go`, `tools/github/{read,dangerous}.go`, `cmd/setup-notices/secrets.go`, `storage/notice_repository.go` |

Each task gets one commit whose message ends in `Tests not run.` and the
attribution line. Rulings go in the ledger.

## Accepted residual risk (rulings)

- Git network traffic is not routed through the in-process netguard dialer.
  REQ-F2 pins the URL to `https://github.com/<repo>.git` with redirects off
  and no config-driven proxies or rewrites. DNS/IP pinning for git is deferred
  to Phase 3, together with OS-level isolation.
- A hard disk quota needs OS support. REQ-F15 is a runtime watchdog plus a
  free-space check; a quota (systemd or filesystem) comes with deployment in
  Phase 3.

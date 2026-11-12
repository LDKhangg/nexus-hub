# Agent workflow for Nexus-Hub

This repository is built by several agents with separate roles. Every agent
working here follows this file. Code, comments, docs and commit messages are in
English; talk to the owner in Vietnamese.

## Roles

| Role | Who | Does | Never does |
|---|---|---|---|
| **Lead** | the main Claude session | writes the plan, dispatches implementers, fixes, writes Test and Review Requests, judges the reports, decides pass or fix | runs `go test`; does the final review of its own branch; pushes or merges without the owner's OK |
| **Implementer** | a subagent dispatched by the Lead, one task at a time | writes the tests and the code for one plan task, commits locally | runs `go test`; changes other tasks' scope; pushes |
| **Tester** | a separate agent that the owner runs (`TESTER.md`) | runs the Test Request exactly as written and returns a Test Report | edits code or tests; "fixes" failures; commits |
| **Reviewer** | a separate agent that the owner runs (`REVIEWER.md`) | reads the branch against the plan and returns a Review Report with proven findings | edits code; runs `go test`; commits |
| **Owner** | the human | approves plans, carries Requests to the Tester/Reviewer and Reports back to the Lead, approves push/merge | n/a |

Lead and Implementer may only run these to prove the code compiles:
`go build ./...`, `go vet ./...`, `gofmt -l cmd internal`, `make generate`
(must be clean on a second run), and `goose -dir db/migrations validate`.

## Loop

1. **Plan.** The Lead writes `docs/superpowers/plans/<date>-<topic>.md`: tasks,
   interfaces, the test names per task, and the global constraints. The owner
   approves it.
2. **Implement.** The Lead dispatches one Implementer per task, in order. Each
   Implementer:
   - writes the failing tests named in the plan first, then the code;
   - runs only the compile checks above;
   - commits with a message that ends in `Tests not run.` and the attribution
     line;
   - returns a short summary: files, tests written, and any decision it made
     with its reason ("Ruling").
   The Lead reads the diff, records Rulings in the ledger
   (`.superpowers/sdd/<plan>/progress.md`), and moves on.
3. **Test Request.** After a task, or a batch of tasks, the Lead writes a Test
   Request (format below) and gives it to the owner.
4. **Test run.** The owner gives the request to the Tester. The Tester returns a
   Test Report (format below).
5. **Verdict.** The Lead reads the report and either:
   - **PASS:** marks the tasks as verified in the ledger, or
   - **FIX:** dispatches an Implementer (or fixes inline) with the failures,
     then sends a new Test Request covering only the affected tests plus the
     full suite.
   A task is "done" only after a PASS report for its commit.
6. **Review.** When every task has a PASS, the Lead writes a Review Request
   (format below). The owner gives it to the Reviewer, who returns a Review
   Report. The Lead fixes every Critical and Important finding (and Minor ones
   it agrees with, or records a ruling), answers each Question, then sends a
   Test Request for the fixes. If the fixes are large, a new Review Request
   covers only the fix commits.
7. **Finish.** After an `APPROVE` review and a PASS test report on the same
   head, the Lead asks the owner before any push, merge or PR.

## Test Request format (Lead → Tester)

```markdown
# Test Request <plan-slug> #<n>

- Branch: <branch>   Commit: <full sha>
- Covers: Task <a>, Task <b>
- Environment:
  - Go: `go version` must be >= the go.mod version
  - DB: `. ./.superpowers/testdb.env` (loopback, database `nexus_storage_test`); or "not needed"
  - Tools on PATH: git (yes/no)
- Commands (run in order, from the repo root, do not change them;
  each line states the exit code that counts as success):
  1. `<command>` → expect exit 0
  2. `<command>` → expect exit <n>
- Must pass: <test names, or "all">
- Must not be skipped: <test names that need DB/git>
- Notes for the Tester: <flakiness hints, expected duration>
```

## Test Report format (Tester → Lead)

The Tester follows `TESTER.md` (full instructions and an example). Answer in
exactly this shape. Keep raw output verbatim; do not summarize away
error lines.

```markdown
# Test Report <plan-slug> #<n>

- Commit tested: <full sha>      (must match the request)
- Environment: <go version>, DB: <yes/no>, git: <version or no>
- Result: PASS | FAIL | ERROR

## Commands
| # | Command | Exit code | Duration |
|---|---|---|---|
| 1 | `...` | 0 | 12s |

## Summary
- Packages: <ok>/<total> ok
- Tests: <passed> passed, <failed> failed, <skipped> skipped

## Failures
For each failing test (or build error):
### <package> <TestName>
- File:line: <from the output>
- Message: <the t.Fatal / panic line, verbatim>
- Output (verbatim, at most 60 lines around the failure):
```text
...
```

## Skipped
- <package> <TestName>: <skip reason, verbatim>

## Data race / panic
- <verbatim excerpt, or "none">

## Anything unusual
- <timeouts, retries you did NOT do, environment problems, or "none">
```

Rules for the Tester:
- Run the commands exactly once, in order. Do not retry a failure to make it
  pass; if you suspect flakiness, say so under "Anything unusual".
- `ERROR` means the tests could not run (build failure, missing DB, wrong
  commit). Explain which under "Anything unusual".
- Never print secrets: the DB password from `testdb.env`, tokens or keys. If
  output contains one, replace it with `[REDACTED]`.

## How the Lead decides

- PASS only if: same commit as the request, every requested test ran, nothing
  in "Must not be skipped" was skipped, no data race, and every command exited
  with its expected code.
- When writing a request: pipelines through `tee` must start with
  `set -o pipefail;` so a failing `go test` is not hidden, and every search
  command (`grep`) must state its expected exit code (`grep` exits 1 when
  nothing matches, which is the good case).
- Otherwise FIX. The Lead first finds the root cause from the report. If the
  report lacks the evidence it needs, it sends a narrower Test Request (for
  example `-run '^TestX$' -v`) instead of guessing.

## Review Request format (Lead → Reviewer)

The Reviewer follows `REVIEWER.md`, which holds the Review Report format.

```markdown
# Review Request <plan-slug> #<n>

- Branch: <branch>   Base: <base sha>   Head: <head sha>
- Plan: <path>          Ledger: <path to progress.md>
- Scope: <tasks, or "whole branch">
- Already verified by tests: <Test Report id and commit>
- Focus: <numbered items, usually the plan's "Review Focus">
- Constraints to check: <usually the plan's "Global Constraints">
- Rulings to challenge: <ledger rulings that need a second opinion>
- Out of scope: <what not to report>
```

How the Lead judges a Review Report:
- The head reviewed must equal the request's head.
- For each finding, the Lead checks the code first. A finding that is wrong is
  answered with evidence in the ledger, not silently dropped.
- Critical and Important findings are fixed before merge. Every fix goes
  through a new Test Request.

## Repo constraints (all roles)

- Shell aliases shadow `git`, `ls`, `cp`, `mv`, `wc`, `ss`: use `command git`,
  `command ls`, etc.
- Never modify existing migrations or hand-edit generated sqlc code; add a new
  migration and run `make generate`.
- Never log or print tokens, Authorization headers, prompts, tool inputs and
  outputs, or payloads. Secrets never go in argv, logs or errors.
- Tests never call the real GitHub, ClickUp, Telegram or Anthropic.
- DB tests use only `TEST_DATABASE_URL` on loopback with the database
  `nexus_storage_test`.
- Commit locally per task; no push or merge without the owner's OK.

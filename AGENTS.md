# Agent workflow for Nexus-Hub

This repository is built by several agents with separate roles. Every agent
working here follows this file. Code, comments, docs and commit messages are in
English; talk to the owner in Vietnamese.

## Product contract

The destination is `docs/product/PCC-001.md`, the owner's Product Completion
Contract; never edit it. `docs/product/ROADMAP.md` splits it into phases, and
`docs/product/TRACEABILITY.md` tracks every PCC ID. Every phase plan names the
PCC IDs it covers, and every `REQ-n` cites its PCC IDs. At the end of a phase,
the Lead updates the traceability matrix from Test and Review evidence, and
writes a Phase Completion Report (PCC §10.4) in `docs/product/reports/`.

## Roles

| Role | Who | Does | Never does |
|---|---|---|---|
| **Lead** | Claude Opus, the main Claude Code session | writes the plan with numbered requirements, writes one brief per task (context, what to change, what to test), hands briefs to the opencode agents, reads diffs and reports, judges them, writes Phase and Fix Briefs | writes production code or tests; runs `go test`; reviews its own plan's code as the Reviewer; pushes or merges without the owner's OK |
| **Implementer** | opencode agent `implementer` (`.opencode/agents/implementer.md`, model `openai/gpt-5.6-sol#medium`) | writes the production code for one brief, runs the compile checks, commits locally, returns an Implementer Report | writes or edits tests; runs `go test`; changes other tasks' scope; pushes |
| **Tester** | opencode agent `tester` (`.opencode/agents/tester.md`, model `openai/gpt-5.6-sol#medium`); full guide `TESTER.md` | designs test cases from the requirements (requirement, human-behaviour and abuse cases), writes and commits test files, reads the CI run, returns a Test Report | edits production code; weakens tests to pass; pushes |
| **Reviewer** | opencode agent `reviewer` (`.opencode/agents/reviewer.md`, model `openai/gpt-6-sol#medium`, read-only); full guide `REVIEWER.md` | judges the branch against the requirements, real user journeys and the constraints; returns a Review Report with proven findings | edits files; runs `go test`; commits |
| **Owner** | the human | approves plans and Test Plans, watches or steers any agent session in the opencode window, pushes branches so CI runs, approves merge | n/a |

If you are an opencode agent, your role is your agent name; your agent file
and this file are your rules. The tool permissions in `.opencode/agents/`
enforce the "Never does" column (deny by default, then an allow list).

## Handoff between Claude and opencode

The Lead hands work to an opencode agent with

```sh
scripts/agent-handoff.sh <implementer|tester|reviewer> "<title>" <brief.md> [session-id]
```

- It runs `opencode run --agent <role>` through opencode's shared background
  server, so the session also shows in the owner's opencode window, live.
- It prints the session id and writes the agent's final report to
  `.superpowers/handoff/<stamp>-<role>.md` (raw events next to it).
- Fix rounds, Test Plan approval and CI run ids go to the **same session**
  (pass its id), so the agent keeps its context.
- One handoff runs at a time per branch: agents share the working tree.
- Briefs live under `.superpowers/sdd/<plan>/` (task briefs `task-<id>.md`,
  Phase and Fix Briefs as below). A brief names the commit message, the
  attribution line, the files to read, the REQ ids, and what is out of scope.
- The Lead never forwards a secret, token or `.env` content in a brief.

Token budget (the owner's OpenAI quota is a 5-hour window):
- Every step of an agent re-sends its whole session context, so long
  sessions are the main cost. Continue a session only for a short follow-up
  while it is warm (within about 10 minutes) and its context is under about
  100k tokens; `agent-handoff.sh` prints both. Otherwise start a new session
  with a short carry-over note (head, what is done, what is next, file
  pointers; under 40 lines).
- Keep each handoff small: one task, or one package/area for tests and
  review (for example 3c and 3e in separate sessions).
- Briefs point to files and line ranges instead of pasting code; under 80
  lines.
- Reports start with a KEY block (result, head, commits, item ids, next) and
  stay under 60 lines, with details only for failures, findings and Rulings.
  The Lead reads the KEY block and opens only the items it must judge.

Lead and Implementer may only run these to prove the code compiles:
`go build ./...`, `go vet ./...`, `gofmt -l cmd internal`, `make generate`
(must be clean on a second run), and `goose -dir db/migrations validate`.
`go vet` also compiles test files, so a production change that breaks a test's
build is caught here.

## CI

`.github/workflows/ci.yml` runs on every push and PR: gofmt, `go vet`,
`go build`, then the full suite with `-race` against a throwaway PostgreSQL
(fakes only, no secrets). Go modules, the build cache and Docker layers are
cached. Each run uploads `test-logs-<sha>` (test.log, test.json, junit.xml,
kept 14 days) and writes failed/skipped tests, races and panics to the run
summary. On `main` and `v*` tags it publishes `ghcr.io/<repo>` tagged with
the short SHA (and `latest` on main). Deploying to the VPS is a later phase.

- CI is where the suite runs. The Tester writes tests and reports the CI
  run; nobody runs the full suite locally (see `TESTER.md`).
- The Lead may read CI results (`gh run view`, `gh run download`) to judge a
  report; reading CI is not running tests. A red `check` job on a Lead or
  Implementer commit is fixed before the Phase Brief.
- Pushing a branch to trigger CI still needs the owner's OK.

## Loop

1. **Plan.** The Lead writes `docs/superpowers/plans/<date>-<topic>.md`: a
   numbered requirement list (`REQ-1`, …, each an observable behaviour), the
   tasks, interfaces, global constraints and review focus. The owner approves
   it.
2. **Implement.** For each task, in order, the Lead writes a task brief and
   hands it to `implementer`. The Implementer writes the production code,
   runs only the compile checks, commits with a message ending in
   `Tests not run.` and the attribution line, and returns an Implementer
   Report (files, checks, Rulings). The Lead reads the diff, re-runs the
   compile checks, and records Rulings in the ledger
   (`.superpowers/sdd/<plan>/progress.md`). Corrections go back to the same
   session.
3. **Phase Brief.** When the phase (or a batch the owner chooses) is
   implemented, the Lead writes one Phase Brief (format below) with a
   "What to test" list, and hands it to `tester` and `reviewer` (one after
   the other: they share the working tree).
4. **Test.** The Tester's first reply is a Test Plan; the Lead relays it to
   the owner, who approves or edits it; the Lead sends the decision to the
   same session. The Tester writes and commits the tests, the owner pushes,
   CI runs the suite, the Lead sends the run id, and the Tester returns a
   Test Report from the CI run.
5. **Review.** The Reviewer returns a Review Report.
6. **Verdict.** The Lead checks every defect (`D…`) and finding (`R…`)
   against the code, then:
   - has the Implementer fix production code (through a task brief) for every
     real Critical/Important finding and every test defect, and Minor
     findings it agrees with;
   - answers each finding it rejects, with evidence, in the ledger and in the
     Fix Brief;
   - if it believes a test is wrong, it raises a **test dispute** in the Fix
     Brief for the owner to decide. It never edits the test.
   Then the Lead writes a Fix Brief. The Tester reruns its suite and adds
   regression tests; the Reviewer checks the fixes.
7. **Finish.** When the Test Report is `PASS` and the Review Report is
   `APPROVE` on the same code head, the Lead asks the owner before any push,
   merge or PR.
   The owner may instead merge earlier, accepting **test debt**: the Lead
   writes every unproven item to `docs/product/test-debt/` (see its README),
   and the next Phase Brief carries the open debt for the Tester and the
   Reviewer to pay first.

## Phase Brief format (Lead → Tester and Reviewer)

```markdown
# Phase Brief <plan-slug> #<n>

- Branch: <branch>   Base: <base sha>   Head: <head sha>
- Plan: <path>   Ledger: <path>
- Goal: <one paragraph: what this phase gives the user>
- Users and journeys:
  - <who>: <what they do, step by step>
- Requirements:
  - REQ-1: <observable behaviour>
  - REQ-2: ...
- Constraints: <the plan's Global Constraints, or a pointer to them>
- Environment: Go >= go.mod version; DB `. ./.superpowers/testdb.env` (loopback, `nexus_storage_test`) or "not needed"; git yes/no; external services: fakes only
- Existing tests: <where they are; written by implementers, not proof>
- What to test: <the Lead's list of behaviours, risks and unconfirmed facts that need proof; the Tester still designs the cases>
- Out of scope: <what not to test or report>
```

A **Fix Brief** uses the same header, plus:

```markdown
- Previous reports: Test Report #<n>, Review Report #<n>
- Fixed: D1 → <commit>, R2 → <commit>
- Rejected, with evidence: R3: <why>
- Test disputes: <test, why the Lead thinks it is wrong> (owner decides)
- Re-check: <rerun the full suite, add regression tests for the fixed items, review commits <a>..<b>>
```

## How the Lead judges reports

- The report's code head must equal the brief's head, or it is sent back.
- Test Report: `PASS` only if every approved case ran, nothing was skipped,
  and there was no race or panic. Each defect is checked against the code. If
  the evidence is not enough, the Lead asks for a narrower run (for example
  `-run '^TestX$' -v`) instead of guessing.
- Review Report: Critical and Important findings block the merge. A rejected
  finding needs evidence, not silence.
- Every fix goes into a Fix Brief, so it is re-tested and re-reviewed.

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
- Commit locally; no push or merge without the owner's OK.

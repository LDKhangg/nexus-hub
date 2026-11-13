# Test debt: Phase 3d (ClickUp Chat DM → auto-approve PR → reaction)

- Merged to `main` by the owner's decision on 2026-11-13 ("B nha": merge with
  test debt and move on), before any Phase 3d test or review.
- Merged head: see the merge commit "Merge branch 'feature/phase3d'"; feature
  head 9a0bdbe (Phase Brief 1 was written for 9f91d92; 9a0bdbe adds REQ-16).
- Plan: docs/superpowers/plans/2026-11-13-phase3d-chat-pr-auto-approve.md
- Ledger: .superpowers/sdd/2026-11-13-phase3d-chat-pr-auto-approve/progress.md
- Brief for Tester and Reviewer: .superpowers/sdd/2026-11-13-phase3d-chat-pr-auto-approve/phase-brief-1.md
  (add REQ-16 and the head 9a0bdbe when it is used).

## What is proven

- CI run 43356596617 on 9f91d92: 568 of 569 existing tests pass, no race. The
  only failure is TestHardening_FsmonitorNotExecuted (TD3-4, already open on
  `main`).
- Lead compile checks on 9a0bdbe: build, vet, gofmt, `make generate`, goose
  validate.
- Owner's home run (2026-11-13, dry run): migrations 00010–00011 applied, the
  poller initialised its cursor for the LEAP team with no error, an unsigned
  ClickUp webhook on the poll-only source is refused (503), the bot posts to
  the owner's group. No DM has been processed yet.
- Nothing else about Phase 3d behaviour is proven: no Phase 3d tests exist.

## Open items

| # | Item | REQ | PCC | Proof needed | Status |
|---|---|---|---|---|---|
| TD3d-1 | Every Phase 3d requirement | REQ-1..REQ-16 | FR-012, FR-013, FR-018, FR-024, FR-025, FR-027, FR-029, FR-082, FR-090, FR-133, FR-135, FR-137, NFR-001, NFR-004 | Approved Test Plan, CI PASS | open |
| TD3d-2 | Independent review of 45caa64..9a0bdbe, with the review focus of Phase Brief 1 (exactly-once DMs, model cannot steer targets, `AccountScoped`/`ActionOnly` never reachable from agent jobs, dry run writes nothing) | all | as above | Review Report APPROVE | open |
| TD3d-3 | Live checks: ClickUp Chat v3 field shapes and message order, the reaction name for ✅ (`white_check_mark` assumed), the "already exists" 400 text | REQ-2, REQ-3, REQ-11 | FR-012 | owner's first live DM, then a test against recorded payloads | open |
| TD3d-4 | Defect found by the Lead during setup: a poll-only ClickUp source stays `is_enabled=false` after `nh init` (only `bind-webhook` enables it), so the poller finds no target; worked around locally by an SQL update | REQ-1 | FR-012 | a CLI path or documented step, then a test | open (fix in the next phase's Fix Brief) |

Phase 2 debt (TD-1..TD-6) and Phase 3 debt (TD3-1..TD3-4) stay open as well.

## Operating safeguard until the debt is paid

`pr-auto-approve` runs with `dry_run: true` until the owner checks the first
reports and asks to go live. The approval itself has no confirmation step by
the owner's decision (D1, D2, D4).

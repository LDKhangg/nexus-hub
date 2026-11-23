# Test debt: Phase 3 (behavior engine)

- Merged to `main` by the owner's decision on 2026-11-12 so it can be
  self-hosted at home the same day, before any Phase 3 test or review.
- Merged head: d1b24e2328a5d56431c15c5d47b2db82a32e048c.
- Plan: docs/plans/2026-11-12-phase3-behavior-engine.md
- Ledger: .superpowers/sdd/2026-11-12-phase3-behavior-engine/progress.md
- Brief for Tester and Reviewer: .superpowers/sdd/2026-11-12-phase3-behavior-engine/phase-brief-1.md

## What is proven

- CI run 43206735312 on d1b24e2: 568 existing tests pass, no race. The only
  failure is TestHardening_FsmonitorNotExecuted, which must change to expect
  `unsafe_repo_config` (owner decision on the D9 dispute).
- Nothing about Phase 3 behaviour is proven: no Phase 3 tests exist yet.

## Open items

| # | Item | REQ | Proof needed | Status |
|---|---|---|---|---|
| TD3-1 | Every Phase 3 requirement | REQ-1..REQ-25 | Approved Test Plan, CI PASS | closed: CI 43394889383 green on 85f5d5d (merged 6c9a0a6) |
| TD3-2 | Independent review of 37b5746..d1b24e2 | all | Review Report APPROVE | open |
| TD3-3 | Live checks of unconfirmed assumptions: ClickUp `list_id`/`parent_id`, description markers and `markdown_content` writes | REQ-4, REQ-16 | owner's first home run, then a test against recorded payloads | open |
| TD3-4 | TestHardening_FsmonitorNotExecuted updated by the Tester | REQ-F1 | CI PASS | closed: CI 43394889383 green on 85f5d5d (merged 6c9a0a6) |

Phase 2 debt (TD-1..TD-6) stays open as well.

## Operating safeguard until the debt is paid

Behaviors run with `dry_run: true`, so nothing is written to ClickUp unless
the owner presses "Đăng" or confirms an action.

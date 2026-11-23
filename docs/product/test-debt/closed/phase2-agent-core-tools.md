# Test debt: Phase 2 (agent core and tools)

- Merged to `main` by the owner's decision on 2026-11-12, before final
  verification (the Tester ran out of budget).
- Merged head: 079acef3efa52ec147402212d711369bcaee1f5d (plus this docs commit).
- Plans: docs/plans/2026-11-10-phase2-agent-core-tools.md,
  docs/plans/2026-11-12-phase2-review-fixes.md
- Ledger: .superpowers/sdd/2026-11-10-phase2-agent-core-tools/progress.md
- PCC IDs touched: see TRACEABILITY.md rows marked IN_PROGRESS with phase 2.

## What is proven

- Test Report #2 on 807c9c5 (tests 25cf393): 58 of 62 approved cases passed,
  0 skipped, no race or panic. The four failures are D2, D7, D8, D9 below.
- Review Report #1 (head 793c048): its 19 findings R1–R19 were fixed in
  d9f698f..15a3e51.

## Open items

| # | Item | REQ | Proof needed | Status |
|---|---|---|---|---|
| TD-1 | D2: secret in a nested tool-input JSON key never reaches the LLM (fix 079acef) | REQ-10, REQ-F5 | TestApproved_ActorNestedJSONKeysCannotEchoKnownSecret passes | closed: CI 43394889383 green on 85f5d5d (merged 6c9a0a6) |
| TD-2 | D7: a crash after merge, then retry with the budget exhausted, returns unknown_outcome and never re-merges (fix 079acef) | REQ-6, REQ-F9 | TestApproved_CrashAfterCommitPushCommentMergeNeverRepeats/github_pr_merge passes | closed: CI 43394889383 green on 85f5d5d (merged 6c9a0a6) |
| TD-3 | D8: merge needs the exact full head SHA; pr_checks prints the full SHA (fix 079acef) | REQ-5 | TestMerge_Preconditions/sha_prefix_only passes | closed: CI 43394889383 green on 85f5d5d (merged 6c9a0a6) |
| TD-4 | D9: git_status allows core.fsmonitor / diff.external (neutralized by hardenedArgs) and still refuses filters etc. (fix 079acef) | REQ-7, REQ-F1 | TestHardening_FsmonitorNotExecuted and TestApproved_StatusCannotExecuteCleanFilter both pass | closed: CI 43394889383 green on 85f5d5d (merged 6c9a0a6) |
| TD-5 | Full suite green on one head with `-race` | all | one full run, exit 0 | closed: CI 43394889383 green on 85f5d5d (merged 6c9a0a6) |
| TD-6 | Independent review of the fixes R1–R19 and D1–D9 (commits 35d9c8c..079acef) | all | Review Report APPROVE | open |

## Accepted residual risks (not debt)

- Git network traffic is not behind netguard; no OS disk quota (from the
  review-fixes plan).

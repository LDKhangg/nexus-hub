# Phase Completion Report

Phase: 9 — Policy engine and approvals

Status: Implemented, tested and reviewed on `feature/phase9` at code head `9bb65d5` (tests `3b8421e`). Not merged into `develop` yet (owner merges). Base `develop` 3cb8928. The plan was approved through the owner's standing instruction on 2026-11-18 (owner offline); the Lead calls in plan §1.3 are open for the owner to change.

Implemented requirements: REQ-1 … REQ-12 of `docs/plans/2026-11-19-phase9-policy-engine-approvals.md` (PCC FR-009, FR-111–FR-116, FR-118, FR-121–FR-130, NFR-023, NFR-034, E2E-004, E2E-009). FR-117 (rule UI) stays in Phase 11.

Delivered components:
- `internal/policy`: rule sets `policy/v1` (YAML: tier defaults, per-action effects `allow` / `approval_required` / `deny`, `approval_ttl_hours`), strict Parse/Validate against the known tools and capabilities, and a deny-by-default `Engine.Decide`. A missing, invalid or DB-edited set falls back to the safe set (read allow, write approval_required, dangerous deny). A store or audit failure denies. When a tool's tier is escalated for an input, the stricter of the action rule and the tier default applies (`tier_escalated`).
- Migrations 00035 (rule sets, decisions; v1 seeded per workspace = today's behaviour: read allow, write allow, dangerous approval_required, `github_pr_merge: approval_required`), 00036 (policy_gate approvals: binding columns, single-use `consumed_at`, one open gate per binding), 00037 (merge target on plan tasks; consumed check fixed for open gates).
- CLI `nh policy show | validate | apply | history | rollback | decisions`. Only the CLI and `nh init` write rule sets.
- Tool executor: asks the engine before every non-read call; `allow_dangerous` retired (no effect, deprecated). An approval grant is verified and consumed in the same transaction as the tool run, and policy is decided again before the side effect. A dry run never consumes a grant.
- Orchestrator capability `github.merge` (behavior-actions). Under approval_required the topic shows "🔐 Cần duyệt: merge PR repo#n (head …)" with Duyệt / Từ chối; the execution waits in `WAITING_FOR_APPROVAL`. Duyệt → re-check, one merge pinned to the head SHA, evidence with the merge SHA. Từ chối, expiry, an ended execution or a press by anyone but the owner never merges. Gate expiry is capped at the execution deadline; a sweep closes gates of ended executions (⌛). Under `allow` the PR and head SHA must appear in an earlier task's result (`merge_target_unproven`).
- Docs: runbook "Policy and approvals (Phase 9)"; architecture §9.

Tests executed: full suite with `-race` against a throwaway PostgreSQL in CI only, fakes only (fake GitHub, Telegram, provider). Batch A (engine, rule sets, CLI, executor gate, grants, migrations 00035/00036), Batch B (merge gate, presses, expiry, dedupe, plan validation, E2E-004 and E2E-009 journeys), Fix B regressions.

Test results: CI run 44363609327 on `3b8421e`: success, 3014 tests, no fail, skip, race or panic.

Evidence:
- Batch A: Review A APPROVE on aa68ffa (no findings; three notes taken into P3). CI 44355436649 red: 8 test defects and production defect D-A-1 Minor (consumed check passed on open gates) → 00037 in a5332d2; tests fixed in 7951720.
- Batch B: Review B CHANGES on 9eb4d4b: R-B-1 Important (a gate outlived its execution), R-B-2..5 Minor (docs, merge target not bound under `allow`, one merge per execution undocumented, expired open gate resent). CI 44360540092: D-B-1 Minor (plan task aliased its merge target), D-B-2 Minor (replayed merge evidence lost the full SHA), 3 test defects. All fixed in 84595c6 and 9bb65d5; re-review APPROVE on 9bb65d5; CI 44363609327 green on 3b8421e.
- Agents: Claude subagents per role (Opus: P1–P3, fixes, reviews; Sonnet: D1, tests). Ledger `.superpowers/sdd/phase9/progress.md` (local).

Known limitations:
- Approval requests are created only for orchestrated merges. A direct agent or behavior tool call that needs approval is refused (`approval_required`) and does not wait.
- One merge per execution (dangerous budget 1; a dry-run merge uses it).
- Under a non-default `github_pr_merge: allow`, the evidence check also accepts worker summaries (untrusted text); it is a consistency check, and the merge is still pinned to the PR's current head SHA (Review N1).
- For `merge_target_unproven` the decision row says `allow` while the tool run is denied.
- Non-owner presses get a toast only on the webhook path (polling ignores them silently).
- A cancelled execution's gate closes within one sweep (about 1 minute), not in the cancel transaction.
- Other EPIC 10 rule kinds (repos, integrations, hours, concurrency, notifications) still live in their current config (later schema versions).
- Live checks after deploy: `nh policy show` on the VPS shows v1; one real `/plan` ending in a merge asks first. Phase 10 live checks U1–U3 are still open.

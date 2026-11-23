# Phase Completion Report

Phase: 12 — Reliability, security and operations hardening

Status: Implemented, tested and reviewed on `feature/phase12` at code head `7c25c37` (last test commit `98babd4`). Not merged into `develop` yet (owner merges). Base `develop` `37b16fa`. Plan `docs/plans/2026-11-19-phase12-reliability-security-ops.md`, approved by the owner on 2026-11-20; batches 12a–12d each had their own Test and Review round.

Implemented requirements: REQ-1 … REQ-16 of the plan, except REQ-13 (restore drill), which is test debt (owner option C, see below). PCC: NFR-001–NFR-007, NFR-010, NFR-012, NFR-013, NFR-017, NFR-024, NFR-027–NFR-030, NFR-032, NFR-040, Gate B, Gate C.

Delivered components:
- 12a Reliability: store-then-ack webhooks with dedup by delivery id or body hash; reconciler and sweepers resume after a crash; graceful shutdown with `shutdown_timeout`; worker failure retry; approvals and policy gates survive restart; queue and workspace backpressure caps. Bounded picks rewritten to `MATERIALIZED` CTEs; `deploy.sh` installs the systemd unit when it changes.
- 12b Observability: `/metrics` on a separate loopback listener (`metrics_addr`, default `127.0.0.1:9090`, never on the public server); `nh status`; `nh execution timeline`; Telegram alerts for stuck and failed executions (dedup per execution and state, hourly cap with one summary).
- 12c Data ops: retention settings (days per category, `0` keeps forever) and an hourly sweeper that deletes in bounded batches, child rows first, never touching rows an open execution, wait, approval, job or alert still points to; per-row FK isolation with a shared skip cap; `nh retention show` and `nh retention run [--dry-run]` (dry-run is the real sweep rolled back, bounded to 20 s and 5 batches per group, prints `status=partial (cap reached | time limit reached, more rows may remain)`). Local load-test harness `cmd/loadtest`.
- 12d Security: git runs inside `bwrap` (read-only system binds, writable bind of the command's workspace and the runner HOME only, private `/tmp`, no `/home`, no network unless the subcommand needs it); network git goes through an in-process CONNECT proxy that allows only `github.com:443`, with `GIT_ALLOW_PROTOCOL=https`; the existing `max_clone_bytes` watcher caps workspace size; missing or broken `bwrap` fails closed (`sandbox_unavailable`). Settings `bwrap_path`, `git_sandbox`. systemd unit allows the namespaces and mount syscalls bwrap needs. CI secret scan (gitleaks 8.28.0, pinned and checksum-verified, full history, values never printed). Gate C report `docs/product/reports/2026-11-23-phase12-gate-c.md`. Runbook sections "Load test (Phase 12)", "Git sandbox (Phase 12)", "Security (Phase 12)", "Recovery (Phase 12)".

Tests executed: full suite with `-race` against a throwaway PostgreSQL in CI only, fakes only. Per batch: 12a TC-1..34, 12b, 12c TC-C-1..20 plus TC-C-FIX-1..10, 12d TC-D-1..17.

Test results: CI run 45111228514 on `7c25c37`: success, 3222 tests, no fail, skip, race or panic; secret scan pass.

Load test (REQ-12, owner decision O2, run locally by the Lead on 2026-11-20): 120 executions (12 duplicate deliveries), all completed, 0 failed, 0 stuck, webhook p50 40.4 ms, p95 59.2 ms, p99 65.3 ms (limit 500 ms). Result: `docs/product/reports/loadtest-2026-11-20.json`.

Evidence:
- 12a: Test PASS + Review APPROVE on `077745d` (CI 44674034454); follow-ups fix-A2/A3 re-reviewed.
- 12b: Test PASS + Review APPROVE on `96d13a5` (CI 44700553497).
- 12c: Test PASS (CI 45104921053) + Review APPROVE on `abe29df` after three fix rounds (round 1: R-C-1 loadtest guard, R-C-2 FK poisoning, R-C-3 dry-run counts; round 2: R1 Critical retry without re-check, R2/R3 Important; round 3: dry-run cancel mapping, cap edge). The deadline-wording Minor was fixed in 12d (`861bf6f`, `7c25c37`).
- 12d: Test PASS (17/17, CI 45111228514) + Review APPROVE on `7c25c37`. Review round 1 R1 (secret scan red): the two findings were planted fake values in redaction tests, ignored by exact fingerprint in `.gitleaksignore`; R2 (per-finding location output) rejected by the Lead with evidence (no values printed), reviewer agreed.
- Agents: Claude subagents (12a, 12b) and opencode agents (12c, 12d) per role. Ledger `.superpowers/sdd/phase12/progress.md` (local).

Test debt:
- D12-1 (NFR-006) and D12-2 (NFR-040 restore part): the Supabase restore drill (REQ-13) was not done; Supabase Free has no daily backups. Manual `pg_dump` steps are in the runbook, marked not drilled. See `docs/product/test-debt/phase12-backup-restore.md`.

Known limitations:
- The git sandbox is proven by tests with a fake `bwrap` (argv, binds, env, proxy, fail-closed); a real `bwrap` run on the VPS is a live check after deploy (below).
- `--share-net` for network git: the proxy is defence in depth on top of the fixed GitHub remote and the hub-controlled git config; it is not kernel-level egress filtering.
- A clone binds its parent directory writable (the repos dir of that owner) for the duration of the clone.
- No command rotates the master key; rotation is manual re-entry of secrets (runbook). No CLI registers GitHub webhooks.
- The execution_events immutability trigger allows DELETE when the transaction sets `nexushub.retention_sweep='on'`; any session of the app role could set it (accepted: the same role owns the data).
- An exact 5-full-batch dry-run still prints partial ("more rows may remain").

Live checks after deploy (with the owner):
1. Install `bubblewrap` on the VPS (`dnf install bubblewrap`), check `user.max_user_namespaces` > 0, deploy; the hub log shows `git sandbox: ready`; one granted clone/fetch works; a non-GitHub remote is refused.
2. `/metrics` answers on the VPS loopback only; one forced stuck execution alerts once.
3. `nh retention run --dry-run` prints counts.

Merge: into `develop` only on the owner's word.

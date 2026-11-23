# Phase Completion Report

Phase: F — Foundation operability

Status: Implemented, tested and reviewed on `feature/phaseF` at head `a58b320` (code head `0726798`). Not merged into `develop` yet (owner merges). Base `develop` `c2024e4`. Plan `docs/plans/2026-11-23-phaseF-foundation-operability.md`, approved by the owner on 2026-11-23 (Q1 bridge + targeted conversion, Q2 keep the deploy.sh migration step, Q3 `nh seed demo` in this phase). Two batches, Fa and Fb, each with its own Test and Review round.

Implemented requirements: REQ-1 … REQ-10 of the plan. PCC: NFR-025, NFR-026, NFR-031, NFR-033, NFR-035, NFR-037, NFR-038, DEP-002, DEP-003, DEP-004, DEP-005.

Plan corrections made by the Lead during the phase (docs only): REQ-5 — a missing master key stays a warning and startup continues, as before Phase F (`c95dcf2`); REQ-7 — the local run ends at the JSON log, because the hub has no production fake Telegram (`061f15e`).

Delivered components:
- Fa logging (F1): `internal/obs` sets one JSON `slog` handler for the server and `nh` (`log_level` setting), bridges the standard `log` package (component `legacy`), and ends in a redacting handler that masks secret shapes in messages, string attributes, errors, Stringers and bytes; `internal/redact` split out of the agent sanitizer with the old API kept. Echo's port banner and panic recovery were replaced by JSON lines; a recovered panic logs only method, route pattern, status and correlation id, never the panic value or stack.
- Fa correlation (F1): every webhook, Telegram update and worker call gets a UUIDv7 correlation id; public routes ignore client headers; worker routes accept `X-Correlation-Id` only when it is a valid UUID. The id is stored on `telegram_updates` (migration 00042), `agent_jobs` (00043) and `workflow_executions`, and every step log line carries `correlation_id` and `execution_id`, also after a restart. The retention sweeper and Telegram inbox now log structured fields.
- Fa health (F2): public `/healthz` (GET and HEAD, no DB, `ok`); `/readyz` only on the loopback metrics listener, bounded to 2 s, closed vocabulary; required checks db, migrations (generated `ExpectedMigrationVersion`), master_key, workflow_worker; `git_sandbox` informational. One readiness summary line at startup; DB failure is fatal with a safe JSON line.
- Fb (F3): `docs/configuration.md` lists every process environment variable and every runtime setting; `make config-doc-check` (also in CI, including the docs-only path) fails on missing, extra or duplicate rows. README "Run it locally" from a fresh clone. `nh seed demo`: only a loopback database whose name ends in `_dev` or `_test`, checked on the URL, on every fallback host and again at DNS lookup and dial time; idempotent; never changes rows it did not create (conflict error instead); fake credentials only. CI `check` job regenerates code with pinned sqlc v1.31.1 and fails on drift, and runs `goose validate`. `.gitignore` covers env, key and dump files.

Tests executed: full suite with `-race` against a throwaway PostgreSQL in CI only, fakes only. Fa TC-F-1..17 plus regressions for review items R1–R3 and CI items D1/D3; Fb TC-Fb-1..18.

Test results: CI run 45144063548 on `a58b320`: success, 3290 tests, no fail, skip, race or panic.

Evidence:
- Fa: Test PASS (CI 45136470919 on `a0ab8fc`, 3260 tests, no fail, skip, race or panic) + Review APPROVE on `a0ab8fc`. Round 1 review: R1 Critical (Echo recovery logged the panic outside the redacting logger), R2 Important (ClickUp agent executions got a fresh correlation id), R3 Important (Telegram inbox id only inside the message text), all fixed in `6cf90d6`. First CI run on the tests (45133605877) found four production defects (Echo port banner, a static test tripped by `readiness.Handler()` in main.go, no structured DB startup line, retention log printed Go maps), fixed in `0a27af4`; three test defects fixed by the Tester in `a0ab8fc`.
- Fb: Test PASS (CI 45142005098 on `a69a8c1`, 3288 tests; CI 45144063548 on `a58b320`, 3290 tests) + Review APPROVE on `a58b320`. Round 1 review: R1 (`localhost` trusted by name; rated Important by the Lead), R2 (env vars not checked), R3 (seed overwrote an existing behavior; the Lead found the same for contact accounts), fixed in `78d8ae4`; R4 (config doc check on the docs-only CI path) rejected by the Lead, AGENTS.md updated (`9191565`), reviewer agreed. CI on the first tests (45138960019) found the Lead's own brief error (every DSN with a pgx fallback refused, which includes the default `sslmode=prefer`) and a seed SQL parameter used as both jsonb and text; both fixed in `78d8ae4`. Round 2 review: env check missed raw-string names, fixed in `0726798` (alias or variable-held names rejected: none in the repo, dynamic legacy refs documented).
- Agents: opencode implementer, tester and reviewer per role. Ledger `.superpowers/sdd/phaseF/progress.md` (local).

Not verified live: `/readyz` and the JSON log on the VPS (needs the merge and deploy); `nh seed demo` and the README journey on a real local machine.

Test debt: none new. Open from Phase 12: D12-1, D12-2 (restore drill).

Next: owner merges `feature/phaseF` into `develop`; deploy runs migrations 00042 and 00043 through `deploy.sh`; then check `journalctl -u nexus-hub -o cat | jq` and `curl -s 127.0.0.1:9090/readyz` on the VPS. Then Phase 13 per ROADMAP.

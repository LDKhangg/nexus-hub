# Phase F — Foundation and operability

> **For agentic workers:** REQUIRED SUB-SKILL: Use `executing-plans` for an assigned task. The role boundaries and sequential handoff process in `AGENTS.md` override generic implementation/TDD instructions. Steps use checkbox (`- [ ]`) syntax for tracking.

**Status:** DRAFT 2026-11-23, waiting for the owner's approval. Every Lead call in §1.3 can be changed by the owner.

**Goal:** Anyone (the owner, an agent, a future maintainer) can run, observe and operate the hub without reading the code: logs are structured JSON with a correlation id that follows one webhook through the workflow; the hub answers `/healthz` and `/readyz`; one document lists every environment variable and setting; a fresh clone runs locally with fakes and demo data by following the README; CI also proves generated code is not stale. This is the last foundation step before Phase 13 (fresh install and real-service acceptance).

**Architecture:** No new subsystem. A small `internal/obs` package (slog setup, correlation id in `context.Context`, a bridge so existing `log.Print*` calls go through slog). Correlation id is created at the webhook/Telegram/worker entry points and carried into workflow executions (stored on the execution row so async steps log it too). Health handlers live on the public Echo server (`/healthz`) and the loopback ops listener (`/readyz`, next to `/metrics`). Docs and a seed command complete the phase.

**Tech Stack:** Go standard `log/slog` (no new dependency), existing Echo, pgx, sqlc, goose.

**Spec:** `docs/product/PCC-001.md` NFR-025, NFR-026, NFR-031, NFR-033, NFR-035, NFR-037, NFR-038, DEP-002, DEP-003, DEP-004, DEP-005; Go code criteria (structured errors, structured logging, composition root). `docs/product/ROADMAP.md` §Phase F. The contract is read-only.

## Global Constraints

- Code, documentation and commits in English; owner-facing talk in Vietnamese.
- Baseline: `develop` `c2024e4` (Phase 12 merged). Branch `feature/phaseF`. Next migration number `00042`.
- One handoff at a time per working tree. Implementer: production code only. Tester: tests only. Reviewer: read-only.
- Nobody runs `go test` locally; CI runs the suite. Allowed checks: `go build ./...`, `go vet ./...`, `gofmt -l cmd internal`, `make generate` (second run clean), `goose -dir db/migrations validate`.
- No behaviour change for the owner's flows: only log format, new endpoints, docs and a dev-only seed command.
- Privacy is unchanged and now enforced in one place: logs carry ids, counts, states, durations, error codes and the correlation id. Never message text, prompts, tool inputs/outputs, tokens, Authorization headers or DSNs. The slog handler redacts known secret shapes as a last line (existing redactor), not as the main guard.
- `/readyz` details (DB, migrations, master key, workers) are not public: loopback ops listener only. Public `/healthz` returns `200 ok` with no detail.
- Never edit old migrations or generated sqlc output by hand.
- Tests use fakes; DB tests only through loopback `TEST_DATABASE_URL`, database `nexus_storage_test`.
- Generic hub: no person, repo or behavior name hard-coded.

## Review Focus

- No private data in any log line after the switch (bridge included); no log line lost (startup, panic, shutdown).
- One correlation id per inbound event, the same id in the webhook log, the workflow execution and every step log, including after a restart.
- `/healthz` never touches the DB; `/readyz` is bounded in time and never exposed through the tunnel.
- The README and the environment document match the code (every setting key and env var listed, no stale ones).

## 1. Scope and decisions

### 1.1 What the owner gets

- `journalctl -u nexus-hub -o cat | jq` shows one JSON object per line with `time`, `level`, `msg`, `component`, `correlation_id`, `execution_id` where known.
- `nh trace <correlation-id>` (or `nh execution timeline`) finds everything one event caused (reuse of `cmd/setup-notices/trace.go` if it fits).
- `curl -fsS https://hub.museyard.me/healthz` → `ok`; on the VPS `curl -s 127.0.0.1:9090/readyz` → JSON per check.
- `docs/configuration.md`: every env var and setting key, default, range, who reads it.
- `make dev` (or documented commands) brings up a local DB, migrations, `nh seed demo` demo data, and the hub with fake providers.

### 1.2 Owner decisions needed

- Q1 (logs): move all 56 files from `log` to `slog` call by call (bigger diff, richer fields), or **bridge** the standard `log` into the slog JSON handler and convert only the entry points and workflow/worker paths that need fields (Lead recommends the bridge plus targeted conversion).
- Q2 (migrations at startup, DEP-004): keep the **documented deploy step** (`deploy.sh` runs goose before switching; Lead recommends) or embed migrations and apply them at startup.
- Q3 (seed/demo data): include a dev-only `nh seed demo` command now (Lead recommends) or leave demo data to Phase 13.

### 1.3 Lead calls

- L1: Two batches: Fa (logging + correlation id + health), Fb (configuration doc, README refresh, seed, CI drift check).
- L2: Correlation id = UUIDv7 string; taken from a trusted header only for worker calls (authenticated), never from public webhook headers; stored in a new nullable `workflow_executions.correlation_id` column (migration 00042) and in job payload metadata.
- L3: `/readyz` checks: DB ping (1 s), goose version equals the binary's expected version, master key loaded, workflow worker heartbeat fresh, git sandbox status (ready/unavailable, informational). Any failing required check → 503.
- L4: `make generate` drift check in CI: run `make generate` and fail if `git status --porcelain` is not empty; also `goose validate` in CI if not there.

### 1.4 Boundaries

Out of scope: external log shipping (Loki, ELK), tracing (OpenTelemetry), Docker Compose and fresh-install test (Phase 13), admin UI (Phase 11), multi-slot workers (Phase 8, deferred).

## 2. Requirements and acceptance

| REQ | Observable behaviour | PCC | Task |
|---|---|---|---|
| REQ-1 | Every log line of the server and `nh` is one JSON object (slog) with `time`, `level`, `msg`, `component`; existing `log.Print*` output goes through the same handler; startup, panic recovery and shutdown lines are kept. Level from setting `log_level` (default info). | NFR-025, Go criteria | F1 |
| REQ-2 | Each inbound webhook, Telegram update and worker call gets a correlation id; the id appears in every log line it causes, on the workflow execution row and in every step log of that execution, also after a restart. Public request headers cannot set it. | NFR-026 | F1 |
| REQ-3 | No log line contains message text, prompts, tool payloads, tokens, Authorization headers or DSNs (tested by planting values on the main paths). | NFR-025, Gate C | F1 |
| REQ-4 | `GET /healthz` on the public server → 200 `ok`, no DB access, no detail. `GET /readyz` on the loopback ops listener → 200 or 503 with per-check JSON (L3), bounded to 2 s. Neither leaks secrets. | NFR-031, DEP-005 | F2 |
| REQ-5 | At startup the hub logs one readiness summary (same checks) and refuses to start when DB or master key fail (current behaviour kept, now logged structurally). | DEP-005 | F2 |
| REQ-6 | `docs/configuration.md` lists every env var and every settings key with default, range and effect; a test or script compares it with `internal/settings` specs so it cannot go stale. | DEP-002, NFR-033 | F3 |
| REQ-7 | README "Run it locally" works on a fresh clone with fakes: DB, migrations, demo seed, hub, one demo webhook producing a notice in the fake Telegram. | NFR-033, NFR-037 | F3 |
| REQ-8 | `nh seed demo` (dev only: refuses unless the DB name ends with `_dev` or `_test` or a `--force-dev` flag on loopback) creates one workspace, integrations with fake credentials, people and one behavior. | NFR-033 | F3 |
| REQ-9 | CI fails when `make generate` changes files or `goose validate` fails; deploy step for migrations (DEP-004) documented in the runbook and configuration doc. | NFR-035, NFR-038, DEP-004 | F3 |
| REQ-10 | Secrets never committed: gitleaks step (Phase 12) referenced in the configuration doc; `.env` and key files in `.gitignore`. | DEP-003 | F3 |

## 3. Domain and interface decisions

- `internal/obs`: `Setup(level, w) *slog.Logger`, `WithCorrelation(ctx, id) context.Context`, `Correlation(ctx) string`, `Logger(ctx) *slog.Logger`, `BridgeStdLog(*slog.Logger)`. Domain packages take a `*slog.Logger` or use `obs.Logger(ctx)`; no global mutable state beyond the std-log bridge set in `main`.
- Echo middleware assigns the correlation id at entry; the webhook store path writes it into the inbound event; the workflow engine copies it onto the execution; workers read it from the execution.
- `/readyz` handler sits in the ops listener built in Phase 12 (`metrics_addr`).

## 4. File ownership map

| Area | Files |
|---|---|
| Logging, correlation (F1) | new `internal/obs/`, `cmd/server/main.go`, `cmd/server/wiring.go`, `internal/delivery/http/*`, `internal/workflow/*`, `internal/behavior/worker_jobs.go`, migration `00042`, `db/query/workflow*.sql`, `cmd/setup-notices/main.go` |
| Health (F2) | `cmd/server/main.go` (routes), ops listener file from Phase 12, new `internal/obs/health.go` |
| Docs, seed, CI (F3) | `docs/configuration.md`, `README.md`, `docs/runbooks/self-host-home.md`, `cmd/setup-notices/seed.go`, `Makefile`, `.github/workflows/ci.yml`, `.gitignore` |

## 5. Task packets

- **Fa** — F1: REQ-1..3, commit `feat(obs): structured logs and correlation ids`. F2: REQ-4..5, commit `feat(obs): health and readiness endpoints`.
- **Fb** — F3: REQ-6..10, commit `docs: configuration, local run and demo seed` (+ `ci: generate drift check`).

## 6. Agent dispatch and evidence gates

```text
F1 → F2 → Batch Fa Tester/CI (REQ-1..5) → Reviewer → fixes
F3 → Batch Fb Tester/CI (REQ-6..10) → Reviewer → fixes
same-head PASS + APPROVE → completion report → owner merges
```

- Agents: opencode agents through `scripts/agent-handoff.sh` (implementer, tester, reviewer); Lead writes briefs and judges.
- Live check after deploy, with the owner: `journalctl` lines are JSON with correlation ids; `/healthz` via the tunnel; `/readyz` on loopback only.

## 7. Phase completion gate

- Test Report PASS and Review Report APPROVE on the same code head; CI green with no skip, race or panic.
- `docs/product/TRACEABILITY.md` updated for the IDs above; Phase Completion Report in `docs/product/reports/`.
- Merge into `develop` only on the owner's word.

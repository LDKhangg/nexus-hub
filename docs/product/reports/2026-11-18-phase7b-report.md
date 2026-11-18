# Phase Completion Report

Phase: 7b — AI orchestrator

Status: Implemented, tested and reviewed on `feature/phase7b` at code head `e4c979b` (tests `475d68b`). Not merged into `develop` yet (owner merges with `/merge` in Telegram). The branch also carries `ci/reuse-branch-tests` (629f7b9): a develop merge whose tree equals an already green branch head skips the duplicate test run.

Implemented requirements: REQ-1 … REQ-17 of `docs/plans/2026-11-16-phase7b-ai-orchestrator.md` (PCC FR-043–FR-056, FR-049/FR-060 delegation, ARCH-002, ARCH-004, NFR-034, FR-131/132 carry-ins). Owner decisions O1–O5 all a; entry `/plan` (`/task` unchanged). REQ-17 route-eval numbers run on the VPS after the merge deploys; the result is appended to the Phase 6b report.

Delivered components:
- Provider-neutral `AIRuntime` (internal/orchestrator) with adapters in internal/adapters/llm and a deterministic `FakeRuntime`; versioned JSON schemas (classify v1/v2, plan, evaluate) with a strict validator and bounded repair that feeds back only path + rule (never model output).
- `Classify`, `Plan` (Go checks: keys, known capabilities, DAG, task cap, budget clamp), `Evaluate` (7 decisions, evidence refs ⊆ input); untrusted text JSON-encoded inside a delimited data block.
- Least-privilege capability registry (investigate → behavior-actions → local-worker; the model cannot raise privilege); budgets (plan iterations 3, replans 2, repair 2, tokens 80k, wall 90 min — wall raised from 20 by Lead ruling because worker jobs run up to 60 min) checked before every model call incl. repair and investigate (combined token cap), checkpointed and restored; O4a gate "Thêm 1 vòng"/"Huỷ" (≤3 extensions).
- Durable workflow `orchestrated-task` (migration 00029): plan → up to 30 × (run_task → evaluate) → report; retries never repeat model calls; interrupted in-process tasks fail `executor_interrupted`; REPLAN v+1 keeps finished tasks (immutable plan rows, migration 00028); COMPLETE only with executor evidence (worker tasks need job id + branch/PR/test); Go conversions audited (`converted_from`).
- Executors: `investigate` (read-only Tier-2 loop, read tools only, durable `tool_runs` audit owned by the workflow execution, migration 00030) and `local-worker` (DELEGATE `repo.write` → 7a worker job; repo: explicit registered → remembered → only registered → clarification; waits with no VPS fallback).
- Telegram: `/plan <request>` (topic or `/plan <TASK-ID>`; unregistered repo word refused), assistant tool `start_task` (≤2000 runes, `too_long` beyond), plan message (keys, deps, executor, caps), clarification bound to WF-n resumed by the owner's reply, `/status WF-n` extras; CLI `nh workflow show` orchestrator object; runbook "Orchestrated tasks (Phase 7b)"; architecture §7.
- Tier-1 comment triage now runs through Classify (classify@2) with the same TriageDecision contract; legacy prompt fallback for providers without structured output.
- Carry-ins: 6b R7 (Recover reopen runs the lifecycle hook), case-insensitive custom IDs (`fc-6583`), command behaviors by custom ID, router prompt rules.
- 7a bug fixed on the way: a follow-up instruction resent while being written was applied twice (D-B / R-A-1).

Tests executed: full suite with `-race` against throwaway PostgreSQL in CI only, fakes only. Batch A (orchestrator units, store, carry-ins, test debt TD-7A-2..4 and TD-TT-1..7), Batch B1 (steps flow, worker path, sayPlan), Batch B2 (investigate, migrations 00029/00030 incl. Down after use, wake query, /plan, start_task, /status, Classify triage equivalence, CLI).

Test results: CI run 44225394222 on `475d68b`: success, 2424 tests, no skip, race or panic.

Evidence:
- Batch A: Tester defects D-A (repair echoed a key) and D-B (double-applied instruction) → 160123a/a67a7c3. Review A CHANGES: R-A-1 Important (taken set never read), R-A-2 Important (carried task rows lost), R-A-3 Important (model could raise executor privilege), R-A-4 Minor (repair skipped budget) → a67a7c3; re-review APPROVE; CI 44042781670 green on f65d91f.
- Batch B: Review B CHANGES on 46871ed: R-B-1 Important (investigate token overspend), R-B-2..5 Minor (unregistered repo, evidence, interrupted retry, 00029 Down) → f7505b8; re-review APPROVE; R-B-6 Minor (start_task length) → e4c979b, final review APPROVE. CI 44225394222 green on 475d68b.
- Lead rulings: REQ-16 replaced two hotfix tests' old lowercase rule; least privilege replaced "recommendation honoured" test expectations; wall budget 90 min.
- Agents: Claude subagents per role (Sonnet: C1, P1, P4c, P5a, P6, tests, small fixes; Opus: P2–P4b, P5b, main fixes, reviews). Ledger `.superpowers/sdd/phase7b/progress.md` (local).

Known limitations:
- Live checks pending after deploy: `/plan` on a real task, route-eval (REQ-17), and still TD-7A-1 (laptop).
- `behavior-actions` executor (clickup.write, telegram.notify) is not wired; Plan is never offered those capabilities (Phase 8).
- Orchestrated worker jobs show no 7a progress message or Retry/Cancel buttons; the topic gets the orchestrator's report.
- Worker waits use WAITING_FOR_USER (no WAITING_FOR_WORKER state); wall time is checked when the execution wakes, so a worker that never returns ends at the 24 h deadline or `/cancel`.
- 30 task runs per execution; one investigate step may overshoot the token cap by at most one model step.
- 00026 (7a) Down still fails once local tasks were used (deployed migration, not changed).
- Claude usage statusLine (owner deferred).

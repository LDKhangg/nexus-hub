# Phase Completion Report

Phase: 6a — Assistant model routing and input-token savings

Status: Implemented, tested and reviewed on `feature/phase6a` at `8bd07d9`. Not merged into `develop` yet (owner decision).

Implemented requirements: REQ-1 … REQ-10 of `docs/plans/2026-11-16-phase6a-assistant-routing.md` (PCC FR-042, FR-043 assistant part, FR-056, NFR-030 partial, ARCH-004). Owner decisions: D1 fast/work/deep tiers, D2 model footer, D3 dropped.

Delivered components:
- Settings `assistant_fast_model` (Haiku), `assistant_model` (now Sonnet by default), `assistant_deep_model` (Opus), `assistant_history_turns` (6).
- History sent to the model = persisted summary + last N exchanges; constant system prompt (the WF hint moved into the owner turn); rolling Anthropic cache breakpoint on the newest turn (system/tools breakpoints kept, ≤ 4).
- Router: one structured call on the fast model (no tools) answers greetings, thanks, acknowledgements and general questions, or escalates to `work` (Sonnet + tools) or `deep` (Opus + tools). Every failure escalates to `work`. A reply to an assistant question (last line ends in `?`/`？`) skips the router (`work_followup`). WF-targeted turns and CLI providers keep the old path.
- `/deep <text>` runs on the deep model without the router; the text is staged in memory, never stored. `/deep` is reserved for behaviors.
- New domain interface `domain.JSONProvider`; the Anthropic adapter implements it with `output_config` json_schema.
- Footer `· haiku|sonnet|opus` on sent answers only; per-turn metadata usage log with route, model, tokens, `router_*` tokens, steps, latency (no text). Runbook section "Assistant models and routing (Phase 6a)".

Tests executed: full suite with `-race` against throwaway PostgreSQL in CI, fakes only. Batch TC-6A01–TC-6A50 plus regressions TC-6A60–TC-6A65.

Test results: CI run 43803113157 on `8bd07d9`: all jobs success; no skip, race or panic.

Evidence:
- Test Report #1 (b0b2447): D-1 router JSON with a trailing `}` accepted → fixed `2a088b8`; CI 43802248169 green.
- Review #1 on `2a088b8`: R-1 Important (a one-word reply to an assistant offer got a fast answer, action lost), R-2 Minor (router paid cache writes), R-3 Minor (router tokens mixed into the work model's counts) → fixed `27919af`; regressions `8bd07d9`.
- Review #2 APPROVE on `8bd07d9`.
- Agents: T1, T2 by the opencode GPT implementer; T3, tests and review by Claude subagents under the same role limits (GPT weekly quota low).
- Ledger: `.superpowers/sdd/2026-11-16-phase6a-assistant-routing/progress.md` (local).

Known limitations:
- R-4 (Minor, accepted): a fast reply that ends with a question sends the owner's next message to Sonnet (`work_followup`); safe, but reduces savings. Planned fix in Phase 6b: the router ends its own replies without a question, or the history records which tier answered.
- Cross-turn cache hits are rare: the history window slides one exchange per turn and the cache TTL is 5 minutes (R-2 second half, deferred).
- The router's judgement (REQ-9) is proven structurally with fakes; real-model quality is checked live after deploy.

# Phase 6a — Assistant Model Routing and Input-Token Savings Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `executing-plans` for an assigned task. The role boundaries and sequential handoff process in `AGENTS.md` override generic implementation/TDD instructions. Steps use checkbox (`- [ ]`) syntax for tracking.

**Status:** APPROVED by the owner on 2026-11-16 (D1, D2; D3 dropped). Easy tasks go to the opencode GPT implementer while its last weekly quota lasts; the hard task and the Tester/Reviewer rounds run as Claude subagents under the same role limits.

**Goal:** Free-text Telegram messages stop going to Opus with 15 tools every time. A cheap first pass decides: simple messages are answered at once by a fast model, work goes to a mid model with tools, hard work to the deep model. The history sent to the model shrinks and is cached, so tool loops pay mostly for new tokens. Each answer shows a small line with the model used.

**Architecture:** In `internal/behavior/assistant.go`, `converse` gains a router step before the existing tool loop: one structured call on `assistant_fast_model` with no tools, the bounded summary and the last turns, returning `answer` (with the reply text) or `escalate` (`work` | `deep`). `work` runs today's tool loop on `assistant_model`, `deep` on `assistant_deep_model`. The Anthropic adapter adds a rolling cache breakpoint on the newest turn. History sent = persisted summary + last N exchanges (setting).

**Tech Stack:** Go (go.mod version). No migration (settings are key/value rows). No new dependency.

**Spec:** `docs/product/PCC-001.md` FR-042 (limit and summarize context), FR-043 (intent classification, assistant part), FR-056 (configurable budget), NFR-030 (usage tracking), ARCH-004 (no vendor in domain logic). The contract is read-only.

## Global Constraints

- Code, documentation and commits are English; owner-facing discussion is Vietnamese.
- Baseline: `develop` at `69ab530`. Branch `feature/phase6a`. No migration.
- One handoff at a time per shared working tree. Implementer changes production only; Tester changes tests only; Reviewer is read-only.
- Lead/Implementer never run `go test`. Checks: `go build ./...`, `go vet ./...`, `gofmt -l cmd internal`, `make generate` (second run clean). The suite runs only in CI.
- **Never answer from the fast model when data or action is needed.** Any message that needs workspace data (ClickUp, GitHub, workflows, people, reports), a tool, an action, a workflow target, or that the router is unsure about, escalates. A router error, timeout, refusal or invalid output escalates to `work` (fail upward, never a fast answer).
- Turns with a resolved workflow target (Phase 6 reply / `WF-n` question) and assistant-tool confirmations skip the router and use `work`, as today.
- Providers without structured output or tool calling (CLI providers) keep today's single path; the router runs only when the provider supports structured output, and caching only on the Anthropic API adapter.
- Privacy unchanged: no prompt, owner text or answer in logs or DB. The per-turn usage log has route, model id, token counts (input, output, cache read, cache write), steps and latency only.
- Tests use fakes only.

## Review Focus

- The fast path can never reach data or actions; every failure fails upward.
- Cache breakpoints stay within the API limit (4) and never change message content or order.
- The model footer never leaks anything but the model's short name; it is not stored in history sent back to the model.
- Behavior with CLI providers and with workflow-targeted turns is unchanged.

## 1. Owner decisions (2026-11-16)

| ID | Decision |
|---|---|
| D1 | Fast model answers simple messages; `assistant_model` (Sonnet) handles work with tools; `assistant_deep_model` (Opus) when the router says `deep` or the owner starts the message with `/deep`. |
| D2 | Every assistant answer ends with a small line naming the model, e.g. `· haiku`, `· sonnet`, `· opus`. |
| D3 | Dropped: no interim model change (no real flow uses the assistant yet). |

## 2. Requirements and acceptance

| ID | Observable behavior | PCC IDs | Task |
|---|---|---|---|
| REQ-1 | Settings `assistant_fast_model` (default `claude-haiku-5-5`), `assistant_deep_model` (default `claude-opus-5-5`), `assistant_history_turns` (default 6, range 1–20); `assistant_model` default becomes `claude-sonnet-5-5`. Same validation as `assistant_model`. `nh settings list` shows them. | FR-056 | T1 |
| REQ-2 | The history sent to the model is the persisted conversation summary (when present) plus the last `assistant_history_turns` exchanges; the 64 KiB and per-answer bounds stay. The in-memory buffer may keep more for the summarizer. | FR-042 | T1 |
| REQ-3 | The system prompt is identical for every turn: the "replies to WF-n" hint moves from the system prompt into the owner turn (a fixed prefix line), so the system cache is not split per workflow. | FR-042 | T1 |
| REQ-4 | Every finished assistant turn logs one metadata line: route, model, steps, input/output/cache-read/cache-write tokens, latency. No text. | NFR-030 | T1 |
| REQ-5 | Each sent answer ends with `\n· <short>` where `<short>` is the family name derived from the model id actually used for the final answer (`haiku`, `sonnet`, `opus`, else the id without the `claude-` prefix, ≤ 32 bytes). The footer is not part of the stored history. | — | T1 |
| REQ-6 | The Anthropic adapter puts one ephemeral cache breakpoint on the last content block of the newest turn when the request asks for history caching (`CacheHistory`), in addition to the existing system/tools breakpoints (≤ 4 total). Plain `Generate` gets the same option for its last message. The assistant asks for it on every call. | FR-042 | T2 |
| REQ-7 | Router: for free text not covered by the skip rules, one structured call on `assistant_fast_model` (no tools, fixed system prompt, summary + last turns, max output ~600 tokens) returns `{"route":"answer","reply":"…"}` or `{"route":"escalate","tier":"work"|"deep"}` validated against a schema. `answer` sends the reply (footer: fast model). `work`/`deep` run the existing tool loop on `assistant_model`/`assistant_deep_model`. Any error or invalid output → `work`. | FR-043 | T3 |
| REQ-8 | `/deep <text>` skips the router and runs the tool loop on `assistant_deep_model`; `/deep` alone replies with a short usage hint. Listed in `/help`. | FR-056 | T3 |
| REQ-9 | The router prompt makes `answer` legal only for greetings, thanks, acknowledgements, and general questions answerable without workspace data; anything mentioning tasks, PRs, workflows, people, reports, status, settings, or asking to do something escalates. A reply that claims workspace facts is not checked by Go, so the prompt and the tests carry this rule; the router's reply is capped (2,000 runes). | FR-043, ARCH-002 | T3 |
| REQ-10 | Runbook section "Assistant models and routing (Phase 6a)": the three settings, `/deep`, the footer, the usage log fields. | NFR-034 | T3 |

## 3. Task split (by difficulty)

| Task | Difficulty | Agent | Files |
|---|---|---|---|
| T1 — settings, history window, stable system, usage log, footer | Easy | opencode `implementer` (GPT) | `internal/settings/settings.go`, `internal/behavior/assistant.go`, `assistant_history.go`, `cmd/server/main.go` |
| T2 — rolling history cache breakpoint | Easy | opencode `implementer` (GPT) | `internal/domain/llm.go`, `internal/adapters/llm/anthropic_tools.go`, `anthropic.go`, `internal/behavior/assistant.go` (set the flag) |
| T3 — router, `/deep`, runbook | Hard | Claude subagent (implementer limits) | `internal/behavior/assistant.go`, new `internal/behavior/assistant_router.go`, `inbox_worker.go`/`commands.go` (`/deep`, `/help`), `docs/runbooks/self-host-home.md` |
| Test batch | — | Claude subagent (tester limits) | `internal/behavior/*_test.go`, `internal/adapters/llm/*_test.go`, `internal/settings/*_test.go` |
| Review | — | Claude subagent (reviewer limits) | read-only |

If the GPT quota ends during T1 or T2, the Lead hands the rest of that brief to a Claude subagent with the same limits.

## 4. Agent dispatch and evidence gates

```text
T1 (GPT) → T2 (GPT) → T3 (Claude) → Tester/CI → Reviewer → fixes → same-head PASS + APPROVE
→ report → merge after the owner's OK (deploys to the VPS) → Phase 6b
```

Test focus: fast answer only for the allowed classes (fake provider); every failure mode escalates; skip rules (workflow target, confirmations, CLI provider); `/deep`; footer present and absent from history; history window = summary + N; system prompt identical across turns with and without a WF target; breakpoint count and placement in the Anthropic request (httptest fake); usage log has no text.

## 5. Phase completion gate

- [ ] All REQ-1–10 cases have CI evidence on the final head; no skip, race or panic.
- [ ] Reviewer APPROVE on the same head; Critical/Important findings resolved.
- [ ] Lead updates `docs/product/TRACEABILITY.md`, writes `docs/product/reports/<date>-phase6a-report.md`. Merge into `develop` needs the owner's OK.

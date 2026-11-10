# Phase 2: Agent Core, Multi-Tool Execution and GitHub/Code Tools Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** When a ClickUp comment arrives, Nexus-Hub runs the `ARCHITECTURE.md` request flow durably:
- filter → durable debounce → sanitized context packet → Tier-1 triage;
- when needed, a Tier-2 ReAct actor coordinates many tools: GitHub REST with the right account or GitHub App, local `git` code reading, and the ClickUp API;
- every tool call passes a strict, tiered permission policy, and the result is reported one-way to Telegram;
- token use is kept low and measured.

**Architecture:**
- The Phase 1 ingest transaction also enqueues a per-thread *agent job*. Its run time is pushed back for coalescing.
- An agent worker builds a sanitized context packet, and Tier-1 picks Ignore, Notify, or Investigate.
- Investigate runs a bounded tool-calling loop. Every call goes through one **executor**, which enforces:
  - tool tiers (read / write / dangerous) and per-repository rules;
  - per-job budgets, dry-run, timeouts, output caps, and secret redaction;
  - audit rows with idempotency keys, so a retried job never repeats a write.
- GitHub is called over REST with a token per configured account: a fine-grained PAT, or a GitHub App installation token minted per call and down-scoped to the repo.
- Code is read from configured local clones through a hardened `git` runner: no shell, minimal environment, external diff/textconv/fsmonitor disabled.
- Results go to Telegram through a durable outbound outbox.

**Tech Stack:** Go (existing module, stdlib `net/http`, `crypto/rsa`, `go/parser`), Echo v4, pgx/v5, sqlc, goose, PostgreSQL, `anthropic-sdk-go/v2` tool use and prompt caching, the `git` CLI, GitHub REST API v3 (`X-GitHub-Api-Version: 2022-11-28`), ClickUp API v2, Telegram Bot API `sendMessage`.

**Spec:** The embedded design below is the Phase 2 specification. It is derived from `ARCHITECTURE.md` §1–§2 and the user's decisions of 2026-11-10:
- Code-first. The core must be solid enough to coordinate many tools before any reply feature. Replies and approvals move to a later phase.
- The AI may call tools, including write and merge/close tools, under strict, configurable permissions.
- No `gh` CLI. Use plain `git` plus the GitHub API with the correct account. Support **both** fine-grained PATs and a GitHub App.
- Add a dedicated code-reading toolset, an input/output sanitizer, and output-token reduction.
- Approved earlier: durable debounce in PostgreSQL, a separate `outbound_actions` outbox, Auto-Pilot off. Owner comments are context only.

Decisions marked **[Claude's call]** need approval with this plan.

## Global Constraints

- Phase 1 behavior is unchanged for sources without `agent.enabled`. Nexus-Hub posts nothing to ClickUp in Phase 2.
- Never modify migrations 00001/00002 or hand-edit generated sqlc code. New schema goes in `00003_create_agent_core.sql`; regenerate with `make generate`.
- **Tools are explicit, typed Go implementations.** There is no generic command/HTTP tool and no shell. `git` argv is built from validated fields only. A user-supplied path or ref always follows `--` or is validated not to start with `-`.
- **Permission tiers.**
  - `read`: needs the repo in `github.repos`.
  - `write`: needs the tool in that repo's `allow`.
  - `dangerous` (merge/close): needs the tool in that repo's `allow`, **and** the env `AGENT_ALLOW_DANGEROUS=1`, **and** at most 1 dangerous call per job, **and** the tool's own preconditions.
  - Nothing is allowed by default.
- Every tool call, including denied and dry-run calls, gets a `tool_runs` audit row. A write or dangerous call is recorded as `started` **before** execution. A retry never re-executes a call that has a `started` or `ok` row with the same idempotency key.
- **Secrets.**
  - DB config stores env-variable names only.
  - Tokens are injected only into the in-process HTTP request that needs them. They are never sent to a subprocess, a log, the LLM, or Telegram.
  - `git` subprocesses get only `PATH`, `HOME`, `LANG`, `GIT_CONFIG_NOSYSTEM=1`, `GIT_TERMINAL_PROMPT=0`, `NO_COLOR=1`.
  - Every tool output and outbound text passes the sanitizer: exact-value redaction plus secret-pattern redaction.
- Comment content, tool output, and repository content are untrusted. The policy, not the prompt, is the security boundary.
- Never log tokens, Authorization headers, prompts, tool inputs and outputs, or payloads. Logs carry IDs and error codes only.
- English identifiers, comments, docs. Talk to the user in Vietnamese.
- DB tests use the guarded `TEST_DATABASE_URL` (loopback, `nexus_storage_test`). Tests never call real GitHub, ClickUp, Telegram, or Anthropic: they use httptest servers, plus temporary git repos created in the test.
- Commit locally on `feature/phase2` at the end of each task. No push or merge without asking.

## Review Focus

1. A prompt-injected comment or repository file that tries to merge, close, comment, read `.env`, or touch a non-configured repo must never get past the policy. Each attempt gives a `denied` audit row and an `is_error` tool result. Covered in Tasks 7, 11, 12, 14.
2. A crash mid-loop followed by a retry must not repeat a write or merge. An ambiguous call (`started` with no result) is reported as "unknown outcome", never re-run. Covered in Tasks 7, 10, 15.
3. A malicious local repo config (`core.fsmonitor`, `diff.external`, textconv drivers, hooks) or a symlink/`..` path must not execute code or escape the clone. A hanging `git` is killed with its process group. Covered in Tasks 8, 12.
4. A wrong account (the token's `login` differs from the config), an expired App token, GitHub rate limits, or a repo redirect must fail visibly with a safe code and never fall back to another account. Covered in Tasks 9, 17.
5. Malformed LLM JSON, invented tool names, the step or token budget being exceeded, a refusal, or a missing API key must end in a Telegram report or fallback, never a lost comment. Covered in Tasks 1, 6, 14, 15.

---

## Embedded Design / Approval Contract

### Request flow (per ClickUp comment)

1. **Ingest.** For a source with `agent.enabled`:
   - Comment from someone else: save as `external_user`, then upsert the thread's pending agent job (no notice).
   - Comment from `own_sender_id`: save as `human_owner`. Context only.
   - Status change: unchanged Phase 1 notice.
2. **Durable debounce.** At most one `pending` job per thread. `run_at = LEAST(now()+DEBOUNCE_WINDOW, first_enqueued_at+DEBOUNCE_MAX_WAIT)`, using the existing defaults of 8s and 30s.
3. **Context packet.**
   - Contents: summary ≤ 800 runes; the last 3 earlier messages, ≤ 300 runes each; the new `external_user` batch after the last done job's `through_message_id`, ≤ 1200 runes, oldest dropped first.
   - Every part is passed through `Sanitizer.CleanInput` first, and truncated parts carry the marker ` [truncated]`.
4. **Tier-1 triage.** `TRIAGE_MODEL` (default `claude-haiku-5-5`), **300 max tokens**, JSON only:
   `{"category":"ignore|notify|investigate","urgency":"low|normal|high","confidence":0..1,"summary":"≤200 runes","goal":"≤300 runes, required for investigate"}`
5. **Routing.** `threshold` defaults to 0.85.
   - `ignore` with confidence ≥ threshold: Ignore.
   - `investigate` with at least one usable tool: Investigate.
   - Everything else: Notify.
6. **Tier-2 actor.**
   - `ACTOR_MODEL` (default `claude-sonnet-5-5`), **1024 max tokens per step**, at most **5 steps**, at most **12 tool calls**.
   - Per-job token budget: `AGENT_MAX_INPUT_TOKENS` (60000) and `AGENT_MAX_OUTPUT_TOKENS` (6000), counted across Tier-1, Tier-2, and the compressor.
   - The loop stops on `end_turn`. The report is ≤ **1500 runes**, at most 8 bullet lines.
   - Stop reasons: `step_limit`, `max_tokens`, `token_budget`.
7. **Outcomes.** Each is committed atomically with an `ai_router` message log entry; Investigate also gets an `ai_actor` entry.
   - *Ignore:* nothing else.
   - *Notify:* outbound `telegram_message`: `🧠 Nexus · <title>\n<urgency> · <summary>\n<task url>`.
   - *Investigate:* outbound `telegram_message`: `🔎 Nexus · <title>\n<urgency> · <summary>\n\n<report>\n\nTools: <name ✓|✗|⛔|🧪 ...>\nTokens: <in>/<out>\n<task url>`.
     - Write and dangerous calls are always listed, with `(write)` / `(DANGEROUS)` labels; `🧪` marks a dry run.
     - All outbound text passes `Sanitizer.CleanOutbound`.
8. **Failures.** A job gets up to 5 attempts, with backoff 5/15/30/60s.
   - Final immediately: `llm_unconfigured`, `llm_refused`.
   - On a final failure, enqueue `⚠️ AI triage unavailable (<code>)` followed by `FormatNotice` of each batch message.
9. **Rolling compressor.** After a successful job, if the thread has ≥ 5 messages after the summary cursor (excluding the newest 3), summarize with `SUMMARY_MODEL` (default `claude-haiku-5-5`, 300 max tokens, ≤ 1200 runes). Update atomically under `LockMessageThread`. Best effort.

### Sanitizer (`internal/agent/sanitizer.go`)

- **`CleanInput(s)`.**
  - Remove C0 and C1 control characters except `\n` and `\t`.
  - Remove zero-width characters: U+200B–U+200D, U+2060, U+FEFF.
  - Remove bidi controls: U+202A–U+202E, U+2066–U+2069. This blocks Trojan-Source tricks.
  - Collapse runs of 3+ blank lines into 2, and trim trailing spaces.
- **`RedactSecrets(s)`.** Exact values from the `Redactor`, plus these patterns, each replaced with `[redacted]`:

  | Pattern | Secret type |
  |---|---|
  | `gh[pousr]_[A-Za-z0-9]{36,}` | GitHub tokens |
  | `github_pat_[A-Za-z0-9_]{60,}` | GitHub fine-grained PAT |
  | `sk-ant-[A-Za-z0-9_-]{20,}` | Anthropic API key |
  | `xox[abprs]-[A-Za-z0-9-]{10,}` | Slack token |
  | `AKIA[0-9A-Z]{16}` | AWS access key |
  | `\b\d{6,12}:[A-Za-z0-9_-]{30,}\b` | Telegram bot token |
  | PEM `-----BEGIN … PRIVATE KEY-----` … `-----END … PRIVATE KEY-----` blocks | private keys |
  | password part of `postgres(ql)?://user:pass@` | DB passwords |
  | `Authorization: \S+` | Authorization headers |

- **`CleanOutbound(s, NeutralizeMentions bool)`.** `CleanInput` + `RedactSecrets`. When mentions are neutralized, each `@login` becomes `` `@login` ``, so an AI comment cannot ping people. GitHub comment bodies always neutralize mentions.

### Tool core

- **`Tool`.** `Spec() ToolSpec` returns `{Name, Description, InputSchema, Tier read|write|dangerous}`. `Run(ctx, Call) (ToolOutput, error)`, where `Call{Input json.RawMessage; Repo *RepoGrant; DryRun bool}`. `ToolOutput{Text, Summary}`; for writes, `Summary` (≤ 300 runes) is stored for idempotent replay.
- **`RepoScoped`.** Optional `Repo(input) (string, error)`. Tools that name a repository implement it, and the executor resolves the `RepoGrant` (account, local path, allow list, merge rules) before running.
- **`Executor.Call(ctx, step, name, input) CallResult`.** Always returns a result, never an error. It runs these checks in order:
  1. tool exists
  2. tool listed in `tools.enabled`
  3. input is valid JSON ≤ 16 KiB
  4. repo resolution: the repo is configured
  5. tier gate: a write or dangerous tool is in the repo's `allow`; a dangerous tool also needs `AGENT_ALLOW_DANGEROUS=1`
  6. budgets: calls ≤ 12, writes+dangerous ≤ `max_writes_per_job` (default 2), dangerous ≤ 1
  7. idempotency (non-read only): key = `sha256(job_id|tool|canonical input)`; a prior `ok` returns its stored summary plus `already executed`, a prior `started` returns `unknown_outcome`
  8. record `started`
  9. if `dry_run` and the tier is non-read: return `DRY RUN: would <summary>` and record `dry_run`
  10. run with `TOOL_TIMEOUT` (30s)
  11. sanitize, compact, and cap the output at **6 KiB**
  12. record the final status

  A failure at checks 1–6 records `denied` with a code.

### GitHub access (REST, no `gh`)

- **Accounts**, under `agent.tools.github.accounts`:
  - **PAT:** `{"type":"pat","token_env":"NEXUS_GITHUB_TOKEN_X","login":"LDKhangg"}`. Before first use in each process, `GET /user` must return `login` equal to the configured value (case-insensitive), cached for 10 minutes. A mismatch gives the error code `account_mismatch`, with no fallback.
  - **GitHub App:** `{"type":"app","app_id":"123","installation_id":"456","private_key_env":"NEXUS_GITHUB_APP_KEY_PATH"}`.
    - The env var holds a **path** to the PEM file. The file must be mode `0600` or stricter, otherwise the error is `key_permissions`.
    - Mint an RS256 JWT with `iat=now-60s`, `exp=now+9m`, `iss=app_id`, using stdlib `crypto/rsa` with no new dependency.
    - Call `POST /app/installations/{id}/access_tokens` with `{"repositories":["<repo name>"],"permissions":{…least needed for the tool…}}`.
    - Cache the token per (installation, repo, permission set) until `expires_at - 5m`.
- **Client.**
  - Base `https://api.github.com`, private with a test override.
  - Headers: `Accept: application/vnd.github+json`, `X-GitHub-Api-Version: 2022-11-28`, `User-Agent: nexus-hub`.
  - Redirects are never followed: a 301 gives `repo_moved`.
  - Responses are read up to 2 MiB.
  - Errors:
    - 401: `unauthorized`
    - 403 with `x-ratelimit-remaining: 0` or 429: `rate_limited`, retryable, honouring `retry-after` or `x-ratelimit-reset`
    - other 403: `forbidden`
    - 404: `not_found`
    - 409/422: `conflict`
    - 5xx and transport: retryable
- **Tools [Claude's call].** All take `repo` (`owner/name`, validated by `ValidRepo`), and outputs are compact text, not raw JSON.

  | Tool | Tier | Input | API call | Output / rules |
  |---|---|---|---|---|
  | `github_pr_view` | read | `{repo, number}` | `GET /repos/{r}/pulls/{n}` | `#n title [state, draft?] author head→base sha=<7> mergeable=<x> url` |
  | `github_pr_checks` | read | `{repo, number}` | head SHA, then `GET /commits/{sha}/check-runs?per_page=100` and `GET /commits/{sha}/status` | one line per check: `name state/conclusion`, plus a totals line |
  | `github_pr_files` | read | `{repo, number, include_patch?}` | `GET /pulls/{n}/files?per_page=100` | `status +a/-d path` per file; patch per file ≤ 1500 runes; ≤ 100 files |
  | `github_issue_view` | read | `{repo, number}` | `GET /issues/{n}` | title, state, labels, body ≤ 1500 runes |
  | `github_pr_comment` | write | `{repo, number, body}` | `POST /issues/{n}/comments` | body ≤ 4000 runes, `🤖 ` prefix, mentions neutralized |
  | `github_pr_merge` | dangerous | `{repo, number, head_sha, method}` | tool re-fetches the PR and checks, then `PUT /pulls/{n}/merge` with `{merge_method, sha: head_sha}` | see merge preconditions below |
  | `github_pr_close` | dangerous | `{repo, number, comment?}` | `PATCH /pulls/{n}` `{state:"closed"}`; optional comment first | |
  | `github_issue_close` | dangerous | `{repo, number, reason: completed\|not_planned, comment?}` | `PATCH /issues/{n}` | |

  - **Merge preconditions**, each failure a `precondition_failed:<reason>` error:
    - not a draft;
    - state is open;
    - `head.sha` equals `head_sha`;
    - `base.ref` is in the repo's `merge_base_branches` (empty means no branch; `main`/`master` only if listed);
    - `method` is in `merge_methods`;
    - if `require_checks_green` (default true): every check run is `completed` with a conclusion in {success, neutral, skipped}, and the combined status is not failure/pending.
  - Deliberately **not** implemented: branch delete, force push, approve review, edit files, repo settings, release, workflow dispatch.
- **ClickUp read tool:** `clickup_get_task {task_id}`, returning name, status, assignee names, due date, and url.

### Code-reading tools (local clones, `git` only) **[Claude's call]**

- The repo's `local_path` in config must be an absolute path to an existing git work tree, checked at startup and at call time.
- Content comes from **committed objects** (`ref`, default `HEAD`), never the working tree, so untracked files such as `.env` are invisible.
- Every invocation runs as `git -C <root> --no-pager` with these settings:
  - `-c core.fsmonitor=`, `-c core.hooksPath=/dev/null`, `-c diff.external=`, `-c core.pager=cat`, `-c safe.directory=<root>`, `-c protocol.allow=never`
  - plus `--no-ext-diff` and `--no-textconv` where the subcommand accepts them
- Validation and filtering:
  - `ref` matches `^[A-Za-z0-9._/@^~-]{1,100}$`, does not start with `-`, and does not contain `..`. Range refs are given as separate `base` and `head` fields.
  - `path` is relative, cleaned, has no `..` and no leading `/` or `-`, and is always passed after `--`.
  - Deny-list, applied to the inputs and filtered out of every listing or search result: `(^|/)\.env`, `\.pem$`, `\.key$`, `id_(rsa|ed25519)`, `(^|/)secrets?/`, `credentials`, `\.p12$`, `\.kdbx$`.

| Tool | Input | Command | Output limits |
|---|---|---|---|
| `code_list_files` | `{repo, path?, ref?, max?≤200}` | `git ls-tree -r --name-only <ref> -- <path>` | ≤ 200 paths and a count of omitted ones |
| `code_search` | `{repo, query, regex?:false, path?, ref?, max?≤50}` | `git grep -n -I --no-color [-F\|-E] -e <query> <ref> -- <path>` | ≤ 50 hits, each line clipped to 200 runes, grouped by file |
| `code_read_file` | `{repo, path, start_line?, end_line?, ref?}` | `git show <ref>:<path>` | ≤ 200 lines per call, line-numbered, 1 MiB object cap; binary files refused |
| `code_outline` | `{repo, path, ref?}` | `git show` content | Go files: `go/parser` declarations (func, method, type, const/var groups) with line numbers. Other files: a line-regex heuristic (`class`, `def`, `function`, `interface`, `type`, `export`, `func`) with line numbers. ≤ 150 entries |
| `git_log` | `{repo, path?, ref?, max?≤20}` | `git log --format=%h %ad %an %s --date=short -n <max> <ref> -- <path>` | |
| `git_diff` | `{repo, base, head, path?, stat_only?}` | `git diff --no-ext-diff --no-textconv --stat` then the patch | patch ≤ 6 KiB |
| `git_blame` | `{repo, path, start_line, end_line}` | `git blame --porcelain -L` | range ≤ 100 lines, compacted to `line sha author date` |

### Token reduction **[Claude's call]**

1. Tier-1 uses a small model and returns a tiny JSON (300 max tokens).
2. Notify never runs Tier-2.
3. The Tier-2 system prompt demands terseness: no preamble, no restating tool output, final report ≤ 8 bullets ≤ 1500 runes. Max 1024 tokens per step.
4. Tools return compact text projections instead of API JSON: per-call cap 6 KiB, line clipping, result limits.
5. **Loop compaction.** When building step *k*, `tool_result` contents from steps ≤ *k*−3 are replaced by `[elided: <tool> <status>, <n> bytes]`. Every `tool_use` keeps its `tool_result` block, as the API requires.
6. **Prompt caching.** `cache_control: ephemeral` is placed on the static system prompt block and on the last tool definition. Confirm the SDK field names via Context7 `/anthropics/anthropic-sdk-go` at implementation time.
7. **Usage accounting.** Every LLM call returns `Usage{Input, Output, CacheRead, CacheWrite}`. Jobs accumulate totals in `agent_jobs`, enforce the per-job budget, and report `Tokens:` in Telegram. `inspect` shows the totals.

### Configuration (ClickUp source `integrations.config.agent`)

```json
{
  "enabled": false,
  "confidence_threshold": 0.85,
  "persona": "",
  "clickup_token_env": "CLICKUP_API",
  "tools": {
    "enabled": ["github_pr_view", "github_pr_checks", "github_pr_files", "github_issue_view",
                "code_list_files", "code_search", "code_read_file", "code_outline",
                "git_log", "git_diff", "git_blame", "clickup_get_task"],
    "dry_run": false,
    "max_writes_per_job": 2,
    "github": {
      "accounts": {
        "personal": {"type": "pat", "token_env": "NEXUS_GITHUB_TOKEN_LDKHANGG", "login": "LDKhangg"},
        "workbot":  {"type": "app", "app_id": "123", "installation_id": "456", "private_key_env": "NEXUS_GITHUB_APP_KEY_PATH"}
      },
      "repos": {
        "LDKhangg/nexus-hub": {
          "account": "personal",
          "local_path": "/home/kane/Dev/nexus-hub",
          "allow": ["github_pr_comment"],
          "merge_methods": ["squash"],
          "merge_base_branches": [],
          "require_checks_green": true
        }
      }
    }
  }
}
```

- Validation:
  - every repo's `account` exists;
  - `allow` contains only known write/dangerous tools;
  - `merge_methods` ⊆ {merge, squash, rebase};
  - `max_writes_per_job` is 0..5;
  - env names are valid;
  - the `login` is required for a PAT;
  - `app_id` and `installation_id` are numeric strings.
- Environment, new: `TRIAGE_MODEL`, `ACTOR_MODEL`, `SUMMARY_MODEL`, `AGENT_POLL_INTERVAL` (1s), `TOOL_TIMEOUT` (30s), `AGENT_MAX_INPUT_TOKENS` (60000), `AGENT_MAX_OUTPUT_TOKENS` (6000), `AGENT_ALLOW_DANGEROUS` (unset means off), `GIT_PATH` (optional), plus the token and key env vars named in config.
- The server starts without an Anthropic key (jobs fall back with `llm_unconfigured`), without `git` (code tools are not registered), and without GitHub credentials (calls fail with `missing_credentials`).

### Decisions marked for approval **[Claude's call]**

- The tool list and tiers above. Merge and close are `dangerous`. Branch delete, force push, review approval, and file edits are not implemented at all.
- Dangerous tools need **three** switches: the repo `allow` list, the env `AGENT_ALLOW_DANGEROUS=1`, and the tool's own preconditions. They also have a limit of 1 per job. Merge requires the exact `head_sha`, green checks, and an explicitly listed base branch; `main` and `master` are never implicit.
- Write and dangerous tools run without per-call human approval, because approvals are deferred. `dry_run` lets you watch what the agent *would* do first.
- The code tools read committed content only (`ref`, default `HEAD`), never uncommitted working-tree files.
- The GitHub App private key is read from a file path held in an env var. The file must be mode 0600, and app tokens are down-scoped per repo and per tool.
- PAT account identity is verified with `GET /user` against the configured `login`.
- Own tool loop instead of the SDK beta tool runner.

### Non-goals (later phases)

- Replies and approvals (Telegram buttons, ClickUp comments, Auto-Pilot).
- GitHub as a webhook source. Repo sync (`git fetch`) by the agent.
- Code edits or commits by the agent. pgvector RAG. BYOI and encryption. Multi-provider LLM. VPS deploy.
- Live acceptance and independent review (Phase 3).

---

## File Structure

| Path | Responsibility |
|---|---|
| `internal/agent/domain.go`, `decision.go` | Triage types, parse, route |
| `internal/agent/sanitizer.go` | `CleanInput`, `RedactSecrets`, `CleanOutbound`, `Redactor` |
| `internal/tenant/agent_config.go` | Agent, tool, account, and repo configuration |
| `db/migrations/00003_create_agent_core.sql`, `db/query/{agent,action,tool_run}.sql` | Schema + queries |
| `internal/adapters/storage/notice_repository.go` (modify) | Agent-aware ingest |
| `internal/memory/builder.go`, `compressor.go` | Packet, rolling summary |
| `internal/config/agent.go` | Env settings |
| `internal/agent/prompts.go`, `tier1_router.go`, `tier2_actor.go` | Tier-1, Tier-2 |
| `internal/agent/tools/{tool,registry,executor}.go` | Tool core |
| `internal/adapters/tools/execrunner/runner.go` | Hardened subprocess runner |
| `internal/adapters/github/{client,auth,app_jwt}.go` | REST client, PAT/App credentials |
| `internal/adapters/tools/github/{read,write,dangerous}.go` | GitHub tools |
| `internal/adapters/tools/code/{git,tools,outline}.go` | Code-reading tools |
| `internal/adapters/tools/clickup/task.go` | ClickUp read tool |
| `internal/adapters/storage/{tool_run_store,agent_store,action_store}.go` | Audit, jobs, outbox |
| `internal/domain/llm.go`, `port.go` (modify), `internal/adapters/llm/anthropic.go` (modify), `usage.go` | Tool calling, caching, usage |
| `internal/agent/orchestrator.go`, `internal/relay/action_worker.go`, `internal/adapters/control/telegram/control.go` | Workers, Telegram send |
| `cmd/server/main.go`, `cmd/setup-notices/main.go`, `.env.example`, `docs/runbooks/clickup-telegram-local.md` | Wiring, CLI, docs |

---

### Task 1: Triage decision parsing and routing

**Files:**
- Modify: `internal/agent/domain.go`
- Create: `internal/agent/decision.go`, `decision_test.go`
- Delete: the empty placeholders `processor.go` and `route.go` (`sanitizer.go` is replaced in Task 2)

**Interfaces:**
- Produces:
  - `Category` with constants `ignore|notify|investigate`.
  - `Urgency` with constants `low|normal|high`.
  - `TriageDecision{Category; Urgency; Confidence float64; Summary, Goal string}`
  - `ParseTriageDecision(text string) (TriageDecision, error)`, with wrapped `ErrInvalidDecision`.
  - `RoutingPolicy{Threshold float64; ToolsAvailable bool}`
  - `Outcome` with constants `OutcomeIgnore`, `OutcomeNotify`, `OutcomeInvestigate`.
  - `Route(TriageDecision, RoutingPolicy) Outcome`

- [ ] **Step 1: Write failing tests** `TestParseTriageDecision`, `TestRoute`:
  - Parse:
    - plain, fenced, or prose-prefixed JSON → ok
    - two objects → the first is used
    - confidence outside [0,1] → error
    - unknown category → error
    - missing urgency → `normal`
    - `investigate` with an empty goal → error
    - summary over 200 runes or goal over 300 runes → error
    - empty string → error
  - Route:
    - `ignore` 0.9 → Ignore; `ignore` 0.5 → Notify; `ignore` 0.85 → Ignore
    - `investigate` with tools → Investigate; without tools → Notify
    - `notify` → Notify
- [ ] **Step 2:** `go test ./internal/agent -run 'TestParseTriageDecision|TestRoute' -count=1`. Expected: FAIL.
- [ ] **Step 3:** Implement. Decode with `json.Decoder` from the first `{`.
- [ ] **Step 4:** Same command. Expected: PASS.
- [ ] **Step 5:** Commit `feat(agent): parse and route Tier-1 triage decisions`.

### Task 2: Sanitizer

**Files:**
- Replace: `internal/agent/sanitizer.go`
- Create: `internal/agent/sanitizer_test.go`

**Interfaces:**
- Produces:
  - `type Redactor struct` and `NewRedactor(values []string) *Redactor`. Values shorter than 6 chars are ignored.
  - `(*Redactor).Add(value string)`, safe for concurrent use. It is used to register freshly minted tokens.
  - `CleanInput(s string) string`
  - `(*Redactor).RedactSecrets(s string) string`
  - `(*Redactor).CleanOutbound(s string, neutralizeMentions bool) string`

- [ ] **Step 1: Write failing tests:**
  - `TestCleanInput`: control characters, zero-width, and bidi (`‮`) removed; `\n` and `\t` kept; blank lines collapsed; Vietnamese and emoji intact
  - `TestRedactSecrets` (table): one case per pattern in the design, plus an exact value added via `Add`, a value shorter than 6 chars left alone, a Postgres URL keeping its user but losing the password, and a PEM block removed entirely
  - `TestCleanOutboundMentions`: `@octocat ping` → `` `@octocat` ping ``; emails like `a@b.com` unchanged
  - `TestRedactorConcurrentAdd` (with `-race`)
- [ ] **Step 2:** `go test ./internal/agent -run 'TestCleanInput|TestRedact|TestCleanOutbound' -count=1`. Expected: FAIL.
- [ ] **Step 3:** Implement with precompiled `regexp`s and an `RWMutex`.
- [ ] **Step 4:** `go test -race ./internal/agent -count=1`. Expected: PASS.
- [ ] **Step 5:** Commit `feat(agent): input/output sanitizer and secret redaction`.

### Task 3: Agent, tool, account, and repo configuration

**Files:**
- Create: `internal/tenant/agent_config.go`, `agent_config_test.go`

**Interfaces:**
- Produces:
  - `type Tier string` with constants `TierRead`, `TierWrite`, `TierDangerous`.
  - `type GitHubAccount struct{ Name, Type, TokenEnv, Login, AppID, InstallationID, PrivateKeyEnv string }`
  - `type RepoGrant struct{ Repo, Account, LocalPath string; Allow, MergeMethods, MergeBaseBranches []string; RequireChecksGreen bool }`
  - `type ToolPolicyConfig struct{ Enabled []string; DryRun bool; MaxWritesPerJob int; Accounts map[string]GitHubAccount; Repos map[string]RepoGrant }`
  - `type AgentConfig struct{ Enabled bool; ConfidenceThreshold float64; Persona, ClickUpTokenEnv string; Tools ToolPolicyConfig }`
  - `ParseAgentConfig(source map[string]any, known map[string]Tier) (AgentConfig, error)`. The `known` map is passed by wiring from the registry, which keeps `tenant` free of tool imports.
  - `ValidRepo(string) bool`, with regex `^[A-Za-z0-9][A-Za-z0-9_.-]{0,38}/[A-Za-z0-9_.-]{1,100}$`. `.` and `..` are rejected as either part.

- [ ] **Step 1: Write failing tests** `TestParseAgentConfig` (table):
  - absent → disabled, with the default `Enabled` list (all read tools present in `known`)
  - threshold outside (0,1] → error
  - unknown tool in `enabled` → error
  - `allow` containing a read tool or an unknown tool → error
  - a repo whose account is missing → error
  - PAT without `login` → error
  - App with a non-numeric `app_id` → error
  - bad env name → error
  - `merge_methods` containing `fast` → error
  - `local_path` relative → error
  - `max_writes_per_job` 6 → error
  - invalid repo key (`../x`, `-o/x`, `a`) → error
  - `require_checks_green` absent → true
  - persona over 500 runes → error
- [ ] **Step 2:** `go test ./internal/tenant -run TestParseAgentConfig -count=1`. Expected: FAIL.
- [ ] **Step 3:** Implement. The `local_path` existence check is done by the code tools at runtime, not here.
- [ ] **Step 4:** Same command. Expected: PASS.
- [ ] **Step 5:** Commit `feat(tenant): agent tool permissions, GitHub accounts and repo grants`.

### Task 4: Schema and agent-aware ingest (durable debounce)

**Files:**
- Create: `db/migrations/00003_create_agent_core.sql`, `db/query/agent.sql`, `action.sql`, `tool_run.sql` (generated sqlc)
- Modify: `internal/adapters/storage/notice_repository.go`, `notice_repository_test.go`, `cmd/server/main.go`
- Test: `internal/adapters/storage/agent_ingest_test.go`

**Schema (exact):**

`agent_jobs`:
- `id bigint identity PK`
- `workspace_id`, `source_integration_id`, `thread_id`, with FK `(workspace_id, thread_id)` → `hub_threads(workspace_id, id)` and FK `(workspace_id, source_integration_id)` → `integrations`
- `status CHECK IN ('pending','processing','done','failed') DEFAULT 'pending'`
- `attempts int DEFAULT 0`
- `run_at timestamptz NOT NULL`
- `first_enqueued_at DEFAULT now()`
- `locked_until`, `claim_token uuid`
- `through_message_id bigint`
- `last_error_code`
- `input_tokens`, `output_tokens`, `cache_read_tokens`, `cache_write_tokens`: `int NOT NULL DEFAULT 0`
- `created_at`, `updated_at`
- `UNIQUE INDEX (thread_id) WHERE status='pending'`
- `INDEX (status, run_at)`

`outbound_actions`:
- `id bigint identity PK`
- `workspace_id`, `integration_id`, with FK `(workspace_id, integration_id)`
- `kind CHECK IN ('telegram_message')`
- `payload jsonb CHECK object`
- `dedupe_key NOT NULL`
- `status`, `attempts`, `next_attempt_at`, `locked_until`, `claim_token`, `last_error_code`: same semantics as `notification_outbox`
- `provider_message_id text`
- `created_at`, `updated_at`, `sent_at`
- `UNIQUE (kind, dedupe_key)`
- `INDEX (status, next_attempt_at, locked_until)`

`tool_runs`:
- `id bigint identity PK`
- `workspace_id uuid NOT NULL`
- `agent_job_id REFERENCES agent_jobs`
- `step int`
- `tool text`
- `tier CHECK IN ('read','write','dangerous')`
- `repo text`, `account text`
- `idempotency_key NOT NULL`
- `input_redacted jsonb`
- `status CHECK IN ('started','ok','error','denied','timeout','unknown_outcome','dry_run')`
- `error_code`, `exit_code int`, `duration_ms int`, `output_bytes int`
- `result_summary text`
- `created_at`, `updated_at`
- `UNIQUE (idempotency_key) WHERE tier <> 'read' AND status IN ('started','ok')`
- `INDEX (agent_job_id)`

RLS is enabled on all three. The Down migration drops them in reverse order.

**Interfaces:**
- Produces:
  - `NewNoticeRepository(pool, newID, DebounceSettings{Window, MaxWait time.Duration})`
  - Queries `UpsertPendingAgentJob` (the conflict branch sets `run_at = LEAST(now()+window, first_enqueued_at+max_wait)`) and `SaveOwnerMessage`.
  - `NoticeRepository` receives the `known` tool-tier map via a setter `SetKnownTools(map[string]tenant.Tier)`, used when parsing the agent config.

- [ ] **Step 1: Write failing DB tests:**
  - `TestAgentIngest_CommentEnqueuesJobNotNotice`
  - `TestAgentIngest_BurstCoalesces`: 3 comments on one task → 1 job with `run_at ≤ first+max`; 2 tasks → 2 jobs
  - `TestAgentIngest_StatusStillNotice`
  - `TestAgentIngest_OwnerCommentContextOnly`
  - `TestAgentIngest_DisabledSourceUnchanged`
  - `TestAgentIngest_DuplicateCallback`
- [ ] **Step 2:** `TEST_DATABASE_URL=... go test ./internal/adapters/storage -run TestAgentIngest -count=1 -v`. Expected: FAIL.
- [ ] **Step 3:** Write the migration and queries, run `make generate`, and add the agent branch in `accept` after the duplicate check.
- [ ] **Step 4:** Run the same command, `goose -dir db/migrations validate`, and `go test ./internal/adapters/storage -count=1`. Expected: PASS, with the Phase 1 tests green.
- [ ] **Step 5:** Commit `feat(storage): agent core schema and durable debounce on ingest`.

### Task 5: Context packet builder

**Files:**
- Create: `internal/memory/builder.go`, `builder_test.go`

**Interfaces:**
- Consumes: a `func(string) string` cleaner, injected so `memory` does not import `agent`; wiring passes `agent.CleanInput`.
- Produces:
  - `ContextPacket{Title, TaskURL, Summary string; Recent, NewBatch []Message}`
  - `BuildPacket(title, taskURL, summary string, history []Message, lastTriagedID int64, clean func(string) string) ContextPacket`
  - `(ContextPacket).Render() string`, a `<ticket_content>` block in which a literal closing tag in content is escaped to `</ticket_content_>`
  - Constants `SummaryBudget=800`, `MessageBudget=300`, `BatchBudget=1200`

- [ ] **Step 1: Write failing tests** `TestBuildPacket`:
  - Recent/NewBatch selection with `lastTriaged=4` over 7 messages
  - owner and AI messages excluded from the batch
  - the cleaner is applied (a bidi character removed)
  - truncation marker
  - an over-budget batch keeps the newest message
  - no split runes
  - closing-tag escape
- [ ] **Step 2:** `go test ./internal/memory -run TestBuildPacket -count=1`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Same command. Expected: PASS.
- [ ] **Step 5:** Commit `feat(memory): sanitized context packet builder`.

### Task 6: Tier-1 router, agent settings, LLM usage

**Files:**
- Create: `internal/agent/prompts.go`, `tier1_router.go`, `tier1_router_test.go`, `internal/config/agent.go`, `agent_test.go`
- Modify: `internal/domain/llm.go` (`LLMResponse` gains `Usage`), `internal/adapters/llm/anthropic.go` (fill `Usage` from `resp.Usage`), `internal/adapters/llm/usage.go`

**Interfaces:**
- Produces:
  - `domain.Usage{Input, Output, CacheRead, CacheWrite int64}` with `(Usage).Add(Usage) Usage`; `LLMResponse{Text string; Usage Usage}`
  - `TriageError{Code string; Retryable bool}`, with codes `llm_unconfigured` (non-retryable), `llm_refused` (non-retryable), `llm_error` (retryable), `invalid_decision` (retryable)
  - `NewTier1Router(llm domain.LLMProvider, model string)` and `(r) Triage(ctx, packet memory.ContextPacket, persona string, toolsAvailable bool) (TriageDecision, domain.Usage, error)`
  - `config.AgentSettings{TriageModel, ActorModel, SummaryModel, GitPath string; PollInterval, ToolTimeout time.Duration; MaxInputTokens, MaxOutputTokens int64; AllowDangerous bool}` and `LoadAgentSettings()`

- [ ] **Step 1: Write failing tests:**
  - `TestTier1Router_Request`: model `claude-haiku-5-5`, `MaxTokens` 300, the system prompt contains `untrusted`, the schema, and "JSON only", and the user message is the rendered packet
  - `TestTier1Router_NoToolsOmitsInvestigate`
  - `TestTier1Router_Errors`: each code
  - `TestTier1Router_ReturnsUsage`
  - `TestAnthropic_GenerateUsage`: an httptest fake API returns `usage` → mapped, including the cache fields
  - `TestLoadAgentSettings`: defaults, and `AGENT_ALLOW_DANGEROUS=1` → true, any other value → false
- [ ] **Step 2:** `go test ./internal/agent ./internal/config ./internal/adapters/llm -run 'TestTier1Router|TestLoadAgentSettings|TestAnthropic_GenerateUsage' -count=1`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Same command. Expected: PASS.
- [ ] **Step 5:** Commit `feat(agent): Tier-1 router with usage accounting`.

### Task 7: Tool core (registry, tiered executor)

**Files:**
- Create: `internal/agent/tools/tool.go`, `registry.go`, `executor.go`, and `*_test.go` for each

**Interfaces:**
- Consumes: `tenant.Tier`, `tenant.ToolPolicyConfig`, `tenant.RepoGrant` (Task 3); `agent.Redactor` (Task 2), passed in as an interface `Sanitizer{ CleanOutbound(string, bool) string }` to avoid an import cycle.
- Produces:
  - `ToolSpec{Name, Description string; InputSchema json.RawMessage; Tier tenant.Tier}`
  - `Call{Input json.RawMessage; Repo *tenant.RepoGrant; DryRun bool}`
  - `ToolOutput{Text, Summary string}`
  - `ToolError{Code string; Retryable bool}`
  - `Tool interface{ Spec() ToolSpec; Run(context.Context, Call) (ToolOutput, error) }`
  - `RepoScoped interface{ Repo(json.RawMessage) (string, error) }`
  - `NewRegistry(...Tool) (*Registry, error)`, with `Get`, `Specs(enabled []string) []ToolSpec`, and `Tiers() map[string]tenant.Tier`
  - `ToolRunStore interface{ Begin(ctx, RunStart) (int64, *PriorRun, error); Finish(ctx, int64, RunFinish) error; Deny(ctx, RunStart, code string) error }`, where `RunStart` carries job, step, tool, tier, repo, account, key, and the redacted input
  - `Limits{MaxCalls=12; MaxDangerous=1; Timeout; OutputCap=6144}`
  - `NewExecutor(reg *Registry, policy tenant.ToolPolicyConfig, limits Limits, allowDangerous bool, store ToolRunStore, s Sanitizer, jobID int64, workspaceID string) *Executor`
  - `(e) Call(ctx, step int, name string, input json.RawMessage) CallResult`, where `CallResult{Content string; IsError bool; Status string; Tier tenant.Tier}`
  - `CanonicalKey(jobID int64, tool string, input json.RawMessage) string`

- [ ] **Step 1: Write failing tests** with fake tools and a fake store, one test per check in the design order:
  - `TestExecutor_UnknownTool`
  - `TestExecutor_NotEnabled`
  - `TestExecutor_InputTooLarge`
  - `TestExecutor_RepoNotConfigured`
  - `TestExecutor_WriteNotAllowedForRepo`
  - `TestExecutor_DangerousNeedsEnv`: allowed in config with the env off → `dangerous_disabled`
  - `TestExecutor_DangerousOnePerJob`
  - `TestExecutor_CallBudget`: the 13th call
  - `TestExecutor_WriteBudget`
  - `TestExecutor_IdempotentReplayOK`: not run, prior summary returned
  - `TestExecutor_IdempotentReplayStarted`: `unknown_outcome`
  - `TestExecutor_DryRun`: a write is not run, `dry_run` is recorded, Content starts with `DRY RUN`; a read in dry-run still runs
  - `TestExecutor_Timeout`
  - `TestExecutor_OutputSanitizedAndCapped`: a 20 KiB output with a `ghp_…` token → ≤ 6 KiB, the token redacted, the ` [truncated]` marker present
  - `TestExecutor_RepoGrantPassedToTool`
  - `TestCanonicalKey_OrderIndependent`
  - `TestRegistry_DuplicateName`
- [ ] **Step 2:** `go test ./internal/agent/tools -count=1`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** `go test -race ./internal/agent/tools -count=1`. Expected: PASS.
- [ ] **Step 5:** Commit `feat(tools): registry and tiered policy executor`.

### Task 8: Hardened subprocess runner

**Files:**
- Create: `internal/adapters/tools/execrunner/runner.go`, `runner_test.go`

**Interfaces:**
- Produces:
  - `Command{Path string; Args []string; Dir string; Stdin []byte; Timeout time.Duration}`
  - `Result{Stdout, Stderr []byte; ExitCode int; TimedOut, Truncated bool}`
  - `New(extraEnv ...string) *Runner`. The base environment is `PATH`, `HOME`, `LANG` from the parent plus `extraEnv`. The output cap is 1 MiB per stream.
  - `(r) Run(ctx, Command) (Result, error)`. It returns an error only when the process fails to start.

- [ ] **Step 1: Write failing tests**, using the helper-process pattern (`os.Args[0] -test.run=TestHelperProcess`):
  - `TestRunner_ArgsVerbatim`: `"; rm -rf /"` arrives as one literal argument
  - `TestRunner_EnvMinimal`: the parent's `ANTHROPIC_API_KEY` and `NEXUS_X` are absent and `extraEnv` is present
  - `TestRunner_Stdin`
  - `TestRunner_NonZeroExit`
  - `TestRunner_TimeoutKillsGroup`: a spawned grandchild is gone after the timeout, and the call returns within timeout + 2s
  - `TestRunner_OutputCap`: 5 MiB → `Truncated`
- [ ] **Step 2:** `go test ./internal/adapters/tools/execrunner -count=1 -v`. Expected: FAIL.
- [ ] **Step 3:** Implement with `Setpgid`, `syscall.Kill(-pid, SIGKILL)` on timeout, and concurrent capped readers.
- [ ] **Step 4:** `go test -race ./internal/adapters/tools/execrunner -count=1`. Expected: PASS.
- [ ] **Step 5:** Commit `feat(tools): hardened subprocess runner`.

### Task 9: GitHub REST client with PAT and GitHub App credentials

**Files:**
- Create: `internal/adapters/github/client.go`, `auth.go`, `app_jwt.go`, and `*_test.go` for each

**Interfaces:**
- Consumes: `tenant.GitHubAccount` (Task 3), `agent.Redactor.Add` (Task 2), passed as a `func(string)`.
- Produces:
  - `type Credentials interface{ Token(ctx, account tenant.GitHubAccount, repo string, perms map[string]string) (string, error) }`
  - `NewCredentials(httpc *http.Client, lookupEnv func(string)(string,bool), register func(string), now func() time.Time) Credentials`
    - PAT: verify `GET /user` login, cached for 10 minutes.
    - App: build a JWT, call `POST /app/installations/{id}/access_tokens` with `repositories` and `permissions`, and cache per key until `expires_at-5m`.
    - Every token obtained is passed to `register`.
  - `NewClient(httpc *http.Client, creds Credentials) *Client`, with `Do(ctx, account tenant.GitHubAccount, repo string, perms map[string]string, method, path string, body any, out any) error`. Errors are `*GitHubError{Code string; Retryable bool; RetryAfter time.Duration}`, with the codes from the design.
  - `signAppJWT(appID string, key *rsa.PrivateKey, now time.Time) (string, error)` and `loadAppKey(path string) (*rsa.PrivateKey, error)`. The loader checks `mode&0o077 == 0` and parses PKCS#1 or PKCS#8.

- [ ] **Step 1: Write failing tests** with a fake GitHub on httptest and an RSA key generated in the test:
  - `TestPAT_LoginVerified`
  - `TestPAT_AccountMismatch` → `account_mismatch`, and the API request is never made
  - `TestPAT_MissingEnv` → `missing_credentials`
  - `TestApp_JWTClaimsAndSignature`: verify with the public key; `iss`, `iat=now-60`, `exp=now+540`
  - `TestApp_TokenDownScoped`: the request body has `repositories:["nexus-hub"]` and the requested permissions
  - `TestApp_TokenCachedUntilExpiry`: the second call within the window makes no new request; after `expires_at-5m` it mints again
  - `TestApp_KeyPermissions`: a 0644 key file → `key_permissions`
  - `TestClient_Headers`: Accept, API version, User-Agent, `Authorization: Bearer <token>`
  - `TestClient_Errors`: 401, 403 forbidden, 403 rate-limit with reset, 429 with retry-after, 404, 422, 500, 301 → `repo_moved` without following
  - `TestClient_TokenRegisteredForRedaction`
- [ ] **Step 2:** `go test ./internal/adapters/github -count=1 -v`. Expected: FAIL.
- [ ] **Step 3:** Implement using stdlib only (`crypto/rsa`, `crypto/sha256`, `encoding/base64` RawURL).
- [ ] **Step 4:** `go test -race ./internal/adapters/github -count=1`. Expected: PASS.
- [ ] **Step 5:** Commit `feat(github): REST client with PAT and GitHub App credentials`.

### Task 10: GitHub read/write tools, ClickUp read tool, tool-run store

**Files:**
- Create:
  - `internal/adapters/tools/github/read.go`, `write.go`, `tools_test.go`
  - `internal/adapters/tools/clickup/task.go`, `task_test.go`
  - `internal/adapters/storage/tool_run_store.go`, `tool_run_store_test.go`

**Interfaces:**
- Consumes: `tools.Tool` / `RepoScoped` / `Call` (Task 7), `github.Client` (Task 9), `tenant.ValidRepo`.
- Produces:
  - `githubtools.NewReadTools(c *github.Client) []tools.Tool`: `github_pr_view`, `github_pr_checks`, `github_pr_files`, `github_issue_view`
  - `githubtools.NewWriteTools(c *github.Client, s tools.Sanitizer) []tools.Tool`: `github_pr_comment`
  - App permission sets per tool: reads use `{"pull_requests":"read","checks":"read","statuses":"read","issues":"read","contents":"read"}`; comment uses `{"issues":"write","pull_requests":"write"}`
  - `clickuptools.NewTaskTool(httpc, lookupEnv, tokenEnv)`: `clickup_get_task`
  - `storage.NewToolRunStore(pool)`, implementing `tools.ToolRunStore`

- [ ] **Step 1: Write failing tests** with a fake GitHub and ClickUp on httptest:
  - `TestPRView_CompactOutput`: an exact single-line format, with no raw JSON
  - `TestPRChecks_Summary`: mixed check runs plus a combined status → a totals line
  - `TestPRFiles_PatchClipped`
  - `TestIssueView_BodyClipped`
  - `TestPRComment_BodyPrefixedAndMentionsNeutralized`: the request body is `🤖 ` followed by text with `` `@user` ``
  - `TestGitHubTools_InputValidation`: unknown field, repo `-x/y`, number 0, a body over 4000 runes → `invalid_input`
  - `TestGitHubTools_UsesGrantAccount`: the call uses `RepoGrant.Account`
  - `TestClickUpGetTask_FieldsOnly`, `TestClickUpGetTask_Errors`, `TestClickUpGetTask_NoRedirect`
  - DB:
    - `TestToolRunStore_WriteIdempotency`: two `Begin` calls with the same key → one row; the second returns `PriorRun`
    - `TestToolRunStore_ReadsNotDeduped`
    - `TestToolRunStore_FinishDenyDryRun`
- [ ] **Step 2:** `TEST_DATABASE_URL=... go test ./internal/adapters/tools/... ./internal/adapters/storage -run 'PRView|PRChecks|PRFiles|IssueView|PRComment|GitHubTools|ClickUpGetTask|ToolRunStore' -count=1 -v`. Expected: FAIL.
- [ ] **Step 3:** Implement. Decode input with `DisallowUnknownFields`.
- [ ] **Step 4:** Same command. Expected: PASS.
- [ ] **Step 5:** Commit `feat(tools): GitHub read/write and ClickUp read tools with audited runs`.

### Task 11: GitHub dangerous tools (merge, close)

**Files:**
- Create: `internal/adapters/tools/github/dangerous.go`, `dangerous_test.go`

**Interfaces:**
- Produces: `githubtools.NewDangerousTools(c *github.Client, s tools.Sanitizer) []tools.Tool`, returning `github_pr_merge`, `github_pr_close`, `github_issue_close`. Their App permissions are `{"contents":"write","pull_requests":"write","checks":"read","statuses":"read"}` for merge and `{"pull_requests":"write","issues":"write"}` for close.

- [ ] **Step 1: Write failing tests** with a fake GitHub:
  - `TestMerge_HappyPath`: the PUT body has `merge_method` and the exact `sha`; the summary is `merged #n into <base> (<method>) sha=<7>`
  - `TestMerge_Preconditions` (table, each with no PUT made):
    - draft
    - closed
    - head SHA mismatch
    - base branch not listed
    - base `main` with an empty list
    - method not allowed
    - a failing check
    - a pending check
    - combined status failure
  - `TestMerge_RequireChecksGreenFalseSkipsChecks`
  - `TestMerge_Conflict409` → `conflict`, non-retryable
  - `TestClosePR_WithComment`: the comment POST comes before the PATCH, and mentions are neutralized
  - `TestCloseIssue_Reason`: `not_planned` passes through; an invalid reason → `invalid_input`
- [ ] **Step 2:** `go test ./internal/adapters/tools/github -run 'TestMerge|TestClose' -count=1 -v`. Expected: FAIL.
- [ ] **Step 3:** Implement. Preconditions are evaluated from fresh API reads in the same call.
- [ ] **Step 4:** Same command. Expected: PASS.
- [ ] **Step 5:** Commit `feat(tools): guarded GitHub merge and close tools`.

### Task 12: Code-reading tools (`git`)

**Files:**
- Create: `internal/adapters/tools/code/git.go`, `tools.go`, `outline.go`, and `*_test.go`

**Interfaces:**
- Consumes: `execrunner.Runner` (Task 8), `tools.Tool` / `RepoScoped` (Task 7).
- Produces:
  - `codetools.New(runner *execrunner.Runner, gitPath string) []tools.Tool`: `code_list_files`, `code_search`, `code_read_file`, `code_outline`, `git_log`, `git_diff`, `git_blame`, all tier `read`
  - `gitArgs(root string, sub ...string) []string`, prepending the hardening `-c` flags from the design
  - `validRef(string) bool`, `cleanRepoPath(string) (string, error)`, `deniedPath(string) bool`

- [ ] **Step 1: Write failing tests.** Each test creates a temporary repo with `git init`, commits files, and sets a local identity.
  - `TestListFiles_DenyListFiltered`: a committed `.env` and `certs/a.pem` are absent and counted as omitted
  - `TestSearch_FixedStringAndLimits`: the query `"a.b"` matches literally; 60 hits → 50 plus "10 more"; lines clipped to 200 runes
  - `TestReadFile_LineRangeNumbered`: lines 10–20, prefixed with numbers
  - `TestReadFile_UntrackedInvisible`: an uncommitted file → `not_found`
  - `TestReadFile_Binary` → `binary_file`
  - `TestReadFile_DeniedPath` → denied
  - `TestReadFile_PathTraversal`: `../x`, `/etc/passwd`, `-n` → `invalid_input`
  - `TestOutline_Go`: funcs, methods, and types with lines
  - `TestOutline_GenericHeuristic`: a `.py` file
  - `TestGitLog_Format`
  - `TestGitDiff_StatAndCappedPatch`
  - `TestGitBlame_RangeLimit`: over 100 lines → `invalid_input`
  - `TestRef_Validation`: `HEAD~2` ok; `--output=x`, `a..b`, `$(x)` → invalid
  - `TestHardening_FsmonitorNotExecuted`: the repo's local config sets `core.fsmonitor` to a script that writes a marker file; after `code_search`/`git_diff` the marker does not exist
  - `TestHardening_ExternalDiffNotExecuted`: the same idea, with `diff.external` and a textconv driver
  - `TestLocalPathMissing` → `repo_unavailable`
- [ ] **Step 2:** `go test ./internal/adapters/tools/code -count=1 -v`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** `go test -race ./internal/adapters/tools/code -count=1`. Expected: PASS.
- [ ] **Step 5:** Commit `feat(tools): hardened git code-reading tools`.

### Task 13: Tool-calling provider with prompt caching and usage

**Files:**
- Modify: `internal/domain/llm.go`, `internal/domain/port.go`, `internal/adapters/llm/anthropic.go`
- Create: `internal/adapters/llm/anthropic_tools_test.go`

**Interfaces:**
- Produces, in `domain`:
  - `ToolDef{Name, Description string; InputSchema json.RawMessage}`
  - `ToolCall{ID, Name string; Input json.RawMessage}`
  - `ToolResult{CallID, Content string; IsError bool}`
  - `Turn{Role; Text string; Calls []ToolCall; Results []ToolResult}`
  - `ToolRequest{Model, System string; MaxTokens int64; Tools []ToolDef; Turns []Turn; CacheStatic bool}`
  - `ToolResponse{Text string; Calls []ToolCall; Stop StopReason; Usage Usage}`, with `StopReason` constants `StopEndTurn`, `StopToolUse`, `StopMaxTokens`, `StopRefusal`
  - `ToolCallingProvider interface{ GenerateWithTools(ctx, *ToolRequest) (*ToolResponse, error) }`
- `*llm.Anthropic` implements it with SDK v2 (`Tools []anthropic.ToolUnionParam`, `StopReasonToolUse`, `ToolUseBlock`, `NewToolResultBlock`). With `CacheStatic`, it sets ephemeral `cache_control` on the system block and the last tool. Confirm the field names via Context7 at implementation time.

- [ ] **Step 1: Write failing tests** against an httptest fake Messages API:
  - `TestAnthropicTools_RequestShape`: tools with `input_schema`; `tool_use` and `tool_result` blocks including `is_error`; with `CacheStatic`, `cache_control` appears on the system block and on the last tool only
  - `TestAnthropicTools_ParsesToolUse`: 2 calls
  - `TestAnthropicTools_EndTurnText`
  - `TestAnthropicTools_Refusal` → `StopRefusal`
  - `TestAnthropicTools_Usage`: cache read/write mapped
  - `TestAnthropicTools_HTTPErrorNoBodyEcho`
- [ ] **Step 2:** `go test ./internal/adapters/llm -run TestAnthropicTools -count=1 -v`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** `go test ./internal/adapters/llm -count=1`. Expected: PASS.
- [ ] **Step 5:** Commit `feat(llm): tool calling with prompt caching and usage`.

### Task 14: Tier-2 ReAct actor with token controls

**Files:**
- Create: `internal/agent/tier2_actor.go`, `tier2_actor_test.go`; add actor prompt constants to `prompts.go`

**Interfaces:**
- Consumes: Tasks 4, 7, 13.
- Produces:
  - `ActorReport{Text string; Steps int; Calls []CallRecord; Stopped string; Usage domain.Usage}`, where `CallRecord{Name string; Tier tenant.Tier; Status string}` and `Stopped` is one of `""`, `step_limit`, `max_tokens`, `token_budget`
  - `NewTier2Actor(llm domain.ToolCallingProvider, model string) *Tier2Actor`
  - `(a) Investigate(ctx, packet, decision TriageDecision, persona string, specs []tools.ToolSpec, exec *tools.Executor, budget TokenBudget) (ActorReport, error)`, where `TokenBudget{RemainingInput, RemainingOutput int64}` and the errors are `*TriageError`
  - Constants `MaxActorSteps=5`, `ActorMaxTokens=1024`, `ReportMaxRunes=1500`, `KeepFullResultSteps=2`

- [ ] **Step 1: Write failing tests** with a scripted fake provider and fake tools:
  - `TestActor_SingleToolThenAnswer`: the tool_result `CallID` matches
  - `TestActor_ParallelCalls`: both results go in one user turn, in order
  - `TestActor_DeniedFedBackAsError`
  - `TestActor_HallucinatedTool`
  - `TestActor_StepLimit`
  - `TestActor_MaxTokensKeepsPartial`
  - `TestActor_TokenBudgetStops`: cumulative input usage exceeds the remaining budget → `token_budget` with no further LLM call
  - `TestActor_LoopCompaction`: the step-4 request has step-1 results replaced by `[elided: …]` while the `tool_use`/`tool_result` pairing is intact; step-3 results stay full
  - `TestActor_RequestSettings`: `MaxTokens` 1024, `CacheStatic` true, tools only from the specs, a system prompt with `untrusted`/terse instructions/the persona/the goal, and the report clipped to 1500 runes
  - `TestActor_Refusal`
- [ ] **Step 2:** `go test ./internal/agent -run TestActor -count=1`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** `go test -race ./internal/agent -count=1`. Expected: PASS.
- [ ] **Step 5:** Commit `feat(agent): Tier-2 ReAct actor with compaction and token budget`.

### Task 15: Agent worker, outcome commit, outbound Telegram actions

**Files:**
- Create:
  - `internal/adapters/storage/agent_store.go`, `agent_store_test.go`
  - `internal/agent/orchestrator.go`, `orchestrator_test.go`
  - `internal/adapters/storage/action_store.go`, `action_store_test.go`
  - `internal/relay/action_worker.go`, `action_worker_test.go`
  - `internal/adapters/control/telegram/control.go`, `control_test.go`
- Modify: `internal/adapters/control/telegram/client.go`. Extract a shared `call(...)`; the `Send` behavior and its tests are unchanged.

**Interfaces:**
- Produces:
  - `AgentJob{ID int64; ClaimToken string; Attempts int; WorkspaceID, ThreadID, SourceIntegrationID string; Usage domain.Usage}`
  - `JobContext{Packet memory.ContextPacket; Agent tenant.AgentConfig; TargetIntegrationID string; ThroughMessageID int64; Batch []domain.NormalizedEvent}`
  - `AgentStore interface`. Every method returns `ErrStaleAgentClaim` on a fencing mismatch.
    - `Claim(ctx, now)`: 60s lease
    - `Load(ctx, AgentJob)`
    - `AddUsage(ctx, AgentJob, domain.Usage) error`
    - `CommitOutcome(ctx, AgentJob, JobContext, TriageDecision, Outcome, *ActorReport) error`
    - `CommitFallback(ctx, AgentJob, JobContext, code string) error`
    - `Retry(ctx, AgentJob, next time.Time, code string) error`
  - `NewOrchestrator(store AgentStore, t1 *Tier1Router, t2 *Tier2Actor, reg *tools.Registry, runs tools.ToolRunStore, s *Redactor, compressor *memory.Compressor, settings config.AgentSettings) *Orchestrator`, with `Run(ctx) error`. One `Executor` per attempt; backoff 5/15/30/60s; max 5 attempts; the token budget comes from settings minus the usage already recorded on the job.
  - `relay.NewActionWorker(store ActionStore, sender ActionSender, s WorkerSettings) *ActionWorker`
  - `telegram.(*Client).SendText(ctx, tokenEnv, chatID string, threadID *int64, text string) (int64, error)`

- [ ] **Step 1: Write failing tests.**
  - Orchestrator, with fakes:
    - `TestOrchestrator_Ignore`
    - `TestOrchestrator_Notify`
    - `TestOrchestrator_Investigate`: the report, calls, and tokens reach `CommitOutcome`
    - `TestOrchestrator_RetryThenSuccess`
    - `TestOrchestrator_ExhaustedFallsBack`
    - `TestOrchestrator_NonRetryableImmediate`
    - `TestOrchestrator_RetryDoesNotRepeatWrite`: attempt 1 runs `github_pr_comment` then the LLM errors; attempt 2 repeats the call → the fake GitHub sees exactly one POST
    - `TestOrchestrator_BudgetCarriesAcrossAttempts`
    - `TestOrchestrator_StaleClaimIgnored`
  - DB:
    - `TestAgentStore_ClaimRespectsRunAt`
    - `TestAgentStore_LeaseRecovery`
    - `TestAgentStore_NewBatchAfterThrough`
    - `TestAgentStore_CommitInvestigate`: the text has the `(write)`/`(DANGEROUS)`/`🧪` labels, the `Tokens:` line, and is sanitized (a planted token is redacted)
    - `TestAgentStore_UsageAccumulates`
    - `TestAgentStore_Fallback`
    - `TestActionStore_ClaimMarkSentDedupe`
  - Action worker: sent / retry / failed / stale
  - `TestSendText_ClipsPlainText`
- [ ] **Step 2:** `TEST_DATABASE_URL=... go test ./internal/agent ./internal/relay ./internal/adapters/... -run 'Orchestrator|AgentStore|ActionStore|ActionWorker|SendText' -count=1 -v`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Same command, plus `go test ./internal/adapters/control/telegram -count=1`. Expected: PASS.
- [ ] **Step 5:** Commit `feat(agent): durable agent worker with one-way Telegram reports`.

### Task 16: Rolling compressor

**Files:**
- Create: `internal/memory/compressor.go`, `compressor_test.go`
- Modify: `internal/adapters/storage/agent_store.go`, `internal/agent/orchestrator.go`

**Interfaces:**
- Produces:
  - `SummaryStore interface{ SummaryCandidates(ctx, ws, thread string) (string, []Message, error); SaveSummary(ctx, ws, thread, summary string, throughID int64) error }`. `SaveSummary` rejects the write if the cursor moved.
  - `NewCompressor(store, llm domain.LLMProvider, model string, clean func(string) string)`, with `Compress(ctx, ws, thread string) (bool, domain.Usage, error)`. It is a no-op when there are fewer than 5 candidates or `llm` is nil.

- [ ] **Step 1: Write failing tests:**
  - 4 candidates → no call
  - 6 candidates → one call with 300 max tokens, a save with `throughID` = the last candidate and ≤ 1200 runes
  - LLM error → no save
  - `TestAgentStore_SaveSummaryRejectsMovedCursor`
  - `TestOrchestrator_CompressAfterSuccess`: usage is added, and an error does not fail the job
- [ ] **Step 2:** `TEST_DATABASE_URL=... go test ./internal/memory ./internal/agent ./internal/adapters/storage -run 'Compress|SaveSummary' -count=1 -v`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Same command. Expected: PASS.
- [ ] **Step 5:** Commit `feat(memory): rolling summary compressor`.

### Task 17: Wiring, setup CLI, runbook, end-to-end

**Files:**
- Modify: `cmd/server/main.go`, `main_test.go`, `cmd/setup-notices/main.go`, `main_test.go`, `.env.example`, `docs/runbooks/clickup-telegram-local.md`
- Create: `internal/adapters/storage/agent_endtoend_test.go`

**Interfaces:**
- Produces:
  - `runServices([]serviceWorker)`: notice worker, orchestrator, action worker. Any unexpected stop shuts the server down, and shutdown waits for all workers.
  - Wiring:
    - The registry holds the read, write, and dangerous GitHub tools, the code tools (only if `exec.LookPath(GIT_PATH or "git")` succeeds), and the ClickUp tool.
    - `notices.SetKnownTools(registry.Tiers())`.
    - The `Redactor` is seeded with the values of `ANTHROPIC_API_KEY`, `CLICKUP_API`, `DATABASE_URL`, `NEXUS_*`, and every token or key env named in the agent configs.
    - The LLM is optional, with a log line.
  - CLI:
    - `enable-agent --source-id <uuid> [--threshold] [--persona] [--own-sender-id] [--tool]... [--dry-run] [--max-writes N]`
    - `disable-agent`
    - `github-account add --source-id <uuid> --name <n> (--pat-env <ENV> --login <login> | --app-id <id> --installation-id <id> --key-env <ENV>)`
    - `github-repo set --source-id <uuid> --repo owner/name --account <n> [--local-path <abs>] [--allow tool]... [--merge-method m]... [--merge-base b]... [--no-require-checks]`
    - `github-check --source-id <uuid>`: for each account, verify the credentials (PAT login match / App token mint) and print `account: ok|error <code>` without printing tokens
    - Every command validates through `ParseAgentConfig` inside one transaction.
  - `inspect` adds three sections, never printing payloads, inputs, reports, or tokens:
    - `agent_jobs`: id, status, attempts, error_code, tokens in/out
    - `tool_runs`: id, job, tool, tier, repo, account, status, error_code, duration
    - `outbound_actions`: id, status, attempts, error_code

- [ ] **Step 1: Write failing tests:**
  - `TestRunServices_AnyWorkerStopShutsDown`
  - `TestRunServices_WaitsForAllWorkers`
  - `TestGitHubRepoSet_WritesConfig`: DB round-trip
  - `TestGitHubRepoSet_RejectsReadToolInAllow`
  - `TestGitHubCheck_ReportsMismatchWithoutToken`: a fake GitHub returns another login
  - `TestInspect_NoLeak`: `SECRET-REPORT`, `SECRET-INPUT`, and the token value are all absent
  - `TestAgentEndToEnd`: real stores, a fake LLM, a fake GitHub, a temp git repo, and a fake Telegram, in this order:
    1. 2 comments in a burst
    2. the clock advances
    3. 1 job runs
    4. Tier-1 says investigate
    5. Tier-2 calls `code_search` then `github_pr_checks`, then answers
    6. 2 `tool_runs ok` rows
    7. 1 Telegram message containing the report, both tools ✓, and `Tokens:`
    8. `through_message_id` = the last comment
- [ ] **Step 2:** `TEST_DATABASE_URL=... go test ./cmd/... ./internal/adapters/storage -run 'RunServices|GitHubRepoSet|GitHubCheck|Inspect|TestAgentEndToEnd' -count=1 -v`. Expected: FAIL.
- [ ] **Step 3:** Implement. Add the new `.env.example` keys (empty values). Add a runbook section "Phase 2: agent core and tools" covering:
  - creating a fine-grained PAT per account (minimum repo permissions)
  - creating and installing a GitHub App (permissions; key file at 0600)
  - `github-account`, `github-repo`, `github-check`
  - dry-run first
  - what dangerous tools do and the three switches
  - reading `inspect`
  - that live acceptance is Phase 3
- [ ] **Step 4:** Full verification:
  - `goose -dir db/migrations validate`
  - `make generate` twice, with no diff the second time
  - `gofmt -l cmd internal` gives empty output
  - `go vet ./...`
  - `TEST_DATABASE_URL=... go test -race ./... -count=1 -v`
  - Expected: all PASS, 0 SKIP.
- [ ] **Step 5:** Commit `feat: wire Phase 2 agent core, GitHub/code tools, CLI and runbook`.

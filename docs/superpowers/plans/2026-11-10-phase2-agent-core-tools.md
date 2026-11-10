# Phase 2: Agent Core, Multi-Tool Execution, GitHub/Git Tools and DB-Backed Secrets Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** When a ClickUp comment arrives, Nexus-Hub runs the `ARCHITECTURE.md` request flow durably: filter → durable debounce → sanitized context packet → Tier-1 triage. When needed, a Tier-2 ReAct actor coordinates many tools under a strict, tiered permission policy:
- GitHub REST with the right account or a GitHub App;
- `git` read and workspace operations (clone, fetch, pull, checkout, commit, push…) on Nexus-managed clones;
- the ClickUp API.

The result is reported one-way to Telegram. All configuration and secrets come from the database. Secrets are AES-256-GCM encrypted. The process is limited enough to run on a VPS.

**Architecture:**
- **Ingest.** The Phase 1 ingest transaction also enqueues a per-thread *agent job*. Its run time is pushed back for coalescing.
- **Agent worker.** It builds a sanitized context packet, and Tier-1 picks Ignore, Notify, or Investigate. Investigate runs a bounded tool-calling loop. Every call goes through one **executor**, which enforces:
  - tiers and per-repository grants;
  - budgets, dry-run, timeouts, output caps, and sanitization;
  - an idempotent audit trail.
- **Subprocesses.** Exactly one binary, `git`, runs through a hardened runner: no shell, minimal environment, rlimits, process-group kill, and a concurrency cap of 2. Mutating git commands run only inside clones that Nexus itself manages under `/var/lib/nexus-hub/repos`.
- **Network.** In-process HTTP goes through a transport restricted to four API hosts.
- **Configuration and secrets.** Both are resolved from PostgreSQL. Secrets live in an encrypted `credentials` table and are decrypted with a master key file. The only environment variables are `DATABASE_URL` and `NEXUS_MASTER_KEY_FILE`.

**Tech Stack:** Go (existing module; stdlib `net/http`, `crypto/aes`, `crypto/cipher`, `crypto/rsa`, `go/parser`, `syscall` rlimits), Echo v4, pgx/v5, sqlc, goose, PostgreSQL, `anthropic-sdk-go/v2` tool use and prompt caching, the `git` CLI, GitHub REST v3 (`X-GitHub-Api-Version: 2022-11-28`), ClickUp API v2, Telegram Bot API `sendMessage`, systemd (unit file only).

**Spec:** The embedded design below is the Phase 2 specification. It is derived from `ARCHITECTURE.md` §1–§2 and §5 ("Credential Decryptor"), and from the user's decisions of 2026-11-10:
- **Scope.** Code-first. The core comes before any reply feature, and replies/approvals move to a later phase.
- **Tools.** The AI may call tools, including write and merge/close tools, under strict, configurable permissions.
- **GitHub.** No `gh` CLI: plain `git` plus the GitHub API with the correct account. Both PATs and a GitHub App are supported.
- **Additions.** A code-reading toolset, a sanitizer, and output-token reduction.
- **VPS limits.** The only binary is `git`. `sudo`, `ssh`, `sh`/`bash`, `docker` and the like are excluded. These git operations are allowed: clone, fetch, pull, checkout, reset, clean, commit, config, **push**.
- **DB is the main configuration source.** Environment variables are hard to manage; settings and secrets live in the DB.
- **Approved earlier.** Durable debounce in PostgreSQL, a separate `outbound_actions` outbox, Auto-Pilot off. Owner comments are context only.
- **Phase 3.** Live acceptance, independent review, and owner commands from Telegram.

Decisions marked **[Claude's call]** need approval with this plan.

## Global Constraints

- Phase 1 behavior is unchanged for sources without `agent.enabled`. Nexus-Hub posts nothing to ClickUp in Phase 2.
- Never modify migrations 00001/00002 or hand-edit generated sqlc code. New schema goes in `00003_create_agent_core.sql`; regenerate with `make generate`.
- **Environment.**
  - The process reads exactly `DATABASE_URL` and `NEXUS_MASTER_KEY_FILE`.
  - Phase 1's legacy `*_env` references keep working as a fallback until `migrate-secrets` has run. A deprecation warning is logged once per reference.
  - Everything else (models, budgets, timeouts, switches, tokens, keys, identities) is read from the DB.
- **Secrets.**
  - Secrets are encrypted at rest with AES-256-GCM. The 32-byte master key is read from a file with mode `0600` or stricter.
  - Decrypted values live only in memory: a cache with a 5-minute TTL, and a redactor that is seeded with every decrypted value.
  - Secrets never reach argv, logs, the LLM, Telegram, `inspect`, or error messages.
- **Subprocesses.**
  - The only executable is `git`, resolved once at startup (`app_settings.git_path`, default `exec.LookPath("git")`).
  - No shell, and no other binary: no `sudo`, `su`, `ssh`, `scp`, `sh`, `bash`, `zsh`, `docker`, `podman`, `systemctl`, `curl`, `wget`, package managers, `go`, `make`, `npm`, `python`.
- **Allowed git subcommands** (closed list; anything else is rejected before exec):
  - read: `rev-parse`, `cat-file`, `ls-tree`, `grep`, `show`, `log`, `diff`, `blame`, `status`, `branch --list`
  - write: `clone`, `fetch`, `pull --ff-only`, `checkout` / `switch` (including `-b` / `-c`), `add`, `commit`, `config --local` (allowlisted keys), `push` (no force)
  - dangerous: `reset --hard`, `clean -fd`, and `push` to a protected/base branch
- **Managed clones.** Mutating git commands (write and dangerous) run only inside `<repos_dir>/<owner>/<repo>`, where `repos_dir` defaults to `/var/lib/nexus-hub/repos`. They never run in a user checkout. Read tools may also use a configured read-only `local_path`.
- **Network.** In-process HTTP may only reach `api.github.com`, `api.clickup.com`, `api.telegram.org`, `api.anthropic.com` (and `github.com` for git over HTTPS). This is enforced by a dialer that also rejects loopback, private, and link-local resolved IPs, and redirects are never followed.
- **Tool policy.**
  - Permission tiers: `read` needs the repo configured; `write` needs the tool in the repo's `allow`; `dangerous` additionally needs the DB switch `allow_dangerous=true`, at most 1 per job, plus the tool's own preconditions. Nothing is allowed by default.
  - Every call, including denied and dry-run calls, gets a `tool_runs` audit row. Non-read calls are recorded `started` before execution and are never re-executed on retry.
- Untrusted input: comment content, tool output, repository content. The policy, not the prompt, is the security boundary.
- Never log tokens, Authorization headers, prompts, tool inputs and outputs, or payloads. Logs carry IDs and error codes only.
- English identifiers, comments, docs. Talk to the user in Vietnamese.
- DB tests use the guarded `TEST_DATABASE_URL` (loopback, `nexus_storage_test`). This is a test-only variable, not read by the server. Tests never call real GitHub, ClickUp, Telegram, or Anthropic: they use httptest, temp git repos, and a local bare repo as the "remote".
- Commit locally on `feature/phase2` at the end of each task. No push or merge without asking.

## Review Focus

1. A prompt-injected comment or repo file that tries to merge, close, push to `main`, force-push, `reset`/`clean` a user checkout, set `core.*`/`credential.*` config, read `.env`, or reach another host must be stopped by the policy. Each attempt gives a `denied` audit row and an `is_error` result. Covered in Tasks 8, 12, 14, 16.
2. A crash mid-loop followed by a retry must not repeat a push, commit, comment, or merge. An ambiguous call is reported as "unknown outcome". Covered in Tasks 8, 11, 17.
3. A malicious repo config (fsmonitor, hooks, external diff, textconv), a symlink or `..` path, a huge clone, or a hanging git must not execute code, escape the clone, or exhaust the VPS. Covered in Tasks 9, 13, 14.
4. A wrong account, a missing or undecryptable secret, a wrong master key, an expired App token, or a rate limit must fail visibly with a safe code and never fall back to another account or secret. Covered in Tasks 5, 10, 19.
5. Malformed LLM JSON, invented tools, budget overruns, a refusal, or a missing LLM credential must end in a Telegram report or fallback, never a lost comment. Covered in Tasks 1, 7, 16, 17.

---

## Embedded Design / Approval Contract

### Request flow (per ClickUp comment)

1. **Ingest.** For a source with `agent.enabled`:
   - comment from someone else: `external_user` message, then upsert the thread's pending agent job (no notice);
   - comment from `own_sender_id`: `human_owner`, context only;
   - status change: unchanged Phase 1 notice.
2. **Durable debounce.** At most one `pending` job per thread. `run_at = LEAST(now()+debounce_window, first_enqueued_at+debounce_max_wait)`. Both values are DB settings with defaults of 8s and 30s.
3. **Context packet.** Every part goes through `CleanInput`, and truncated parts carry the ` [truncated]` marker.
   - summary ≤ 800 runes;
   - the last 3 earlier messages, ≤ 300 runes each;
   - the new `external_user` batch after the last done job's `through_message_id`, ≤ 1200 runes.
4. **Tier-1.** `triage_model` (default `claude-haiku-5-5`), 300 max tokens, JSON only:
   `{"category":"ignore|notify|investigate","urgency":"low|normal|high","confidence":0..1,"summary":"≤200","goal":"≤300, required for investigate"}`
5. **Routing.** `threshold` defaults to 0.85.
   - `ignore` with confidence ≥ threshold: Ignore;
   - `investigate` with a usable tool: Investigate;
   - everything else: Notify.
6. **Tier-2.**
   - `actor_model` (default `claude-sonnet-5-5`), 1024 max tokens per step.
   - Limits: 5 steps, 12 tool calls, and a per-job token budget of `max_input_tokens` (60000) and `max_output_tokens` (6000).
   - The report is ≤ 1500 runes and ≤ 8 bullets.
   - Stop reasons: `step_limit`, `max_tokens`, `token_budget`.
7. **Outcomes.** Committed atomically with `ai_router` (and `ai_actor`) messages.
   - *Notify:* `🧠 Nexus · <title>\n<urgency> · <summary>\n<task url>`.
   - *Investigate:* `🔎 Nexus · <title>\n<urgency> · <summary>\n\n<report>\n\nTools: <name ✓|✗|⛔|🧪 (write)|(DANGEROUS)>\nTokens: <in>/<out>\n<task url>`.
   - All outbound text goes through `CleanOutbound`.
8. **Failures.** Up to 5 attempts, with backoff 5/15/30/60s.
   - `llm_unconfigured` and `llm_refused` are final immediately.
   - On a final failure, the fallback message is `⚠️ AI triage unavailable (<code>)` followed by `FormatNotice` of the batch.
9. **Compressor.** Runs after success when there are ≥ 5 messages past the cursor (excluding the newest 3). `summary_model` (default `claude-haiku-5-5`), 300 tokens, ≤ 1200 runes, atomic under `LockMessageThread`.

### Configuration and secrets in the DB

- **`credentials`:**
  - `id uuid PK`, `workspace_id uuid REFERENCES workspaces`
  - `name text`, `kind text` (`telegram_bot|clickup_token|clickup_webhook|github_pat|github_app_key|anthropic_key`)
  - `ciphertext bytea`, `nonce bytea` (12 bytes), `key_version int DEFAULT 1`
  - `created_at`, `updated_at`
  - `UNIQUE (workspace_id, name)`
  - The GCM additional authenticated data is `workspace_id|name|kind`. A row copied to another name or workspace therefore fails to decrypt.
- **`app_settings`** (global key → `jsonb`) **[Claude's call]**:

  | Key | Default |
  |---|---|
  | `triage_model` | `claude-haiku-5-5` |
  | `actor_model` | `claude-sonnet-5-5` |
  | `summary_model` | `claude-haiku-5-5` |
  | `agent_poll_interval` | 1s |
  | `tool_timeout` | 30s |
  | `git_network_timeout` | 120s |
  | `max_input_tokens` | 60000 |
  | `max_output_tokens` | 6000 |
  | `allow_dangerous` | false |
  | `git_path` | — |
  | `repos_dir` | `/var/lib/nexus-hub/repos` |
  | `tmp_dir` | `/var/lib/nexus-hub/tmp` |
  | `max_clone_bytes` | 2 GiB |
  | `debounce_window` | 8s |
  | `debounce_max_wait` | 30s |
  | `notice_poll_interval` | 1s |
  | `notice_request_timeout` | 10s |
  | `anthropic_credential` | `anthropic` |

  - Unknown keys are rejected by the CLI, and values are validated by type and range.
- **Credential references in config:**
  - Phase 1: Telegram `bot_token_ref`, ClickUp `webhook_secret_ref`.
  - Phase 2: agent `clickup_token_ref`; GitHub accounts `token_ref` (PAT) / `private_key_ref` (App).
  - Each ref names a row in `credentials` for the same workspace.
  - Legacy `bot_token_env` / `webhook_secret_env` are still honored until migrated.
- **`SecretResolver.Resolve(ctx, workspaceID, ref) (string, error)`.** Error codes: `missing_credentials`, `decrypt_failed`, `master_key_unavailable`. It is used by the Telegram client, the ClickUp parser factory, GitHub credentials, the LLM client, and the ClickUp tool.
- **CLI** (`setup-notices`). Secrets are read from a no-echo TTY prompt or stdin, never argv.
  - `secret set --workspace-id <uuid> --name <n> --kind <k>`
  - `secret list`: names, kinds, and updated_at only
  - `secret delete`
  - `migrate-secrets --workspace-id <uuid>`: copies the values of legacy `*_env` references from the current process env into `credentials` and rewrites the configs to `*_ref`
  - `settings get|set <key> <value>`
  - `master-key init --path <file>`: generates 32 random bytes, writes the file with mode 0600, and refuses to overwrite an existing file

### Sanitizer (`internal/agent/sanitizer.go`)

- **`CleanInput`:** removes C0/C1 control characters except `\n`/`\t`, zero-width characters (U+200B–U+200D, U+2060, U+FEFF), and bidi controls (U+202A–U+202E, U+2066–U+2069); collapses blank lines.
- **`RedactSecrets`:** exact values from the redactor, plus these patterns:
  - GitHub tokens: `gh[pousr]_…`, `github_pat_…`
  - Anthropic: `sk-ant-…`
  - Slack: `xox[abprs]-…`
  - AWS: `AKIA…`
  - Telegram bot token: `\d{6,12}:[A-Za-z0-9_-]{30,}`
  - PEM private keys
  - the password in `postgres://user:pass@`
  - `Authorization: …` headers
  - `x-access-token:…@` in git URLs
- **`CleanOutbound(s, neutralizeMentions)`:** `CleanInput` plus `RedactSecrets`, optionally turning `@login` into `` `@login` ``. GitHub comments, commit messages, and Telegram text all pass through it.

### Tool core

- **`Tool`.** `Spec() ToolSpec` gives `{Name, Description, InputSchema, Tier}`. `Run(ctx, Call) (ToolOutput, error)`, where `Call{Input; Repo *RepoGrant; Account *ResolvedAccount; DryRun bool}`. `RepoScoped` tools expose `Repo(input)`.
- **`Executor.Call`** checks in this order:
  1. the tool exists
  2. it is enabled
  3. the input is ≤ 16 KiB of JSON
  4. the repo is configured
  5. tier gate: the tool is in `allow`; dangerous tools also need `allow_dangerous`; a tool may *escalate* a call to dangerous through `Tier(input, grant)`, e.g. a push to a protected branch
  6. budgets: ≤ 12 calls, ≤ `max_writes_per_job` (2) non-read calls, ≤ 1 dangerous
  7. idempotency for non-read calls: a prior `ok` returns its summary, a prior `started` returns `unknown_outcome`
  8. record `started`
  9. dry-run for non-read calls: return `DRY RUN: would …`
  10. run with the tier timeout
  11. sanitize, compact, and cap the output at 6 KiB
  12. record the final status

### GitHub REST

- **Accounts** (in agent config):
  - PAT: `{"type":"pat","token_ref":"gh_personal","login":"LDKhangg","commit_name":"…","commit_email":"…"}`. Verified with `GET /user`, cached for 10 minutes; a mismatch gives `account_mismatch`.
  - App: `{"type":"app","app_id":"…","installation_id":"…","private_key_ref":"gh_app_key","commit_name":"…","commit_email":"…"}`. The JWT is signed RS256 (`iat-60s`, `exp+9m`). Installation tokens are down-scoped (`repositories`, `permissions`) and cached until `expires_at-5m`.
- **Client.** Base `https://api.github.com` with a test override. Headers: Accept, API version, `User-Agent: nexus-hub`. No redirects (301 → `repo_moved`). 2 MiB response cap. The error codes are `unauthorized`, `forbidden`, `rate_limited` (retryable, retry-after/reset), `not_found`, `conflict`, and retryable 5xx/transport.
- **Tools.** All outputs are compact text. Merge preconditions are unchanged: not draft, open, `head_sha` matches, base branch in `merge_base_branches` (never implicit `main`/`master`), method allowed, green checks unless disabled.

  | Tier | Tool |
  |---|---|
  | read | `github_pr_view`, `github_pr_checks`, `github_pr_files`, `github_issue_view` |
  | write | `github_pr_comment` (`🤖 ` prefix, mentions neutralized) |
  | dangerous | `github_pr_merge`, `github_pr_close`, `github_issue_close` |

- **ClickUp read:** `clickup_get_task`.

### Git tools **[Claude's call]**

- **Common hardening, on every invocation:**
  - `git -C <root> --no-pager -c core.fsmonitor= -c core.hooksPath=/dev/null -c diff.external= -c core.pager=cat -c safe.directory=<root> -c protocol.allow=never -c protocol.https.allow=always -c credential.helper= -c core.askPass=`
  - plus `--no-ext-diff --no-textconv` where applicable
  - Child env: `PATH`, `HOME=<tmp_dir>/home`, `LANG=C.UTF-8`, `GIT_CONFIG_NOSYSTEM=1`, `GIT_CONFIG_GLOBAL=/dev/null`, `GIT_TERMINAL_PROMPT=0`, `NO_COLOR=1`.
- **Network commands** (`clone`/`fetch`/`pull`/`push`): the token is passed only via child env, never argv, as `GIT_CONFIG_COUNT=1`, `GIT_CONFIG_KEY_0=http.https://github.com/.extraheader`, `GIT_CONFIG_VALUE_0=AUTHORIZATION: basic <base64(x-access-token:TOKEN)>`. The remote URL is always `https://github.com/<owner>/<repo>.git`, built from the validated repo name.
- **Validation:**
  - `ref`/`branch` match `^[A-Za-z0-9._/-]{1,100}$`, do not start with `-`, contain no `..` / `@{` / `//`, do not end with `.lock` or `/`, and pass `git check-ref-format --branch`;
  - `path` is relative, cleaned, has no `..`, does not start with `/` or `-`, and is always passed after `--`;
  - the deny-list (`.env`, `*.pem`, `*.key`, `id_rsa|ed25519`, `secrets/`, `credentials`, `*.p12`, `*.kdbx`) applies to reads, listings, search results, and `add`.
- **Read tools** (read from committed objects; `local_path` or managed clone):

  | Tool | Command / behavior |
  |---|---|
  | `code_list_files` | `ls-tree`, ≤ 200 |
  | `code_search` | `grep -n -I -F/-E`, ≤ 50 hits, lines ≤ 200 runes |
  | `code_read_file` | `show <ref>:<path>`, ≤ 200 lines, line-numbered, binary refused, 1 MiB object cap |
  | `code_outline` | Go AST; regex heuristic for other files |
  | `git_log` | ≤ 20 entries |
  | `git_diff` | `--stat` + patch ≤ 6 KiB |
  | `git_blame` | ≤ 100 lines |
  | `git_status` | `status --porcelain=v2 --branch`, managed clone only |

- **Workspace tools** (managed clone only):

  | Tool | Tier | Input | Command / behavior |
  |---|---|---|---|
  | `git_sync` | write | `{repo}` | clones if absent (`clone --filter=blob:none --no-recurse-submodules`), otherwise `fetch --prune origin`. Clone size is checked against `max_clone_bytes` after the operation (`du`-equivalent walk in Go); over the limit, the clone is removed and the error is `clone_too_large`. |
  | `git_pull` | write | `{repo}` | `pull --ff-only` |
  | `git_checkout` | write | `{repo, branch, create?, start_point?}` | `switch [-c] <branch> [<start_point>]` |
  | `git_commit` | write | `{repo, paths[], message}` | `add -- <paths>` (deny-list checked; ≤ 100 paths) then `commit -m <message>`. Author/committer come from the grant account's `commit_name`/`commit_email` via `-c user.name= -c user.email=`. The message is sanitized and ≤ 2000 runes. No `--amend`, no `--no-verify` needed (hooks off). |
  | `git_config_set` | write | `{repo, key, value}` | key in {`user.name`, `user.email`, `pull.ff`, `branch.<name>.remote`, `branch.<name>.merge`}; everything else gives `config_key_denied` |
  | `git_push` | write, or **dangerous** when the target branch is in `merge_base_branches`, is the remote default branch, or matches `protected_branches` (default `["main","master"]`) | `{repo, branch}` | `push origin <branch>:<branch>` (never `--force`, `+refspec`, `--delete`, or `--mirror`). The branch must match a pattern in the repo's `push_branches` (default `["nexus/*"]`). |
  | `git_reset_hard` | dangerous | `{repo, ref}` | `reset --hard <ref>` |
  | `git_clean` | dangerous | `{repo}` | `clean -fd` (never `-x`, so ignored files stay) |

- **Limitation, noted on purpose.** There is no file-editing tool in Phase 2, so `git_commit` only commits changes that exist in the managed clone (e.g. after a checkout or a reset). Agent code edits are a later phase.

### VPS runtime limits **[Claude's call]**

- **Per subprocess:**
  - Timeout: read 30s, network 120s. The process group is killed on timeout.
  - `RLIMIT_CPU`: 20s (network commands 90s).
  - `RLIMIT_AS`: 1 GiB.
  - `RLIMIT_FSIZE`: 512 MiB.
  - Output: 1 MiB per stream.
  - `nice` 10.
  - At most 2 concurrent git processes, and a per-repo mutex for workspace tools.
- **Network:** the restricted HTTP transport described in Global Constraints. Git over HTTPS goes only to `github.com`, because the remote URL is built in code and `protocol.allow=never` blocks other transports.
- **systemd unit:** `deploy/systemd/nexus-hub.service`, a file only; deployment is a later phase.
  - `User=nexus`, `NoNewPrivileges=yes`
  - `ProtectSystem=strict`, `ProtectHome=yes`, `PrivateTmp=yes`, `PrivateDevices=yes`, `ProtectKernelTunables=yes`, `ProtectControlGroups=yes`
  - `ReadWritePaths=/var/lib/nexus-hub`
  - `MemoryMax=1G`, `CPUQuota=80%`, `TasksMax=64`
  - `RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX`
  - `CapabilityBoundingSet=`, `SystemCallFilter=@system-service`, `LockPersonality=yes`
  - `LoadCredential=master.key:/etc/nexus-hub/master.key` with `Environment=NEXUS_MASTER_KEY_FILE=%d/master.key`

### Token reduction **[Claude's call]**

1. Tier-1 uses a small model, 300 tokens.
2. Notify skips Tier-2.
3. Tier-2: a terse prompt, 1024 tokens per step, a report ≤ 1500 runes.
4. Tools return compact text, capped at 6 KiB.
5. Loop compaction: `tool_result` contents from steps ≤ k−3 are replaced by `[elided: <tool> <status>, <n> bytes]`, and the pairing is preserved.
6. Prompt caching: ephemeral on the system block and the last tool. Confirm the SDK fields via Context7 `/anthropics/anthropic-sdk-go`.
7. Usage accounting per call and per job; the budget is enforced, and usage is shown in Telegram and `inspect`.

### Agent configuration (ClickUp source `integrations.config.agent`)

```json
{
  "enabled": false, "confidence_threshold": 0.85, "persona": "", "clickup_token_ref": "clickup",
  "tools": {
    "enabled": ["github_pr_view","github_pr_checks","github_pr_files","github_issue_view",
                "code_list_files","code_search","code_read_file","code_outline","git_log","git_diff","git_blame",
                "git_status","clickup_get_task"],
    "dry_run": false, "max_writes_per_job": 2,
    "github": {
      "accounts": {
        "personal": {"type":"pat","token_ref":"gh_personal","login":"LDKhangg","commit_name":"Nexus Bot","commit_email":"nexus@example.invalid"},
        "workbot":  {"type":"app","app_id":"123","installation_id":"456","private_key_ref":"gh_app_key","commit_name":"nexus[bot]","commit_email":"nexus@example.invalid"}
      },
      "repos": {
        "LDKhangg/nexus-hub": {
          "account": "personal", "local_path": "", "managed": true,
          "allow": ["github_pr_comment","git_sync","git_pull","git_checkout","git_commit","git_push"],
          "push_branches": ["nexus/*"], "protected_branches": ["main","master"],
          "merge_methods": ["squash"], "merge_base_branches": [], "require_checks_green": true
        }
      }
    }
  }
}
```

### Decisions marked for approval **[Claude's call]**

- **Git tool set and tiers** as above. Push escalates to dangerous for protected or base branches. Force, delete, and mirror pushes are never possible.
- **`app_settings` keys and defaults.** Secret AAD binds each ciphertext to workspace, name, and kind. There is a 5-minute decrypted-value cache.
- **`credentials` are workspace-scoped**, while `app_settings` are global.
- **Workspace tools run only on managed clones**, under `repos_dir`.
- **Dangerous tools need the DB switch `allow_dangerous`**, the repo `allow` list, and the tool's preconditions, with at most 1 per job.
- **Write and dangerous tools run without per-call approval**, because approvals are deferred. `dry_run` is available.
- **Own tool loop**, not the SDK beta runner.

### Non-goals (later phases)

- Replies and approvals. Owner commands from Telegram (Phase 3).
- Agent code edits. GitHub webhook source. pgvector. BYOI dynamic pools. Multi-provider LLM. Actual VPS deploy.
- Master-key rotation command (`key_version` is stored for it).
- Live acceptance and independent review (Phase 3).

---

## File Structure

| Path | Responsibility |
|---|---|
| `internal/agent/domain.go`, `decision.go`, `sanitizer.go` | Triage types/routing, sanitizer |
| `internal/tenant/agent_config.go` | Agent, tool, account, repo grants; credential refs |
| `internal/secrets/{crypto,store,resolver}.go` | AES-GCM, master key, resolver + cache |
| `internal/settings/settings.go` | Typed `app_settings` with defaults/validation |
| `db/migrations/00003_create_agent_core.sql`, `db/query/{agent,action,tool_run,credential,setting}.sql` | Schema + queries |
| `internal/adapters/storage/notice_repository.go` (modify), `credential_store.go`, `settings_store.go` | Ingest, secret/settings persistence |
| `internal/adapters/control/telegram/client.go`, `internal/delivery/http/parser_factory.go` (modify) | Resolve Phase 1 secrets via resolver |
| `internal/memory/builder.go`, `compressor.go` | Packet, summary |
| `internal/agent/prompts.go`, `tier1_router.go`, `tier2_actor.go`, `orchestrator.go` | Agent |
| `internal/agent/tools/{tool,registry,executor}.go` | Tool core |
| `internal/adapters/tools/execrunner/runner.go` | Hardened `git`-only runner |
| `internal/adapters/netguard/transport.go` | Host-allowlisted HTTP transport |
| `internal/adapters/github/{client,auth,app_jwt}.go` | GitHub REST + credentials |
| `internal/adapters/tools/github/{read,write,dangerous}.go` | GitHub tools |
| `internal/adapters/tools/code/{git,read,workspace,outline}.go` | Git read + workspace tools |
| `internal/adapters/tools/clickup/task.go` | ClickUp read tool |
| `internal/adapters/storage/{tool_run_store,agent_store,action_store}.go` | Audit, jobs, outbox |
| `internal/domain/llm.go`, `port.go`, `internal/adapters/llm/anthropic.go`, `usage.go` | Tool calling, caching, usage |
| `internal/relay/action_worker.go`, `internal/adapters/control/telegram/control.go` | Outbound Telegram |
| `cmd/server/main.go`, `cmd/setup-notices/*.go`, `deploy/systemd/nexus-hub.service`, `.env.example`, runbook | Wiring, CLI, deploy file, docs |

---

### Task 1: Triage decision parsing and routing

**Files:**
- Modify: `internal/agent/domain.go`
- Create: `internal/agent/decision.go`, `decision_test.go`
- Delete: the empty placeholders `processor.go` and `route.go`

**Interfaces:**
- Produces:
  - `Category` with constants `ignore|notify|investigate`
  - `Urgency` with constants `low|normal|high`
  - `TriageDecision{Category; Urgency; Confidence float64; Summary, Goal string}`
  - `ParseTriageDecision(string) (TriageDecision, error)`, with wrapped `ErrInvalidDecision`
  - `RoutingPolicy{Threshold float64; ToolsAvailable bool}`
  - `Outcome` with constants `OutcomeIgnore`, `OutcomeNotify`, `OutcomeInvestigate`
  - `Route(TriageDecision, RoutingPolicy) Outcome`

- [ ] **Step 1: Failing tests** `TestParseTriageDecision`, `TestRoute`:
  - Parse:
    - plain, fenced, or prose-prefixed JSON → ok
    - two objects → the first is used
    - confidence outside [0,1] → error
    - unknown category → error
    - missing urgency → `normal`
    - `investigate` without a goal → error
    - summary over 200 runes or goal over 300 runes → error
    - empty string → error
  - Route:
    - `ignore` 0.9 → Ignore; 0.5 → Notify; 0.85 → Ignore
    - `investigate` with tools → Investigate; without tools → Notify
    - `notify` → Notify
- [ ] **Step 2:** `go test ./internal/agent -run 'TestParseTriageDecision|TestRoute' -count=1`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Same command. Expected: PASS.
- [ ] **Step 5:** Commit `feat(agent): parse and route Tier-1 triage decisions`.

### Task 2: Sanitizer

**Files:**
- Replace: `internal/agent/sanitizer.go`
- Create: `sanitizer_test.go`

**Interfaces:**
- Produces:
  - `NewRedactor([]string) *Redactor`; values shorter than 6 chars are ignored
  - `(*Redactor).Add(string)`, concurrency-safe
  - `CleanInput(string) string`
  - `(*Redactor).RedactSecrets(string) string`
  - `(*Redactor).CleanOutbound(string, bool) string`

- [ ] **Step 1: Failing tests:**
  - `TestCleanInput`: control, zero-width, and bidi characters removed; `\n`/`\t` kept; Vietnamese and emoji intact
  - `TestRedactSecrets` (table): one case per pattern, plus an exact value, a short value ignored, a Postgres URL keeping its user, a PEM block removed, and `https://x-access-token:abc@github.com` redacted
  - `TestCleanOutboundMentions`: emails unchanged
  - `TestRedactorConcurrentAdd`
- [ ] **Step 2:** `go test ./internal/agent -run 'TestCleanInput|TestRedact|TestCleanOutbound' -count=1`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** `go test -race ./internal/agent -count=1`. Expected: PASS.
- [ ] **Step 5:** Commit `feat(agent): sanitizer and secret redaction`.

### Task 3: Agent, tool, account, and repo configuration

**Files:**
- Create: `internal/tenant/agent_config.go`, `agent_config_test.go`

**Interfaces:**
- Produces:
  - `Tier` with constants `read|write|dangerous`
  - `GitHubAccount{Name, Type, TokenRef, Login, AppID, InstallationID, PrivateKeyRef, CommitName, CommitEmail string}`
  - `RepoGrant{Repo, Account, LocalPath string; Managed bool; Allow, PushBranches, ProtectedBranches, MergeMethods, MergeBaseBranches []string; RequireChecksGreen bool}`
  - `ToolPolicyConfig{Enabled []string; DryRun bool; MaxWritesPerJob int; Accounts map[string]GitHubAccount; Repos map[string]RepoGrant}`
  - `AgentConfig{Enabled bool; ConfidenceThreshold float64; Persona, ClickUpTokenRef string; Tools ToolPolicyConfig}`
  - `ParseAgentConfig(source map[string]any, known map[string]Tier) (AgentConfig, error)`
  - `ValidRepo(string) bool`, `ValidCredentialRef(string) bool` (`^[a-z0-9][a-z0-9_.-]{0,62}$`), `MatchBranchPattern(pattern, branch string) bool` (only a trailing `*` glob is supported)

- [ ] **Step 1: Failing tests** `TestParseAgentConfig` (table):
  - absent → disabled, with read-tool defaults
  - threshold out of range → error
  - unknown tool → error
  - a read tool in `allow` → error
  - an account missing for a repo → error
  - PAT without `login`, `token_ref`, or a commit identity → error
  - App with non-numeric IDs → error
  - invalid ref names → error
  - `merge_methods` containing `fast` → error
  - relative `local_path` → error
  - `managed:false` with a workspace tool in `allow` → error
  - push pattern `*` alone or containing `..` → error
  - defaults: `push_branches ["nexus/*"]`, `protected_branches ["main","master"]`, `require_checks_green` true
  - `max_writes_per_job` 6 → error
  - bad repo keys → error
  - `TestMatchBranchPattern`
- [ ] **Step 2:** `go test ./internal/tenant -run 'TestParseAgentConfig|TestMatchBranchPattern' -count=1`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Same command. Expected: PASS.
- [ ] **Step 5:** Commit `feat(tenant): agent tool permissions, accounts and repo grants`.

### Task 4: Schema and agent-aware ingest

**Files:**
- Create: `db/migrations/00003_create_agent_core.sql`, `db/query/{agent,action,tool_run,credential,setting}.sql` (generated sqlc)
- Modify: `internal/adapters/storage/notice_repository.go`, tests, `cmd/server/main.go`
- Test: `internal/adapters/storage/agent_ingest_test.go`

**Schema (exact):**

`agent_jobs`:
- `id bigint identity PK`
- `workspace_id`, `source_integration_id`, `thread_id`, with composite FKs to `hub_threads(workspace_id, id)` and `integrations(workspace_id, id)`
- `status CHECK IN ('pending','processing','done','failed') DEFAULT 'pending'`
- `attempts int DEFAULT 0`
- `run_at NOT NULL`, `first_enqueued_at DEFAULT now()`
- `locked_until`, `claim_token uuid`
- `through_message_id bigint`
- `last_error_code`
- `input_tokens`, `output_tokens`, `cache_read_tokens`, `cache_write_tokens`: `int NOT NULL DEFAULT 0`
- `created_at`, `updated_at`
- `UNIQUE INDEX (thread_id) WHERE status='pending'`
- `INDEX (status, run_at)`

`outbound_actions`:
- `id bigint identity PK`
- `workspace_id`, `integration_id`, with composite FK
- `kind CHECK IN ('telegram_message')`
- `payload jsonb CHECK object`
- `dedupe_key NOT NULL`
- `status`, `attempts`, `next_attempt_at`, `locked_until`, `claim_token`, `last_error_code`, `provider_message_id`
- `created_at`, `updated_at`, `sent_at`
- `UNIQUE (kind, dedupe_key)`
- `INDEX (status, next_attempt_at, locked_until)`

`tool_runs`:
- `id bigint identity PK`
- `workspace_id NOT NULL`
- `agent_job_id REFERENCES agent_jobs`
- `step int`, `tool text`
- `tier CHECK IN ('read','write','dangerous')`
- `repo`, `account`
- `idempotency_key NOT NULL`
- `input_redacted jsonb`
- `status CHECK IN ('started','ok','error','denied','timeout','unknown_outcome','dry_run')`
- `error_code`, `exit_code`, `duration_ms`, `output_bytes`
- `result_summary`
- `created_at`, `updated_at`
- `UNIQUE (idempotency_key) WHERE tier <> 'read' AND status IN ('started','ok')`
- `INDEX (agent_job_id)`

`credentials`:
- `id uuid PK`
- `workspace_id REFERENCES workspaces`
- `name text CHECK (name ~ '^[a-z0-9][a-z0-9_.-]{0,62}$')`
- `kind CHECK IN (…six kinds…)`
- `ciphertext bytea NOT NULL`
- `nonce bytea CHECK (octet_length(nonce)=12)`
- `key_version int NOT NULL DEFAULT 1`
- `created_at`, `updated_at`
- `UNIQUE (workspace_id, name)`

`app_settings`:
- `key text PK`
- `value jsonb NOT NULL`
- `updated_at`

RLS is enabled on all five tables. The Down migration drops them in reverse order.

**Interfaces:**
- Produces:
  - `NewNoticeRepository(pool, newID, DebounceSettings{Window, MaxWait})`
  - `SetKnownTools(map[string]tenant.Tier)`
  - queries `UpsertPendingAgentJob`, `SaveOwnerMessage`, and the credential and setting CRUD queries

- [ ] **Step 1: Failing DB tests:**
  - `TestAgentIngest_CommentEnqueuesJobNotNotice`
  - `TestAgentIngest_BurstCoalesces`
  - `TestAgentIngest_StatusStillNotice`
  - `TestAgentIngest_OwnerCommentContextOnly`
  - `TestAgentIngest_DisabledSourceUnchanged`
  - `TestAgentIngest_DuplicateCallback`
  - `TestMigration00003_Constraints`: a bad credential name → rejected; a nonce of length 11 → rejected
- [ ] **Step 2:** `TEST_DATABASE_URL=... go test ./internal/adapters/storage -run 'TestAgentIngest|TestMigration00003' -count=1 -v`. Expected: FAIL.
- [ ] **Step 3:** Write the migration and queries, run `make generate`, and add the agent branch in `accept`.
- [ ] **Step 4:** Run the same command, `goose -dir db/migrations validate`, and `go test ./internal/adapters/storage -count=1`. Expected: PASS, with Phase 1 green.
- [ ] **Step 5:** Commit `feat(storage): agent core, credentials and settings schema; durable debounce`.

### Task 5: Encrypted secrets, DB settings, resolver; migrate Phase 1 lookups

**Files:**
- Create:
  - `internal/secrets/crypto.go`, `store.go`, `resolver.go`, `*_test.go`
  - `internal/settings/settings.go`, `settings_test.go`
  - `internal/adapters/storage/credential_store.go`, `settings_store.go`, `*_test.go`
- Modify:
  - `internal/tenant/notification_config.go`: accept `bot_token_ref` / `webhook_secret_ref`, or the legacy `*_env`
  - `internal/adapters/control/telegram/client.go` and `internal/delivery/http/parser_factory.go`: take a `SecretResolver` instead of `lookupEnv`, keeping the legacy env fallback only for `*_env` refs
  - `internal/config/config.go`: reads only `DATABASE_URL` and `NEXUS_MASTER_KEY_FILE`; other getters are removed and their callers moved to `settings`

**Interfaces:**
- Produces:
  - `secrets.LoadMasterKey(path string) ([]byte, error)`: exactly 32 raw bytes, or 44 bytes of base64; mode must satisfy `&0o077==0`; error codes `master_key_unavailable` / `key_permissions`
  - `secrets.Seal(key []byte, plaintext []byte, aad string) (nonce, ciphertext []byte, err error)` and `secrets.Open(key, nonce, ciphertext []byte, aad string) ([]byte, error)`
  - `secrets.AAD(workspaceID, name, kind string) string`
  - `secrets.Store interface{ Put(ctx, workspaceID, name, kind string, nonce, ct []byte) error; Get(ctx, workspaceID, name string) (kind string, nonce, ct []byte, err error); List(ctx, workspaceID) ([]Meta, error); Delete(ctx, workspaceID, name) error }`, implemented by `storage.CredentialStore`
  - `secrets.NewResolver(store Store, key []byte, redactor interface{ Add(string) }, legacyEnv func(string)(string,bool), ttl time.Duration, now func() time.Time) *Resolver`
    - `Resolve(ctx, workspaceID, ref string) (string, error)`
    - `SetSecret(ctx, workspaceID, name, kind, value string) error`
    - A ref with the prefix `env:` uses the legacy env, with a one-time warning. A nil key means every DB ref fails with `master_key_unavailable`.
  - `settings.Settings` struct, typed fields for every key in the design table; `settings.Load(ctx, store) (Settings, error)` applies defaults; `settings.Validate(key string, raw json.RawMessage) error`; `storage.SettingsStore` with Get/Set/List

- [ ] **Step 1: Failing tests:**
  - `TestSealOpenRoundTrip`
  - `TestOpenWrongAADFails`: same ciphertext under a different name or workspace → error
  - `TestOpenWrongKeyFails`
  - `TestNonceUnique`: 1000 seals give no duplicate nonce
  - `TestLoadMasterKey`: raw 32 bytes ok; base64 ok; 0644 → `key_permissions`; 31 bytes → error; missing file → `master_key_unavailable`
  - `TestResolver_CacheTTL`: the second resolve within the TTL does not hit the store; after the TTL it does
  - `TestResolver_RegistersRedaction`
  - `TestResolver_MissingAndDecryptErrors`: safe codes, with no plaintext or ciphertext in the error text
  - `TestResolver_LegacyEnvRef`
  - `TestSettings_DefaultsAndValidation`: `max_input_tokens` -1 → error; unknown key → error; `allow_dangerous` defaults to false
  - DB: `TestCredentialStore_RoundTrip`, `TestCredentialStore_WorkspaceScoped`, `TestSettingsStore_RoundTrip`
  - `TestTelegramClient_UsesResolver`, `TestParserFactory_UsesResolver`, `TestNotificationConfig_RefOrLegacyEnv`
  - The Phase 1 tests stay green after their setup is updated to use a fake resolver
- [ ] **Step 2:** `TEST_DATABASE_URL=... go test ./internal/secrets ./internal/settings ./internal/adapters/... ./internal/delivery/... ./internal/tenant -count=1 -v`. Expected: FAIL.
- [ ] **Step 3:** Implement with AES-256-GCM and a 12-byte random nonce from `crypto/rand`.
- [ ] **Step 4:** Run the same command, plus `go test ./... -count=1`. Expected: PASS.
- [ ] **Step 5:** Commit `feat(secrets): encrypted DB credentials, DB settings and resolver`.

### Task 6: Context packet builder

**Files:**
- Create: `internal/memory/builder.go`, `builder_test.go`

**Interfaces:**
- Produces:
  - `ContextPacket{Title, TaskURL, Summary string; Recent, NewBatch []Message}`
  - `BuildPacket(title, taskURL, summary string, history []Message, lastTriagedID int64, clean func(string) string) ContextPacket`
  - `(ContextPacket).Render() string`, a `<ticket_content>` block with closing tags inside content escaped to `</ticket_content_>`
  - Constants `SummaryBudget=800`, `MessageBudget=300`, `BatchBudget=1200`

- [ ] **Step 1: Failing tests** `TestBuildPacket`:
  - Recent/NewBatch selection
  - owner and AI messages excluded from the batch
  - the cleaner is applied
  - truncation marker
  - an over-budget batch keeps the newest message
  - no split runes
  - closing-tag escape
- [ ] **Step 2:** `go test ./internal/memory -run TestBuildPacket -count=1`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Same command. Expected: PASS.
- [ ] **Step 5:** Commit `feat(memory): sanitized context packet builder`.

### Task 7: Tier-1 router and LLM usage; LLM credential from DB

**Files:**
- Create: `internal/agent/prompts.go`, `tier1_router.go`, `tier1_router_test.go`
- Modify: `internal/domain/llm.go` (`Usage`), `internal/adapters/llm/anthropic.go` and `client.go` (the API key comes from `Resolver` via `app_settings.anthropic_credential`; usage is mapped), `usage.go`

**Interfaces:**
- Produces:
  - `domain.Usage{Input, Output, CacheRead, CacheWrite int64}` with `Add`; `LLMResponse.Usage`
  - `TriageError{Code; Retryable}`, with codes `llm_unconfigured`, `llm_refused`, `llm_error`, `invalid_decision`
  - `NewTier1Router(llm domain.LLMProvider, model string)` and `Triage(ctx, packet, persona string, toolsAvailable bool) (TriageDecision, domain.Usage, error)`
  - `llm.NewClientFromSecret(ctx, resolver, workspaceID string, s settings.Settings, httpc) (*Client, error)`. A missing credential returns `(nil, ErrLLMUnconfigured)` and is not fatal.

- [ ] **Step 1: Failing tests:**
  - `TestTier1Router_Request`: model, 300 tokens, the system prompt contains `untrusted`/the schema/"JSON only", and the user message is the packet
  - `TestTier1Router_NoToolsOmitsInvestigate`
  - `TestTier1Router_Errors`
  - `TestTier1Router_ReturnsUsage`
  - `TestAnthropic_GenerateUsage` (httptest)
  - `TestNewClientFromSecret_Missing`
- [ ] **Step 2:** `go test ./internal/agent ./internal/adapters/llm -run 'TestTier1Router|TestAnthropic_GenerateUsage|TestNewClientFromSecret' -count=1`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Same command. Expected: PASS.
- [ ] **Step 5:** Commit `feat(agent): Tier-1 router with usage accounting`.

### Task 8: Tool core (registry, tiered executor)

**Files:**
- Create: `internal/agent/tools/tool.go`, `registry.go`, `executor.go`, and tests

**Interfaces:**
- Produces:
  - `ToolSpec{Name, Description string; InputSchema json.RawMessage; Tier tenant.Tier}`
  - `Call{Input json.RawMessage; Repo *tenant.RepoGrant; Account *tenant.GitHubAccount; DryRun bool}`
  - `ToolOutput{Text, Summary string}`
  - `ToolError{Code string; Retryable bool}`
  - `Tool interface{ Spec() ToolSpec; Run(context.Context, Call) (ToolOutput, error) }`
  - `RepoScoped interface{ Repo(json.RawMessage) (string, error) }`
  - `TierEscalator interface{ EffectiveTier(json.RawMessage, *tenant.RepoGrant) tenant.Tier }`, used by `git_push`
  - `NewRegistry(...Tool)`, with `Get`, `Specs(enabled)`, and `Tiers()`
  - `ToolRunStore interface{ Begin; Finish; Deny }`, with `RunStart` and `PriorRun`
  - `Limits{MaxCalls=12; MaxDangerous=1; ReadTimeout; NetworkTimeout; OutputCap=6144}`
  - `NewExecutor(reg, policy tenant.ToolPolicyConfig, limits Limits, allowDangerous bool, store ToolRunStore, s Sanitizer, jobID int64, workspaceID string)`
  - `Call(ctx, step, name, input) CallResult{Content string; IsError bool; Status string; Tier tenant.Tier}`
  - `CanonicalKey(jobID, tool, input) string`
  - A tool declares a network timeout by implementing `NetworkTool{ UsesNetwork() bool }`.

- [ ] **Step 1: Failing tests** with fake tools and a fake store, one per check:
  - `TestExecutor_UnknownTool`
  - `TestExecutor_NotEnabled`
  - `TestExecutor_InputTooLarge`
  - `TestExecutor_RepoNotConfigured`
  - `TestExecutor_WriteNotAllowedForRepo`
  - `TestExecutor_DangerousNeedsSwitch`
  - `TestExecutor_EscalatedTierGated`: a fake push to `main` becomes dangerous and is denied with the switch off
  - `TestExecutor_DangerousOnePerJob`
  - `TestExecutor_CallBudget`
  - `TestExecutor_WriteBudget`
  - `TestExecutor_IdempotentReplayOK`
  - `TestExecutor_IdempotentReplayStarted`
  - `TestExecutor_DryRun`
  - `TestExecutor_Timeouts`: read vs network
  - `TestExecutor_OutputSanitizedAndCapped`
  - `TestExecutor_GrantAndAccountPassed`
  - `TestCanonicalKey_OrderIndependent`
  - `TestRegistry_DuplicateName`
- [ ] **Step 2:** `go test ./internal/agent/tools -count=1`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** `go test -race ./internal/agent/tools -count=1`. Expected: PASS.
- [ ] **Step 5:** Commit `feat(tools): registry and tiered policy executor`.

### Task 9: Hardened `git`-only runner and network guard

**Files:**
- Create: `internal/adapters/tools/execrunner/runner.go`, `rlimit_linux.go`, `runner_test.go`, `internal/adapters/netguard/transport.go`, `transport_test.go`

**Interfaces:**
- Produces:
  - `execrunner.Command{Args []string; Dir string; Stdin []byte; Env []string; Timeout time.Duration; Network bool}`. Note: no `Path` field; the binary is fixed.
  - `execrunner.Result{Stdout, Stderr []byte; ExitCode int; TimedOut, Truncated bool}`
  - `execrunner.New(gitPath, tmpDir string) (*Runner, error)`. It verifies that `gitPath` is absolute and executable and that its base name is `git`.
  - `(r) Run(ctx, Command) (Result, error)`:
    - rejects `Args[0]`, the first non-`-c` argument, when it is not in the allowed-subcommand set (`subcommand_denied`);
    - applies the rlimits (`RLIMIT_CPU` 20s, or 90s if `Network`; `RLIMIT_AS` 1 GiB; `RLIMIT_FSIZE` 512 MiB) through a re-exec shim or `SysProcAttr` plus `prlimit` after start on Linux;
    - nice 10, a semaphore of 2, process-group kill, a 1 MiB cap per stream;
    - env = base env + `Command.Env`, with `HOME=<tmpDir>/home`.
  - `netguard.NewTransport(allowedHosts []string) *http.Transport`. The `DialContext` resolves the host, rejects anything not allowlisted, and rejects loopback, private, link-local, and unspecified IPs (`host_denied` / `address_denied`). `netguard.Client(timeout, hosts)` returns an `*http.Client` with `CheckRedirect` returning `http.ErrUseLastResponse`.

- [ ] **Step 1: Failing tests:**
  - `TestRunner_RejectsNonGitBinary`: path `/bin/sh` → error at `New`
  - `TestRunner_SubcommandAllowlist`: `daemon`, `submodule`, `remote`, `gc`, `worktree`, `filter-branch`, `-c x=y daemon` → `subcommand_denied`; `log` ok
  - `TestRunner_ArgsVerbatim`
  - `TestRunner_EnvMinimal`: the parent's `DATABASE_URL` and `ANTHROPIC_API_KEY` are absent; `Command.Env` is present
  - `TestRunner_TimeoutKillsGroup`
  - `TestRunner_OutputCap`
  - `TestRunner_Concurrency`: a third concurrent call waits
  - `TestRunner_CPULimit`: Linux; a busy-loop child is killed by SIGXCPU/SIGKILL
  - These tests use a fake `git` built by the test as a small Go binary named `git` in a temp dir, so behavior is controllable.
  - `TestNetguard_AllowsListedHost`: httptest served via an allowlisted name, mapped through a test resolver hook
  - `TestNetguard_DeniesUnlisted`
  - `TestNetguard_DeniesPrivateIP`
  - `TestNetguard_NoRedirect`
- [ ] **Step 2:** `go test ./internal/adapters/tools/execrunner ./internal/adapters/netguard -count=1 -v`. Expected: FAIL.
- [ ] **Step 3:** Implement. Unexported resolver/dial hooks exist for tests only.
- [ ] **Step 4:** `go test -race` on the same packages. Expected: PASS.
- [ ] **Step 5:** Commit `feat(tools): git-only hardened runner and network guard`.

### Task 10: GitHub REST client with PAT and GitHub App credentials

**Files:**
- Create: `internal/adapters/github/client.go`, `auth.go`, `app_jwt.go`, and tests

**Interfaces:**
- Produces:
  - `Credentials interface{ Token(ctx, workspaceID string, account tenant.GitHubAccount, repo string, perms map[string]string) (string, error) }`
  - `NewCredentials(httpc *http.Client, resolver *secrets.Resolver, now func() time.Time) Credentials`
    - PAT: resolve `token_ref`, then verify `GET /user` login with a 10-minute cache.
    - App: resolve `private_key_ref` (the PEM text), sign the JWT, and mint a down-scoped installation token cached until `expires_at-5m`.
    - Every token is added to the redactor.
  - `NewClient(httpc, creds) *Client`, with `Do(ctx, workspaceID string, account, repo string, perms, method, path string, body, out any) error`, returning `*GitHubError{Code; Retryable; RetryAfter}`
  - `signAppJWT(appID string, key *rsa.PrivateKey, now time.Time) (string, error)` and `parseAppKey(pem string) (*rsa.PrivateKey, error)` (PKCS#1 or PKCS#8)

- [ ] **Step 1: Failing tests** (httptest, with an RSA key generated in the test):
  - `TestPAT_LoginVerified`
  - `TestPAT_AccountMismatch`: no API call is made
  - `TestPAT_MissingCredential` → `missing_credentials`
  - `TestApp_JWTClaimsAndSignature`
  - `TestApp_TokenDownScoped`
  - `TestApp_TokenCachedUntilExpiry`
  - `TestApp_BadKey` → `invalid_app_key`
  - `TestClient_Headers`
  - `TestClient_Errors`: 401, 403, 403 rate limit, 429, 404, 422, 500, 301
  - `TestClient_TokenRegisteredForRedaction`
- [ ] **Step 2:** `go test ./internal/adapters/github -count=1 -v`. Expected: FAIL.
- [ ] **Step 3:** Implement with stdlib only.
- [ ] **Step 4:** `go test -race ./internal/adapters/github -count=1`. Expected: PASS.
- [ ] **Step 5:** Commit `feat(github): REST client with PAT and GitHub App credentials`.

### Task 11: GitHub read/write tools, ClickUp read tool, tool-run store

**Files:**
- Create: `internal/adapters/tools/github/read.go`, `write.go`, `tools_test.go`, `internal/adapters/tools/clickup/task.go`, `task_test.go`, `internal/adapters/storage/tool_run_store.go`, `tool_run_store_test.go`

**Interfaces:**
- Produces:
  - `githubtools.NewReadTools(c)`: `github_pr_view`, `github_pr_checks`, `github_pr_files`, `github_issue_view`
  - `githubtools.NewWriteTools(c, s)`: `github_pr_comment`
  - `clickuptools.NewTaskTool(httpc, resolver)`: `clickup_get_task`, which uses the agent's `clickup_token_ref`
  - `storage.NewToolRunStore(pool)`
  - App permission sets: read `{pull_requests,checks,statuses,issues,contents: read}`; comment `{issues,pull_requests: write}`

- [ ] **Step 1: Failing tests:**
  - `TestPRView_CompactOutput`
  - `TestPRChecks_Summary`
  - `TestPRFiles_PatchClipped`
  - `TestIssueView_BodyClipped`
  - `TestPRComment_PrefixedAndNeutralized`
  - `TestGitHubTools_InputValidation`
  - `TestGitHubTools_UsesGrantAccount`
  - `TestClickUpGetTask_FieldsOnly`, `_Errors`, `_NoRedirect`
  - DB: `TestToolRunStore_WriteIdempotency`, `_ReadsNotDeduped`, `_FinishDenyDryRun`
- [ ] **Step 2:** `TEST_DATABASE_URL=... go test ./internal/adapters/tools/... ./internal/adapters/storage -run 'PRView|PRChecks|PRFiles|IssueView|PRComment|GitHubTools|ClickUpGetTask|ToolRunStore' -count=1 -v`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Same command. Expected: PASS.
- [ ] **Step 5:** Commit `feat(tools): GitHub read/write and ClickUp read tools with audited runs`.

### Task 12: GitHub dangerous tools (merge, close)

**Files:**
- Create: `internal/adapters/tools/github/dangerous.go`, `dangerous_test.go`

**Interfaces:**
- Produces: `githubtools.NewDangerousTools(c, s)`: `github_pr_merge`, `github_pr_close`, `github_issue_close`, using the App permission sets from the design.

- [ ] **Step 1: Failing tests:**
  - `TestMerge_HappyPath`
  - `TestMerge_Preconditions` (table, no PUT in any case): draft, closed, SHA mismatch, base not listed, `main` with an empty list, method not allowed, failing check, pending check, combined status failure
  - `TestMerge_RequireChecksGreenFalse`
  - `TestMerge_Conflict409`
  - `TestClosePR_WithComment`
  - `TestCloseIssue_Reason`
- [ ] **Step 2:** `go test ./internal/adapters/tools/github -run 'TestMerge|TestClose' -count=1 -v`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Same command. Expected: PASS.
- [ ] **Step 5:** Commit `feat(tools): guarded GitHub merge and close tools`.

### Task 13: Git read tools

**Files:**
- Create: `internal/adapters/tools/code/git.go` (shared hardening, validation, clone-root resolution), `read.go`, `outline.go`, `read_test.go`

**Interfaces:**
- Consumes: `execrunner.Runner` (Task 9), `tools.*` (Task 8).
- Produces:
  - `codetools.NewReadTools(r *execrunner.Runner, reposDir string) []tools.Tool`: `code_list_files`, `code_search`, `code_read_file`, `code_outline`, `git_log`, `git_diff`, `git_blame`, `git_status`
  - `hardenedArgs(root string, sub ...string) []string`, `validRef`, `validBranch`, `cleanRepoPath`, `deniedPath`
  - `cloneRoot(grant *tenant.RepoGrant, reposDir string) (string, error)`: the managed clone when `grant.Managed`, otherwise `local_path`; it must be an existing work tree, else `repo_unavailable`

- [ ] **Step 1: Failing tests** on temp repos committed in the test:
  - `TestListFiles_DenyListFiltered`
  - `TestSearch_FixedStringAndLimits`
  - `TestReadFile_LineRangeNumbered`
  - `TestReadFile_UntrackedInvisible`
  - `TestReadFile_Binary`
  - `TestReadFile_DeniedPath`
  - `TestReadFile_PathTraversal`
  - `TestOutline_Go`, `TestOutline_Generic`
  - `TestGitLog_Format`
  - `TestGitDiff_StatAndCappedPatch`
  - `TestGitBlame_RangeLimit`
  - `TestGitStatus_ManagedOnly`
  - `TestRef_Validation`
  - `TestHardening_FsmonitorNotExecuted`, `TestHardening_ExternalDiffAndTextconvNotExecuted`: a marker file is never created
  - `TestRepoUnavailable`
- [ ] **Step 2:** `go test ./internal/adapters/tools/code -run 'List|Search|ReadFile|Outline|GitLog|GitDiff|GitBlame|GitStatus|Ref_|Hardening|RepoUnavailable' -count=1 -v`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Same command with `-race`. Expected: PASS.
- [ ] **Step 5:** Commit `feat(tools): hardened git code-reading tools`.

### Task 14: Git workspace tools on managed clones

**Files:**
- Create: `internal/adapters/tools/code/workspace.go`, `workspace_test.go`

**Interfaces:**
- Consumes: Task 13 helpers, `github.Credentials` (Task 10) for HTTPS tokens, `tenant.MatchBranchPattern`.
- Produces:
  - `codetools.NewWorkspaceTools(r *execrunner.Runner, creds github.Credentials, reposDir string, maxCloneBytes int64, s tools.Sanitizer) []tools.Tool`
    - `git_sync`, `git_pull`, `git_checkout`, `git_commit`, `git_config_set`, `git_push`: write
    - `git_reset_hard`, `git_clean`: dangerous
    - `git_push` implements `TierEscalator`
    - All implement `RepoScoped`; the network ones implement `NetworkTool`.
  - A per-repo `sync.Mutex` map serializes workspace tools on the same clone.
  - The remote base URL is private (`https://github.com/`), with an unexported override used by tests to point at a local bare repo through the `file://` protocol. That protocol is only allowed under the override.
  - Token injection uses `GIT_CONFIG_COUNT`/`KEY_0`/`VALUE_0` in `Command.Env`. App permission sets: sync/pull `{contents: read}`, push `{contents: write}`.

- [ ] **Step 1: Failing tests.** A local bare repo acts as the remote; the managed dir is a temp dir.
  - `TestSync_ClonesThenFetches`: the first call clones, the second fetches; the `.git/config` remote URL contains no token; argv never contains the token (the fake runner records argv)
  - `TestSync_TokenOnlyInEnv`
  - `TestSync_CloneTooLarge`: set `maxCloneBytes` small → clone removed, `clone_too_large`
  - `TestSync_RefusesNonManaged`: `managed:false` → `not_managed`
  - `TestPull_FFOnly`: a diverged branch → `pull_not_fast_forward`, working tree unchanged
  - `TestCheckout_CreateBranch`
  - `TestCheckout_InvalidBranch`: `-f`, `a..b`, `x.lock` → invalid
  - `TestCommit_IdentityFromAccount`: author and committer equal the account's `commit_name`/`commit_email`
  - `TestCommit_DenyListedPathRefused`: `.env` → denied, nothing staged
  - `TestCommit_MessageSanitized`: a token in the message is redacted
  - `TestCommit_HooksNotRun`: a pre-commit hook that writes a marker → no marker
  - `TestConfigSet_AllowlistOnly`: `user.name` ok; `core.fsmonitor`, `credential.helper`, `url.x.insteadOf`, `alias.x`, `remote.origin.url` → `config_key_denied`
  - `TestPush_PatternEnforced`: `nexus/feat` ok; `feature/x` → `branch_not_allowed`
  - `TestPush_EscalatesProtected`: `EffectiveTier` for `main` (protected), for a base branch, and for the remote default branch → dangerous
  - `TestPush_NeverForce`: the argv has no `--force`/`+`/`--delete`/`--mirror`; a non-fast-forward rejection → `push_rejected`
  - `TestResetHard_ManagedOnlyAndValidRef`
  - `TestClean_NoIgnoredFilesRemoved`: an ignored file survives
  - `TestWorkspace_PerRepoSerialized`
- [ ] **Step 2:** `go test ./internal/adapters/tools/code -run 'Sync|Pull|Checkout|Commit|ConfigSet|Push|ResetHard|Clean|Workspace' -count=1 -v`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Same command with `-race`. Expected: PASS.
- [ ] **Step 5:** Commit `feat(tools): git workspace tools on managed clones`.

### Task 15: Tool-calling provider with prompt caching and usage

**Files:**
- Modify: `internal/domain/llm.go`, `port.go`, `internal/adapters/llm/anthropic.go`
- Create: `anthropic_tools_test.go`

**Interfaces:**
- Produces:
  - `ToolDef`, `ToolCall`, `ToolResult`, `Turn`
  - `ToolRequest{Model, System; MaxTokens; Tools; Turns; CacheStatic bool}`
  - `ToolResponse{Text; Calls; Stop StopReason; Usage}`
  - `StopReason` constants `StopEndTurn`, `StopToolUse`, `StopMaxTokens`, `StopRefusal`
  - `ToolCallingProvider`, implemented by `*llm.Anthropic` with SDK v2 (`ToolUnionParam`, `StopReasonToolUse`, `ToolUseBlock`, `NewToolResultBlock`; ephemeral `cache_control` on the system block and the last tool, confirmed via Context7)

- [ ] **Step 1: Failing tests:**
  - `TestAnthropicTools_RequestShape`: includes `cache_control` placement
  - `TestAnthropicTools_ParsesToolUse`
  - `TestAnthropicTools_EndTurnText`
  - `TestAnthropicTools_Refusal`
  - `TestAnthropicTools_Usage`
  - `TestAnthropicTools_HTTPErrorNoBodyEcho`
- [ ] **Step 2:** `go test ./internal/adapters/llm -run TestAnthropicTools -count=1 -v`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** `go test ./internal/adapters/llm -count=1`. Expected: PASS.
- [ ] **Step 5:** Commit `feat(llm): tool calling with prompt caching and usage`.

### Task 16: Tier-2 ReAct actor with token controls

**Files:**
- Create: `internal/agent/tier2_actor.go`, `tier2_actor_test.go`; actor prompts go in `prompts.go`

**Interfaces:**
- Produces:
  - `ActorReport{Text; Steps; Calls []CallRecord; Stopped; Usage}`
  - `NewTier2Actor(llm domain.ToolCallingProvider, model string)`
  - `Investigate(ctx, packet, decision, persona string, specs []tools.ToolSpec, exec *tools.Executor, budget TokenBudget) (ActorReport, error)`
  - Constants `MaxActorSteps=5`, `ActorMaxTokens=1024`, `ReportMaxRunes=1500`, `KeepFullResultSteps=2`

- [ ] **Step 1: Failing tests:**
  - `TestActor_SingleToolThenAnswer`
  - `TestActor_ParallelCalls`
  - `TestActor_DeniedFedBackAsError`
  - `TestActor_HallucinatedTool`
  - `TestActor_StepLimit`
  - `TestActor_MaxTokensKeepsPartial`
  - `TestActor_TokenBudgetStops`
  - `TestActor_LoopCompaction`
  - `TestActor_RequestSettings`
  - `TestActor_Refusal`
- [ ] **Step 2:** `go test ./internal/agent -run TestActor -count=1`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** `go test -race ./internal/agent -count=1`. Expected: PASS.
- [ ] **Step 5:** Commit `feat(agent): Tier-2 ReAct actor with compaction and token budget`.

### Task 17: Agent worker, outcome commit, outbound Telegram actions

**Files:**
- Create: `internal/adapters/storage/agent_store.go`, `action_store.go`, `internal/agent/orchestrator.go`, `internal/relay/action_worker.go`, `internal/adapters/control/telegram/control.go`, and tests
- Modify: `internal/adapters/control/telegram/client.go`, extracting a shared `call` while `Send` keeps its behavior

**Interfaces:**
- Produces:
  - `AgentJob`, `JobContext`
  - `AgentStore{Claim; Load; AddUsage; CommitOutcome; CommitFallback; Retry}`, returning `ErrStaleAgentClaim` on a fencing mismatch
  - `NewOrchestrator(store, t1, t2, reg, runs, redactor, compressor, settings)`, with `Run`
  - `relay.NewActionWorker(store, sender, WorkerSettings)`
  - `telegram.(*Client).SendText(ctx, workspaceID, tokenRef, chatID string, threadID *int64, text string) (int64, error)`, with the token resolved through the resolver

- [ ] **Step 1: Failing tests:**
  - `TestOrchestrator_Ignore`
  - `TestOrchestrator_Notify`
  - `TestOrchestrator_Investigate`
  - `TestOrchestrator_RetryThenSuccess`
  - `TestOrchestrator_ExhaustedFallsBack`
  - `TestOrchestrator_NonRetryableImmediate`
  - `TestOrchestrator_RetryDoesNotRepeatWrite`: a `git_push` to a bare repo runs once across two attempts
  - `TestOrchestrator_BudgetCarriesAcrossAttempts`
  - `TestOrchestrator_StaleClaimIgnored`
  - DB:
    - `TestAgentStore_ClaimRespectsRunAt`
    - `_LeaseRecovery`
    - `_NewBatchAfterThrough`
    - `_CommitInvestigate`: labels, the `Tokens:` line, a planted secret redacted
    - `_UsageAccumulates`
    - `_Fallback`
    - `TestActionStore_ClaimMarkSentDedupe`
  - The action worker cases
  - `TestSendText_ClipsPlainText`
- [ ] **Step 2:** `TEST_DATABASE_URL=... go test ./internal/agent ./internal/relay ./internal/adapters/... -run 'Orchestrator|AgentStore|ActionStore|ActionWorker|SendText' -count=1 -v`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Same command, plus `go test ./internal/adapters/control/telegram -count=1`. Expected: PASS.
- [ ] **Step 5:** Commit `feat(agent): durable agent worker with one-way Telegram reports`.

### Task 18: Rolling compressor

**Files:**
- Create: `internal/memory/compressor.go`, `compressor_test.go`
- Modify: `agent_store.go`, `orchestrator.go`

**Interfaces:**
- Produces:
  - `SummaryStore{SummaryCandidates; SaveSummary}`; `SaveSummary` rejects a moved cursor
  - `NewCompressor(store, llm, model, clean)`, with `Compress(ctx, ws, thread) (bool, domain.Usage, error)`

- [ ] **Step 1: Failing tests:**
  - 4 candidates → no call
  - 6 candidates → one call with 300 tokens and a save ≤ 1200 runes
  - LLM error → no save
  - `TestAgentStore_SaveSummaryRejectsMovedCursor`
  - `TestOrchestrator_CompressAfterSuccess`
- [ ] **Step 2:** `TEST_DATABASE_URL=... go test ./internal/memory ./internal/agent ./internal/adapters/storage -run 'Compress|SaveSummary' -count=1 -v`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Same command. Expected: PASS.
- [ ] **Step 5:** Commit `feat(memory): rolling summary compressor`.

### Task 19: Wiring, setup CLI, systemd unit, runbook, end-to-end

**Files:**
- Modify: `cmd/server/main.go`, `main_test.go`, `cmd/setup-notices/main.go`, `init.go`, `main_test.go`, `.env.example` (reduced to `DATABASE_URL` + `NEXUS_MASTER_KEY_FILE`), `docs/runbooks/clickup-telegram-local.md`
- Create: `cmd/setup-notices/secrets.go`, `settings.go`, `github.go`, `deploy/systemd/nexus-hub.service`, `internal/adapters/storage/agent_endtoend_test.go`

**Interfaces:**
- Produces:
  - **Server startup:**
    1. load the master key; if unavailable, log a warning and continue, so that legacy env refs still work
    2. `settings.Load`
    3. build the resolver, the netguard HTTP clients, and the registry. The registry holds the GitHub read/write/dangerous tools, the code read and workspace tools (if `git` is found), and the ClickUp tool.
    4. `notices.SetKnownTools(registry.Tiers())`
    5. `runServices([]serviceWorker{noticeWorker, orchestrator, actionWorker})`
    6. ensure `repos_dir` and `tmp_dir` exist with mode 0700
  - **CLI** (`setup-notices`):
    - `master-key init --path`
    - `secret set|list|delete`
    - `migrate-secrets --workspace-id`
    - `settings get|set|list`
    - `enable-agent` / `disable-agent`
    - `github-account add --source-id --name (--pat-ref <ref> --login <l> | --app-id --installation-id --key-ref <ref>) --commit-name --commit-email`
    - `github-repo set --source-id --repo --account [--managed] [--local-path] [--allow t]... [--push-branch p]... [--protected-branch b]... [--merge-method m]... [--merge-base b]... [--no-require-checks]`
    - `github-check --source-id`
    - `init` gains `--bot-token-ref` / `--webhook-secret-ref`
    - Every command validates through the parsers in one transaction, and secrets are never accepted via flags.
  - **`inspect`** adds `agent_jobs` (with tokens), `tool_runs` (tool, tier, repo, account, status, error_code, duration), `outbound_actions`, and `credentials` (name, kind, updated_at only).
  - **`deploy/systemd/nexus-hub.service`** contains exactly the directives from the design.

- [ ] **Step 1: Failing tests:**
  - `TestRunServices_AnyWorkerStopShutsDown`
  - `TestRunServices_WaitsForAllWorkers`
  - `TestSecretSet_ReadsStdinNotArgs`: the value is absent from the parsed args and is stored encrypted, with the ciphertext ≠ plaintext
  - `TestMigrateSecrets_RewritesRefs`: the legacy `bot_token_env` becomes `bot_token_ref`, and Phase 1 delivery still works through the resolver
  - `TestMasterKeyInit_NoOverwrite`
  - `TestSettingsSet_Validates`
  - `TestGitHubRepoSet_WritesConfig`
  - `TestGitHubCheck_ReportsMismatchWithoutToken`
  - `TestInspect_NoLeak`: report, input, token, and ciphertext are all absent
  - `TestSystemdUnit_Directives`: parse the file and assert that the required directives are present
  - `TestAgentEndToEnd`: real stores and resolver, a fake LLM, a fake GitHub, a bare repo as the remote, and a fake Telegram, in this order:
    1. 2 comments in a burst
    2. 1 job
    3. Tier-1 investigate
    4. Tier-2 calls `git_sync`, `code_search`, and `github_pr_checks`, then answers
    5. 3 `tool_runs ok` rows
    6. 1 Telegram message with the report, the tools, and `Tokens:`
    7. no secret in any persisted text column
- [ ] **Step 2:** `TEST_DATABASE_URL=... go test ./cmd/... ./internal/adapters/storage -run 'RunServices|SecretSet|MigrateSecrets|MasterKeyInit|SettingsSet|GitHubRepoSet|GitHubCheck|Inspect|SystemdUnit|TestAgentEndToEnd' -count=1 -v`. Expected: FAIL.
- [ ] **Step 3:** Implement. Runbook section "Phase 2: agent core, tools and DB secrets", covering:
  - master key creation
  - `migrate-secrets`
  - `secret set`
  - settings
  - PAT and GitHub App setup (minimum permissions)
  - the managed clones directory
  - dry-run first
  - the dangerous switch and its meaning
  - the systemd hardening explanation
  - reading `inspect`
  - live acceptance in Phase 3
- [ ] **Step 4:** Full verification:
  - `goose -dir db/migrations validate`
  - `make generate` twice, with no diff the second time
  - `gofmt -l cmd internal` gives empty output
  - `go vet ./...`
  - `TEST_DATABASE_URL=... go test -race ./... -count=1 -v`
  - Expected: all PASS, 0 SKIP.
- [ ] **Step 5:** Commit `feat: wire Phase 2 agent core, tools, DB secrets, CLI, systemd unit and runbook`.

# Configuration reference

Nexus-Hub reads bootstrap configuration from the process environment. Runtime
settings live in PostgreSQL and apply after restart unless a command says
otherwise. Keep credentials out of settings, command arguments, Git and logs.

## Process environment

| Variable | Required | Purpose | Example placeholder |
|---|---|---|---|
| `DATABASE_URL` | Yes | PostgreSQL connection used by `nexus-server`, `nh` and goose. | `postgres://<user>:<password>@127.0.0.1:5432/<database>?sslmode=disable` |
| `NEXUS_MASTER_KEY_FILE` | For encrypted credentials | Absolute path to the owner-readable master-key file. | `/home/<user>/.config/nexus-hub/master.key` |

An integration may still refer to a legacy environment variable through a
config field such as `bot_token_env` or `webhook_secret_env`. The process must
then define that named variable. Migrate these references with
`nh migrate-secrets`; new credentials should use `nh secret set`, which reads
the value from stdin and encrypts it with the master key.

Create the key once with `nh master-key init --file <path>`, protect it with
mode `0600`, and back it up separately from the database. Never commit the key.

## Runtime settings

Use `nh settings list`, `nh settings get <key>`, and
`nh settings set <key> <value>`. Values below are defaults when no database
override exists. Durations use Go duration syntax. Byte limits are in bytes.

| Key | Type | Default | Allowed range or values | Effect | Read by |
|---|---|---:|---|---|---|
| `triage_model` | model name | `claude-haiku-5-5` | non-empty model name | Selects triage model. | Agent |
| `actor_model` | model name | `claude-sonnet-5-5` | non-empty model name | Selects action model. | Agent |
| `summary_model` | model name | `claude-haiku-5-5` | non-empty model name | Selects summary model. | Agent |
| `max_input_tokens` | integer | `60000` | `1000..2000000` | Caps agent input. | Agent |
| `max_output_tokens` | integer | `6000` | `100..200000` | Caps agent output. | Agent |
| `tool_timeout` | duration | `30s` | `1s..600s` | Limits one tool call. | Agent |
| `git_network_timeout` | duration | `120s` | `1s..1800s` | Limits Git network calls. | Agent tools |
| `agent_poll_interval` | duration | `1s` | `100ms..60s` | Sets agent queue polling. | Agent worker |
| `anthropic_credential` | credential reference | `anthropic` | credential name | Selects Anthropic secret. | Anthropic provider |
| `llm_provider` | enum | `anthropic` | `anthropic`, `claude_cli`, `opencode_cli` | Selects LLM backend. | LLM runtime |
| `claude_cli_path` | executable path | empty | empty or executable path | Overrides Claude CLI location. | Claude CLI provider |
| `claude_cli_timeout` | duration | `120s` | `5s..1800s` | Limits Claude CLI calls. | Claude CLI provider |
| `claude_cli_concurrency` | integer | `1` | `1..4` | Caps concurrent Claude CLI calls. | Claude CLI provider |
| `opencode_cli_path` | executable path | empty | empty or executable path | Overrides opencode location. | opencode provider |
| `opencode_cli_timeout` | duration | `120s` | `5s..1800s` | Limits opencode calls. | opencode provider |
| `opencode_cli_concurrency` | integer | `1` | `1..4` | Caps concurrent opencode calls. | opencode provider |
| `git_path` | Git executable path | empty | empty or Git executable | Overrides Git location. | Agent tools |
| `bwrap_path` | executable path | `/usr/bin/bwrap` | executable path | Selects bubblewrap binary. | Sandbox |
| `git_sandbox` | boolean | `true` | `true`, `false` | Enables Git tool sandboxing. | Sandbox |
| `repos_dir` | absolute path | `/var/lib/nexus-hub/repos` | absolute path | Stores managed repositories. | Agent tools |
| `tmp_dir` | absolute path | `/var/lib/nexus-hub/tmp` | absolute path | Stores temporary work. | Agent tools |
| `max_clone_bytes` | integer | `2147483648` | `1048576..107374182400` | Rejects oversized clones. | Agent tools |
| `allow_dangerous` | boolean | `false` | `true`, `false` | Deprecated; has no effect. | Compatibility loader |
| `debounce_window` | duration | `8s` | `1s..300s` | Groups nearby events. | Relay |
| `debounce_max_wait` | duration | `30s` | `1s..1800s` | Caps event grouping delay. | Relay |
| `notice_poll_interval` | duration | `1s` | `100ms..30s` | Sets notice queue polling. | Notice worker |
| `notice_request_timeout` | duration | `10s` | `1s..30s` | Limits notice delivery requests. | Notice worker |
| `behavior_workers` | integer | `2` | `1..8` | Sets behavior worker count. | Behavior engine |
| `behavior_poll_interval` | duration | `1s` | `100ms..60s` | Sets behavior queue polling. | Behavior engine |
| `behavior_max_tokens` | integer | `100000` | `1000..1000000` | Caps one behavior run. | Behavior engine |
| `clickup_chat_poll_interval` | duration | `30s` | `10s..600s` | Sets ClickUp Chat polling. | ClickUp poller |
| `listen_addr` | address | `:8080` | host and port | Binds HTTP server. | Server |
| `public_base_url` | public URL | empty | empty or `http(s)` URL | Builds public callbacks. | Server and setup |
| `log_level` | enum | `info` | `debug`, `info`, `warn`, `error` | Sets structured log level. | Server |
| `metrics_addr` | loopback address | `127.0.0.1:9090` | loopback host and port | Binds Prometheus metrics. | Server |
| `shutdown_timeout` | duration | `25s` | `1s..80s` | Limits graceful shutdown. | Server |
| `owner_telegram_chat_id` | integer | `0` | `-4503599627370496..4503599627370496` | Sets fallback owner chat. | Telegram runtime |
| `owner_telegram_user_id` | integer | `0` | `0..4503599627370496` | Identifies Telegram owner. | Telegram runtime |
| `owner_clickup_user_id` | digit string | empty | empty or decimal digits | Identifies ClickUp owner. | ClickUp and behaviors |
| `telegram_mode` | enum | `auto` | `auto`, `webhook`, `poll` | Selects Telegram ingestion mode. | Telegram runtime |
| `telegram_assistant` | boolean | `true` | `true`, `false` | Enables assistant messages. | Telegram assistant |
| `telegram_command_topic` | topic name | empty | empty or topic name | Restricts command topic. | Telegram runtime |
| `assistant_model` | Claude model | `claude-sonnet-5-5` | supported Claude model | Selects normal assistant model. | Telegram assistant |
| `assistant_fast_model` | Claude model | `claude-haiku-5-5` | supported Claude model | Selects fast assistant model. | Telegram assistant |
| `assistant_deep_model` | Claude model | `claude-opus-5-5` | supported Claude model | Selects deep assistant model. | Telegram assistant |
| `assistant_history_turns` | integer | `6` | `1..20` | Limits assistant history. | Telegram assistant |
| `assistant_timeout` | duration | `180s` | `10s..900s` | Limits assistant calls. | Telegram assistant |
| `assistant_rate_per_minute` | integer | `30` | `1..120` | Rate-limits assistant requests. | Telegram assistant |
| `topic_close_statuses` | status list | `complete,closed,done` | comma-separated statuses | Closes topics for matching states. | Telegram topic worker |
| `message_link_retention_days` | integer | `30` | `1..3650` | Retains message links. | Telegram cleanup |
| `clickup_draft_expiry_hours` | integer | `24` | `1..168` | Expires ClickUp drafts. | ClickUp actions |
| `worker_min_claude_left` | integer | `10` | `0..100` | Pauses workers below quota percentage. | Local worker scheduler |
| `worker_min_disk_free` | integer | `5368709120` | `0..17592186044416` | Pauses workers below free bytes. | Local worker scheduler |
| `worker_max_ram_used` | integer | `90` | `1..100` | Pauses workers above RAM percentage. | Local worker scheduler |
| `queue_max_pending` | integer | `500` | `1..1000000` | Caps queued orchestrator work. | Orchestrator |
| `workspace_max_concurrent_executions` | integer | `4` | `0..64` | Caps workspace executions; zero stops starts. | Orchestrator |
| `llm_daily_budget_usd` | float | `0` | `0..1000000` | Sets daily LLM budget; zero disables cap. | Orchestrator |
| `orchestrator_min_confidence` | float | `0.6` | `0.01..1` | Sets minimum plan confidence. | Orchestrator |
| `orchestrator_max_repair` | integer | `2` | `0..5` | Caps plan repair passes. | Orchestrator |
| `orchestrator_max_plan_iterations` | integer | `3` | `1..10` | Caps planning iterations. | Orchestrator |
| `orchestrator_max_replans` | integer | `2` | `0..10` | Caps workflow replans. | Orchestrator |
| `orchestrator_max_repair_attempts` | integer | `2` | `0..20` | Caps execution repairs. | Orchestrator |
| `orchestrator_max_tokens` | integer | `80000` | `1000..2000000` | Caps orchestration tokens. | Orchestrator |
| `orchestrator_max_wall_minutes` | integer | `90` | `1..1440` | Caps workflow duration. | Orchestrator |
| `alerts_enabled` | boolean | `true` | `true`, `false` | Enables operational alerts. | Alert worker |
| `alert_stuck_running_minutes` | integer | `15` | `1..1440` | Alerts on stuck running work. | Alert worker |
| `alert_stuck_queued_minutes` | integer | `30` | `1..1440` | Alerts on stuck queued work. | Alert worker |
| `retention_enabled` | boolean | `true` | `true`, `false` | Enables retention cleanup. | Retention worker |
| `retention_message_logs_days` | integer | `30` | `0..3650` | Retains message logs. | Retention worker |
| `retention_tool_runs_days` | integer | `90` | `0..3650` | Retains tool runs. | Retention worker |
| `retention_policy_decisions_days` | integer | `180` | `0..3650` | Retains policy decisions. | Retention worker |
| `retention_workflow_days` | integer | `90` | `0..3650` | Retains workflow records. | Retention worker |
| `retention_outbound_days` | integer | `30` | `0..3650` | Retains outbound records. | Retention worker |

## Database migrations

For a local checkout, apply committed migrations before starting either
binary:

```sh
goose -dir db/migrations postgres "$DATABASE_URL" up
```

For deployment, stop new traffic, back up PostgreSQL, run the packaged goose
binary against the release migrations, and start the new release only after
`up` succeeds. The VPS release script follows this order. Migrations are
forward-only in normal operation; never edit a migration already deployed.

CI validates migration syntax, regenerates sqlc output and rejects generated
drift. It also scans Git history with gitleaks. `.gitleaksignore` contains only
reviewed fingerprints for intentional fake fixtures; do not add broad paths or
real findings to it.

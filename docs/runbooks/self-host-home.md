# Self-host at home (main, Phases 1–3)

This guide runs Nexus-Hub on a home Linux machine that stays on. It receives
ClickUp webhooks and Telegram messages through a public HTTPS tunnel. It covers
Phase 1 notices, the Phase 2 agent and Phase 3 behaviors with Telegram commands,
and (section 9) PR auto-approve from ClickUp Chat DMs.

Phase 3 is merged with test debt (`docs/product/test-debt/phase3-behavior-engine.md`).
Keep every behavior on `dry_run: true`: then nothing is written to ClickUp until
you press "Đăng" or confirm.

Never paste tokens, secrets or webhook responses into chat, Git or shell
arguments. Secrets go in through `secret set` on stdin.

Commands below use `nh` for `go run ./cmd/setup-notices`. Build it once:

```sh
cd ~/Dev/nexus-hub && git checkout main && git pull
go build -o ~/.local/bin/nexus-server ./cmd/server
go build -o ~/.local/bin/nh ./cmd/setup-notices
```

## 1. PostgreSQL (Docker, survives reboots)

```sh
read -rs -p 'DB password: ' POSTGRES_PASSWORD; export POSTGRES_PASSWORD
docker volume create nexus-home-pg
docker run -d --name nexus-home-pg --restart unless-stopped \
  -p 127.0.0.1:5433:5432 -e POSTGRES_PASSWORD -e POSTGRES_DB=nexus_hub \
  -v nexus-home-pg:/var/lib/postgresql/data postgres:17-alpine
unset POSTGRES_PASSWORD
```

Create `~/.config/nexus-hub/env` (mode 0600) with exactly these two lines,
using your URL-encoded password:

```sh
DATABASE_URL=postgres://postgres:<password>@127.0.0.1:5433/nexus_hub?sslmode=disable
NEXUS_MASTER_KEY_FILE=/home/<you>/.config/nexus-hub/master.key
```

Load it in each terminal you use for setup, then migrate:

```sh
set -a; . ~/.config/nexus-hub/env; set +a
goose -dir db/migrations postgres "$DATABASE_URL" up      # applies 00001–00011
nh master-key init --path "$NEXUS_MASTER_KEY_FILE"         # 32 random bytes, 0600; back it up offline
```

## 2. Public HTTPS tunnel

ClickUp and Telegram must reach `127.0.0.1:8080`. Use a tunnel whose URL does
not change on restart, otherwise every webhook must be re-registered.

**Recommended: Tailscale Funnel** (free, stable `https://<machine>.<tailnet>.ts.net`):

```sh
curl -fsSL https://tailscale.com/install.sh | sh
sudo tailscale up                      # log in once
sudo tailscale funnel --bg 8080        # public https on 443 → local 8080
tailscale funnel status                # shows the public URL
```

Enable Funnel for the tailnet in the Tailscale admin console if asked.

**Alternative:** Cloudflare quick tunnel (`cloudflared tunnel --url
http://127.0.0.1:8080`). It needs no account, but its URL changes on every
restart. When your Cloudflare domain is ready, switch to a named tunnel on
your domain and update the two webhooks (sections 5 and 6).

Call the URL `$PUBLIC` below, with no trailing slash.

## 3. Telegram bot and your chat ID

1. Create a bot with BotFather and keep the token for step 4.
2. Find your own numeric user ID, which is also your private chat ID with the
   bot. Send `/start` to the bot, then read `message.chat.id` from `getUpdates`
   in a trusted client. Do this before section 6: `getUpdates` stops working
   once a webhook is set.
3. Use that same ID for notices, so everything arrives in your private chat
   with the bot. Phase 3 only accepts commands from your private chat.

## 4. Workspace, secrets and settings

```sh
nh init --workspace-name 'Home' --chat-id '<your-chat-id>' --team-id '<clickup-team-id>' \
  --bot-token-ref telegram --webhook-secret-ref clickup-webhook
# save workspace_id (WS), source_id (SRC), target_id (TG), callback_path

nh secret set --workspace-id $WS --name telegram  --kind telegram_bot     # paste bot token
nh secret set --workspace-id $WS --name anthropic --kind anthropic_key    # paste API key (or use the Claude CLI: section 4b)
nh secret set --workspace-id $WS --name clickup   --kind clickup_token    # paste ClickUp personal token

nh settings set listen_addr 127.0.0.1:8080
nh settings set public_base_url "$PUBLIC"
nh settings set owner_telegram_chat_id '<your-chat-id>'
nh settings set owner_clickup_user_id '<your ClickUp numeric user id>'
nh profile set --workspace-id $WS --language vi --timezone Asia/Ho_Chi_Minh --quiet-hours 22:00-07:00 --digest per_run
```

The server runs as your user (section 6), so point its data directories at a
path you can write. The defaults under `/var/lib/nexus-hub` are for the
system unit; as a user service the server stops at start with "cannot create
agent data directory":

```sh
mkdir -p ~/.local/share/nexus-hub/repos ~/.local/share/nexus-hub/tmp
nh settings set repos_dir "$HOME/.local/share/nexus-hub/repos"
nh settings set tmp_dir   "$HOME/.local/share/nexus-hub/tmp"
```

For the agent and GitHub (accounts, repos, `enable-agent`), follow
`clickup-telegram-local.md` §P2.4–P2.6. Behaviors take their ClickUp and GitHub
grants from the agent configuration of the ClickUp source, so do run
`nh enable-agent --source-id $SRC` with `dry_run` on.

## 4b. LLM provider: start with the Claude CLI, switch to an API key later

The hub needs an LLM for triage, the agent, thread summaries and behavior
runs. Pick one with `llm_provider`:

| `llm_provider` | Uses | Needs |
|---|---|---|
| `anthropic` (default) | the Anthropic API | the `anthropic` credential (section 4) |
| `claude_cli` | the Claude Code CLI already signed in on this machine (`claude -p`) | `claude_cli_path` |
| `opencode_cli` | the opencode CLI already signed in on this machine (for example with OpenAI) | `opencode_cli_path`, `provider/model` model ids |

Start with `claude_cli` while you build trust in the hub; no API key is
needed. Sign in to Claude Code once as the same user the server runs as
(`claude`, then `/login`), then:

```sh
nh settings set claude_cli_path "$(readlink -f "$(command -v claude)")"   # absolute path to the executable
nh settings set llm_provider claude_cli
nh settings set claude_cli_timeout 120s       # optional: one call (5s..30m)
nh settings set claude_cli_concurrency 1      # optional: calls at once (1..4)
nh llm check                                   # {"provider":"claude_cli","model":…,"ok":true,"latency_ms":…,"input_tokens":…}
systemctl --user restart nexus-hub             # the log line "llm provider: claude_cli" confirms it
```

How the hub runs the CLI: directly (no shell), in a fresh empty temp
directory that is deleted afterwards, with only `HOME`, `PATH`, `LANG` and
`XDG_CONFIG_HOME`/`XDG_DATA_HOME` in its environment, with every built-in
tool, MCP server, project setting and session file turned off
(`--tools "" --strict-mcp-config --setting-sources "" --no-session-persistence`).
The prompt and the ticket context go in on stdin, never in arguments. The
agent's tools are still run by the hub, under its own policy; the CLI only
proposes calls. The hub never reads or copies the CLI's sign-in files.

Notes:

- `PATH` is passed through: if `claude` is a script that needs `node`, make
  sure the service's `PATH` contains it (for example `Environment=PATH=…` in
  the unit file).
- The model settings (`triage_model`, `actor_model`, `summary_model`) keep
  their Claude model ids with `claude_cli`.
- Usage counts as on your Claude plan. A plan limit shows as
  `llm_unavailable`, a lost sign-in as `llm_unconfigured` (sign in again);
  a slow call as `llm_timeout`. Logs and Telegram show only these codes,
  never the CLI's text.
- `nh llm check` never prints the prompt or the answer. For `anthropic` it
  needs `--workspace-id $WS`.

Switch to the API key when you are ready; nothing else changes:

```sh
nh secret set --workspace-id $WS --name anthropic --kind anthropic_key    # paste API key (stdin)
nh settings set llm_provider anthropic
nh llm check --workspace-id $WS
systemctl --user restart nexus-hub
```

To go back, set `llm_provider` to `claude_cli` again and restart.

### opencode with OpenAI models

The hub can also use [opencode](https://opencode.ai) (v2) signed in on this
machine, for example with your OpenAI account. Sign in once as the same
user the server runs as (`opencode auth login`, pick OpenAI). With opencode
the model settings are `provider/model[#variant]` ids, so switch the
provider and the three models in one command (the hub refuses a provider
whose models do not fit and names them):

```sh
nh settings set llm_provider opencode_cli \
  opencode_cli_path "$(readlink -f "$(command -v opencode)")" \
  triage_model openai/gpt-6-luna actor_model openai/gpt-6-luna summary_model openai/gpt-6-luna
nh settings set opencode_cli_timeout 120s     # optional: one call (5s..30m)
nh settings set opencode_cli_concurrency 1    # optional: calls at once (1..4)
systemctl --user restart nexus-hub            # the log line "llm provider: opencode_cli" confirms it
nh llm check                                  # {"provider":"opencode_cli","model":"openai/gpt-6-luna","ok":true,…,"input_tokens":…}
```

`input_tokens` shows what the hub's agent prompt costs per call (a few
hundred tokens). A behavior or profile `model` must also be a
`provider/model` id while `opencode_cli` is the provider; a run with a
Claude id then fails with `llm_unconfigured` (and the other way round).

How the hub runs opencode: `opencode run --standalone --format json
--model <id> --agent nexus-hub --title nexus-hub`, directly (no shell), in a
fresh empty temp directory deleted afterwards, the prompt on stdin. The hub
writes an `opencode.json` into that directory defining the agent
`nexus-hub` with its system prompt, every permission denied and no MCP
server. The child's `XDG_CONFIG_HOME` is a fresh empty directory, so your
global opencode config, `AGENTS.md`, plugins and MCP servers are never
loaded; `XDG_DATA_HOME` (where your sign-in lives) passes through when set.
Each run's session is deleted afterwards (`opencode session delete`), so
hub calls do not stay in your opencode history.

Notes:

- opencode v2 always offers its built-in `skill` tool; it is inert here
  (skills are denied and none can load). As a fail-safe, **any tool call
  in opencode's output fails the call** with `llm_error` (logged as
  `cli_tool_use`) and its text is not used. The agent's real tools are
  still run only by the hub.
- opencode has no JSON-schema flag: the hub asks for one JSON object in the
  prompt, checks it against the schema and asks once more with the error
  when it does not fit; a second miss is `llm_error`.
- A missing sign-in and an unknown model both show as `llm_unconfigured`.

## 5. ClickUp webhook

Register a webhook on your team (as in `clickup-telegram-local.md` §5), with:

- endpoint: `$PUBLIC/webhook/clickup/$SRC`
- events: `taskCommentPosted`, `taskStatusUpdated` and `taskAssigneeUpdated`
- scope: a `list_id` or the whole team (no `task_id` for real use)

Store the returned secret with `nh secret set --workspace-id $WS --name
clickup-webhook --kind clickup_webhook`, then run `nh bind-webhook
--source-id $SRC --webhook-id <id>` (restart the server if it is already running).

## 6. Run the server as a user service

Create `~/.config/systemd/user/nexus-hub.service`:

```ini
[Unit]
Description=Nexus-Hub
After=network-online.target

[Service]
EnvironmentFile=%h/.config/nexus-hub/env
ExecStart=%h/.local/bin/nexus-server
Restart=on-failure
RestartSec=5s

[Install]
WantedBy=default.target
```

```sh
systemctl --user daemon-reload
systemctl --user enable --now nexus-hub
sudo loginctl enable-linger "$USER"       # keeps it running without a login session
journalctl --user -u nexus-hub -f         # logs: ids, codes and corr= only
```

Then register Telegram's webhook. This stores a fresh secret and never prints
it:

```sh
nh telegram webhook set    --workspace-id $WS --integration-id $TG
nh telegram webhook status --workspace-id $WS --integration-id $TG   # url_matches: true
```

Stop the machine from sleeping. For example, set `HandleLidSwitch=ignore` in
`/etc/systemd/logind.conf`, or turn sleep off in the desktop power settings.

## 7. Your behavior

```sh
cp examples/behaviors/spec-review.vars.example.yaml ~/.config/nexus-hub/spec-review.vars.yaml   # edit values
nh behavior import --workspace-id $WS examples/behaviors/spec-review.yaml --vars ~/.config/nexus-hub/spec-review.vars.yaml
nh behavior validate --workspace-id $WS spec-review
nh behavior publish  --workspace-id $WS spec-review
nh behavior enable   --workspace-id $WS spec-review
```

## 8. Smoke test

1. Send `/help` to the bot. It replies with the commands, including `/review`.
2. Comment on a task in the webhook scope. You get a notice, or an AI report
   when the agent is on.
3. Send `/review <task id or URL>`. It replies "Đã nhận … (run #N)". Drafts,
   asks and the digest follow.
4. Send `/trace #N`, or run `nh trace --workspace-id $WS N` / `nh runs
   --workspace-id $WS`.
5. Press "Đăng" on one draft only if the text is right. Check the
   `⟦nexus:oq-answers:start⟧` block in the task description. `/undo N` reverts
   it.

Write down anything odd: the run number, the `corr=` value and what you
expected. These go to the Tester as live evidence for TD3-3.

## 9. PR auto-approve from ClickUp Chat DMs

When someone sends you a ClickUp Chat direct message with a GitHub pull
request link, the `pr-auto-approve` behavior approves the pull request with
your own GitHub account, adds a reaction to the DM with your ClickUp account
and tells you on Telegram. There is no confirmation step, and anyone who can
DM you can get a PR approved on any repo your token reaches; add `senders`
to the trigger to narrow it (see the file's header). The DM text is never
stored, logged or sent to Telegram, and no model is called.

**a. A GitHub token that approves as you.** Create a personal access token
on your own GitHub account (an App account would approve as a bot and is
refused):

- classic: scope `repo` (every repo you can reach); or
- fine-grained: the repos to cover, with "Pull requests: Read and write" and
  "Metadata: Read". A fine-grained token covers one resource owner (you or
  one organization); create one per owner if needed.

Store it and add it as a `pat` account on the ClickUp source (no
`github-repo` grant is needed for approving). Behaviors take their GitHub
accounts and ClickUp token from this source's agent configuration:

```sh
nh secret set --workspace-id $WS --name gh_me --kind github_pat    # paste the token
nh github-account add --source-id $SRC --name me --pat-ref gh_me --login '<your GitHub login>'
nh github-check --source-id $SRC                                   # ok per account, never a token
```

`--login` must be your GitHub login: it is checked against the token, and it
is how the hub tells your own PR (`own_pr`, never approved) and your earlier
approval (`already_approved`) apart. The source's agent configuration must
not have `"dry_run": true` in `agent.tools` once you go live (step f):
that policy turns every approval into "policy dry run".

**b. Settings.** `owner_clickup_user_id` is required (section 4): without it
no DM ever matches, since the trigger only takes messages from others. The
poll interval is optional (default `30s`, `10s`..`10m`):

```sh
nh settings set owner_clickup_user_id '<your ClickUp numeric user id>'
nh settings set clickup_chat_poll_interval 30s
systemctl --user restart nexus-hub
```

ClickUp has no webhooks for Chat, so the server polls the DMs of the
source's team (`--team-id` of `nh init`) with your ClickUp token. It polls
only while an enabled behavior has a `clickup.chat_message` trigger, and
the first poll starts from "now": older DMs are never processed.

**c. Import, validate, publish, enable** (the file starts with
`dry_run: true`):

```sh
cp examples/behaviors/pr-auto-approve.vars.example.yaml ~/.config/nexus-hub/pr-auto-approve.vars.yaml   # set github_account: "me"
nh behavior import   --workspace-id $WS examples/behaviors/pr-auto-approve.yaml --vars ~/.config/nexus-hub/pr-auto-approve.vars.yaml
nh behavior validate --workspace-id $WS pr-auto-approve
nh behavior publish  --workspace-id $WS pr-auto-approve
nh behavior enable   --workspace-id $WS pr-auto-approve
```

Validation refuses an unknown account or an account that is not `pat`.

**d. Smoke test in dry run.** From another ClickUp account, DM yourself a
message with a PR link (`https://github.com/<owner>/<repo>/pull/<n>`). Within
about one poll interval (30s) Telegram shows "would approve <owner>/<repo>#<n>
/ would add reaction …". Check the run:

```sh
nh runs  --workspace-id $WS --behavior pr-auto-approve   # subject clickup:chat:<id>, prs, run_actions
nh trace --workspace-id $WS <run id>                      # each PR outcome, review id, reaction
```

On Telegram, `/trace #<run id>` or `/trace clickup:chat:<message id>` shows
the same.

**e. Probe the reaction name.** The default is `white_check_mark` (✅). ClickUp
only accepts names it knows, and a dry run does not call ClickUp. On the
first live run (step f), if the Telegram report says `reaction_unsupported`,
change `reaction` in the values file, import again and publish:

```sh
nh behavior import  --workspace-id $WS examples/behaviors/pr-auto-approve.yaml --vars ~/.config/nexus-hub/pr-auto-approve.vars.yaml
nh behavior publish --workspace-id $WS pr-auto-approve
```

**f. Go live.** Copy the example to your own file, set `dry_run: false`,
import it with the same values file and publish. Only runs started after
the publish use the new version:

```sh
cp examples/behaviors/pr-auto-approve.yaml ~/.config/nexus-hub/pr-auto-approve.yaml   # set dry_run: false
nh behavior import  --workspace-id $WS ~/.config/nexus-hub/pr-auto-approve.yaml --vars ~/.config/nexus-hub/pr-auto-approve.vars.yaml
nh behavior publish --workspace-id $WS pr-auto-approve
```

`/undo <run id>` removes the reaction the run added. Nexus never dismisses a
GitHub approval: the reply lists each approved PR to dismiss by hand on
GitHub.

Downtime: on restart the poller catches up from its stored cursor. It reads
at most about 250 new messages per DM channel in one poll; after a long
downtime, a single DM with a bigger backlog loses its oldest messages
(`chat_backlog_truncated` in the logs). Those messages start no run.

## Upkeep

- Updates: `git pull`, rebuild both binaries, run `goose … up`, then
  `systemctl --user restart nexus-hub`.
- Backups: back up the Docker volume `nexus-home-pg` and the master key. The
  credentials cannot be read without the key.
- When the tunnel URL changes: update `public_base_url`, rerun `nh telegram
  webhook set`, and update the ClickUp webhook endpoint (`PUT
  /api/v2/webhook/<id>`).
- Downtime: while the machine is off, webhooks fail. ClickUp retries only for
  a while; Telegram keeps updates for 24 hours.

# Self-host at home (develop, Phases 1–3)

This guide runs Nexus-Hub on a home Linux machine that stays on. It receives
ClickUp webhooks and Telegram messages through a public HTTPS tunnel. It covers
Phase 1 notices, the Phase 2 agent and Phase 3 behaviors with Telegram commands,
(section 9) PR auto-approve from ClickUp Chat DMs and (section 10) Telegram
reports when someone tags you in ClickUp.

Phase 3 is merged with test debt (`docs/product/test-debt/phase3-behavior-engine.md`).
Keep every behavior on `dry_run: true`: then nothing is written to ClickUp until
you press "Đăng" or confirm.

To run the hub on a VPS with continuous deployment from GitHub instead, see
`deploy/vps/README.md`.

Never paste tokens, secrets or webhook responses into chat, Git or shell
arguments. Secrets go in through `secret set` on stdin.

## Security (Phase 12)

Expose public endpoints only through an HTTPS tunnel; use a named Cloudflare
tunnel for a stable production URL. Keep the metrics listener on loopback.
CI scans full Git history for credentials. Gate C evidence is recorded in
`docs/product/reports/2026-11-23-phase12-gate-c.md`.

Commands below use `nh` for `go run ./cmd/setup-notices`. Build it once:

```sh
cd ~/Dev/nexus-hub && git checkout develop && git pull
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
KillMode=mixed
TimeoutStopSec=100

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

At home without a tunnel (`public_base_url` empty) you can skip the webhook:
with `telegram_mode` `auto` (the default) the hub polls Telegram itself
(`getUpdates`), and the log says `telegram: receiving updates by long
polling`. If a webhook is still set, the hub removes it once and logs
`telegram: webhook removed for polling`. To force a mode:
`nh settings set telegram_mode webhook` (or `poll`), then restart the hub.
Use `webhook` only when Telegram can reach `public_base_url`.

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
request link and asks you to approve it, the `pr-auto-approve` behavior
approves the pull request with your own GitHub account, adds a reaction to
the DM with your ClickUp account and tells you on Telegram. One model call
per DM decides whether the sender asks for an approval (verdict
`approve_request`) or something else (`other`); only `approve_request` with
confidence 0.8 or more approves. There is no confirmation step, and anyone
who can DM you and ask can get a PR approved on any repo your token
reaches; add `senders` to the trigger to narrow it (see the file's header).
The DM text goes to the model only: it is never stored in the trigger,
logged or sent to Telegram.

**0. An LLM provider.** The approval needs a working provider (section 4b).
Any of `anthropic`, `claude_cli` or `opencode_cli` works; for example
opencode with `openai/gpt-6-luna`, or `claude_cli` on your Claude plan.
Check it before going on:

```sh
nh llm check          # "ok": true (add --workspace-id $WS for anthropic)
```

Each DM with a PR link costs one model call. Without a provider, or when
the model gives no valid verdict, nothing is approved and the report says
"AI: không đưa ra được đánh giá (<code>)".

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

The poller reads only enabled sources. `nh bind-webhook` (section 5)
enables the source; if you only poll (no ClickUp webhook), enable it
yourself:

```sh
nh source enable --source-id $SRC     # {"source_id":…,"is_enabled":true}
```

It refuses a source without a team id. `nh source disable --source-id $SRC`
turns it off again. Enabling does not open webhooks: a source without a
webhook secret keeps refusing callbacks.

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

If you imported an earlier version of `pr-auto-approve` (without
`instructions` and `verdict`), import the updated file again with the same
values file and publish: only runs started after the publish use the AI
verdict. If you keep your own copy (step f), merge the new `instructions`,
`verdict`, `profile` and the `when` line on `github.pr_approve` into it.

**d. Smoke test in dry run.** From another ClickUp account, DM yourself a
message with a PR link (`https://github.com/<owner>/<repo>/pull/<n>`) that
asks you to approve it. Within about one poll interval (30s) plus the model
call, Telegram shows the line `AI: approve_request (0.9x)` with a short
summary, then "would approve <owner>/<repo>#<n> / would add reaction …".
Try a few messages before going live:

| DM (with a PR link) | Expected |
|---|---|
| "anh approve giúp em PR này nhé" | `AI: approve_request`, would approve, would react |
| "đừng approve vội, em còn sửa" | `AI: other`, approval skipped by the AI verdict, no reaction |
| "anh review giúp em, chưa cần approve" | `AI: other`, skipped |
| "PR này merge chưa anh?" | `AI: other`, skipped |
| a message telling the AI or the bot what to answer | low confidence (at most 0.5), skipped |

A borderline message may come back as `approve_request` below 0.8: it is
skipped as low confidence, which is the intended safe side. Check the run:

```sh
nh runs  --workspace-id $WS --behavior pr-auto-approve   # subject clickup:chat:<id>, prs, run_actions
nh trace --workspace-id $WS <run id>                      # the verdict, each PR outcome, review id, reaction
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

## 10. Mention reports

The `mention-report` behavior sends you one short Telegram report when
someone tags you in a ClickUp task comment or a ClickUp Chat channel
message: who, where, what they ask you and a suggested next step. It
writes nothing to ClickUp. It needs an LLM provider (section 9, step 0)
and costs one model call per tagged comment or message. Your own comments
and messages never start a run.

**a. Channel mentions (poll only).** These need only the chat poller of
section 9: `owner_clickup_user_id` set and the source enabled
(`nh source enable --source-id $SRC`, section 9 b). If you did section 9,
there is nothing more to do. The poller reads only channels with new
messages and starts a run only for a message that tags you
(`[@you](#user_mention#<your id>)`; group mentions do not count).

**b. Task comment mentions (ClickUp webhook).** ClickUp sends task comments
only through a webhook, so the server must be reachable over HTTPS. Skip
this step if your section 5 webhook already covers `taskCommentPosted` on
the whole team (a `list_id` scope only reports comments in that list).

1. A tunnel to `127.0.0.1:8080` (section 2). Tailscale Funnel keeps its URL;
   a Cloudflare quick tunnel is quicker to try but changes URL on restart:

   ```sh
   cloudflared tunnel --url http://127.0.0.1:8080     # prints https://<random>.trycloudflare.com
   # or: sudo tailscale funnel --bg 8080 && tailscale funnel status
   ```

   Set `PUBLIC` to that URL (no trailing slash) and store it:

   ```sh
   nh settings set public_base_url "$PUBLIC"
   systemctl --user restart nexus-hub
   ```

2. Register the webhook for `taskCommentPosted` on your team. The ClickUp
   token goes into a header file through the shell's `read` and `printf`
   (built-ins in bash and zsh, so it never appears in a process's
   arguments; `curl -H @file` needs curl 7.55 or later), and the response,
   which holds the webhook secret, goes into a private file that is never
   printed:

   ```sh
   umask 077
   printf 'ClickUp token: '; read -rs CU; printf 'Authorization: %s\n' "$CU" > ~/.config/nexus-hub/cu-auth.hdr; unset CU; echo
   printf '{"endpoint":"%s/webhook/clickup/%s","events":["taskCommentPosted","taskStatusUpdated","taskAssigneeUpdated"]}' \
     "$PUBLIC" "$SRC" > ~/.config/nexus-hub/cu-webhook.json
   curl -sS -o ~/.config/nexus-hub/cu-webhook-resp.json -w '%{http_code}\n' \
     -H @"$HOME/.config/nexus-hub/cu-auth.hdr" -H 'Content-Type: application/json' \
     --data @"$HOME/.config/nexus-hub/cu-webhook.json" \
     "https://api.clickup.com/api/v2/team/<clickup-team-id>/webhook"   # expect 200
   jq -r '.id' ~/.config/nexus-hub/cu-webhook-resp.json                 # the webhook id (not secret)
   ```

   Keep `taskStatusUpdated` and `taskAssigneeUpdated` in the list when other
   behaviors (for example `spec-review`) use them; a team can have only a
   limited number of webhooks, so prefer one webhook with all three events
   over a second one. If a webhook for this endpoint already exists, update
   its events with `PUT /api/v2/webhook/<id>` instead (same header file).

3. Store the secret from the response (stdin, never echoed), bind the
   webhook and remove the private files:

   ```sh
   jq -j '.webhook.secret' ~/.config/nexus-hub/cu-webhook-resp.json | \
     nh secret set --workspace-id $WS --name clickup-webhook --kind clickup_webhook
   nh bind-webhook --source-id $SRC --webhook-id '<webhook id>'
   systemctl --user restart nexus-hub
   rm -f ~/.config/nexus-hub/cu-webhook-resp.json ~/.config/nexus-hub/cu-webhook.json
   ```

   Keep `cu-auth.hdr` (mode 0600) only while you still need it, then
   remove it too.

4. Telegram webhook, so `/trace` and `/undo` reach the server (section 6):

   ```sh
   nh telegram webhook set    --workspace-id $WS --integration-id $TG
   nh telegram webhook status --workspace-id $WS --integration-id $TG   # url_matches: true
   ```

   Commands are accepted only from the owner chat (`owner_telegram_chat_id`).
   If that chat is a group rather than your private chat with the bot, also
   set your own numeric Telegram user id, so only you can use them there:

   ```sh
   nh settings set owner_telegram_user_id '<your Telegram user id>'
   systemctl --user restart nexus-hub
   ```

When the tunnel URL changes (quick tunnel restart), update
`public_base_url`, rerun `nh telegram webhook set` and update the ClickUp
webhook endpoint (see Upkeep).

**c. Import, validate, publish, enable.** No values file is needed:

```sh
nh behavior import   --workspace-id $WS examples/behaviors/mention-report.yaml
nh behavior validate --workspace-id $WS mention-report
nh behavior publish  --workspace-id $WS mention-report
nh behavior enable   --workspace-id $WS mention-report
```

The file runs with `dry_run: false`: it only reports on Telegram, and
`dry_run` holds back ClickUp writes, of which it has none. The report is a
`notify` message, so it waits for the end of your quiet hours and follows
your digest mode (`nh profile set --digest per_run` to get it at once).

**d. Smoke test.** From another ClickUp account:

1. In a Chat channel, write a message that tags you and asks something.
   Within about one poll interval plus the model call, Telegram shows the
   report: who (their ClickUp name), which channel, what they ask, a next
   step and a link to the channel. `/trace
   clickup:chat:<message id>` or `/trace #<run id>` shows the run, with the
   `Kênh` and `Nhắc tới` lines.
2. Comment on a task and tag yourself (`@you`) with a question. The report
   names the commenter and the task, says what the comment asks and links
   the task. `/trace <task id>` shows the run (the trigger
   `clickup.comment_added`).
3. A message or comment without your tag, and one you write yourself, start
   no run (`nh runs --workspace-id $WS --behavior mention-report`).

**e. Rate limits.** Every channel with new messages costs at least one
ClickUp call per poll, on top of the DM polling. In a busy workspace the
poller may hit ClickUp's limit: the logs show `chat_rate_limited` and the
poller backs off, which delays DMs (and so `pr-auto-approve`) too. If you
see it more than once in a while, raise the interval:

```sh
nh settings set clickup_chat_poll_interval 60s
systemctl --user restart nexus-hub
```

After a long downtime with more than about 500 active channels, the
channels beyond that are cut (`chat_mention_channels_truncated` in the
logs) and mentions in them are lost.

**f. Please report the first tagged comment.** How ClickUp's webhook
carries the tags of a task comment is not yet confirmed against a live
payload. After step d.2, send the Lead the result of `/trace <task id>`
(a `mention-report` run for that comment, or no run at all) and the `corr=` value
from `journalctl --user -u nexus-hub` if there is one. Do not paste the
webhook payload or any token. If no run starts while channel mentions
work, task comment mentions are not detected yet.

## 11. People and report style

The directory fills itself as Nexus sees a ClickUp user who DMs or tags you, a
GitHub PR author, or a Telegram sender. Reports then use the contact's current
name instead of a platform ID. List or inspect what Nexus knows with:

```sh
nh contact list --workspace-id $WS [--platform clickup] [--search '<text>']
nh contact show --workspace-id $WS <contact-id>
```

Name a contact by contact ID or `platform:external-id`. Quote names containing
spaces. Telegram offers the same `/alias` command; reports about an unnamed
ClickUp sender may also have a **Đặt tên** button—press it and reply with the
name.

```sh
nh contact alias --workspace-id $WS clickup:<user-id> 'anh Định'
# Telegram: /alias clickup:<user-id> anh Định
```

If two contacts are one person, move every account from the source into the
target. If one account was joined incorrectly, find its account ID with
`contact show` and split it into a new contact. Forgetting deletes the contact
and all its accounts; `--yes` is required. A later platform sighting can add
that person again.

```sh
nh contact merge  --workspace-id $WS <source-contact-id> <target-contact-id>
nh contact split  --workspace-id $WS <account-id>
nh contact forget --workspace-id $WS --yes <contact-id>
```

Choose `short`, `normal`, or `detailed` with `/prefs [style]`; `/prefs` alone
shows buttons for the style and optional lines. The CLI can set several values
at once (add `--behavior <name>` for a behavior-specific override):

```sh
nh report-prefs set --workspace $WS style=short show_reaction=false
nh report-prefs set --workspace $WS show_ai_summary=true show_links=true \
  show_pr_titles=true show_trace_hint=true language=vi
```

The owner's default is `short`, with the reaction line off and the AI summary,
links, PR titles, and trace hint on. For example:

```text
✅ Approved PR #1601 cho anh Định
AI: approve_request (0.94)
https://github.com/org/repo/pull/1601
```

Templates can be set for these kinds: `pr_approve`, `pr_skipped`,
`verdict_unavailable`, `mention`, `notify`, `fyi`, `digest`, and `failure`.
Write the template to a file (or use `--file -` to read stdin), then save,
preview, or reset it:

```sh
nh report-template set     pr_approve --workspace $WS --file report.txt
nh report-template preview pr_approve --workspace $WS
nh report-template reset   pr_approve --workspace $WS
```

`--behavior <name>` selects a behavior-specific template. On Telegram, send
`/template <kind>`, then reply to the bot's forced-reply message with the new
template; `/template <kind> reset` restores the default.

Placeholders are `{contact}`, `{contact_platform}`, `{pr_number}`, `{pr_repo}`,
`{pr_title}`, `{prs}`, `{verdict}`, `{confidence}`, `{ai_summary}`, `{items}`, `{outcome}`,
`{reason}`, `{task}`, `{channel}`, `{link}`, `{behavior}`, and `{run_id}`. Start
a line with `?{x}` to omit the whole line when placeholder `x` is empty; for
example, `?{link}{link}`. A template is limited to 1,000 characters and 12
lines. Unknown placeholders and control characters are rejected. Failure and
skipped-approval reports always retain a non-empty reason.

### Talk to the bot

You can also write to the bot in plain words. Nexus sends the conversation to
the `assistant_model` through your configured LLM provider. The assistant can
answer general questions and use bounded read tools for contacts, behavior
runs and traces, report preferences and templates, and workflow executions,
history and jobs.

```text
lần chạy gần nhất ra sao?          → reads the latest run and trace
workflow 42 đang ở đâu?            → reads its steps and history
giải thích đoạn Go này             → answers as an open conversation
đặt tên clickup:12345 là chị Lan   → asks first: "Làm" / "Huỷ"
resume workflow 42                 → asks first: "Làm" / "Huỷ"
```

Reads answer at once. Changes (a contact name, report style, or workflow
cancel/resume/retry) only run after you press **Làm**; **Huỷ** leaves them
unchanged. A confirmed workflow change remains safely retryable from the same
button after a temporary failure, but only until that confirmation expires;
resume confirmations are pinned to the wait shown when they were proposed.
Long answers are sent as ordered messages below Telegram's size limit. Recent
conversation (up to 20 exchanges with a 64 KiB transmitted-history budget) is
kept separately per workspace and chat, only in memory, and is forgotten when
Nexus restarts. Messages, prompts, tool inputs/results and answers are never
logged.

The defaults are model `claude-sonnet-5-5` (see "Assistant models and routing
(Phase 6a)" below for the fast and deep models), a 180-second turn timeout and 30
owner messages per chat per minute. Configure them with
`nh settings set assistant_model <claude-model-id>`,
`nh settings set assistant_timeout <duration>` and
`nh settings set assistant_rate_per_minute <number>`. Turn the assistant off
with `nh settings set telegram_assistant false`.

## 12. Durable workflows (Phase 5)

From Phase 5 every behavior run and every comment-pipeline batch is a durable
**execution**: a row pinned to one immutable published version, with ordered
steps, an append-only history and one job in a persistent queue. A restart
picks up where it stopped. The `nh workflow` commands below inspect and recover
executions. They print only IDs, states, reason codes, counters and references.
Message text, prompts, tool input and output, approval payloads and tokens are
never printed.

What is proven so far: the engine, cutover and these commands are tested
against PostgreSQL with fake providers only (see the Phase 5 completion report, written to
`docs/product/reports/`). Live ClickUp, Telegram and Claude behaviour after a
crash is still open evidence. Write down what you see (execution ID, state,
reason).

### States and reasons

An execution moves through `RECEIVED` → `QUEUED` → `RUNNING` (or `TRIAGING`,
`PLANNING`, `EVALUATING`) and ends `COMPLETED`, `FAILED` or `CANCELLED`. While
it waits for you it is `WAITING_FOR_APPROVAL` or `WAITING_FOR_USER`. An ended
execution never runs again. The `reason` field explains the last change:

| Reason | Meaning | What to do |
|---|---|---|
| `step_retry` | a step failed and is queued again after a backoff (5s, 15s, 30s, 60s) | nothing; it retries by itself, at most 5 attempts in all |
| `attempts_exhausted`, `non_retryable` | the step gave up; the job is a dead letter | read `workflow dead-letter`, fix the cause, then `workflow retry` |
| `deadline_exceeded` | the execution's own deadline passed | `workflow retry` starts a child with a fresh deadline |
| `lease_expired` | a worker stopped mid-step (crash, restart); the step is queued again | nothing |
| `side_effect_uncertain` | an action started but its outcome is unknown | check ClickUp/GitHub/Telegram by hand; this execution is never retried or resent |
| `cancelled` | you or the system cancelled it | nothing |

### Waits, deadlines and leases

- **Waits.** A step that asks you (a "Đăng" draft, an approval) records a
  durable wait and holds no job lease. Your button press is stored with the
  decision and wakes the step, even across a restart. An alias, report-style
  or feedback prompt never blocks an execution. If an execution stays
  waiting after you decided, `nh workflow resume` queues it again. It never
  decides for you.
- **Deadlines.** A step attempt has a 10-minute timeout by default. An
  execution may also have its own deadline (`workflow.execution_deadline_seconds` in a
  schema 2 behavior). It is fixed when the execution starts. When it passes,
  the execution fails with `deadline_exceeded`, even while waiting.
- **Leases.** A worker that claims a job holds a lease (15 minutes by
  default, always longer than the step timeout). Every write checks the claim.
  A worker that comes back after its lease expired, or after a cancel, writes
  nothing. The reconciler runs at startup and every 30 seconds. It requeues
  expired leases and lost wakeups and never steals a live lease.
- **Uncertainty.** If the hub crashes between sending a request and saving
  its receipt, it does not resend. The execution fails with
  `side_effect_uncertain`, and `workflow retry` refuses it. Decide by hand
  what happened.

### Restore and publish

Editing or restoring a behavior never changes a running execution: each one
keeps the version it started with.

```sh
nh behavior restore  --workspace-id $WS spec-review --version 3   # old version → draft
nh behavior validate --workspace-id $WS spec-review
nh behavior publish  --workspace-id $WS spec-review               # new version N+1
```

New triggers use the new version. Existing executions and their retries stay
on their pinned version.

### Inspect

```sh
nh workflow list    --workspace-id $WS                      # newest first, 50 per page
nh workflow list    --workspace-id $WS --state FAILED --limit 20
nh workflow list    --workspace-id $WS --cursor <next_cursor>
nh workflow show    --workspace-id $WS WF-42                # state, waits, resources, instruction versions, lineage
nh workflow steps   --workspace-id $WS 42                   # steps, attempts, usage, checkpoint references
nh workflow history --workspace-id $WS 42                   # ordered timeline
nh workflow jobs    --workspace-id $WS --status processing  # leases in flight
nh workflow dead-letter --workspace-id $WS                  # failed jobs with their reason
```

Lists print one JSON object with the rows and `next_cursor`. Pass it back with
`--cursor` for the next page; `""` means the end. Every command needs
`--workspace-id`. An execution of another workspace is `not_found`.

Caller-, provider- and configuration-supplied references are never printed.
Fields such as triggers, source IDs, behavior names, step keys and checkpoint
references appear as stable `sha256:...` digests instead. Use matching digests
across `show`, `steps`, `history`, `jobs` and `dead-letter` to correlate rows;
legacy run/job IDs and validated lifecycle states, kinds and reason codes stay
readable. An empty optional reference has an empty digest value.

`nh trace` shows the run's executions at the end ("Executions"), and
`trace --json` has them as `executions`. `nh inspect` adds `workflow_executions`
and `jobs` rows with their `legacy_run_id` / `legacy_job_id`. The old run and
job IDs are unchanged.

### Recover

```sh
nh workflow cancel --workspace-id $WS 42   # {"outcome":"cancelled","state":"CANCELLED",...}
nh workflow resume --workspace-id $WS 42   # {"outcome":"resumed","state":"QUEUED",...}
nh workflow retry  --workspace-id $WS 42   # {"retry_execution_id":57,"outcome":"created",...}
```

- **cancel** stops future work at once, even for a worker still running.
  A request already sent is not rolled back. Errors: `already_cancelled`,
  `not_cancellable` (it already ended).
- **resume** works only for a waiting execution whose decision is already
  recorded. Errors: `not_resumable` (not waiting, already ended, or no decision
  yet).
- **retry** works only for a `FAILED` execution. It creates one linked child
  execution on the same version. Finished steps with a reusable checkpoint are
  kept, and action identities stay the same, so a finished action is not done
  twice. The failed parent is not changed. Running retry again returns the
  same child (`"outcome":"existing"`). If the child also fails, retry the
  child. Errors: `not_retryable` (not FAILED), `side_effect_uncertain`,
  `unsafe_retry` (a finished step has no reusable checkpoint). There is no
  force-resend.

A failed command exits nonzero and prints the reason code on stderr.

### Cutover (one time, per workspace)

A new binary keeps a workspace on the legacy queues until you cut it over.
After the cutover the durable queue is the only scheduler for its behavior
runs and agent batches. Plan a maintenance window:

1. Back up the database:
   `docker exec nexus-home-pg pg_dump -U postgres -Fc nexus_hub > ~/nexus-hub-$(date +%F).dump`.
   Also keep the master key backup.
2. Stop the service: `systemctl --user stop nexus-hub`. Wait for running
   steps to finish, or for their leases to expire (`workflow jobs --status
   processing`).
3. Install the new binaries and run `goose … up`.
4. Preflight. It is read-only, so nothing changes:
   `nh workflow cutover --workspace-id $WS`. Check `ready: true`. Each
   `blocked` entry has a record ID and a code (`behavior_version_invalid`,
   `approval_step_unmappable`). Fix those, then run the preflight again.
   `in_flight_tool_runs` count actions with an unknown outcome. They are kept
   as uncertain and never resent.
5. Apply: `nh workflow cutover --workspace-id $WS --apply`. It locks the
   workspace and imports or fences everything in one transaction, or writes
   nothing and exits with `cutover_blocked`. A second apply is a no-op
   (`already_cut_over: true`).
6. Start the new binary: `systemctl --user start nexus-hub`.

Never start an old binary from before Phase 5 against a cut-over database. It
does not know the fence and would schedule work a second time. Disabling the
new worker is not a rollback either. The only way back is to restore the
step 1 backup, which loses everything since then. Get a separately approved
recovery plan first. No historical rows are deleted: old runs, agent jobs,
approvals and tool receipts stay readable with `trace`, `runs` and `inspect`.

### Restart and shutdown

`systemctl --user restart nexus-hub` is safe at any time after the cutover.
On shutdown the worker stops claiming. A step cut off mid-attempt writes
nothing more, and its lease expires. At startup the reconciler requeues it.
It continues from its last checkpoint, never from the beginning. Waiting
executions stay waiting. A button pressed while the hub was down arrives when
it starts (Telegram keeps updates for 24 hours) and wakes the step.

### Shutdown (Phase 12)

On SIGTERM the hub stops claiming and lets in-flight HTTP requests and job
steps finish for up to `shutdown_timeout`. A step still running then is cut
off, its lease is released, and the next start resumes it.

| Setting | Default | Range | Meaning |
|---|---|---|---|
| `shutdown_timeout` | `25s` | `1s`..`80s` | drain bound for HTTP requests and in-flight steps |

Both systemd units and the user unit above set `KillMode=mixed` and `TimeoutStopSec=100`. SIGTERM
goes to the hub process only, so git and CLI children of a draining step keep
running and the step records their real outcome. Keep `TimeoutStopSec`
above `shutdown_timeout` plus a few seconds. Never set `KillMode=control-group`
(the systemd default) on a hub unit: systemd would SIGTERM the children too,
and a step could record a child's exit 143 as a normal result. A custom unit
or a manual `kill` of a child has the same risk; stop the hub, not its
children.

### Git sandbox (Phase 12)

Production git runs inside `bubblewrap`, with `git_sandbox=true` and
`bwrap_path=/usr/bin/bwrap` by default. On Oracle Linux 9, install it with
`dnf install bubblewrap`. Confirm `sysctl user.max_user_namespaces` is greater
than zero before restarting the service. Keep `git_sandbox=false` only for a
non-production development box; startup logs a warning when it is disabled.

After restart, `nh status` should work and the journal should contain
`git sandbox: ready`. `sandbox_unavailable` means the binary validation,
namespace probe, or loopback egress proxy failed. Git tools remain visible but
fail closed, without starting git. Check the package, `bwrap_path`, user
namespace limit, and unit sandbox settings, then restart.

Network git commands receive only an HTTPS CONNECT proxy that accepts the exact
authority `github.com:443`; local commands have a separate network namespace.
The network commands use `--share-net`, so the proxy and fixed GitHub remote are
defence in depth rather than a kernel-level destination filter. The sandbox
binds only the command workspace and its private HOME as writable. The existing
`max_clone_bytes` preflight, watcher, and final size check enforce the workspace
disk cap on host paths outside the sandbox.

## Conversations and control (Phase 6)

Every durable execution has an owner-facing reference such as `WF-101`. Use
that reference in Telegram and with `nh workflow show`, `cancel`, `resume`,
`retry` and `history`; the CLI also accepts the positive numeric ID. A
reference is valid only inside its workspace, so another workspace's reference
is reported as not found.

When an owner message needs a workflow target, Nexus resolves it in this order:

1. exactly one explicit `WF-n` in the message;
2. the single workflow bound to the Telegram message being replied to;
3. otherwise, a clarification with at most five buttons for active workflows
   bound to that conversation.

An unknown or foreign reference, multiple references, or an ambiguous reply
does nothing until the owner chooses a target. Clarification buttons expire
after 10 minutes; a valid choice continues the original request once.

### Instructions and control commands

The explicit instruction forms are `WF-n <text>`, `/note WF-n <text>`, and a
text reply to a message bound to one workflow. Each accepted update is an
immutable numbered version (`v1`, `v2`, and so on). It does not interrupt work
already in progress. The behavior model step and the comment-pipeline
investigate step read the available versions when their next attempt starts;
an update received during a model or provider call waits for the next such
step or retry and is never injected mid-call. Instructions cannot widen
allowed actions, trusted targets, budgets, dry-run behavior or approval rules.

A reply that ends with `?`, and a reply without an explicit reference to a
message bound to a finished workflow (for example a final report), go to the
assistant with that workflow as context instead of being stored. An explicit
`WF-n <text>` or `/note WF-n <text>` for a finished workflow is refused.
Free-text messages ending in `?` are questions for the assistant, even when
they start with `WF-n`; use `/note` to force an instruction.

The owner-chat commands are:

- `/status WF-n`: state, current step, waits, latest instruction version and
  bound-resource metadata;
- `/list`: up to 20 active workflows in the workspace, newest first;
- `/cancel WF-n` and `/resume WF-n`: propose the operation through the normal
  **Làm** / **Huỷ** confirmation path;
- `/note WF-n <text>`: add a versioned instruction.

`/note` arguments are limited to 512 characters because commands are stored
in the durable inbox. For longer instructions send `WF-n <text>`, which is
kept only in memory until it is stored as an instruction (up to 2,000
characters).

For `/status`, `/cancel`, `/resume` and `/note`, omitting the reference uses the
same reply/clarification resolution rules where the command form permits it.

### Conversation privacy and restart behavior

The database stores message routing metadata, bindings and a sanitized
conversation summary, but no raw Telegram chat text. After an assistant
exchange, `triage_model` produces the summary from the previous summary and
the exchange. A stored summary is at most 2,000 bytes and is never written to
logs; if summary generation fails or is oversized, the previous summary is
kept. Raw recent chat context remains in memory only and disappears on
restart. The persisted summary is loaded after restart, while workflows,
bindings, instruction versions and waits continue from durable storage.

`nh workflow show --workspace-id $WS WF-101` reports `resources` grouped by
kind with a count. It prints external IDs only for metadata-only task/PR
identifiers and Telegram `<integration>:<chat>:<message>` identifiers, never resource text.
Its `instructions` object contains only `count`, `latest_version`, `latest_at`
and `used_version`; `used_version` is the highest version recorded by an
`owner_instructions` checkpoint. Instruction text and conversation summaries
are never printed. All lookups are workspace-scoped.

## Assistant models and routing (Phase 6a)

Free-text messages no longer all go to one large model with every tool. With
the Anthropic API provider, a first pass on a fast model decides how to answer:

| Setting | Default | Used for |
|---|---|---|
| `assistant_fast_model` | `claude-haiku-5-5` | the router; answers simple messages itself |
| `assistant_model` | `claude-sonnet-5-5` | work: the tool loop with the hub tools |
| `assistant_deep_model` | `claude-opus-5-5` | hard work, and every `/deep` message |
| `assistant_history_turns` | `6` (1–20) | recent exchanges sent with each call, after the stored summary |

Set them with `nh settings set <key> <value>`; `nh settings list` shows them.
The models take the same Claude model ids as `assistant_model`.

Routing rules:

- The router sees the stored summary, the last `assistant_history_turns`
  exchanges and your message, has no tools, and returns one JSON object with
  all three fields `route`, `reply` and `tier`:
  `{"route":"answer","reply":"…","tier":"none"}` (a short reply) or
  `{"route":"escalate","reply":"","tier":"work"|"deep"}`. `route` decides:
  an answer needs a non-empty reply of at most 2,000 characters (its tier is
  ignored); an escalation needs tier `work` or `deep` (its reply is ignored).
- It may answer itself only greetings, thanks, acknowledgements and general
  questions that need no workspace data. Anything about tasks, PRs,
  workflows, runs, people, reports, status or settings, any request to do or
  change something, and anything it is unsure about escalates.
- It fails upward: a router error, timeout, refusal, cut-off or invalid
  output runs the turn on `assistant_model`, never as a fast answer.
- Skips: a message that replies to a workflow (a `WF-n` target) and the CLI
  providers (Claude Code, opencode), which have no native structured-output
  API call, skip the router and keep the single path on `assistant_model`.
  Write changes still only run after you press **Làm**.
- Follow-ups: when the assistant's previous answer ends with a question (its
  last line ends in `?`), your next message skips the router and runs on
  `assistant_model`, so a short "ok" or "ừ" to an offer reaches the tools.
- `/deep <câu hỏi>` skips the router and runs the turn on
  `assistant_deep_model`, through the same queue and rate limit. `/deep`
  alone replies with a usage hint. Like plain text, its text is held only in
  memory, never in the update row.

Every answer ends with a small line naming the model that wrote it, for
example `· haiku`, `· sonnet` or `· opus`. The line is not kept in the
conversation history sent back to the model.

Each finished turn logs one metadata line, never text:

```text
msg="assistant turn" route=work model=claude-sonnet-5-5 steps=3 input_tokens=… output_tokens=… cache_read_tokens=… cache_write_tokens=… router_model=claude-haiku-5-5 router_input_tokens=… router_output_tokens=… router_cache_read_tokens=… router_cache_write_tokens=… router_error= latency_ms=…
```

`route` is `answer` (fast reply), `work`, `deep` (router or `/deep`),
`work_fallback` (the router failed), `work_followup` (a reply to the
assistant's question; router not run) or `skip` (router not run). The token
fields count only the final `model`'s calls. The `router_*` fields count the
router call that ran before a `work`, `deep` or `work_fallback` turn; they are
empty or zero when the router did not run and on `answer` turns, where the
fast call is itself the final answer (counted under `model`). `steps`
includes the router call. To estimate cost, sum the token fields per `model`
and the `router_*` fields per `router_model` from the service log (for
example `journalctl --user -u nexus-hub | grep 'assistant turn'`), then
multiply by that model's prices: input tokens at the input price, cache-write
tokens at the cache-write price, cache-read tokens at the much lower
cache-read price, and output tokens at the output price. A high
`cache_read_tokens` share means the cached prefix (system prompt, tools and
history) is working.

`router_error` is empty unless the router failed (`route=work_fallback`); then
it holds one fixed code, never text: `provider_error`, `timeout`,
`nil_response`, `stop_refusal`, `stop_max_tokens`, `stop_other` (the call
stopped for another reason), `decode` (not one JSON object with only the
known fields), `trailing_data`, `missing_field`, `bad_route`, `empty_reply`,
`reply_too_long` or `bad_tier`. Many `work_fallback` lines with the same
code point at the fast model or the router prompt, not at your messages.

## Telegram topics (Phase 6b)

Each workspace can have its own Telegram forum group. Inside it, every ClickUp
task you are involved in gets its own topic, a fixed topic can hold one kind of
notice (for example "PR approve"), and General stays for free chat, `/list` and
system alerts. Telegram only; topics in private bot chats are not supported.

### Turn it on

1. In the group, turn on **Topics** (Group settings, then Topics). Telegram
   converts the group to a supergroup, and its chat id changes to `-100…`.
2. Make the bot an admin with the **Manage topics** right.
3. Tell the hub the group's chat id for the workspace. It is stored as
   `owner_chat_id` on the workspace's Telegram control integration:

```sh
nh telegram group set  --workspace-id $WS --integration-id $TG --chat-id -1001234567890
nh telegram group show --workspace-id $WS --integration-id $TG
```

4. Set your Telegram user id, so the hub accepts your messages from the group
   (with `owner_telegram_user_id` at 0, group messages are ignored; `group set`
   prints a `warning` in that case):

```sh
nh settings set owner_telegram_user_id '<your Telegram user id>'
```

Run `set` again whenever the chat id changes, in particular right after the
conversion in step 1 (the old id stops working). A running server picks the
value up within a short while. Without `owner_chat_id` the hub keeps using the
integration's `chat_id`, then the global `owner_telegram_chat_id`. Inbound
messages are accepted only from that group and your Telegram user id
(`owner_telegram_user_id`).

### Where messages go (`report.topic`)

A behavior YAML chooses the topic of its reports with `report.topic`:

| Value | Destination |
|---|---|
| `task` (default) | the topic of the ClickUp task bound to the run; without a task, the `workflow` topic |
| `workflow` | a topic for the run (`WF-<id> · <behavior name>`) |
| `general` | the General chat |
| any other text (1-64 bytes) | a fixed topic of that name, for example `"PR approve"` |

Topic names are built when the topic is created (`<task id> · <task title>`,
at most 128 bytes) and are never stored or logged; the database keeps only
subject keys, thread ids, state and times. Reports, approval prompts,
clarifications and failure notices of a run all follow the same rule.

A ClickUp task topic is named `<custom id> · <task title>` (for example
`FC-6583 · Login`), or `<task id> · <task title>` when the task has no custom
id. Right after the topic is created the bot posts one message with the same
text and a **Mở task ClickUp** button to the task, and pins it; pinning needs
the bot's **Pin messages** admin right (without it the message stays,
unpinned, and the log says `telegram topic: task link not pinned`). Topics made
before this change keep their old name until you run
`nh telegram topics rename --workspace-id $WS`: it renames every task topic,
posts and pins the missing links in open topics, prints counts only
(`renamed`, `linked`, `failed`) and is safe to run again.

### Fallback

A message is never lost because of a topic. If the chat is not a forum, the bot
lacks **Manage topics**, or a topic call fails, the same message goes to
General, the topic is retried on a later send, and the log carries a fixed code
(`telegram topic: sent to General (<code>)`). A chat without topics is
remembered for 10 minutes so every send does not retry. A message that fell
back to General while its topic exists carries an **Mở topic** button.

### Jump links

- Paste a ClickUp task link (`https://app.clickup.com/t/<id>` or
  `https://app.clickup.com/t/<team>/<id>`) alone in General, or send
  `/topic <task id or link>`. The bot replies with an **Mở topic** button to
  that task's topic and creates the topic if needed. A task the workspace's
  ClickUp source cannot read gets a short "Không đọc được task" reply.
- Custom task IDs work too (`/topic FC-6583`, the bare ID in a link, or
  `https://app.clickup.com/t/<team>/FC-6583`). The hub resolves the custom ID
  to the canonical task with the link's team, or with the ClickUp source's
  `clickup_team_id` when the ID is bare; the two must match. The topic is
  keyed by the canonical task ID, so webhook and behavior messages for the
  same task land in the same topic.
- The assistant can do the same from plain words ("tạo topic cho task này
  <link>", "mở topic FC-6583"): it has the tool `open_task_topic` in the
  owner's forum group and sends the **Mở topic** button. It changes nothing
  but the group's topics, so it asks for no confirmation.
- `/list` in General adds one **WF-n · Mở topic** button for each active
  workflow whose task has a topic.
- Inside a topic, text without a `WF-n` goes to the task's single active
  workflow, asks you to pick when there are several, or asks the assistant
  with the task as context.

### Notices in General, task topic on demand

A behavior with `report.topic: general` (for example `mention-report`) or a
fixed topic keeps its notices there instead of opening one topic per run. When
the run is about a ClickUp task, the notice carries a **📌 Mở topic task**
button:

- Press it: the hub creates the task's topic (same name
  `<custom id> · <title>` and pinned task link as `/topic`) or reuses the one
  that exists, and replies with the **Mở topic** button. Pressing again never
  makes a second topic.
- Reply `/topic` (no argument) to the notice: the same as the button. Without a
  task behind the replied-to message, `/topic` keeps its usage reply.
- Reply with free text to the notice ("tạo topic", "làm task này", a
  `/plan`-like request): it goes to the assistant with only the task ID as
  context (the notice text is never passed as instructions). A reply to a
  message of an active workflow stays an instruction for that workflow.

A notice without a ClickUp task (for example a ClickUp Chat channel message)
has no button. PR approvals keep their fixed `PR approve` topic.

### Command topic (`telegram_command_topic`)

To keep commands out of General, pick a fixed topic name, create the topic and
set the name, then restart the hub:

```sh
nh telegram topics ensure --workspace-id $WS --name 'Lệnh'
nh settings set telegram_command_topic 'Lệnh'
```

`ensure` creates the fixed topic in the owner chat when it is missing and
prints `{"workspace_id":…,"chat_id":…,"thread_id":n}`; it is safe to run
again. Messages in that topic are handled exactly as in General: commands,
the assistant, a pasted task link (jump button), `/topic`, `/plan <TASK-ID>`;
replies go to that topic. Empty (the default) keeps today's behaviour. The
name is 1-64 bytes and cannot be `task`, `workflow` or `general`.

### Prune finished workflow topics

```sh
nh telegram topics prune --workspace-id $WS
```

Deletes, in Telegram (with its messages) and in the store, every `workflow`
topic (`WF-<id> · …`) whose execution has finished. Task topics, fixed topics
and topics of active executions are never touched. A topic already deleted in
Telegram still has its row removed. Prints counts only
(`{"deleted":n,"kept":n,"failed":n}`; `kept` counts workflow topics left in
place) and is safe to run again. Needs the bot's **Delete messages** admin
right in the group.

### Lifecycle

A topic closes (never deleted; history stays) when its ClickUp task reaches a
done or closed status and no workflow bound to the task is active. A task counts as done
when ClickUp's status type is done or closed, or the status name (trimmed, case
does not matter) is listed in the setting `topic_close_statuses` (comma separated, default
`complete,closed,done`):

```sh
nh settings set topic_close_statuses "complete,closed,done,shipped"
```

If a workflow of the task is still running when the status changes, the topic
closes after the last one ends (checked every 30 seconds; the task status is
then read from ClickUp if no status event was seen since the last restart). Any
new message for a closed topic reopens it first.

### Inspect

```sh
nh telegram topics --workspace-id $WS
```

Prints one JSON object with, per topic, the subject kind and key
(`clickup_task:<id>`, `workflow:<id>`, `fixed:<name>`), thread id, state
(`open`, `closed`, or `reserved` while being created), chat and integration
ids and times. It never prints topic or task names.

### Send a message to the owner chat

```sh
printf '%s' 'Phase report text' | nh telegram send --workspace-id $WS
printf '%s' 'Phase report text' | nh telegram send --workspace-id $WS --topic 'Reports'
```

The text comes from stdin (1 to 4000 characters, plain text, never argv) and
goes to the workspace's owner chat: General, or the fixed-name topic given by
`--topic` (created on first use; General when topics are unavailable, shown as
`"topic":"fallback_general"`). Prints `{"status":"sent","message_id":n}`. The
text is never logged. Needs `DATABASE_URL` and `NEXUS_MASTER_KEY_FILE`.

### Check the router (route-eval)

```sh
nh assistant route-eval --workspace-id $WS
nh assistant route-eval --workspace-id $WS --thread-history task
nh assistant route-eval --workspace-id $WS --file cases.txt --thread-history general
```

Runs the Phase 6a router prompt, on the fast model, over a built-in set of
owner messages (greetings, thanks, general questions, task and status
questions, action requests, follow-ups, design requests) or over `--file`
(one `<expected>|<message>` per line; expected is `answer`, `work`, `deep` or
`escalate`; lines starting with `#` are comments). `--thread-history` adds a
small synthetic earlier exchange (`general` or `task`). Per case it prints
the expected and got route, tier, `router_error` and token counts, never the
reply or the message text. The exit status is not 0 when any case does not
match or the router fails. It needs `llm_provider` `anthropic`.

## Local worker (Phase 7a)

The hub (on the VPS or at home) stays the orchestrator. Heavy work (clone,
edit, build, test, run Claude Code) runs on your own laptop when it is
online, with your own `claude` and `gh` logins. From a task topic you send
`/task <what to do>`; the laptop works in an isolated git worktree, pushes a
branch, opens a draft PR from your account, and the result appears in the same
topic. The laptop always connects **out** to the hub over HTTPS: no inbound
port, no fixed IP, and it keeps working when you change networks.

Nothing ever runs on the hub, on GitHub Actions or with the API key as a
fallback: with the laptop offline the job waits and the topic says so.

`/task` remembers the repository per ClickUp task: without `owner/repo` it
uses the repository of the task's last local task, as long as that repository
is still registered; naming one explicitly makes it the remembered one.

When Cloudflare Access protects the hub's host, add a Bypass Access
application for `<host>/worker/v1`: the worker authenticates with its own
device token and cannot pass an Access login.

### What the laptop needs

- `git`, `gh` (logged in as you: `gh auth status`), and `claude` (logged in
  with your subscription). `executor: opencode` is refused for now: the
  worker cannot impose a permission sandbox on opencode that a repository's
  own opencode config (or files written during a run) cannot loosen.
- A ClickUp read-only token in a file of mode 0600 (the worker reads the task
  title, description and recent comments itself; the hub never sends it a
  secret).
- The `nh` binary built for the laptop (`go build -o ~/.local/bin/nh
  ./cmd/setup-notices`; `nh` also builds for macOS and Windows).

### Install and enroll

1. Register the repositories on the hub (`nh github-repo ...`). Only
   registered repositories can be worked on.
2. In Telegram send `/worker enroll`. The bot answers with a one-time code
   (single use, valid 10 minutes).
3. On the laptop:

   ```sh
   nh worker enroll --hub https://hub.example.com --name laptop \
     --capabilities repo.write,executor.claude
   ```

   `nh` asks for the code on the terminal (it never goes in argv). It stores
   the device token in `~/.config/nexus-hub/worker-token` (mode 0600); the
   hub keeps only its hash. A local task needs the capability `repo.write`,
   so enroll with at least that one.
4. Write `~/.config/nexus-hub/worker.yaml` (no secret inside):

   ```yaml
   hub_url: https://hub.example.com
   repos_dir: ~/Dev/nexus-repos        # clones and per-job worktrees live here
   executor: claude                    # the only supported executor
   clickup:
     token_file: ~/.config/nexus-hub/clickup-token
   claude:
     model: ""                         # empty: the CLI default
     # allowed_tools: [Read, Edit, MultiEdit, Write, Glob, Grep, LS, TodoWrite]   # the default
   repos:
     owner/name:
       test_command: ["go", "test", "./..."]   # argv, no shell
       test_timeout_minutes: 15
   limits:
     wall_minutes: 60
     max_turns: 50
     max_changed_lines: 2000
   retention_days: 7                   # finished worktrees are removed after this
   ```

   Claude Code runs headless (`claude -p`), so nobody can approve a tool
   prompt. It may only use the allow-list: file tools only (`Read`, `Edit`,
   `MultiEdit`, `Write`, `Glob`, `Grep`, `LS`, `TodoWrite`). It gets **no
   shell at all**, not even "read-only" git: a prefix rule such as
   `Bash(git diff:*)` also allows `git diff --no-index` (reads any file,
   for example the worker token) and `git log --output=` (writes anywhere).
   The config refuses every `Bash` rule, `WebFetch`, `WebSearch` and MCP
   tools (`mcp__…`). The worker runs the repository's `test_command` itself
   after the executor finishes.

   File reads are confined to the worktree: the worker passes
   `--settings` with `permissions.blockReadsOutsideWorkingDirectories`
   and always denies `Read` on `~/.ssh`, `~/.config`, `~/.local/state`,
   `~/.gnupg`, `~/.aws` and `/etc` (this also covers `Glob` and `Grep`).
   The config refuses `allowed_tools` entries with absolute, `~/` or `..`
   paths. This is a permission rule inside `claude`, not an OS sandbox.
   Writes outside the working directory are blocked too, and `Edit` is
   denied under `~/.ssh`, `~/.config`, `~/.local`, `~/.gnupg`, `~/.aws` and
   `/etc`. These settings need `claude` 2.1.314 or newer; older releases
   ignore them, so the worker refuses to run jobs with an older or
   unreadable `claude --version` (the job fails with `executor_unsupported`
   and the worker log says so at start).

   The worker's own git commands in a worktree (status, add, diff, commit,
   push) run with `core.fsmonitor=false` and `core.hooksPath=/dev/null` at
   command-line precedence, and refuse to run when the worktree's `.git`
   link no longer points into `<clone>/.git/worktrees/`, so files the
   executor writes cannot make the worker run code. Nested `.git`
   directories are never staged.

   Credentials stay with the worker. `claude` and the `test_command` get
   the scrubbed environment (no `NEXUS_*`, `*_TOKEN`, `*_KEY`, secrets) and
   additionally no git/GitHub/SSH environment route: no `SSH_AUTH_SOCK`, no askpass
   helper, no `GH_*`/`GITHUB_*`, `GH_CONFIG_DIR` pointing to an empty
   directory, `GIT_CONFIG_GLOBAL=/dev/null`, `GIT_CONFIG_NOSYSTEM=1`,
   `GIT_TERMINAL_PROMPT=0`. `claude` keeps your real `HOME` (its
   subscription login lives there). The `test_command` runs code the
   executor wrote, so it also gets an empty `HOME` (under
   `<state_dir>/jobs/<job>/sandbox/`) with `XDG_CONFIG_HOME`,
   `XDG_DATA_HOME` and `XDG_STATE_HOME` unset; `GOPATH`, `GOMODCACHE`,
   `GOCACHE` and `XDG_CACHE_HOME` keep pointing at your real caches. Other
   toolchains whose caches default under `HOME` (cargo, npm, …) need their
   cache variable set in the service environment. This is environment
   isolation, not an OS sandbox: the test command still runs as your user
   and can read files your user can read. OpenSSH reads keys from the
   passwd home (`~/.ssh`) even with an empty `HOME`, so an unprotected key
   is not hidden from the test command. For repositories that use a
   `test_command`, use passphrase-protected keys with ssh-agent only (the
   agent socket is not passed on), or a read-only deploy key. Only configure
   a `test_command` for repositories whose task sources you trust.
5. Check: `nh worker status`, then `/worker` in Telegram (one card per device).

### Run it as a user service

The worker must survive logout and restart by itself.

Linux (`systemd --user`):

```ini
# ~/.config/systemd/user/nexus-worker.service
[Unit]
Description=Nexus-Hub local worker
After=network-online.target

[Service]
ExecStart=%h/.local/bin/nh worker run
Restart=always
RestartSec=5s

[Install]
WantedBy=default.target
```

```sh
systemctl --user daemon-reload
systemctl --user enable --now nexus-worker
sudo loginctl enable-linger "$USER"   # keeps it running without a login session
journalctl --user -u nexus-worker -f  # logs: ids and codes only
```

macOS: a `launchd` agent in `~/Library/LaunchAgents` that runs `nh worker run`
with `KeepAlive` true. Windows: a Task Scheduler task "at log on" that runs
`nh worker run` and restarts on failure.

`gh` and `claude` must work without an unlocked desktop. On Linux, when a
login keeps its token in the desktop keyring, a service started at boot cannot
read it. Test it the way the service runs:

```sh
systemd-run --user --wait --collect -p StandardOutput=journal gh auth status
systemd-run --user --wait --collect claude --version
```

If either fails, log in again so the token is stored in a file (`gh auth login`
with `GH_CONFIG_DIR` readable by the service; for `claude`, sign in once from
a plain terminal), or unlock the keyring at login.

### Screen lock, sleep, lid

- A locked screen is fine: the worker keeps running.
- While a job runs, the worker holds a sleep inhibitor (logind on Linux; the
  platform equivalent elsewhere, best effort). With no job, the machine sleeps
  normally. The worker never changes power settings.
- Closing the lid suspends the laptop unless you decide otherwise. If you want
  jobs to survive a closed lid on external power, set
  `HandleLidSwitchExternalPower=ignore` in `/etc/systemd/logind.conf` (your
  choice; it is off by default).
- When the laptop sleeps or loses the network in the middle of a job, the
  lease (10 minutes) runs out, the job becomes `INTERRUPTED` and the topic says
  so with Retry and Cancel buttons. The same device can resume it from its
  worktree within 24 hours by simply coming back online.

### Commands

| Where | Command | What it does |
|---|---|---|
| Telegram, in a task topic | `/task <instruction>` | start the `local-task` workflow for that task |
| Telegram, anywhere | `/task <TASK-ID or link> <instruction>` | same; custom ids like `FC-6583` work. With several registered repos, write `owner/repo` before the instruction |
| Telegram | `/worker` | one card per device: state, CPU, RAM, disk, Claude limits, tokens, jobs, failures, busy hours, last seen |
| Telegram | `/worker enroll`, `/worker list`, `/worker revoke <name>` | enrollment code, short list, revoke at once |
| Telegram | `/status [WF-n]`, `/cancel [WF-n]`, `/note [WF-n] <text>` | the workflow commands of Phase 6; `/status` shows the worker job state |
| Telegram | `/retry [WF-n]` | queue a failed, interrupted or needs-input job again |
| Telegram, in a task topic | `/comment <text>` | draft a ClickUp comment on that task from the text (preview with Đăng/Sửa/Huỷ); never a note to the running job (Phase 10) |
| Laptop | `nh worker run [--config path]` | poll the hub and run jobs until stopped |
| Laptop | `nh worker status [--json]` | the worker's own state and journal |
| Laptop | `nh worker clean` | remove worktrees of finished jobs now |
| Hub | `nh worker devices --workspace-id $WS [--json]` | the same fields as the cards |
| Hub | `nh worker jobs --workspace-id $WS`, `nh workflow show WF-n` | jobs, audit events, and the worker job of a workflow |

Text you send in the topic while the job runs is stored as an instruction of
the workflow and delivered to the worker with its next heartbeat (every 20 s).

### What the topic shows

- One message "Đang chờ máy local online" when no matching device is online.
- One message when every matching device is held back, for example "máy local
  sắp hết hạn mức Claude, chờ reset lúc ...".
- Which device took the job, with `WF-n`.
- One progress message, edited in place: phase, files, lines, tests, turns.
- The result: summary, branch, PR link, test outcome.
- On failure or interruption: a reason and Retry and Cancel buttons (bound to
  that chat, valid 24 hours; `/retry WF-n` works after that).

### Settings

```sh
nh settings set worker_min_claude_left 10       # percent of the Claude session or week left
nh settings set worker_min_disk_free 5368709120 # bytes free in repos_dir (5 GB)
nh settings set worker_max_ram_used 90          # percent of RAM in use
```

A job is not given to a device below `worker_min_claude_left` or
`worker_min_disk_free`, or above `worker_max_ram_used`; it waits and the topic
says why. Unknown values never block. Restart the hub after changing them.

### Offline, revoke, lost laptop

- **Offline:** the job waits in `WAITING_FOR_WORKER`. When a matching device
  comes online it takes the job and the topic is told. Nothing runs elsewhere.
- **Revoke:** `/worker revoke <name>` (or `nh worker revoke --workspace-id $WS
  <name>`). The token stops working at once on every endpoint, even for a
  running job. A lost laptop: revoke it first, then enroll the new one.
- **Rotate:** enroll again with a new code; the old device name must be revoked
  first.
- **Never merged by the worker:** it pushes a `nexus/<task>-<short>` branch and
  opens a draft PR. It never merges, approves, force-pushes or pushes to the
  default branch. Your own account cannot approve its own PR.

## Orchestrated tasks (Phase 7b)

`/plan <request>` starts an AI-planned task: the hub classifies the request,
asks the model for a plan (tasks with dependencies, executors and a budget),
shows the plan in Telegram under `WF-n`, runs the tasks in dependency order as
durable workflow steps and reports the result with evidence links. `/task`
is unchanged: it sends one instruction straight to the local worker with no
plan. In a ClickUp task topic `/plan <request>` uses that task; elsewhere use
`/plan <TASK-ID> <request>`, otherwise the bot replies with usage. In a chat
with the assistant, the `start_task` tool does the same as `/plan`.
`/status WF-n` shows the plan version, task states, last decision and the
budget used.

### Plan message and decisions

The plan message lists each task with its key, executor, dependencies and the
budget caps. After every task the model decides one of `EXECUTE`, `DELEGATE`,
`REQUEST_CLARIFICATION`, `REQUEST_APPROVAL`, `REPLAN`, `COMPLETE` or `FAIL`.
Go checks every decision and writes it, with its reason code, to the audit
(a decision Go changed shows `converted_from`). A clarification question or
approval (Làm/Huỷ) is bound to `WF-n`; the execution waits in
`WAITING_FOR_USER` until you answer. A replan creates plan version v+1; older
versions and finished task results are kept.

### Evidence rule for COMPLETE

`COMPLETE` is accepted only when every required task succeeded and holds at
least one evidence reference that belongs to this execution (a step output, a
resource binding, or for a code task the worker result: branch or PR URL, test
outcome). Otherwise Go turns it into `REPLAN` when budget remains, or `FAIL`
with reason `missing_evidence`.

### Budgets

Each execution has caps; the counters are checkpointed, so a hub restart
resumes with the same numbers. Set a cap with `nh settings set <key> <value>`:

| Setting | Default |
|---|---|
| `orchestrator_max_plan_iterations` | 3 |
| `orchestrator_max_replans` | 2 |
| `orchestrator_max_repair_attempts` | 2 |
| `orchestrator_max_tokens` | 80000 |
| `orchestrator_max_wall_minutes` | 90 |

`orchestrator_min_confidence` (default 0.6) is the Classify confidence below
which the bot asks you a question. Wall time runs from the start, including
waits for the laptop; waits never spend tokens. When a limit is reached no
more model calls are made and the execution waits for you
(`planning_budget_exceeded`) with two buttons: "Thêm 1 vòng" adds budget (at
most 3 times per execution, 10 more wall minutes each time) and "Huỷ" ends
it. If you do not answer, the wait expires and the execution fails.

### Executors

- **investigate** is read-only: it reads ClickUp and GitHub through the agent
  tools and every call is audited in `tool_runs`. It never writes anywhere.
- **Local worker** (`repo.write`): the plan delegates the task through the same
  machinery as `/task` (see Local worker, Phase 7a). The repository is the one
  named in the plan, else the task's remembered repository, else the only
  registered repository, else the bot asks. The laptop must be online: when it
  is offline the job waits in `WAITING_FOR_WORKER` and the wait counts against
  the wall budget. An orchestrated job sends no progress message to the topic;
  only the plan, decisions and the final report appear.
- A task is given only to an executor that covers all its capabilities;
  otherwise it is blocked and the bot asks you or fails.

### Inspect

```sh
nh workflow show --workspace-id $WS WF-12
```

For an orchestrated execution the JSON has an `orchestrator` object: `plans`
(version, status, created), `tasks` (key, title, executor, state,
`depends_on`, evidence references), `decisions` (time, decision, reason, task
key, `converted_from`), `usage` (tokens and cost) and `budget` (used and caps
from the latest checkpoint). It holds metadata only: titles, enums, codes,
counters and ids; no model text.

### Triage

The comment pipeline's Tier-1 triage now goes through the orchestrator
`Classify` call (prompt `classify@2`). If `Classify` cannot produce a valid
result, triage falls back to the previous (legacy) triage path, so routing,
reports and dedupe behave as before.

### Known limits

- Behavior actions are not wired as plan executors yet; only investigate and
  the local worker are offered.
- The static step graph has 30 task slots: one execution can run at most 30
  task runs, including reruns after a replan.
- Wall time is checked when the execution wakes, so an execution may run a
  little past its cap before it stops.

## Task topics and ClickUp replies (Phase 10)

Phase 10 puts the life of one ClickUp task in one Telegram topic, and lets you
answer in ClickUp from that topic. It needs the Telegram topics of Phase 6b.

### The flow

1. A notice about a ClickUp task arrives in General with the
   **📌 Mở topic task** button (see "Notices in General, task topic on demand").
2. Press it. The hub opens the task's topic (or reuses it).
3. From then on, ClickUp activity on that task goes to the topic, not to
   General: a comment, a threaded reply, a status change and an assignee
   change. Each message shows who, what kind of event, and the text or the
   change. Comment text is shown as untrusted text.
4. A task without a topic behaves as before: its notices stay in General.
5. A behavior run about that task whose `report.topic` is `general` also
   reports into the topic. Fixed topics (for example `PR approve`) and
   workflow topics do not change.
6. A closed topic is reopened when new activity arrives. If Telegram says the
   topic no longer exists, the message goes to General once with the
   **📌 Mở topic task** button. It never goes to another topic.

Not shown in the topic: comments the hub posted itself, and a comment that
started a behavior run (the run's own report covers it). A redelivered
webhook gives no second message. Inside a task topic,
`notice_mentions_only` does not filter: you see all activity of that task.

### The hub always asks before it writes to ClickUp

Every comment the hub writes on your behalf is a draft first. The draft is
shown in the task topic as an exact preview with these buttons:

- **Đăng**: posts the text once. The preview becomes "✅ Đã đăng lên
  ClickUp" with a link to the task.
- **Sửa**: press it, then reply to the preview with the new text. A new
  preview appears and the old buttons stop working.
- **Huỷ**: posts nothing.
- **Thử lại**: shown only after a failed post that is safe to retry.

A second press, a press on an old preview, or a press by anyone but the owner
does nothing. A draft that you do not decide expires (setting below) and posts
nothing. The text is deleted from the database once the draft is posted,
cancelled or expired. A failed post shows "❌ Không đăng được: <reason code>".

### Ask the assistant to draft a comment

In a task topic, write to the assistant as usual:

- "comment lên task là ..." drafts a comment on that task.
- "tag anh X hỏi deadline": the assistant resolves the name through the
  people directory (section 11) and tags the person (up to 5 people). An
  unknown or ambiguous name gets a question back, never a guess.
- "đổi status sang review": the draft sets the status after the comment.
- Reply to a Telegram message that shows a ClickUp comment: the draft becomes
  a threaded reply to that comment.

The assistant only makes the draft. It never posts. Outside a task topic it
needs an explicit task id or URL.

While a job of the task is running, plain text in the topic goes to that job
as an instruction (one active job), or the hub asks which job (several). Use
`/comment <text>` to be sure the text becomes a comment draft:

- `/comment <text>` in a task topic always starts the draft flow for that
  task, even while one or more jobs are running. It never becomes a note to a
  job. The assistant drafts the comment and shows the preview; nothing is
  posted until you press **Đăng**.
- Sent as a reply to a Telegram message that shows a ClickUp comment of the
  same task, the draft becomes a threaded reply to that comment.
- `/comment` without text, or outside a task topic (General, a fixed topic),
  only answers a short usage line.
- Text over 4000 characters gets the usual "too long" note. The text is kept
  only in memory until the assistant reads it, like other assistant text.

### Post a task result: 📤 Đăng lên ClickUp

When a `/task` or `/plan` execution on a ClickUp task ends (completed or
failed), its final report in the topic has the **📤 Đăng lên ClickUp** button.
Press it to get a draft with the summary and the artifact links (PR URL,
branch). Then press **Đăng** as above. A failed or cancelled post never
changes the state of the execution. An orchestrated plan that includes a
`clickup.write` task also creates a draft and waits for your decision.

### When the outcome is unknown

If the hub cannot tell whether ClickUp received a comment (for example a
timeout after the request was sent), the preview shows "❌ Không đăng được:
unknown_outcome" without **Thử lại**. The hub never retries by itself, so it
cannot post twice. Open the task in ClickUp and check by hand whether the
comment is there. The same applies to a dry run or a denied post: no retry
button, the draft is over.

### Settings

| Setting | Default | Range | Meaning |
|---|---|---|---|
| `message_link_retention_days` | 30 | 1 to 3650 | How long the hub remembers which Telegram message shows which ClickUp comment (needed for "reply to a comment"). Older links are pruned hourly. |
| `clickup_draft_expiry_hours` | 24 | 1 to 168 | How long a draft waits for your decision before it expires. |

### Migrations

Run `goose … up` after the update: `00031` (`telegram_message_links`),
`00032` (`clickup_drafts`), `00033` (workflow state `WAITING_FOR_WORKER`) and
`00034` (draft state `failed_final`).

### Live checks still open

These facts are unconfirmed against the real ClickUp until checked once:

- U1: tags inside threaded replies, and whether a tagged person is notified
  when the comment is sent with `notify_all: false`. Also the exact comment
  request shape.
- U2: the webhook shape of a threaded reply (which field carries the parent
  comment id).
- U3: the task link shown after posting. The fallback is
  `https://app.clickup.com/t/<task id>`.

`nh telegram topics` is unchanged by Phase 10.

## Policy and approvals (Phase 9)

Every write or dangerous action the hub takes (tool calls from the agent,
behaviors, workflows, drafts, and orchestrated tasks) asks the policy engine
first. The engine reads the workspace's active rule set, answers `allow`,
`approval_required` or `deny`, and records each answer in `policy_decisions`.
Only the `nh policy` command changes the rule set. No assistant tool, behavior
action or capability can write it. A change applies to the next decision; no
restart is needed.

### The rule file

```yaml
schema: policy/v1
defaults: {read: allow, write: allow, dangerous: approval_required}
actions:
  github_pr_merge: approval_required
approval_ttl_hours: 24
```

- `schema` must be `policy/v1`. Any other value is refused.
- `defaults` sets an effect for each tier (`read`, `write`, `dangerous`). All
  three are required.
- `actions` sets an effect for one named tool or orchestrator capability. The
  name must be known to this hub build (the CLI and the server use the same
  list). An exact action rule wins over the tier default.
- Effects are `allow`, `approval_required` and `deny`. Anything else is invalid.
- `approval_ttl_hours` is 1 to 168 (default 24): how long a pending approval
  stays open.
- Errors name the path in the file (for example `actions.foo`), never the
  value.

Every workspace starts on version 1, which keeps the behaviour from before
Phase 9: read allow, write allow, dangerous `approval_required`, and
`github_pr_merge: approval_required`. `nh init` applies the same version to a
new workspace.

**Tier escalation.** A tool can raise its own tier for a given input. When an
action rule exists but the raised tier's default is stricter, the stricter
effect wins and the decision reason is `tier_escalated`.

**Safe fallback set.** When there is no active rule set, the stored set is
invalid (edited by hand, or no longer valid with the current tools), or the
store cannot be read, the hub uses a built-in set: read `allow`, write
`approval_required`, dangerous `deny`, no action rules. The decision reason says
which case applied (`fallback_no_rule_set`, `fallback_invalid_rule_set`,
`fallback_store_error`). If the decision cannot be recorded, the answer becomes
`deny` (`record_failed`). A call with an unknown action is denied
(`unknown_action`).

### `nh policy`

```sh
nh policy show --workspace-id $WS               # the active rule set as a policy/v1 file, with version and sha256
nh policy validate ./policy.yaml                # check only; no workspace, changes nothing
nh policy apply ./policy.yaml --workspace-id $WS
# {"version":2,"body_sha256":"…","active":true,"changed":true}
nh policy history --workspace-id $WS --limit 10 # one JSON row per stored version, newest first
nh policy rollback 1 --workspace-id $WS         # re-activates version 1 as a new version
nh policy decisions --workspace-id $WS --limit 20
```

- `apply` validates, stores a new version and activates it in one transaction.
  Applying a file equal to the active set adds no version (`"changed":false`).
- `rollback N` never deletes anything: it copies version N into a new active
  version and reports `restored_from_version`. It refuses a version that is
  not valid with the current tools; apply a corrected file instead.
- `--limit` is 1 to 500 (default 20) and works only on `history` and
  `decisions`.
- `decisions` prints the action, tier, resource key, effect, reason, rule key
  and rule set version, plus execution, tool run and approval ids when they
  exist. It prints hashes of inputs, never inputs.
- When `show` finds no active or no valid set, it prints a comment line and the
  built-in safe set.

### Merge approval

`/plan` can end with a task that merges a pull request (capability
`github.merge`; the plan names the repo, PR number and the head SHA from
earlier task evidence). It is offered only when the merge tool is wired; an
unwired merge tool means the capability is not offered to the planner, and a
task that asks for it anyway ends with `executor_unavailable`. The workspace
grant must also list a merge method (`merge_not_configured` otherwise). With
`github_pr_merge: approval_required`:

1. The execution's topic (General as fallback) shows
   `🔐 Cần duyệt: merge PR owner/repo#12 (head abc1234)`, the rule that asked,
   the expiry and the buttons **Duyệt** and **Từ chối**. The task waits in
   `WAITING_FOR_APPROVAL`. The expiry is `approval_ttl_hours`, but never later
   than the execution's hard deadline (24 hours after it started).
2. Only the workspace owner counts. Another person's press is answered with a
   toast and ignored. A second press, a press on an old message and a press
   after expiry do nothing. The decision records the Telegram user and the
   internal member.
3. **Duyệt**: the hub decides again, checks that the PR head still equals the
   approved SHA and the checks are green, then merges once. A restart while it
   waits keeps the wait. A restart after the approval never merges twice.
4. **Từ chối** or expiry: nothing merges, the task is blocked
   (`approval_rejected` or `approval_expired`) and the plan's evaluate step
   decides what next.
5. A gate never outlives its execution. When the execution completes, fails,
   is cancelled or passes its deadline, a press on its message is stale (no
   "Đang kiểm tra lại rồi merge"), and within a minute the sweep expires the
   gate and edits the message to `⌛ Hết hạn` without buttons. A new `/plan`
   for the same PR then gets its own gate instead of
   `approval_pending_elsewhere`. An open gate past its expiry is likewise
   expired and replaced when the same merge asks again.

An approval is bound to one action, one resource, one input hash and one
execution, and is used up by the call it allows. It cannot approve a different
call.

With `github_pr_merge: allow` the merge runs after the normal checks, but only
when an earlier succeeded task of the same execution names the exact target in
its evidence (its evidence refs, branch, PR URL or summary contain both
`owner/repo#12` or the PR's GitHub URL, and the full head SHA). Otherwise the
task is blocked with `merge_target_unproven` and nothing happens. Under
`approval_required` the owner's approval of the target shown in the message is
the binding instead. With `deny` the task is blocked with `policy_denied` and
nothing happens. If the rule changes to `deny` after you approved, the merge
is refused with `policy_denied` when it runs; `revalidate_denied` means the
grant did not match the call, or the rule changed in the moment between the
grant check and the merge.

One execution merges at most once: the merge runs with a dangerous-action
budget of 1, and a dry-run merge (tools `dry_run`) uses that budget too.

A direct agent tool call (not an orchestrated task) that hits
`approval_required` never waits: it is denied with a tool result that says the
owner must approve, and nothing runs.

### Reason codes you may see

| Code | Meaning |
|---|---|
| `policy_denied` | the rule set says `deny` |
| `approval_required` | the rule set asks for approval and no approved grant matched (direct calls) |
| `revalidate_denied` | the grant did not match the call, or the rule turned `deny` between the grant check and the run (a rule already `deny` when the approved call runs gives `policy_denied`) |
| `approval_rejected` | you pressed Từ chối |
| `approval_expired` | no decision within `approval_ttl_hours` (capped at the execution's deadline) |
| `approval_pending_elsewhere` | the same action on the same resource is already waiting for approval in another execution |
| `approval_not_sent` | the Telegram message could not be sent; the hub can send it again later |
| `merge_not_configured` | no merge method in the GitHub grant, or the merge tool is not enabled in the workspace's tools |
| `merge_target_unproven` | `allow` merge whose repo#number and head SHA no earlier succeeded task of the execution named; nothing merged |
| `executor_unavailable` | the merge tool or the policy engine is not wired, so the hub cannot run the merge |
| `no_owner_member` | the workspace has no member with role owner to approve |
| `head_sha_mismatch` | the PR head changed after approval was asked; nothing merged |
| `tier_escalated` | a stricter tier default overrode the action rule |

Other reasons in `nh policy decisions`: `action_rule`, `tier_default`,
`unknown_action`, `invalid_request`, `fallback_*`, `record_failed`,
`engine_unavailable`.

### `allow_dangerous` is retired

The setting is still accepted so old configs load, but it has no effect. If it
is true at startup the hub logs one line saying so, and `nh settings` marks it
deprecated. Dangerous tools follow the rule set: set `defaults.dangerous` or
an `actions` entry.

## Load test (Phase 12)

Use a disposable local database whose name ends in `_loadtest`. The harness
applies all migrations, creates and removes its own workspace, and refuses any
non-loopback database host. It uses only the local PostgreSQL connection and a
temporary HTTP listener on `127.0.0.1`.

Start a disposable PostgreSQL container:

```sh
docker run --rm --name nexus-loadtest-pg -p 127.0.0.1:55432:5432 -e POSTGRES_PASSWORD=loadtest -e POSTGRES_DB=nexus_loadtest postgres:17-alpine
```

In another terminal, run the harness from the repository root. Install
`goose` first; the harness passes the database URL to it through the process
environment, never through command arguments.

```sh
DATABASE_URL='postgres://postgres:loadtest@127.0.0.1:55432/nexus_loadtest?sslmode=disable' go run ./cmd/loadtest -n 120 -concurrency 10 -duplicates 0.1 -out docs/product/reports/loadtest-$(date +%F).json -timeout 5m
```

The one-line result gives delivery count, duplicate count, client latency
percentiles, execution states, total wall time, Go and PostgreSQL versions,
and CPU count. The JSON report also contains server-side latency and failed
checks. A successful run has `passed: true`, exactly `n` completed executions,
no failed or stuck executions, no HTTP failures, and client `p95_ms` below
500 ms. Stop the container after reading the report; its database is
disposable even if cleanup was interrupted.

## Recovery (Phase 12)

### Restore the database

Supabase Free has no daily backups. The scheduled backup and restore drill are
still open as test debt D12-1/D12-2 in
`docs/product/test-debt/phase12-backup-restore.md`. This procedure is **not yet
drilled — test debt D12-1/D12-2**. Until that debt closes, take a manual dump
before a risky change and copy it to encrypted offline storage.

On the VPS, let systemd read `/etc/nexus-hub/env`; never source, echo or pass
its `DATABASE_URL` on the command line:

```sh
systemd-run --pty --wait --collect -p User=nexus -p EnvironmentFile=/etc/nexus-hub/env \
  /bin/sh -c 'umask 077; export PGDATABASE="$DATABASE_URL"; exec pg_dump --format=custom --file=/var/lib/nexus-hub/nexus-hub.dump'
```

Copy `/var/lib/nexus-hub/nexus-hub.dump` through a trusted encrypted channel,
then remove the VPS copy. Restore on an administrator workstation into a
disposable PostgreSQL 17 database, not over production:

```sh
read -rs -p 'Scratch DB password: ' POSTGRES_PASSWORD; export POSTGRES_PASSWORD
docker run -d --name nexus-restore-pg -p 127.0.0.1:55432:5432 \
  -e POSTGRES_PASSWORD -e POSTGRES_DB=nexus_restore postgres:17-alpine
until docker exec nexus-restore-pg pg_isready -U postgres -d nexus_restore; do sleep 1; done
PGPASSWORD="$POSTGRES_PASSWORD" pg_restore --host=127.0.0.1 --port=55432 \
  --username=postgres --dbname=nexus_restore --clean --if-exists \
  --no-owner --no-privileges /path/to/nexus-hub.dump
SCRATCH_DATABASE_URL="host=127.0.0.1 port=55432 user=postgres password=$POSTGRES_PASSWORD dbname=nexus_restore sslmode=disable"
GOOSE_DRIVER=postgres GOOSE_DBSTRING="$SCRATCH_DATABASE_URL" \
  goose -dir db/migrations status
```

Every migration must report applied before starting the hub. Use an offline
copy of the matching production master key; a new key cannot decrypt restored
credentials. From the matching release or checkout, run:

```sh
DATABASE_URL="$SCRATCH_DATABASE_URL" \
  NEXUS_MASTER_KEY_FILE=/path/to/offline/nexus-hub-master.key nexus-server &
SCRATCH_HUB_PID=$!
DATABASE_URL="$SCRATCH_DATABASE_URL" \
  NEXUS_MASTER_KEY_FILE=/path/to/offline/nexus-hub-master.key nh status
kill "$SCRATCH_HUB_PID"
docker rm -f nexus-restore-pg
unset POSTGRES_PASSWORD SCRATCH_DATABASE_URL SCRATCH_HUB_PID
```

Confirm expected execution, queue and worker counts. Use `nh workflow list
--workspace-id $WS` for workspace-level confirmation. Do not run retention or
point the public tunnel at the scratch hub.

### Back up and rotate the master key

Production reads `NEXUS_MASTER_KEY_FILE=/etc/nexus-hub/master.key` from
`/etc/nexus-hub/env`. The file is owned by `nexus:nexus`, mode `0400`. Copy it
to an encrypted removable volume, verify its mode, unmount the volume and keep
it separately from database backups:

```sh
install -o root -g root -m 0600 /etc/nexus-hub/master.key \
  /media/<offline-volume>/nexus-hub-master.key
stat -c '%U:%G %a %n' /media/<offline-volume>/nexus-hub-master.key
```

Without this key, encrypted Telegram, ClickUp, GitHub and provider credentials
in the database are unrecoverable. Nexus-Hub has no master-key rotate or rekey
command. **Rotation: manual re-entry of secrets.** During a maintenance window,
record credential names with `nh secret list --workspace-id $WS`, stop the
hub, preserve the old key offline, create and install a new key, then re-enter
every credential with `nh secret set` from stdin before restarting. Keep the
old key until a database restore and all integrations have been verified.

### Recover a stuck execution

Telegram alerts say `stuck running`, `stuck queued`, `approval overdue` or
`failed`, with execution ID, age and safe error code. Start with hub totals,
then inspect the ordered execution history:

```sh
nh status --workspace-id $WS
nh execution timeline <WF-n|id> --workspace-id $WS
```

For an active or waiting execution that must stop, cancel it. Cancel does not
undo a request already sent. Retry accepts only `FAILED` and creates one linked
child; it refuses an uncertain or unsafe side effect. A recorded decision that
did not wake a waiting execution can be resumed:

```sh
nh workflow cancel --workspace-id $WS <WF-n|id>
nh workflow retry  --workspace-id $WS <WF-n|id>
nh workflow resume --workspace-id $WS <WF-n|id>
```

Do not retry `side_effect_uncertain`; check ClickUp, GitHub or Telegram by hand.
A running job with an expired lease normally recovers through the reconciler
after `systemctl restart nexus-hub` without an operator retry.

### Re-register webhooks after a URL change

Update the URL first, then restart so the server and commands use it:

```sh
nh settings set public_base_url 'https://<new-host>'
systemctl restart nexus-hub
nh telegram webhook set    --workspace-id $WS --integration-id $TG
nh telegram webhook status --workspace-id $WS --integration-id $TG
```

Update the ClickUp webhook endpoint to
`https://<new-host>/webhook/clickup/$SRC` in ClickUp. If replacement creates a
new webhook ID, bind it without putting its signing secret on the command line:

```sh
nh secret set --workspace-id $WS --name clickup-webhook --kind clickup_webhook
nh bind-webhook --source-id $SRC --webhook-id '<new-webhook-id>'
```

Nexus-Hub does not currently receive GitHub webhooks, and no `nh` GitHub
webhook registration command exists. GitHub credentials and repository grants
therefore need no URL update.

### Read metrics and alerts

`metrics_addr` defaults to `127.0.0.1:9090` and accepts only loopback. Check the
configured value with `nh settings get metrics_addr`. On the VPS, read the
default listener with:

```sh
curl -s 127.0.0.1:9090/metrics
```

Use the configured loopback address instead if it differs. Main series:

- `nexushub_executions{state=...}` and `nexushub_queue_pending_jobs` show work
  by state and queue depth. `nexushub_queue_oldest_due_age_seconds` shows the
  oldest due wait.
- `nexushub_worker_devices_online` and
  `nexushub_worker_newest_heartbeat_age_seconds` show worker availability;
  heartbeat age `-1` means no heartbeat exists.
- `nexushub_errors_total{kind,code}` counts safe error classes, while
  `nexushub_webhook_duration_seconds` is webhook latency.
- `nexushub_llm_tokens_today` and `nexushub_llm_cost_usd_today` show current
  UTC-day usage. `nexushub_ops_scrape_error 1` means the database snapshot
  failed for that scrape.
- `nexushub_alerts_sent_total`, `nexushub_alerts_suppressed_total` and
  `nexushub_alerts_failed_total` show sent, rate-merged and exhausted alerts.
  `nexushub_retention_deleted_total` counts rows removed by retention.

An alert means: `stuck running` has not progressed past the configured running
threshold; `stuck queued` is overdue in the queue; `approval overdue` passed
its approval expiry plus grace; `failed` ended unsuccessfully. A merged-alert
text means the 20-per-hour cap suppressed individual messages. Use `nh status`
and the execution timeline to identify the current condition before acting.

### Recover the git sandbox

`sandbox_unavailable` fails closed: git did not run. On Oracle Linux 9, install
or repair bubblewrap, confirm user namespaces are enabled, then restart:

```sh
dnf install bubblewrap
sysctl user.max_user_namespaces
nh settings get bwrap_path
systemctl restart nexus-hub
journalctl -u nexus-hub -n 100 --no-pager
```

`user.max_user_namespaces` must be greater than zero, `bwrap_path` must name the
validated binary, and the journal must say `git sandbox: ready`. If the error
remains, check systemd namespace restrictions and the loopback egress proxy;
never disable `git_sandbox` on production as a workaround.

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

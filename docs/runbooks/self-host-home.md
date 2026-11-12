# Self-host at home (main, Phases 1–3)

This guide runs Nexus-Hub on a home Linux machine that stays on. It receives
ClickUp webhooks and Telegram messages through a public HTTPS tunnel. It covers
Phase 1 notices, the Phase 2 agent and Phase 3 behaviors with Telegram commands.

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
goose -dir db/migrations postgres "$DATABASE_URL" up      # applies 00001–00009
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
nh secret set --workspace-id $WS --name anthropic --kind anthropic_key    # paste API key
nh secret set --workspace-id $WS --name clickup   --kind clickup_token    # paste ClickUp personal token

nh settings set listen_addr 127.0.0.1:8080
nh settings set public_base_url "$PUBLIC"
nh settings set owner_telegram_chat_id '<your-chat-id>'
nh settings set owner_clickup_user_id '<your ClickUp numeric user id>'
nh profile set --workspace-id $WS --language vi --timezone Asia/Ho_Chi_Minh --quiet-hours 22:00-07:00 --digest per_run
```

For the agent and GitHub (accounts, repos, `enable-agent`), follow
`clickup-telegram-local.md` §P2.4–P2.6. Behaviors take their ClickUp and GitHub
grants from the agent configuration of the ClickUp source, so do run
`nh enable-agent --source-id $SRC` with `dry_run` on.

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

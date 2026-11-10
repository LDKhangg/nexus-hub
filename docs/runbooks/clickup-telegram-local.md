# ClickUp to Telegram: local Phase 1

## Scope and safety

This setup forwards ClickUp comments and status changes to one private Telegram
group. It does not receive Telegram replies or use AI. Notices are persisted in a
PostgreSQL outbox before the webhook is acknowledged. A worker sends and retries
them independently of incoming HTTP requests.

Run these steps only against your own development database and authorized ClickUp
location/Telegram group. Creating a public tunnel, registering a real webhook, and
sending real notices are separate operator-authorized actions. Never paste tokens,
webhook secrets, registration responses, or database credentials into chat or Git.
Do not enable shell tracing (`set -x`). Topics are not access-control boundaries.

## 1. Local PostgreSQL

Install Go, Docker, goose, and sqlc. From the repository root, choose a development
database password in your terminal (not a reused production password):

```bash
read -rs -p 'Local PostgreSQL password: ' POSTGRES_PASSWORD
export POSTGRES_PASSWORD
docker volume create nexus-phase1-postgres
docker run --name nexus-phase1-postgres \
  --publish 127.0.0.1:5433:5432 \
  --env POSTGRES_PASSWORD \
  --env POSTGRES_DB=nexus_hub_dev \
  --volume nexus-phase1-postgres:/var/lib/postgresql/data \
  --detach postgres:17-alpine
unset POSTGRES_PASSWORD
```

Wait for PostgreSQL to be ready. The named volume survives container restarts;
do not remove it to troubleshoot a delivery problem. On later runs, use
`docker start nexus-phase1-postgres`, not a second `docker run` with the same name.
This container is separate from the disposable automated-test database.

Copy `.env.example` to `.env` only if `.env` does not already exist. Edit the local
file without overwriting existing settings. Set `DATABASE_URL` to
`postgres://postgres:<URL-encoded-password>@127.0.0.1:5433/nexus_hub_dev?sslmode=disable`.
Quote shell values as needed. `sslmode=disable` is for this loopback database only.
Never use an unknown hosted URL for the following migration commands.

The application does not automatically load `.env`. In each operator/server
terminal, load only your trusted local file:

```bash
set -a
. ./.env
set +a
goose -dir db/migrations postgres "$DATABASE_URL" status
goose -dir db/migrations postgres "$DATABASE_URL" up
```

Migration 00002 adds `notification_outbox`; migration 00001 is unchanged. Do not
run `down` against retained history. RLS is enabled without frontend policies;
use the trusted backend database role. Never expose the setup CLI as a public API.

## 2. Telegram bot and destination

1. Create a bot through Telegram's official BotFather and store its token as
   `NEXUS_TELEGRAM_BOT_TOKEN` in local `.env`. Reload the environment afterward.
2. Create/select a private group and add the bot. Give it permission to send
   messages; administrator privileges are not required when normal sending is
   allowed. Confirm everyone in the group may see the configured ClickUp notices.
3. Retrieve the group ID through a trusted Bot API client using `getUpdates`
   after a group command mentioning the bot. Inspect `message.chat.id`, not the
   individual sender's ID. Coordinate with any existing bot consumer; do not
   delete an existing webhook or compete with another long-poller blindly.
4. Record the signed decimal group ID (commonly `-100...`). Do not place the bot
   token in a browser URL, shell argument, screenshot, or logs. No ongoing
   Telegram webhook/polling service is needed for outbound delivery.

An existing forum topic may be selected with its positive `message_thread_id`.
Otherwise omit it. Phase 1 does not create topics or automatically follow group
migrations; a migrated chat requires an explicit destination configuration update.

## 3. Create configurable local routing

Choose the external ClickUp workspace/team ID and a test task or List you own.
The external team ID is NOT the internal workspace UUID.

```bash
go run ./cmd/setup-notices init \
  --workspace-name 'Personal development' \
  --chat-id '<signed-group-id>' \
  --team-id '<clickup-team-id>'
```

Replace placeholders before execution. Save the returned `workspace_id`,
`source_id`, `target_id`, and `callback_path`; these are identifiers, not secrets.
The command stores routing/configuration in integration rows. The source starts
disabled until its registered webhook is bound. It makes no external API calls.

Optional flags: `--message-thread-id`, `--own-sender-id`, `--bot-token-env`, and
`--webhook-secret-env`. The defaults reference `NEXUS_TELEGRAM_BOT_TOKEN` and
`NEXUS_CLICKUP_WEBHOOK_SECRET`. To rerun idempotently, supply the saved
`--workspace-id`, `--source-id`, and `--target-id`; omitting IDs creates new rows.
Existing unrelated JSON config and credential ciphertext are preserved. No global
workspace/integration-ID environment variables or hardcoded destination are needed.

## 4. Start the local server and an authorized HTTPS tunnel

```bash
go run ./cmd/server
```

Use a trusted HTTPS tunnel of your choice to local port 8080, only after approving
public exposure. Combine its public origin with the returned callback path:

```text
https://<your-tunnel-host>/webhook/clickup/<source-integration-uuid>
```

The old `/webhook/clickup` path is not accepted. The UUID selects a candidate;
the raw-body HMAC signature authenticates the callback. Missing secrets never
disable verification. No inbound Telegram route is needed.

## 5. Register and bind the ClickUp webhook

Using a trusted local API client, make the following request with your authorized
ClickUp API credential. Keep credentials in the client's secret storage or process
environment, not literal commands or shared request collections:

```text
POST https://api.clickup.com/api/v2/team/<team-id>/webhook
Authorization: <ClickUp personal API credential, or the appropriate OAuth value>
Content-Type: application/json
```

```json
{
  "endpoint": "https://<your-tunnel-host>/webhook/clickup/<source-integration-uuid>",
  "events": ["taskCommentPosted", "taskStatusUpdated"],
  "task_id": "<authorized-test-task-id>"
}
```

For List scope, replace `task_id` with the numeric `list_id`; choose one scope.
Do not subscribe to the companion `taskUpdated` event. The response contains the
webhook ID and `webhook.secret`. Save the ID separately and put the secret into
local `.env` as `NEXUS_CLICKUP_WEBHOOK_SECRET` (or the configured environment
reference). Do not log or commit the response. Reload the environment, then:

```bash
go run ./cmd/setup-notices bind-webhook \
  --source-id '<saved-source-id>' \
  --webhook-id '<registered-webhook-id>'
```

Binding enables the source only when the referenced secret exists locally. Restart
the server with the reloaded environment so it sees the secret too. Callbacks
during the unbound setup interval may be rejected; do not generate acceptance
events until binding/restart is complete. There is never an unsigned testing window.

If the tunnel origin changes, update the registered endpoint with ClickUp's
`PUT /api/v2/webhook/<webhook-id>` API. If you recreate the registration instead,
replace the local secret, bind the new webhook ID, and restart. Old signatures
and webhook IDs must not be accepted by the new configuration.

## 6. Verify and inspect delivery

With explicit permission to modify the chosen task and send to the group:

1. Add one recognizable comment. Confirm its actor, content, task ID/title, and
   task link in the intended group (and topic, if configured).
2. Change the task status. Confirm the before/after status notice in that group.
3. Run `go run ./cmd/setup-notices inspect`. Check that the corresponding jobs are
   `sent`, not merely accepted by the webhook handler. Measure latency; 10 seconds
   is a healthy-path goal, not an uptime or delivery SLA.
4. For a controlled restart check, temporarily stop only the worker (a local test
   harness can keep the acceptor running), commit a notice, and restart the worker.
   Verify the pending job is delivered. Stopping the whole server also stops
   ingestion; it cannot persist callbacks it never received.

Use automated tests for duplicate callbacks, API failures, and worker restart
without modifying real credentials or requiring a public test endpoint:

```bash
go test ./internal/adapters/storage -run TestNoticeEndToEnd -count=1 -v
```

This test needs the guarded, dedicated `TEST_DATABASE_URL` described in the test
fixture: a loopback PostgreSQL database named `nexus_storage_test`. It creates and
drops its own unique child database, and skips without that environment variable.
Never treat a skipped DB test as verified end-to-end behavior. The fake Telegram
transport performs no real send. Run full verification with the same dedicated URL:

```bash
goose -dir db/migrations validate
make generate
go vet ./...
go test ./... -count=1
go test -race ./... -count=1
```

## 7. Failure handling and limits

- `inspect` prints safe job IDs/status/attempts/error codes, not message bodies or
  credentials. HTTP 401 means invalid signature; 404 means an unknown/ineligible
  integration; 503 indicates configuration/database unavailability. Check local
  configuration without printing tokens.
- The worker uses one sender, a 1-second poll, a 10-second request timeout, a
  60-second lease, and up to 8 attempts. Transient backoff is
  2/4/8/16/32/60/60 seconds; Telegram `retry_after` takes precedence when longer.
- Fix configuration or permission failures before deliberately retrying:

  ```bash
  go run ./cmd/setup-notices retry-failed --job-id '<failed-job-id>'
  ```

  Only `failed` jobs may be reset to pending. Sent/processing jobs are refused.
  A changed source routing target does not silently reroute already committed
  jobs. A disabled source/destination fails visibly rather than falling back.
- Original history and queued jobs commit together. Database failure rolls back
  both; concurrent callback duplicates do not create extra delivery jobs.
- Sending is not exactly once: a timeout or process crash after Telegram accepted
  a message but before the database acknowledgement can produce a duplicate.
  Failed/exhausted jobs remain inspectable; there is no infinite retry guarantee.
- Notices are plain text, capped at 4000 UTF-16 units with a truncation marker;
  original full content stays in message history. Missing title/actor use explicit
  fallbacks. No AI or task-title API enrichment is required.
- Laptop, process, tunnel, or database downtime can miss callbacks beyond ClickUp's
  retry window. A local PostgreSQL volume is durable storage, not an always-on VPS.
  Back up retained history before any later deployment/cutover.

Phase 1 live acceptance is complete only after BOTH a real comment and a real
status notice are observed in the intended group. Record automated and live
results separately; neither substitutes for the other.

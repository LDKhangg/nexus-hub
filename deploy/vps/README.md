# VPS deployment (Oracle Linux 9, x86_64)

Every green `ci` run on `develop` triggers `.github/workflows/deploy.yml`. It
builds static linux/amd64 binaries for the exact commit CI tested, packages
them with the migrations, copies the tarball to the VPS over SSH and runs
`/opt/nexus-hub/bin/deploy.sh`, which migrates the database, switches the
`/opt/nexus-hub/current` symlink, restarts `nexus-hub` and health-checks it.

The server needs no Go toolchain and no Docker. The database is Supabase
(PostgreSQL 17). A Cloudflare Tunnel publishes `127.0.0.1:8080`; nothing else
is exposed.

| File | Installed to |
|---|---|
| `deploy.sh` | `/opt/nexus-hub/bin/deploy.sh` (root:root 0755) |
| `nexus-hub.service` | `/etc/systemd/system/nexus-hub.service` (root:root 0644; first time by hand, then by `deploy.sh`) |
| `sudoers-deploy` | `/etc/sudoers.d/nexus-hub-deploy` (root:root 0440) |

Never paste secrets into chat, Git or shell arguments.

## 1. One-time server setup

Run as root on the VPS from a checkout or copy of this directory.

```sh
# Users: nexus runs the service; deploy only receives tarballs over SSH.
useradd --system --home-dir /var/lib/nexus-hub --shell /sbin/nologin nexus
useradd --create-home --shell /bin/bash deploy

# Directories.
install -d -o root   -g root   -m 0755 /opt/nexus-hub /opt/nexus-hub/bin /opt/nexus-hub/releases
install -d -o deploy -g deploy -m 0700 /opt/nexus-hub/incoming
install -d -o nexus  -g nexus  -m 0700 /var/lib/nexus-hub
install -d -o root   -g nexus  -m 0750 /etc/nexus-hub

# Deploy script, unit and sudoers rule.
install -o root -g root -m 0755 deploy.sh /opt/nexus-hub/bin/deploy.sh
install -o root -g root -m 0644 nexus-hub.service /etc/systemd/system/nexus-hub.service
install -o root -g root -m 0440 sudoers-deploy /etc/sudoers.d/nexus-hub-deploy
visudo -cf /etc/sudoers.d/nexus-hub-deploy
systemctl daemon-reload
systemctl enable nexus-hub      # it starts on the first deploy

# Deploy key: paste the public half of the key stored in VPS_SSH_KEY.
install -d -o deploy -g deploy -m 0700 /home/deploy/.ssh
install -o deploy -g deploy -m 0600 /dev/null /home/deploy/.ssh/authorized_keys
${EDITOR:-vi} /home/deploy/.ssh/authorized_keys
```

The env file `/etc/nexus-hub/env` (root:nexus 0640) holds exactly two keys:

```ini
DATABASE_URL=postgres://...      # Supabase direct or session-pooler URL, sslmode=require
NEXUS_MASTER_KEY_FILE=/etc/nexus-hub/master.key
```

Write it with an editor, not with `echo` (the URL holds the DB password):

```sh
install -o root -g nexus -m 0640 /dev/null /etc/nexus-hub/env
${EDITOR:-vi} /etc/nexus-hub/env
```

Use the Supabase direct connection or the session pooler (port 5432). The
transaction pooler (port 6543) does not keep session state that migrations
and the server rely on.

The master key (nexus:nexus 0400; the server refuses a key readable by group or others) is 32 random bytes. Copy the existing key
from the old host, or create a new one only for a new database (a new key
cannot decrypt credentials stored with the old one):

```sh
install -o nexus -g nexus -m 0400 /path/to/master.key /etc/nexus-hub/master.key
```

The server listens on `:8080` by default. Bind it to loopback once the
database is migrated (after the first deploy), and keep the firewall closed.
Run `nh` as the service user with the env file loaded by systemd, so the
database URL never reaches a shell or argv:

```sh
systemd-run --pty --wait --collect -p User=nexus -p EnvironmentFile=/etc/nexus-hub/env \
  /opt/nexus-hub/current/nh settings set listen_addr 127.0.0.1:8080
systemctl restart nexus-hub
```

`nh` takes secrets on stdin, so the same `systemd-run --pty` form works for
`nh secret set ...` (see `docs/runbooks/self-host-home.md`).

## 2. GitHub secrets and variable

In the repository settings, create the environment `production` and add these
secrets (environment or repository level):

| Secret | Value |
|---|---|
| `VPS_HOST` | the VPS address or host name for SSH |
| `VPS_USER` | `deploy` |
| `VPS_SSH_KEY` | the private half of a dedicated ed25519 key, no passphrase |
| `VPS_KNOWN_HOSTS` | output of `ssh-keyscan -t ed25519 <host>`, checked against the host's own `ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub` |

Then set the repository variable `DEPLOY_ENABLED` to `true`. Any other value
turns deploys off (CI still runs).

## 3. What a deploy does

1. Builds `nexus-server`, `nh` and `goose` (pinned in the workflow) with
   `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 -trimpath -ldflags "-s -w"`.
2. Packages them with `db/migrations/`, `deploy/vps/deploy.sh`,
   `deploy/vps/nexus-hub.service` and `REVISION`
   into `nexus-hub-<sha>.tar.gz` and a sha256 checksum.
3. Copies the tarball to `/opt/nexus-hub/incoming/` and runs
   `sudo /opt/nexus-hub/bin/deploy.sh <tarball> <sha256>`.
4. `deploy.sh` verifies the checksum, extracts to
   `/opt/nexus-hub/releases/<sha>/`, runs `goose up` as `nexus` (the DSN goes
   through `GOOSE_DBSTRING`, never argv), switches `current`, installs the
   release's `deploy/vps/nexus-hub.service` to
   `/etc/systemd/system/nexus-hub.service` when it differs (root:root 0644,
   temp file plus rename, then `systemctl daemon-reload`), restarts the unit
   and waits up to 30 s for it to be active and for `127.0.0.1:8080` to
   accept connections. Before it replaces the unit it keeps the installed one
   as `/etc/systemd/system/nexus-hub.service.prev` (systemd ignores that
   name). A failed restart does not stop the script: the health check then
   fails. On failure (unit install, `daemon-reload`, restart or health check)
   it switches back to the previous release and reinstalls that release's
   unit when it differs; when that release ships no unit file, or its unit
   cannot be installed, it puts the `.prev` copy back and reloads systemd.
   Then it restarts the unit and exits 1. It keeps the last 3 releases and
   deletes the tarball.

Migrations are forward-only. A rollback restores the previous binary, not the
previous schema, so every migration must work with the release before it.

Unit changes therefore reach the VPS with the next deploy; do not edit
`/etc/systemd/system/nexus-hub.service` by hand, the next deploy replaces it.

The installed `/opt/nexus-hub/bin/deploy.sh` is not replaced by a deploy. When
`deploy/vps/deploy.sh` changes, install the copy from the new release (a
`deploy.sh` older than the unit-install step never installs the unit, so do
this once after the first release that ships it, then deploy again or run
`systemctl daemon-reload && systemctl restart nexus-hub`):

```sh
install -o root -g root -m 0755 /opt/nexus-hub/current/deploy/vps/deploy.sh /opt/nexus-hub/bin/deploy.sh
```

## 4. Deploy by hand

GitHub, Actions, `deploy`, "Run workflow", and enter a branch, tag or full
commit SHA in `ref` (default `develop`). Or:

```sh
gh workflow run deploy.yml -f ref=develop
```

A manual run does not wait for CI on that commit; deploy only commits whose
`ci` run is green.

## 5. Roll back

Re-dispatch the workflow with the older commit SHA:

```sh
ls -t /opt/nexus-hub/releases                 # on the VPS: the kept releases
gh workflow run deploy.yml -f ref=<older-sha>
```

The migrations already applied stay; goose skips them. For an emergency
without GitHub, switch the symlink by hand on the VPS:

```sh
ln -sfn /opt/nexus-hub/releases/<older-sha> /opt/nexus-hub/current.new
mv -Tf /opt/nexus-hub/current.new /opt/nexus-hub/current
systemctl restart nexus-hub
```

## 6. Check

```sh
systemctl status nexus-hub
journalctl -u nexus-hub -n 100 --no-pager
cat /opt/nexus-hub/current/REVISION
```

## Notes from the first cutover (2026-11-15)

- Install `git` on the server (`dnf -y install git`); without it the code and
  workspace tools are disabled.
- When the database is restored from a laptop install, delete the path-valued
  settings that point at the old host so the defaults under
  `/var/lib/nexus-hub` apply: `repos_dir`, `tmp_dir`, `claude_cli_path`,
  `opencode_cli_path` (otherwise the server exits with "cannot create agent
  data directory"). Set `llm_provider` to `anthropic` on a 1 GB server.
- Restore as the application role with `pg_restore --no-owner --no-privileges`
  from a full dump; a data-only dump fails on circular foreign keys.
- Store the Anthropic key as the `anthropic` credential with
  `nh secret set --kind anthropic_key` run as the service user (value on stdin).

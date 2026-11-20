#!/usr/bin/env bash
# deploy.sh: install one Nexus-Hub release on the VPS. Run as root through the
# narrow sudoers rule in deploy/vps/sudoers-deploy:
#
#   sudo /opt/nexus-hub/bin/deploy.sh /opt/nexus-hub/incoming/nexus-hub-<sha>.tar.gz <sha256>
#
# Steps: verify the checksum, extract to /opt/nexus-hub/releases/<sha>, run the
# goose migrations, switch /opt/nexus-hub/current atomically, install the
# release's systemd unit when it differs from the installed one, restart the
# unit, health-check it, and roll the symlink (and the unit) back on failure.
#
# Migrations are forward-only: a failed health check rolls back the binary
# (the symlink), never the schema. Every migration must stay compatible with
# the previous release for that reason.
#
# Logs go to stdout only. Never print environment values: DATABASE_URL holds
# the database password.
set -euo pipefail
umask 022

readonly ROOT=/opt/nexus-hub
readonly INCOMING=$ROOT/incoming
readonly RELEASES=$ROOT/releases
readonly CURRENT=$ROOT/current
readonly ENV_FILE=/etc/nexus-hub/env
readonly UNIT=nexus-hub
readonly UNIT_FILE=/etc/systemd/system/$UNIT.service
readonly UNIT_SRC=deploy/vps/nexus-hub.service
readonly SERVICE_USER=nexus
readonly HEALTH_HOST=127.0.0.1
readonly HEALTH_PORT=8080
readonly HEALTH_TIMEOUT=30
readonly KEEP=3

log() { printf '%s deploy: %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*"; }
die() { log "ERROR: $*"; exit 1; }

[[ $EUID -eq 0 ]] || die "must run as root"
[[ $# -eq 2 ]] || die "usage: deploy.sh $INCOMING/nexus-hub-<sha>.tar.gz <sha256>"

tarball=$1
want_sum=$2

# The sudoers wildcard admits any argument text, so validate strictly here.
[[ $want_sum =~ ^[0-9a-f]{64}$ ]] || die "checksum must be 64 lower-case hex characters"
name=${tarball#"$INCOMING"/}
[[ $tarball == "$INCOMING/$name" && $name =~ ^nexus-hub-([0-9a-f]{40})\.tar\.gz$ ]] \
  || die "tarball must be $INCOMING/nexus-hub-<40-hex sha>.tar.gz"
sha=${BASH_REMATCH[1]}
[[ -f $tarball && ! -L $tarball ]] || die "tarball is missing or not a regular file"

work=$(mktemp -d "$RELEASES/.work-XXXXXX")
cleanup() { rm -rf -- "$work"; rm -f -- "$tarball"; }
trap cleanup EXIT

# Copy into a root-owned directory first, so the file cannot change between
# the checksum and the extraction.
cp -- "$tarball" "$work/release.tar.gz"
got_sum=$(sha256sum "$work/release.tar.gz" | cut -d' ' -f1)
[[ $got_sum == "$want_sum" ]] || die "checksum mismatch for $name"
log "checksum ok for $sha"

stage=$work/release
mkdir "$stage"
tar -xzf "$work/release.tar.gz" -C "$stage" --no-same-owner --no-same-permissions
for f in nexus-server nh goose REVISION; do
  [[ -f $stage/$f && ! -L $stage/$f ]] || die "release is missing $f"
done
[[ -d $stage/db/migrations ]] || die "release is missing db/migrations"
[[ $(tr -d '[:space:]' < "$stage/REVISION") == "$sha" ]] || die "REVISION does not match $sha"
chown -R root:root "$stage"
chmod -R u=rwX,go=rX "$stage"
chmod 755 "$stage" "$stage/nexus-server" "$stage/nh" "$stage/goose"

release=$RELEASES/$sha
previous=""
if [[ -L $CURRENT ]]; then
  previous=$(readlink -f -- "$CURRENT" || true)
fi

if [[ -d $release && $release == "$previous" ]]; then
  log "release $sha is already current; redeploying it in place"
else
  rm -rf -- "$release"
  mv -- "$stage" "$release"
fi
touch -- "$release" # pruning keeps the newest by mtime
log "extracted to $release"

# Read DATABASE_URL from the env file without executing it (an unquoted '&'
# in a URL would break shell sourcing) and hand it to goose through the
# GOOSE_DRIVER/GOOSE_DBSTRING environment, never through argv. Runs in a
# subshell as the service user so the value does not leak into this shell.
log "running migrations"
(
  db_url=""
  while IFS= read -r line || [[ -n $line ]]; do
    line=${line%$'\r'}
    case $line in
      DATABASE_URL=*) db_url=${line#DATABASE_URL=} ;;
    esac
  done < "$ENV_FILE"
  if [[ $db_url == \"*\" || $db_url == \'*\' ]]; then
    db_url=${db_url:1:${#db_url}-2}
  fi
  [[ -n $db_url ]] || { log "ERROR: DATABASE_URL is not set in $ENV_FILE"; exit 1; }
  export GOOSE_DRIVER=postgres GOOSE_DBSTRING=$db_url
  unset db_url
  cd /
  exec runuser -u "$SERVICE_USER" -- "$release/goose" -dir "$release/db/migrations" up
) || die "migrations failed; the current release is unchanged"
log "migrations done"

switch_to() {
  local target=$1
  ln -sfn -- "$target" "$CURRENT.new"
  mv -Tf -- "$CURRENT.new" "$CURRENT"
}

# install_unit <release dir>: install the unit file the release ships when it
# differs from $UNIT_FILE (root:root 0644, temp file plus rename, so systemd
# never reads a half-written unit), then reload systemd. A release without a
# unit file (older than this step) keeps the installed one. Errors are
# returned, not fatal, so a rollback still restarts the previous binary.
install_unit() {
  local src=$1/$UNIT_SRC tmp
  if [[ ! -f $src || -L $src ]]; then
    log "release ${1##*/} ships no unit file; keeping $UNIT_FILE"
    return 0
  fi
  if [[ -f $UNIT_FILE ]] && cmp -s -- "$src" "$UNIT_FILE"; then
    return 0
  fi
  tmp=$(mktemp "${UNIT_FILE%/*}/.$UNIT.service.XXXXXX") || return 1
  if ! install -o root -g root -m 0644 -T -- "$src" "$tmp" || ! mv -Tf -- "$tmp" "$UNIT_FILE"; then
    rm -f -- "$tmp"
    return 1
  fi
  systemctl daemon-reload || return 1
  log "installed $UNIT_FILE from release ${1##*/}; systemd reloaded"
}

healthy() {
  local deadline=$((SECONDS + HEALTH_TIMEOUT))
  while ((SECONDS < deadline)); do
    if systemctl is-active --quiet "$UNIT" \
      && (exec 3<>"/dev/tcp/$HEALTH_HOST/$HEALTH_PORT") 2>/dev/null; then
      return 0
    fi
    sleep 1
  done
  return 1
}

switch_to "$release"
if ! install_unit "$release"; then
  if [[ -n $previous && -d $previous && $previous != "$release" ]]; then
    switch_to "$previous"
    log "rolled back current -> $previous"
  fi
  die "could not install $UNIT_FILE; $UNIT was not restarted"
fi
log "current -> $release; restarting $UNIT"
systemctl restart "$UNIT"

if ! healthy; then
  log "health check failed after ${HEALTH_TIMEOUT}s"
  if [[ -n $previous && -d $previous && $previous != "$release" ]]; then
    switch_to "$previous"
    install_unit "$previous" || log "could not reinstall the unit of ${previous##*/}; keeping $UNIT_FILE"
    log "rolled back current -> $previous; restarting $UNIT (migrations are not rolled back)"
    systemctl restart "$UNIT" || true
  else
    log "no previous release to roll back to"
  fi
  exit 1
fi
log "healthy: $UNIT is active and $HEALTH_HOST:$HEALTH_PORT accepts connections"

# Keep the newest $KEEP releases; never delete the current one.
mapfile -t all < <(find "$RELEASES" -mindepth 1 -maxdepth 1 -type d -regextype posix-extended \
  -regex '.*/[0-9a-f]{40}' -printf '%T@ %p\n' | sort -rn | cut -d' ' -f2-)
for ((i = KEEP; i < ${#all[@]}; i++)); do
  old=${all[i]}
  [[ $old == "$release" ]] && continue
  rm -rf -- "$old"
  log "pruned ${old##*/}"
done

log "deployed $sha"

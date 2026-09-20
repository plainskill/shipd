#!/usr/bin/env bash
#
# Update the running shipd service from the local registry artifact image.
# The registry is the update channel; this extracts the binary, swaps it in,
# restarts the service, and rolls back automatically if the health check fails.
#
# Runs on pscA as root:  sudo /stack/compose/shipd/update.sh [image]
#
set -euo pipefail

IMAGE="${1:-localhost:5000/atlas/shipd:latest}"
BIN=/usr/local/bin/shipd
HEALTH=http://127.0.0.1:8900/healthz

log() { printf '[shipd-update] %s\n' "$*"; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

log "pulling $IMAGE"
docker pull "$IMAGE" >/dev/null

CID="$(docker create "$IMAGE")"
docker cp "$CID:/shipd" "$TMP/shipd" >/dev/null
docker rm "$CID" >/dev/null
chmod 0755 "$TMP/shipd"

NEW="$("$TMP/shipd" -version)"
CUR="$("$BIN" -version 2>/dev/null || echo unknown)"
log "current=$CUR new=$NEW"

if [ "$NEW" = "$CUR" ]; then
  log "already at $NEW — nothing to do"
  exit 0
fi

cp -p "$BIN" "$TMP/shipd.rollback" 2>/dev/null || true
install -m 0755 "$TMP/shipd" "$BIN"
systemctl restart shipd

for i in $(seq 1 10); do
  sleep 1
  if curl -fsS "$HEALTH" >/dev/null 2>&1; then
    log "updated to $NEW"
    exit 0
  fi
done

log "health check failed — rolling back to $CUR" >&2
if [ -f "$TMP/shipd.rollback" ]; then
  install -m 0755 "$TMP/shipd.rollback" "$BIN"
  systemctl restart shipd
  sleep 2
  curl -fsS "$HEALTH" >/dev/null 2>&1 && log "rolled back to $CUR" || log "ROLLBACK ALSO FAILED — inspect journalctl -u shipd" >&2
fi
exit 1

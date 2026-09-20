#!/usr/bin/env bash
#
# Build shipd on the build host, push the artifact image to the local registry,
# then update the running service from that image.
#
#   ./deploy.sh [version]
#
# Overridable: SHIPD_REMOTE, SHIPD_STAGE, SHIPD_IMAGE, SHIPD_HEALTH_URL.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REMOTE="${SHIPD_REMOTE:-pscA}"
STAGE="${SHIPD_STAGE:-/tmp/shipd-build}"
IMAGE="${SHIPD_IMAGE:-localhost:5000/atlas/shipd}"
UPDATE="${SHIPD_UPDATE:-/stack/compose/shipd/update.sh}"
HEALTH_URL="${SHIPD_HEALTH_URL:-https://apps.plainskill.net/healthz}"
VERSION="${1:-$(cd "$ROOT" && git describe --tags --always --dirty 2>/dev/null || echo dev)}"

info() { printf '[deploy] %s\n' "$*"; }
err()  { printf '[deploy] ERROR: %s\n' "$*" >&2; }

info "version: $VERSION  remote: $REMOTE"

# --- stage the module source on the build host (whole tree: future
# --- subpackages the server imports must travel too) --------------------
info "syncing source to $REMOTE:$STAGE ..."
TARBALL="$(mktemp)"
tar czf "$TARBALL" -C "$ROOT" --exclude=.git --exclude=dist --exclude=deploy --exclude=node_modules .
scp -q "$TARBALL" "$REMOTE:$STAGE.tar.gz"
rm -f "$TARBALL"

# --- build + push + update (values passed pre-quoted; script on stdin) ----
ssh "$REMOTE" "STAGE=$(printf '%q' "$STAGE") IMAGE=$(printf '%q' "$IMAGE") VERSION=$(printf '%q' "$VERSION") UPDATE=$(printf '%q' "$UPDATE") bash -s" <<'REMOTE_SCRIPT'
set -euo pipefail
rm -rf "$STAGE" && mkdir -p "$STAGE"
tar xzf "$STAGE.tar.gz" -C "$STAGE" && rm -f "$STAGE.tar.gz"
cd "$STAGE"
echo "[deploy] building image $IMAGE:$VERSION"
docker build --build-arg VERSION="$VERSION" -t "$IMAGE:$VERSION" -t "$IMAGE:latest" . >/dev/null
docker push "$IMAGE:$VERSION" >/dev/null
docker push "$IMAGE:latest" >/dev/null
echo "[deploy] pushed; updating service via $UPDATE"
sudo "$UPDATE"
REMOTE_SCRIPT

# --- verify ------------------------------------------------------------
sleep 2
STATUS=$(curl -s -o /dev/null -w "%{http_code}" "$HEALTH_URL" 2>/dev/null || echo 000)
if [ "$STATUS" = "200" ]; then
  info "live — $HEALTH_URL HTTP $STATUS"
else
  err "$HEALTH_URL returned $STATUS"
  ssh "$REMOTE" "sudo journalctl -u shipd -n 15 --no-pager" || true
  exit 1
fi
info "done"

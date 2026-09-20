#!/usr/bin/env bash
#
# Build shipd on pscA, push the artifact image to the local registry, then
# update the running service from that image.
#
#   ./deploy.sh [version]
#
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REMOTE="${SHIPD_REMOTE:-pscA}"
STAGE="${SHIPD_STAGE:-/tmp/shipd-build}"
IMAGE="${SHIPD_IMAGE:-localhost:5000/atlas/shipd}"
VERSION="${1:-$(cd "$ROOT" && git describe --tags --always --dirty 2>/dev/null || echo dev)}"

info() { printf '[deploy] %s\n' "$*"; }
err()  { printf '[deploy] ERROR: %s\n' "$*" >&2; }

info "version: $VERSION"

# --- stage source on the build host -----------------------------------
info "syncing source to $REMOTE:$STAGE ..."
ssh "$REMOTE" "rm -rf $STAGE && mkdir -p $STAGE"
scp -q "$ROOT"/{go.mod,Dockerfile,dashboard.html} "$REMOTE:$STAGE/"
scp -q "$ROOT"/*.go "$REMOTE:$STAGE/"

# --- build + push ------------------------------------------------------
info "building image and pushing to registry ..."
ssh "$REMOTE" "cd $STAGE && docker build --build-arg VERSION=$VERSION -t $IMAGE:$VERSION -t $IMAGE:latest . >/dev/null && docker push $IMAGE:$VERSION >/dev/null && docker push $IMAGE:latest >/dev/null && echo pushed"

# --- update the running service from the registry ----------------------
info "updating service from the registry ..."
ssh "$REMOTE" "sudo /stack/compose/shipd/update.sh"

# --- verify ------------------------------------------------------------
sleep 2
STATUS=$(curl -s -o /dev/null -w "%{http_code}" https://apps.plainskill.net/healthz 2>/dev/null || echo 000)
if [ "$STATUS" = "200" ]; then
  info "live — healthz HTTP $STATUS"
else
  err "healthz returned $STATUS"
  ssh "$REMOTE" "sudo journalctl -u shipd -n 15 --no-pager" || true
  exit 1
fi
info "done"

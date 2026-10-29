#!/usr/bin/env bash
# Deploys an image tag: pull, restart, wait until healthy, roll back to the
# previous tag when the new version does not become healthy.
#
#   ./deploy.sh <image-tag> [registry-user]    registry token read from stdin
#   SKIP_PULL=1 ./deploy.sh bootstrap           use an image already on the host
set -euo pipefail
cd "$(dirname "$0")"

TAG="${1:?usage: deploy.sh <image-tag> [registry-user]}"
REG_USER="${2:-}"
[ -f .env ] || { echo "missing .env (run init-env.sh first)" >&2; exit 1; }

exec 9>/tmp/cleads-deploy.lock
flock -n 9 || { echo "another deployment is running" >&2; exit 1; }

envval() { sed -n "s/^$1=//p" .env | tail -n1; }
set_tag() {
  if grep -q '^IMAGE_TAG=' .env; then sed -i "s|^IMAGE_TAG=.*|IMAGE_TAG=$1|" .env; else echo "IMAGE_TAG=$1" >>.env; fi
}

PREV="$(envval IMAGE_TAG)"

if [ -n "$REG_USER" ]; then
  docker login ghcr.io -u "$REG_USER" --password-stdin >/dev/null
  trap 'docker logout ghcr.io >/dev/null 2>&1 || true' EXIT
fi

set_tag "$TAG"
docker compose config -q # fail early on a broken compose file or missing variable
if [ "${SKIP_PULL:-0}" != 1 ]; then
  docker compose pull api worker
fi
docker compose up -d --remove-orphans

healthy() {
  for _ in $(seq 1 60); do
    if curl -fsS --max-time 3 http://127.0.0.1:8080/readyz >/dev/null &&
      curl -fsS --max-time 3 http://127.0.0.1:8081/healthz >/dev/null; then
      return 0
    fi
    sleep 2
  done
  return 1
}

if healthy; then
  echo "deployed $TAG (previous: ${PREV:-none})"
  docker image prune -af --filter "until=168h" >/dev/null 2>&1 || true
  docker compose ps
  exit 0
fi

echo "deployment of $TAG is not healthy" >&2
docker compose ps >&2 || true
docker compose logs --tail=80 api worker >&2 || true
if [ -n "$PREV" ] && [ "$PREV" != "$TAG" ]; then
  echo "rolling back to $PREV" >&2
  set_tag "$PREV"
  docker compose up -d --remove-orphans
  if healthy; then echo "rolled back to $PREV" >&2; fi
fi
exit 1

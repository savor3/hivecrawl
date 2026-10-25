#!/bin/sh
# Updates this HiveCrawl node to the newest release and rebuilds it.
#
# You rarely need this: the updater service in docker-compose.yml runs it for
# you (every 6 hours, and when you press "Check for updates" in Settings).
#
#   ./update.sh            update now, if a newer release is out
#   ./update.sh --check    only say whether an update is out
#   ./update.sh --status   the same as key=value lines (used by the updater)
#
# If the new version doesn't come up healthy, the previous one is put back.
set -eu

cd "$(dirname "$0")"
MODE=update
case "${1:-}" in
  --check) MODE=check ;;
  --status) MODE=status ;;
  --quiet|"") ;;
  *) echo "update: unknown option $1 (try --check)" >&2; exit 2 ;;
esac
die() { echo "update: $*" >&2; exit 1; }

# Inside the updater container git runs as the folder's owner.
git_() {
  if [ -n "${UPDATE_AS:-}" ]; then
    HOME=/tmp su-exec "$UPDATE_AS" git -c safe.directory='*' "$@"
  else
    git "$@"
  fi
}

[ -d .git ] || die "this folder is not a git clone; reinstall with: git clone https://github.com/savor3/hivecrawl"
command -v docker >/dev/null || die "docker not found"

# Releases always come from the official repository over HTTPS, whatever
# origin points at (and without needing SSH keys in the updater).
REPO_URL=${HC_UPDATE_REPO:-https://github.com/savor3/hivecrawl.git}
git_ fetch --quiet --force --no-tags "$REPO_URL" '+refs/tags/v*:refs/tags/v*'
latest=$(git_ tag -l 'v[0-9]*' --sort=-v:refname | head -n 1)
current=$(git_ describe --tags --exact-match 2>/dev/null || git_ describe --tags 2>/dev/null || git_ rev-parse --short HEAD)

available=no
if [ -n "$latest" ] && [ "$current" != "$latest" ] && ! git_ merge-base --is-ancestor "$latest" HEAD 2>/dev/null; then
  available=yes
fi

case $MODE in
  status)
    echo "current=$current"; echo "latest=${latest:-none}"; echo "available=$available"
    exit 0 ;;
  check)
    if [ -z "$latest" ]; then echo "No releases published yet."
    elif [ $available = yes ]; then
      echo "Update available: $current -> $latest"
      echo "Release notes: https://github.com/savor3/hivecrawl/releases/tag/$latest"
    else echo "Up to date ($current)."; fi
    exit 0 ;;
esac

[ $available = yes ] || { echo "Up to date ($current)."; exit 0; }
git_ diff --quiet && git_ diff --cached --quiet || die "tracked files were changed here; commit or stash them first (git stash)"

# The address the node answers on: from the updater, the compose service;
# on the host, the port docker compose publishes (HC_PORT, 3002 by default).
url=${HC_HEALTH_URL:-}
if [ -z "$url" ]; then
  port=$(sed -n 's/^HC_PORT=//p' .env 2>/dev/null | tail -n 1 | tr -d "\"' ")
  url="http://127.0.0.1:${port:-3002}/v1/config"
fi
healthy() {
  i=0
  while [ $i -lt 45 ]; do
    if curl -fsS -o /dev/null "$url" 2>/dev/null; then return 0; fi
    i=$((i + 1)); sleep 2
  done
  return 1
}
# The updater doesn't rebuild itself while it is running.
up() {
  if [ -n "${HC_UPDATER:-}" ]; then
    docker compose up -d --build $(docker compose config --services | grep -vx updater)
  else
    docker compose up -d --build --remove-orphans
  fi
}

echo "$(date -u '+%F %T') updating $current -> $latest"
previous=$(git_ rev-parse HEAD)
git_ -c advice.detachedHead=false checkout --quiet "$latest"
if up >/dev/null 2>&1 && healthy; then
  echo "$(date -u '+%F %T') now running $latest"
  # The updater can't replace itself while it runs: a one-off helper
  # container rebuilds it (a no-op when the updater didn't change).
  if [ -n "${HC_UPDATER:-}" ]; then
    docker run -d --rm -v /var/run/docker.sock:/var/run/docker.sock -v "$PWD:$PWD" -w "$PWD" \
      docker:28.5.2-cli compose up -d --build --no-deps updater >/dev/null 2>&1 || true
  fi
  exit 0
fi
echo "$(date -u '+%F %T') $latest did not start; went back to $current" >&2
git_ -c advice.detachedHead=false checkout --quiet "$previous"
up >/dev/null 2>&1 || true
exit 1

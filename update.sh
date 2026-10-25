#!/bin/sh
# Updates this HiveCrawl node to the newest release and rebuilds it.
#
#   ./update.sh            update now (if a newer release is out)
#   ./update.sh --check    only say whether an update is out
#   ./update.sh --auto     update every night by itself (adds a cron job)
#   ./update.sh --no-auto  stop the nightly update
#
# Runs on the host, not in the container: the node itself never gets
# access to Docker or your files. If the new version does not come up
# healthy, the previous version is put back.
set -eu

cd "$(dirname "$0")"
DIR=$(pwd)
LOG="$DIR/update.log"
QUIET=0
say() { [ "$QUIET" = 1 ] || echo "$@"; }
die() { echo "update: $*" >&2; exit 1; }

cron_line="17 4 * * * cd '$DIR' && ./update.sh --quiet >> '$LOG' 2>&1"
case "${1:-}" in
  --auto)
    command -v crontab >/dev/null || die "crontab not found; run ./update.sh from your own scheduler instead"
    (crontab -l 2>/dev/null | grep -vF "$DIR' && ./update.sh"; echo "$cron_line") | crontab -
    echo "Nightly updates on (04:17 every day). Log: $LOG"
    echo "Turn off with: ./update.sh --no-auto"
    exit 0 ;;
  --no-auto)
    (crontab -l 2>/dev/null | grep -vF "$DIR' && ./update.sh") | crontab - 2>/dev/null || true
    echo "Nightly updates off."
    exit 0 ;;
  --quiet) QUIET=1 ;;
  --check|"") ;;
  *) die "unknown option $1 (try --check, --auto or --no-auto)" ;;
esac

[ -d .git ] || die "this folder is not a git clone; reinstall with: git clone https://github.com/savor3/hivecrawl"
command -v docker >/dev/null || die "docker not found"

git fetch --quiet --tags --force origin
latest=$(git tag -l 'v[0-9]*' --sort=-v:refname | head -n 1)
[ -n "$latest" ] || { say "No releases published yet."; exit 0; }
current=$(git describe --tags --exact-match 2>/dev/null || git rev-parse --short HEAD)

if [ "$current" = "$latest" ]; then
  say "Up to date ($latest)."
  exit 0
fi
# A clone of main that already contains the release is not behind it.
if git merge-base --is-ancestor "$latest" HEAD 2>/dev/null; then
  say "This node ($current) already includes $latest."
  exit 0
fi
if [ "${1:-}" = "--check" ]; then
  echo "Update available: $current -> $latest"
  echo "Release notes: https://github.com/savor3/hivecrawl/releases/tag/$latest"
  echo "Install with: ./update.sh"
  exit 0
fi
git diff --quiet && git diff --cached --quiet || die "you changed tracked files here; commit or stash them first (git stash)"

echo "$(date -u '+%F %T') updating $current -> $latest"
previous=$(git rev-parse HEAD)
git -c advice.detachedHead=false checkout --quiet "$latest"

# The host port docker compose publishes (HC_PORT in .env, 3002 by default).
port=$(sed -n 's/^HC_PORT=//p' .env 2>/dev/null | tail -n 1 | tr -d '"'"'"' ')
port=${port:-3002}
healthy() {
  i=0
  while [ $i -lt 30 ]; do
    if curl -fsS -o /dev/null "http://127.0.0.1:$port/v1/config" 2>/dev/null; then return 0; fi
    i=$((i + 1)); sleep 2
  done
  return 1
}

if docker compose up -d --build --remove-orphans >/dev/null 2>&1 && healthy; then
  echo "$(date -u '+%F %T') now running $latest"
  exit 0
fi

echo "$(date -u '+%F %T') $latest did not start; going back to $current" >&2
git -c advice.detachedHead=false checkout --quiet "$previous"
docker compose up -d --build --remove-orphans >/dev/null 2>&1 || true
exit 1

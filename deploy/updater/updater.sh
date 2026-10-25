#!/bin/sh
# Keeps this HiveCrawl node on the newest release.
#
# Every HC_UPDATE_INTERVAL seconds (default 6 h), and whenever the node asks
# (the "Check for updates" button), it runs ../../update.sh in the node's own
# folder: fetch the release tags from GitHub, and if a newer one is out, check
# it out, rebuild and put the old version back if the node doesn't come up.
#
# It shares one small folder (/update) with the node: it writes status.json
# there and reads the node's "check-now" flag. It has no ports and takes no
# other input.
set -u

REPO=/hivecrawl
STATE=/update
INTERVAL=${HC_UPDATE_INTERVAL:-21600}
AUTO=${HC_AUTO_UPDATE:-true}

# docker compose resolves paths on the host, so the repo must be reachable
# here at its host path: find it from this container's own mount.
HOST_DIR=$(docker inspect --format '{{range .Mounts}}{{if eq .Destination "/hivecrawl"}}{{.Source}}{{end}}{{end}}' "$(hostname)" 2>/dev/null)
if [ -z "$HOST_DIR" ]; then
  echo "updater: can't find the node's folder; is /hivecrawl mounted?" >&2
  sleep 3600; exit 1
fi
if [ "$HOST_DIR" != "$REPO" ] && [ ! -e "$HOST_DIR" ]; then
  mkdir -p "$(dirname "$HOST_DIR")" && ln -s "$REPO" "$HOST_DIR"
fi
mkdir -p "$STATE" && chown 65532:65532 "$STATE" # the node runs as uid 65532

# Git runs as the folder's owner, so files keep their owner on the host.
export UPDATE_AS="$(stat -c %u:%g "$REPO")"
export HC_UPDATER=1 HC_HEALTH_URL=http://hivecrawl:3002/v1/config

now() { date -u '+%Y-%m-%dT%H:%M:%SZ'; }
# Safe inside a JSON string: no quotes, backslashes or control characters.
clean() { printf '%s' "$1" | tr '\n\r\t' '   ' | tr -d '"\\\000-\037' | cut -c1-300; }
checked=""; updated=""; current=""; latest=""; available=no; state="starting"; message=""
[ -f "$STATE/updated-at" ] && updated=$(cat "$STATE/updated-at")

write_status() {
  tmp="$STATE/.status.json"
  printf '{"auto":%s,"available":%s,"state":"%s","current":"%s","latest":"%s","checkedAt":"%s","updatedAt":"%s","seenAt":"%s","message":"%s"}\n' \
    "$([ "$AUTO" = true ] && echo true || echo false)" "$([ "$available" = yes ] && echo true || echo false)" "$state" "$(clean "$current")" "$(clean "$latest")" \
    "$checked" "$updated" "$(now)" "$(clean "$message")" > "$tmp" && mv "$tmp" "$STATE/status.json"
  chmod 644 "$STATE/status.json"
}

check() {
  state=checking; write_status
  out=$(cd "$HOST_DIR" && ./update.sh --status 2>&1)
  checked=$(now)
  current=$(printf '%s\n' "$out" | sed -n 's/^current=//p')
  latest=$(printf '%s\n' "$out" | sed -n 's/^latest=//p')
  available=$(printf '%s\n' "$out" | sed -n 's/^available=//p')
  if [ -z "$available" ]; then
    available=no; state=failed; message="$out"; write_status; return
  fi
  if [ "$available" = yes ] && [ "$AUTO" = true ]; then
    state=updating; message="installing $latest"; write_status
    if out=$(cd "$HOST_DIR" && ./update.sh --quiet 2>&1); then
      updated=$(now); echo "$updated" > "$STATE/updated-at"
      current=$latest; available=no; state=idle; message="updated to $latest"
    else
      state=failed; message=$(printf '%s\n' "$out" | tail -n 1)
    fi
  elif [ "$available" = yes ]; then
    state=idle; message="$latest is out; auto-update is off (HC_AUTO_UPDATE=false)"
  else
    state=idle; message="up to date"
  fi
  echo "$(now) $message"
  write_status
}

sleep 20 # let the node start first
check
last=$(date +%s)
while :; do
  sleep 5
  if [ -f "$STATE/check-now" ]; then
    rm -f "$STATE/check-now"
    check; last=$(date +%s)
  elif [ $(( $(date +%s) - last )) -ge "$INTERVAL" ]; then
    check; last=$(date +%s)
  elif [ $(( $(date +%s) % 60 )) -lt 5 ]; then
    write_status # heartbeat, so the node knows the updater is running
  fi
done

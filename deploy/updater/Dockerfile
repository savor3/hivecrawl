# Keeps the node on the newest HiveCrawl release (see updater.sh).
FROM docker:28.5.2-cli
RUN apk add --no-cache git curl su-exec
COPY updater.sh /updater.sh
ENTRYPOINT ["/bin/sh", "/updater.sh"]

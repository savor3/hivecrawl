# Cloudflare WARP as a SOCKS5 proxy for SearXNG's fallback engines.
# wgcf registers a free WARP account; wireproxy exposes it as SOCKS5 on :40000.
FROM alpine:3.21 AS fetch
ARG TARGETARCH
ARG WGCF_VERSION=2.3.1
ARG WIREPROXY_VERSION=1.1.3
RUN apk add --no-cache curl
RUN set -eu; \
    # The legacy (non-BuildKit) builder does not set TARGETARCH.
    ARCH="${TARGETARCH:-$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')}"; \
    case "$ARCH" in \
      amd64) WGCF_SHA=4fb7a136aa470ece11d17949c7db07b040ec9c604742dc8680bcf1889f85d0c8; WP_SHA=e88c1d090740373fc606c1bafd81d9a5eadc642cce5667616e20e9d7a444f51c ;; \
      arm64) WGCF_SHA=710ab2d8ac145d81a4cf0c11bdb0e05914f77ae2a45f67b87e718d84b4cb7301; WP_SHA=370e00bd2167960d1ecd1c3c1439715bbaa94a0a110a2040468670c9af6021b6 ;; \
      *) echo "unsupported arch $ARCH"; exit 1 ;; \
    esac; \
    curl -fsSL -o /wgcf "https://github.com/ViRb3/wgcf/releases/download/v${WGCF_VERSION}/wgcf_${WGCF_VERSION}_linux_${ARCH}"; \
    echo "$WGCF_SHA  /wgcf" | sha256sum -c -; \
    curl -fsSL -o /wp.tgz "https://github.com/windtf/wireproxy/releases/download/v${WIREPROXY_VERSION}/wireproxy_linux_${ARCH}.tar.gz"; \
    echo "$WP_SHA  /wp.tgz" | sha256sum -c -; \
    tar -xzf /wp.tgz -C / wireproxy; \
    chmod +x /wgcf /wireproxy

FROM alpine:3.21
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 warp && mkdir /data && chown warp /data
COPY --from=fetch /wgcf /wireproxy /usr/local/bin/
COPY entrypoint.sh /entrypoint.sh
USER warp
WORKDIR /data
VOLUME /data
EXPOSE 40000
ENTRYPOINT ["/entrypoint.sh"]

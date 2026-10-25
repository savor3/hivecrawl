#!/bin/sh
# Register a free WARP account on first start (kept in /data), then serve
# it as SOCKS5 on :40000 inside the compose network only.
set -eu
cd /data
if [ ! -f wgcf-account.toml ]; then
  echo "warp: registering a new WARP account"
  wgcf register --accept-tos
fi
if [ ! -f wgcf-profile.conf ]; then
  wgcf generate
fi
cat > wireproxy.conf <<CONF
WGConfig = /data/wgcf-profile.conf

[Socks5]
BindAddress = 0.0.0.0:40000
CONF
echo "warp: SOCKS5 listening on :40000"
exec wireproxy -c /data/wireproxy.conf

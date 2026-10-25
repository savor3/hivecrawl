# Run a public HiveCrawl node

Public nodes are the network's entry points. Home nodes tunnel through them, new nodes find the network through
them, and every public node is another IP address that search engines see as an ordinary user. If you have a
small server, running one is the most useful thing you can do for the network.

It takes about ten minutes, and it can cost nothing: see [free servers](#free-servers) below.

## What you need

| | Minimum | Comfortable |
|---|---|---|
| CPU | 1 core | 2 cores |
| Memory | 1 GB (add swap, see below) | 2 GB or more |
| Disk | 10 GB free | 20 GB |
| OS | Any Linux that runs Docker, x86-64 or Arm | Ubuntu 24.04 |
| Network | A public IPv4 address, ports 80 and 443 open | |

Measured on a live public node: about 140 MB of memory when idle. Headless Chrome (for JavaScript pages and
screenshots) is allowed up to 1 GB while it renders, which is why 1 GB machines need swap.

You also need a domain name pointing at the server. If you don't have one, use a free
[sslip.io](https://sslip.io) name: for the IP `203.0.113.7` it is `hc.203-0-113-7.sslip.io`, with nothing to set up.

## 1. Install Docker

```bash
curl -fsSL https://get.docker.com | sh
```

On a 1 GB machine, add 2 GB of swap first:

```bash
sudo fallocate -l 2G /swapfile && sudo chmod 600 /swapfile && sudo mkswap /swapfile && sudo swapon /swapfile
echo '/swapfile none swap sw 0 0' | sudo tee -a /etc/fstab
```

## 2. Start the node

```bash
git clone https://github.com/savor3/hivecrawl && cd hivecrawl
cat > .env <<'EOF'
HC_PUBLIC_URL=https://hc.203-0-113-7.sslip.io
HC_NODE_NAME=my-public-node
EOF
docker compose up -d --build
```

Use your own address in `HC_PUBLIC_URL`: it is how other nodes reach you. The node binds to `127.0.0.1:3002` only,
so nothing is exposed until the next step.

## 3. Put HTTPS in front of it

[Caddy](https://caddyserver.com/docs/install) gets and renews the certificate by itself:

```bash
sudo apt install -y caddy
sudo tee /etc/caddy/Caddyfile >/dev/null <<'EOF'
hc.203-0-113-7.sslip.io {
    reverse_proxy 127.0.0.1:3002
}
EOF
sudo systemctl reload caddy
```

Open ports 80 and 443 in your provider's firewall (on Oracle Cloud, in the subnet's security list *and* with
`sudo iptables` on the server, since its Ubuntu images block them by default).

## 4. Check it

```bash
curl -s https://hc.203-0-113-7.sslip.io/v1/config     # answers, without a key
docker compose exec hivecrawl /hivecrawl login        # prints your API key and sign-in link
```

Sign in to the web UI at your address and open **Network**. Within a minute you should see other nodes, and the
built-in public nodes list yours. Peers show as **verified** once your node has answered their test search.

That's all. The updater keeps the node on the newest release by itself; you can turn that off under
**Settings → Updates**.

## Free servers

| Provider | What's free | Notes |
|---|---|---|
| [Oracle Cloud Always Free](https://www.oracle.com/cloud/free/) | Arm servers, 2 cores and 12 GB in total, no end date | The best free option. A card is needed to sign up; free capacity is sometimes unavailable in busy regions. |
| [Google Cloud free tier](https://cloud.google.com/free) | One e2-micro (1 GB), in three US regions | Works with swap; slow when pages need Chrome. |
| Azure, AWS | Credits or a small server for 6–12 months | Fine until the credits end. |

Choose a region far from existing public nodes: a node in a new country adds the most.

## Good to know

- **What a public node shares.** Its IP address answers searches for other nodes when their engines are blocked,
  never your API key or AI key, and never page fetches unless you set `HC_SERVE_SCRAPES=true`.
  See [Is it safe to run?](../README.md#is-it-safe-to-run).
- **Load.** Each node limits how much any one peer can ask (`HC_PEER_RATE`), and the number of home nodes it
  holds tunnels for (`HC_MAX_TUNNELS`).
- **Stopping.** `docker compose down` takes it off the network; other nodes forget it within minutes.

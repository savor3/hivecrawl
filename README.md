<p align="center">
  <img src="docs/assets/banner.jpg" alt="HiveCrawl — peer-to-peer web search and crawling for AI agents" width="100%">
</p>

<p align="center">
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-AGPL--3.0-f5a524?style=flat-square" alt="License: AGPL-3.0"></a>
  <img src="https://img.shields.io/badge/go-1.26-f5a524?style=flat-square&logo=go&logoColor=white" alt="Go 1.26">
  <img src="https://img.shields.io/badge/API-Firecrawl%20compatible-f5a524?style=flat-square" alt="Firecrawl compatible">
  <img src="https://img.shields.io/badge/MCP-built%20in-f5a524?style=flat-square" alt="MCP built in">
  <img src="https://img.shields.io/badge/self--hosted-docker%20compose-f5a524?style=flat-square&logo=docker&logoColor=white" alt="Self-hosted with Docker Compose">
</p>

# HiveCrawl

**Self-hosted, peer-to-peer web search, scraping and crawling with a Firecrawl-compatible API.**

<sub>Formerly HiveQuery. Nodes running HiveQuery are on a separate network; update and they rejoin as HiveCrawl.</sub>

HiveCrawl gives you Firecrawl's search, scrape, crawl, map, batch scrape, extract and agent endpoints on your own
machines — no paid search API, no hosted scraping service. Each node runs [SearXNG](https://github.com/searxng/searxng) for results. When search
engines block a node (CAPTCHAs, `429 Too Many Requests`), the node retries the blocked engines through
Cloudflare WARP and through other nodes in the network — including home machines behind a router, whose
residential IPs get blocked far less — and merges everything into one Firecrawl-shaped response.

```python
from firecrawl import Firecrawl          # the official Firecrawl SDK, unchanged

app = Firecrawl(api_key="hc-…", api_url="https://your-node.example.com")
app.search("kerala monsoon forecast", limit=5)
app.scrape("https://en.wikipedia.org/wiki/Kerala").markdown
```

AI agents connect over **MCP** (`/mcp`) and get search, scrape, map, crawl, extract and agent tools.

### Why HiveCrawl instead of Firecrawl?

- **Search that doesn't get blocked when you self-host.** Self-hosted Firecrawl searches from one IP (one SearXNG,
  or scraping DuckDuckGo), and engines block a single IP within minutes. HiveCrawl retries only the blocked engines
  through Cloudflare WARP and through other nodes — including home connections with residential IPs — and merges
  the answers.
- **No per-query bill.** Firecrawl cloud charges credits per search and scrape. HiveCrawl runs on machines you
  already have; the network is the people running nodes.
- **Your queries stay on your infrastructure.** Nothing goes to a third-party API unless you route it to peers you chose.
- **Small to run.** One ~10 MB Go binary plus SearXNG, a small WARP proxy and one headless Chrome — no Redis,
  Postgres or worker fleet.
- **Bring any AI model.** Extract, agent and the `json`/`summary` formats use the model you pick in the web UI —
  Anthropic (Claude), OpenAI, Gemini, DeepSeek, OpenRouter, Groq or a local Ollama — billed to your own account and
  never used for other people in the network.
- **No code changes.** Same request and response bodies; the official Firecrawl SDKs work by changing `api_url`.
  MCP is built in.

**Choose Firecrawl instead** if you need its hosted reliability at scale, its proprietary search index, residential
proxies on demand, or features listed under [where Firecrawl is better](#hivecrawl-vs-firecrawl).

---

## What's new in 0.4.1

- **Auto-update switch.** Settings → Updates has an **Auto-update** on/off switch (on by default).
- The updater now refreshes itself after installing a release.

## What's new in 0.4.0

- **A redesigned web UI.** A sidebar for navigation, larger search and URL bars, roomier result cards, and a new
  sign-in screen. A graphite dark theme and a light theme, with a **Light / Dark / Match system** switch at the
  bottom of the sidebar that remembers your choice. On a phone the sidebar becomes a top bar with scrolling tabs.
  [See the screens](#feature-tour).
- **Automatic updates, always on.** A small updater service checks for new releases every 6 hours and installs
  them by itself; if a release doesn't start, it puts the previous version back. **Settings → Updates** shows the
  version, the latest release, when it last checked and updated, and a **Check for updates** button. The version
  is also at the bottom of the sidebar. [How updating works](#updating).
- **Signed releases.** Every release ships Linux, macOS and Windows binaries and a source bundle, each with
  [SLSA level 3](https://slsa.dev) provenance, so you can check a download was built by this repository's
  GitHub workflow from the tagged source. Tests run on every push.

## Contents

- [Why HiveCrawl instead of Firecrawl?](#why-hivecrawl-instead-of-firecrawl)
- [Why HiveCrawl exists](#why-hivecrawl-exists)
- [HiveCrawl vs Firecrawl](#hivecrawl-vs-firecrawl)
- [Feature tour](#feature-tour)
- [Quick start](#quick-start)
- [Signing in to the web UI](#signing-in-to-the-web-ui)
- [AI features (LLM)](#ai-features-llm)
- [Updating](#updating)
- [Architecture](#architecture)
- [How a search is answered](#how-a-search-is-answered)
- [The mesh](#the-mesh)
- [Trust and abuse resistance](#trust-and-abuse-resistance)
- [Firecrawl compatibility](#firecrawl-compatibility)
- [MCP for AI agents](#mcp-for-ai-agents) (Claude Code, Cursor, …)
- [Configuration](#configuration)
- [Measured performance](#measured-performance)
- [Is it safe to run?](#is-it-safe-to-run)
- [Limitations](#limitations)
- [Development](#development)
- [License](#license)

---

## Why HiveCrawl exists

Search is the part of an AI agent's toolchain that is hardest to self-host. Running a crawler and index like
Google's is out of reach, so every "self-hosted" search tool ends up scraping the big engines — and the big
engines block scrapers. We measured it from a single residential IP and a single cloud server:

| Engine | What happened |
|---|---|
| DuckDuckGo | CAPTCHA on the **first** request |
| Mojeek | CAPTCHA on the first request |
| Brave | `429` after ~4 queries, then suspended |
| Bing | No block — it **silently returns unrelated results** to scrapers (a query for "golang tutorial" returned restaurants in Ajman) |
| Google (via SearXNG's CSE engine) | Held up the longest |

Firecrawl's open-source self-hosted search has the same problem: without its proprietary hosted engine it uses
a SearXNG instance if you configure one, and otherwise scrapes DuckDuckGo, which blocks almost immediately. A single
SearXNG on a single IP hits the same walls, just a little later. Paid SERP APIs solve it by renting large pools of
residential proxies.

HiveCrawl's idea is to make that pool **cooperative**. Every person who runs a node contributes an IP address that
engines see as an ordinary user. A node that gets blocked borrows a working route from the network; in return it
answers searches for others. The more nodes join, the less any single IP is used, and the less anyone is blocked.

It is built to be the search backend you can actually own:

- **Drop-in for Firecrawl.** Same request and response bodies; the official Python and JavaScript SDKs work unmodified.
- **Cheap to run.** A node is a ~10 MB Go binary using single-digit MB of RAM, plus SearXNG (~100 MB) and a tiny WARP proxy.
- **Works behind a home router.** No port forwarding: home nodes dial out and keep a tunnel to public nodes.
- **Agent-ready.** Built-in MCP server.

## HiveCrawl vs Firecrawl

Firecrawl is an excellent, mature platform. HiveCrawl implements the same API and puts the part that is hardest to
self-host — search that doesn't get blocked — on a cooperative network instead of a paid index.

**Where HiveCrawl is better**

| | HiveCrawl | Firecrawl self-hosted | Firecrawl cloud |
|---|---|---|---|
| Search that keeps working when engines block you | ✅ blocked engines retried via WARP + peers | ⚠️ one SearXNG or DuckDuckGo scraping, from one IP | ✅ proprietary engine |
| Cost | Free (your own nodes) | Free | Paid per credit |
| Residential IPs without buying proxies | ✅ peers behind home routers | ❌ | (their infrastructure) |
| Runs behind NAT / home router | ✅ outbound tunnels | — | — |
| Footprint | 1 Go binary + SearXNG + WARP proxy + headless Chrome | Node API + workers + Redis + Postgres + Playwright | — |
| Queries stay on infrastructure you choose | ✅ | ✅ | ❌ sent to a third party |
| MCP server | ✅ built in | separate project | separate project |
| AI model | ✅ any: Anthropic, OpenAI, Gemini, DeepSeek, OpenRouter, Ollama — set in the UI | OpenAI-compatible via env | theirs, paid in credits |
| Shared result cache across a network | ✅ | — | ✅ |

**What HiveCrawl now implements from Firecrawl**

| Firecrawl feature | HiveCrawl |
|---|---|
| `crawl`, `map`, `batch/scrape` | ✅ background jobs, robots.txt, sitemaps, include/exclude paths, depth, webhooks (signed), pagination |
| `extract`, `agent` (LLM-powered) | ✅ with the model you configure; wildcard URLs, web search, sources |
| `json` and `summary` formats | ✅ |
| JavaScript rendering, `actions`, `waitFor`, `mobile`, screenshots | ✅ headless Chrome with stealth patches |
| PDF / DOCX parsing | ✅ |
| `images` / `news` search sources | ✅ |
| Anti-bot for **scraping** | ✅ Chrome TLS/HTTP2 fingerprint → WARP retry → real browser → (opt-in) a peer's IP |

**Where Firecrawl is still better**

| | |
|---|---|
| Scale and uptime | A managed service with SLAs; a HiveCrawl network is as reliable as its nodes |
| Search index | Firecrawl cloud has its own engine; HiveCrawl depends on what SearXNG's engines return |
| Proxies | Firecrawl rents residential/mobile proxies on demand; HiveCrawl only has WARP and peers who opt in |
| Extra formats and endpoints | `changeTracking`, `branding`, `/parse` file upload, browser profiles and sessions are not implemented |

## Feature tour

Every screenshot below is the real UI of a running node, with live results (the API key is masked).

**Search the web, news and images** — blocked engines are retried through WARP and peers, and the result says
which node and engines answered.

<p align="center"><img src="docs/assets/ui/search-web.png" alt="Web search results for kerala monsoon forecast with source, engines and blocked engines shown, and a cURL snippet" width="100%"></p>

<table>
<tr>
<td width="50%"><img src="docs/assets/ui/search-news.png" alt="News results with publish dates and thumbnails"><br><sub><b>News</b> with dates and thumbnails, filterable by time</sub></td>
<td width="50%"><img src="docs/assets/ui/search-images.png" alt="Image results in a grid with sizes"><br><sub><b>Images</b> with sizes and source pages</sub></td>
</tr>
</table>

**Scrape any page** — clean markdown, HTML, links, a screenshot, an AI summary or AI-extracted JSON; JavaScript
pages, mobile view and PDF/DOCX included.

<table>
<tr>
<td width="50%"><img src="docs/assets/ui/scrape-ai.png" alt="Scrape result with an AI summary and extracted JSON"><br><sub><b>AI summary + extract</b> on one page</sub></td>
<td width="50%"><img src="docs/assets/ui/scrape-screenshot.png" alt="Scrape result showing a full screenshot of the page"><br><sub><b>Screenshot</b> from the node's headless Chrome</sub></td>
</tr>
<tr>
<td width="50%"><img src="docs/assets/ui/scrape-markdown.png" alt="Rendered markdown of a scraped page"><br><sub><b>Markdown preview</b>, ready for an LLM</sub></td>
<td width="50%"><img src="docs/assets/ui/map.png" alt="Map of a site's URLs filtered by a search term"><br><sub><b>Map</b> a site's URLs from sitemaps and links</sub></td>
</tr>
<tr>
<td width="50%"><img src="docs/assets/ui/crawl.png" alt="Crawl progress, list of crawled pages and one page opened"><br><sub><b>Crawl</b> a site in the background, read every page</sub></td>
<td width="50%"><img src="docs/assets/ui/ai-extract.png" alt="AI extract result as JSON with sources"><br><sub><b>Extract</b> structured data from pages</sub></td>
</tr>
<tr>
<td width="50%"><img src="docs/assets/ui/ai-agent.png" alt="AI agent answer with a cited source"><br><sub><b>Agent</b> searches and reads the web, answers with sources</sub></td>
<td width="50%"><img src="docs/assets/ui/network.png" alt="Network page with peers, routes and node capabilities"><br><sub><b>Network</b>: peers, routes, verification, capabilities</sub></td>
</tr>
<tr>
<td width="50%"><img src="docs/assets/ui/connect.png" alt="Connect page with MCP and SDK snippets"><br><sub><b>Connect</b>: MCP and SDK snippets with your URL and key</sub></td>
<td width="50%"><img src="docs/assets/ui/search-light.png" alt="The search page in the light theme"><br><sub><b>Light</b> and dark themes, or follow the system</sub></td>
</tr>
<tr>
<td width="50%"><img src="docs/assets/ui/settings.png" alt="Settings with node access, the AI model form and the Updates panel"><br><sub><b>Settings</b>: node access, AI model and updates</sub></td>
<td width="50%" align="center"><img src="docs/assets/ui/mobile.png" alt="The search page on a phone, with the sidebar as a top bar" width="60%"><br><sub>Works on a <b>phone</b></sub></td>
</tr>
</table>

**Claude Code using HiveCrawl instead of Firecrawl** — a real session: one search, one scrape, an answer with
its source. No Firecrawl account, no credits, no daily cap: it runs on your own node.

<p align="center"><img src="docs/assets/ui/claude-code.png" alt="Claude Code calling hivecrawl_search for news and hivecrawl_scrape on an article, then answering with three bullets and the source URL" width="100%"></p>

**The official Firecrawl SDK, unchanged** — only `api_url` points at your node. This is the real output of the
test suite against a node:

<p align="center"><img src="docs/assets/ui/sdk-python.png" alt="19 of 19 Firecrawl Python SDK checks passing against a HiveCrawl node" width="100%"></p>

## Quick start

Requirements: Docker with Compose.

```bash
git clone https://github.com/savor3/hivecrawl && cd hivecrawl
cp .env.example .env        # optional: every setting has a default
docker compose up -d --build
```

Then [sign in to the web UI](#signing-in-to-the-web-ui) at `http://localhost:3002`. It has Search (web, news,
images), Scrape, Map, Crawl and AI playgrounds with ready-to-copy cURL, Python and Node snippets, a Network page,
Settings and a Connect page with MCP setup.

**That's all it takes to join the network.** There is nothing you have to set. A node with no
network settings joins the public **community network** on its own: it contacts the built-in public nodes, finds
the rest through gossip, and — if it's a home machine — opens its tunnels. No keys to exchange, no peers to type
in. It also makes its own API key on first start (or set `HC_API_KEYS` yourself).

| You want | Set |
|---|---|
| Join the community network (default) | nothing |
| Your own private network | the same `HC_NETWORK_KEY` on every node (`openssl rand -hex 24`), and `HC_PEERS` = its public nodes |
| A node that talks to nobody | `HC_NETWORK=off` |

**A public node** (VPS) also sets `HC_PUBLIC_URL` and sits behind a reverse proxy for TLS (the compose file binds
to `127.0.0.1` only). Public nodes are the network's entry points, so running one helps everyone:

```
# Caddyfile
hc.example.com {
    reverse_proxy 127.0.0.1:3002
}
```

**A home node** (behind a router) leaves `HC_PUBLIC_URL` empty. It needs no open ports: it dials out to public
nodes and keeps two tunnels open.

**How nodes find each other.** Like BitTorrent, a new node needs just one address to start from — the community
network ships with some, a private network lists its own in `HC_PEERS`. From there gossip discovers everyone else.
Each node also saves the public nodes it has reached (`/data/peers-*.json`), so after a restart it rejoins through
any of them even if the original starting points are gone. The more public nodes exist, the less the network
depends on any one of them.

**Why more nodes make it better.** Every node is another set of IP addresses that search engines see as an
ordinary user. When an engine blocks your node, only that engine is retried — through WARP and through other
nodes, each taking a different part — so with more nodes each IP does less work and gets blocked less. Home nodes
add residential IPs, which engines rarely block. Nodes that answer searches for others earn priority; take-only
nodes stay on a small trial allowance. See [the mesh](#the-mesh) and
[trust and abuse resistance](#trust-and-abuse-resistance).

**The community network is public by design.** Its key is published, so it is not a secret: members are
authenticated by their own node keys, and nobody can use your node's API, settings or AI key. But community
members see the searches your node sends them when your own engines are blocked (not who asked). For queries
you'd rather keep among people you trust, run a private network. A node's identity is created on first start and
kept in the `hc-data` volume.

## Signing in to the web UI

Every node is private. The UI, the API and MCP all need the node's **API key**, so nobody else — not other people on
your network, not other nodes in the mesh — can use your node or spend your AI credits. You sign in once per browser.

**1. Open the UI.** Until you sign in, it shows only this screen:

<p align="center"><img src="docs/assets/setup/1-sign-in.png" alt="The sign-in screen: step 1 shows a command to run, step 2 a field to paste the key" width="80%"></p>

**2. On the machine running the node**, in the `hivecrawl` folder, run the command shown (there is a Copy button):

```bash
docker compose exec hivecrawl /hivecrawl login
```

<p align="center"><img src="docs/assets/setup/2-login-command.png" alt="The command prints a sign-in link and the API key" width="80%"></p>

**3. Open the printed link** — you are signed in. Or paste the key into the field and press **Sign in**.

The link carries the key after `#`, which browsers never send to a server; the UI saves it in that browser and
removes it from the address bar at once. A wrong key is refused and the UI stays locked. **Settings → Node access**
(also the key icon at the bottom of the sidebar) has the sign-out button.

Where the key comes from: `HC_API_KEYS` in `.env` if you set it, otherwise a random key the node made on first
start and saved in its data volume (`/data/api-key`, readable only by the node). To change it, set `HC_API_KEYS`
and run `docker compose up -d`. If the UI is reached through another address (a domain or SSH tunnel), set
`HC_UI_URL` so the printed link uses it.

**Settings → Node access** shows which key the browser uses and can copy the key or the sign-in link for your other
devices, the Firecrawl SDKs and MCP clients.

## AI features (LLM)

Extract, agent and the `json` / `summary` scrape formats need an AI model. Pick one in **Settings → AI model**:

<p align="center"><img src="docs/assets/setup/3-settings.png" alt="Settings: node access, and the AI model form with provider, base URL, model and API key" width="80%"></p>

1. Choose a provider — **Anthropic (Claude)**, OpenAI, Google Gemini, DeepSeek, OpenRouter, Groq, Ollama on the
   same machine, or any OpenAI-compatible API. The base URL fills in.
2. Paste your API key from that provider, press **Load models** and pick one, then **Test connection**.
3. **Save.** It applies immediately, without a restart.

Two different keys, two different jobs:

| | Node API key | AI provider key |
|---|---|---|
| What it is | The password for *your node* | Your account with Anthropic, OpenAI, … |
| Who uses it | You: browser, SDKs, MCP clients | Only your node, when it calls the model |
| Where it lives | `HC_API_KEYS` or `/data/api-key` | `/data/settings.json` (owner-only); the UI shows only `sk-…3d6b` |

The model is **used only for requests made with your node's API key**. Mesh peers can ask your node for searches
(and, if you opt in, page fetches), but there is no mesh route to the model, so other people in the network can
never spend your credits. You can also configure it with `HC_LLM_PROVIDER`, `HC_LLM_BASE_URL`, `HC_LLM_MODEL` and
`HC_LLM_API_KEY`; settings saved in the UI take precedence.

## Updating

There is nothing to do: nodes update themselves. The `updater` service in `docker-compose.yml` starts with the
rest of the node and:

1. checks GitHub for a newer release every 6 hours, and right away when you press **Check for updates** in
   **Settings → Updates**;
2. if there is one, checks out that release tag, rebuilds the node with `docker compose up -d --build` and waits
   for it to answer;
3. if the new version doesn't come up, goes back to the version you had and rebuilds that.

Your `.env`, settings and the `hc-data` volume are kept. The Settings page shows the version, the latest release,
when it last checked, when it last updated, and an **Auto-update** switch — on by default; turn it off if
you'd rather install updates yourself.

<p align="center"><img src="docs/assets/ui/settings.png" alt="Settings → Updates: version, latest release, last checked, last updated, auto-update on, and a Check for updates button" width="80%"></p>

| You want | Set in `.env` |
|---|---|
| Updates installed automatically (default) | nothing |
| Only see that an update is out | turn off **Auto-update** in Settings → Updates (or `HC_AUTO_UPDATE=false`) |
| Check more or less often | `HC_UPDATE_INTERVAL` in seconds (default `21600`, 6 h) |
| No updater at all | remove the `updater` service from `docker-compose.yml` |

Without the updater you can update by hand in the `hivecrawl` folder: `./update.sh` (or `./update.sh --check`).
Updates come only from release tags of [savor3/hivecrawl](https://github.com/savor3/hivecrawl), and the node must
be a git clone of it.

**Verify a release download.** Each binary and the source bundle come with an SLSA provenance file
(`*.intoto.jsonl`). With [slsa-verifier](https://github.com/slsa-framework/slsa-verifier):

```bash
slsa-verifier verify-artifact hivecrawl-linux-amd64 \
  --provenance-path hivecrawl-linux-amd64.intoto.jsonl \
  --source-uri github.com/savor3/hivecrawl --source-tag v0.4.0
```

## Architecture

<p align="center">
  <img src="docs/assets/hero.jpg" alt="A search goes into the hive of nodes (home machines, servers, laptops), is answered by whichever routes work, and comes out as ranked results" width="100%">
</p>

A query enters the network at your node, is answered by whichever routes are working — your node's own engines,
Cloudflare WARP, servers and home machines — and comes back as one ranked list. In detail:

<p align="center"><img src="docs/assets/diagrams/architecture.png" alt="Inside one node: your apps call the API with the node key; the node holds search, scrape, crawl, AI and mesh modules; SearXNG, WARP and headless Chrome are private containers; the AI provider and other nodes are outside" width="100%"></p>

One node is four containers from `docker-compose.yml`:

| Container | Role |
|---|---|
| `hivecrawl` | The Go node: API, web UI, MCP, cache, orchestrator, mesh, scraper |
| `searxng` | Meta-search over Google CSE, Brave, DuckDuckGo, Startpage, Mojeek, Wikipedia; each also has a disabled `<engine> warp` copy that goes out through WARP |
| `warp` | Registers a free Cloudflare WARP account on first start (`wgcf`) and exposes it as SOCKS5 inside the compose network only (`wireproxy`) |
| `browser` | Headless Chrome for JavaScript pages, actions and screenshots, on its own network that only the node can reach |

Code layout:

| Package | What it does |
|---|---|
| `internal/api` | Firecrawl-compatible REST API, MCP server, embedded web UI |
| `internal/node` | Search orchestration: cache → local → scatter blocked engines → merge |
| `internal/mesh` | Identity, signing, gossip, NAT tunnels and relay, rate limits, priority, probes, reputation |
| `internal/searx` | SearXNG JSON client |
| `internal/scrape` | Fetch tiers (Chrome fingerprint → WARP → browser → peers), clean, HTML/PDF/DOCX to markdown, SSRF protection |
| `internal/browser` | Headless Chrome: rendering, actions, screenshots, stealth patches |
| `internal/crawl`, `internal/jobs` | Crawl and map (robots.txt, sitemaps), background jobs, webhooks |
| `internal/llm`, `internal/settings` | Anthropic and OpenAI-compatible clients; settings saved from the UI |
| `internal/cache` | TTL + LRU result cache |
| `internal/netx` | Outbound connections resolved through public DNS |
| `internal/e2e`, `internal/sim` | Multi-node end-to-end test and a 1,000-node network simulation |

## How a search is answered

<p align="center">
  <img src="docs/assets/how-it-works.jpg" alt="Three steps: a node's search engines, two of them blocked; the blocked engines retried through Cloudflare WARP and a home node; all results merged into one ranked list" width="100%">
</p>

**Blocked engines → retried through WARP and home nodes → merged into one ranked list.** Step by step:

<p align="center"><img src="docs/assets/diagrams/search-flow.png" alt="A query goes to the cache, then this node's engines; if engines are blocked, only those are retried at once through WARP and peers, then everything is merged by rank" width="100%"></p>

1. **Cache.** Normalized query → cached merged results (default 1 hour). Repeat queries cost nothing upstream.
2. **Local first.** The node's own SearXNG runs every direct engine. If it returns enough results
   (`HC_MIN_RESULTS`, default 8), that's the answer — most queries end here and use nobody else's capacity.
3. **Scatter only what failed.** SearXNG reports which engines were blocked. Only those are retried, at the
   same time, through WARP and through peers. Each peer gets a *different* subset (engines are assigned to the
   least-loaded peer that hasn't reported that engine blocked), so no single IP is hammered.
4. **Merge.** Reciprocal rank fusion: a link found by several routes ranks higher; duplicates collapse.
5. **Report.** The body is exactly Firecrawl's; how it was answered is in headers:
   `X-HC-Source: local+warp+peer:home-laptop`, `X-HC-Engines`, `X-HC-Blocked`, `X-HC-Took-Ms`, `X-HC-Node`.

A live example from the deployed network, where a cloud server's DuckDuckGo was CAPTCHA-blocked:

```
query: "alleppey boat race 2026"   52 results in 2.7s
source:  local+warp+peer:home-laptop
engines: google cse, brave, duckduckgo warp, duckduckgo
blocked: duckduckgo   ← retried via WARP and via the home node's residential IP
```

Peers only ever answer from their own cache and SearXNG — a request is never forwarded again, so one query
cannot fan out across the network.

## The mesh

<p align="center">
  <img src="docs/assets/mesh.jpg" alt="Home computers behind ordinary routers each open an outbound connection up to public servers, which are linked to each other" width="100%">
</p>

**Home nodes only dial out.** No port forwarding, no open ports on your router: each home node keeps a tunnel
to public nodes, and searches reach it through those tunnels.

<p align="center"><img src="docs/assets/diagrams/mesh.png" alt="Three home networks behind routers each open outbound tunnels up to public nodes A, B and C, which gossip with each other; node C reaches the laptop by asking node A to relay down the laptop's tunnel" width="100%"></p>

- **Public nodes** are reachable over HTTPS. **Home nodes** dial out and keep tunnels to 2 public nodes
  (`HC_TUNNELS`), so they need no port forwarding. Others reach a home node through a relay endpoint on any
  public node holding its tunnel.
- **Relay choice** uses *power of two choices*: pick two known public nodes at random, tunnel to the less loaded.
  This spreads thousands of home nodes evenly even though load figures from gossip are seconds old.
- **Gossip** contacts a random sample of 8 peers per 30-second round and shares a capped sample of its peer
  table, so traffic grows linearly with the network, not quadratically. Peers also share which engines are
  blocked for them, so blocked engines get routed elsewhere.
- **Heartbeats** run from both ends of a tunnel; home connections that die silently (Wi-Fi, router NAT) are
  detected in under a minute and redialed.

## Trust and abuse resistance

Anyone who contributes can search, so the network has to stop freeloaders, impersonators and liars.

| Threat | Defense |
|---|---|
| Outsiders using your node | The REST API, MCP and UI require the node's own `HC_API_KEYS`. |
| Joining a private network you weren't invited to | Every mesh request carries an HMAC of that network's key. (The community network's key is public on purpose; the defenses below are what protect it.) |
| Impersonating another node (to steal its priority) | Each node has an ed25519 key; its ID is derived from the public key and every request is signed. |
| Take-only clients that never serve | **Proof of participation:** a new node asking for searches is probed back over the mesh and must answer a probe search, signed with its key, whose results overlap the prober's own. Until then it gets a 3/minute trial. |
| Making many identities | Identities are free, but each one stays on the trial allowance until it proves it serves; a per-IP limit caps what one machine can take. |
| Heavy users | Per-node token buckets. **Tit-for-tat priority:** each 10 searches a peer answered for you raises its allowance with you by 1× (up to 3×); unbroken uptime adds a bonus; peers that only take drop to 0.5×. When a node is busy, contributors wait for a slot and others get `429`. |
| Junk or poisoned results | Every peer answer is scored by how much it overlaps other routes for the same query; peers that consistently return links nobody else sees sink to the back of the queue. |

All scores are local: a node only trusts what it has observed itself, so they cannot be inflated by claims.

## Firecrawl compatibility

<p align="center">
  <img src="docs/assets/firecrawl-compatible.jpg" alt="Two puzzle pieces fitting together: a flame-orange piece and a honeycomb-amber piece" width="70%">
</p>

**Drop-in for Firecrawl:** keep your code and SDKs, change only the API URL.

Tested with the official SDKs — `firecrawl-py` 4.54 and `@mendable/firecrawl-js` 4.51 — pointed at a node.

| Endpoint | Support |
|---|---|
| `POST /v2/search`, `POST /v1/search` | ✅ `query`, `limit` (≤100), `tbs` (`qdr:h/d/w/m/y`), `lang`, `country`, `includeDomains`, `excludeDomains`, `ignoreInvalidURLs`, `timeout`, `scrapeOptions` |
| search `sources` | ✅ `web`, `news` (with dates and thumbnails), `images` (with sizes) |
| `POST /v2/scrape`, `POST /v1/scrape` | ✅ formats `markdown`, `html`, `rawHtml`, `links`, `screenshot` (`@fullPage`), `json`, `summary`; `onlyMainContent`, `includeTags`, `excludeTags`, `waitFor`, `mobile`, `actions`, `timeout`; PDF and DOCX |
| `POST /v2/crawl`, `GET/DELETE /v2/crawl/{id}`, `/errors`, `/ongoing` | ✅ |
| `POST /v2/batch/scrape` and status | ✅ |
| `POST /v2/map` | ✅ sitemaps + links, `search`, `limit` |
| `POST /v2/extract`, `POST /v2/agent` and status | ✅ needs an [AI model](#ai-features-llm) |
| `changeTracking`, `branding`, `/parse` | ❌ |

Response bodies match Firecrawl's exactly (`{"success":true,"data":{"web":[{"url","title","description","position"}]},"creditsUsed":0,"id":"…"}`);
HiveCrawl-specific details go only in `X-HC-*` headers. Extension: `"localOnly": true` skips WARP and peers.

## MCP for AI agents

Every node serves a streamable-HTTP MCP server at `/mcp`, protected by the same API key.

| Tool | What it does |
|---|---|
| `hivecrawl_search` | Web search; `scrape: true` also returns each result's main content as markdown |
| `hivecrawl_scrape` | One page as clean markdown with title and metadata (optionally its links) |
| `hivecrawl_map` | List a site's URLs |
| `hivecrawl_crawl`, `hivecrawl_crawl_status` | Crawl a site in the background and read the pages |
| `hivecrawl_extract` | Structured data from pages, by prompt or schema (needs an AI model) |
| `hivecrawl_agent` | Researches a question with search and scraping, returns an answer with sources (needs an AI model) |

**Claude Code**

```bash
claude mcp add -s user --transport http hivecrawl https://your-node.example.com/mcp \
  --header "Authorization: Bearer <HC_API_KEY>"
claude mcp get hivecrawl     # should say ✔ Connected
```

`-s user` makes it available in every project. Start a new Claude Code session afterwards; running sessions don't
pick up new MCP servers.

If it doesn't work:

| You see | Fix |
|---|---|
| `✘ Failed to connect` | Wrong API key or URL. Print the key with `docker compose exec hivecrawl /hivecrawl login`. |
| `already exists` | Remove the old entry first: `claude mcp remove hivecrawl -s user` |
| Claude says it has no `hivecrawl` tool | Start a new session, then check `claude mcp list` |

**Claude Desktop, Cursor and other clients**

```json
{
  "mcpServers": {
    "hivecrawl": {
      "type": "http",
      "url": "https://your-node.example.com/mcp",
      "headers": { "Authorization": "Bearer <HC_API_KEY>" }
    }
  }
}
```

The web UI's Connect page shows these with your node's URL and key filled in. Once added, an agent uses the
`hivecrawl_*` tools like any other — see the [Claude Code session](#feature-tour) above.

**HiveCrawl first.** The server tells the agent to use HiveCrawl for web search, news, reading pages and crawling
before any other search or scraping tool it has (built-in web search, Firecrawl and the like), and to fall back to
those only if a HiveCrawl call fails. You don't need to ask for it in each prompt.

## Configuration

All settings are environment variables (see `.env.example`).

| Variable | Default | Meaning |
|---|---|---|
| `HC_NODE_NAME` | hostname | Display name shown to peers |
| `HC_API_KEYS` | generated | Comma-separated keys for the API, MCP and UI. Empty = a key made on first start, saved in `/data/api-key` |
| `HC_UI_URL` | `HC_PUBLIC_URL`, else `http://localhost:$HC_PORT` | Address used in the printed sign-in link |
| `HC_NETWORK` | `community` | `community` joins the public network; `off` runs alone. Ignored when `HC_NETWORK_KEY` is set |
| `HC_NETWORK_KEY` | — | Your own private network's shared key (≥16 chars) |
| `HC_PUBLIC_URL` | — | This node's public HTTPS URL. Empty = home node (tunnels out) |
| `HC_PEERS` | community nodes | Extra public node URLs to contact first; gossip and the saved peer list find the rest |
| `SEARXNG_SECRET` | random on each start | SearXNG's secret key |
| `HC_MIN_RESULTS` | `8` | Fewer local results than this triggers WARP and peers |
| `HC_WARP` | `true` | Retry blocked engines through Cloudflare WARP |
| `HC_ENGINES` | `google cse,brave,duckduckgo,startpage,mojeek` | Direct engines; must match `deploy/searxng/settings.yml` |
| `HC_CACHE_TTL` / `HC_CACHE_SIZE` | `1h` / `5000` | Result cache |
| `HC_PEER_RATE` | `30` | Base searches/minute one peer may ask (contributors earn up to 3×) |
| `HC_MAX_CONCURRENT` | `4` | Peer searches served at once |
| `HC_TUNNELS` / `HC_MAX_TUNNELS` | `2` / `1000` | Tunnels a home node keeps / a public node accepts |
| `HC_DNS` | `1.1.1.1:53,8.8.8.8:53` | Resolvers for all outbound connections |
| `HC_DATA_DIR` | `/data` in Docker | Where the node's identity key lives |
| `HC_PORT` | `3002` | Host port (bound to 127.0.0.1) |
| `HC_BROWSER` / `HC_BROWSER_TABS` | `browser:9222` / `4` | Headless Chrome; empty disables JS rendering |
| `HC_TIMEZONE` | from public IP | Browser time zone |
| `HC_CRAWL_MAX_PAGES` / `HC_CRAWL_CONCURRENCY` | `5000` / `10` | Crawl caps |
| `HC_WEBHOOK_SECRET` | — | Signs webhooks (`X-Firecrawl-Signature`) |
| `HC_LLM_PROVIDER`, `HC_LLM_BASE_URL`, `HC_LLM_MODEL`, `HC_LLM_API_KEY` | — | AI model (`anthropic` or OpenAI-compatible); usually set in the UI instead |
| `HC_SERVE_SCRAPES` | `false` | Let verified peers fetch pages through this node's IP |
| `HC_PEER_SCRAPES` | `true` | Ask opted-in peers when every local tier is blocked |
| `HC_UPDATE_CHECK` | `true` | The node asks GitHub twice a day whether a newer release is out (reads the public release list only) |
| `HC_AUTO_UPDATE` | `true` | The updater installs new releases by itself; `false` only checks |
| `HC_UPDATE_INTERVAL` | `21600` | Seconds between the updater's checks |

## Measured performance

**Network simulation** (`internal/sim`): 1,000 home nodes and 10 public nodes in one process, every home node
seeded with the *same* single public node (worst case), fake engines with 20–80 ms latency:

| Metric | Result |
|---|---|
| Tunnels established | 2,000 / 2,000 in ~6 s, spread 164–235 per public node |
| Searches | 400 / 400 succeeded (189 routed through peers); p50 79 ms, p95 151 ms, p99 164 ms |
| Gossip | 8 hellos per node per round; ≈112 KB/s per public node at the production interval |
| Peer table | ~1,000 entries (capped at 5,000) |
| Memory | ~360 KB per simulated node, including its test servers |

**A real node:** the Go process uses ~8–10 MB of RAM and the image is ~10 MB; SearXNG uses ~80–140 MB, the WARP
proxy ~7 MB and headless Chrome ~200–260 MB. Typical searches take ~1 s locally and ~1.5–2.7 s when blocked engines are retried through WARP and peers.

## Is it safe to run?

Short answer: **nothing that arrives from the network — searches, pages, peers — can run anything on your
machine, and nobody else can use your node.** Everything from the network is treated as data, every container
that touches it is locked down, and nothing reaches your home network. The one component with more power is the
updater, which only installs this repository's releases ([details below](#the-updater-honestly)). Here is exactly
why, so you can check it yourself.

<p align="center"><img src="docs/assets/diagrams/security.png" alt="Your machine: hivecrawl, headless Chrome, SearXNG and WARP containers, all locked down; you have full access with your key; other nodes can only send signed searches, probes and opt-in page fetches; everyone else is blocked; private and internal addresses are refused on every route" width="100%"></p>

### It never runs code it receives

- **The Go node has no way to run programs.** There is no `os/exec`, no shell, no plugins, no scripting
  engine and no `eval` anywhere in the code (`grep -rn os/exec .` finds nothing). The image is distroless:
  it contains the binary and nothing else, not even `/bin/sh`.
- **Everything from other nodes is parsed as data.** Peers can send exactly three kinds of message — a
  search, a probe search, and (only if *you* enable it) a page fetch. Each is decoded into a fixed Go struct and
  answered with results. A message can't make the node do anything else. Requests are capped at 64 KB, responses
  at 8 MB, and tunnel frames at 32 MB.
- **Scraped pages are data too.** HTML, PDF and DOCX are parsed by memory-safe Go code. JavaScript from web
  pages only ever runs inside the headless Chrome container (below), never on your machine. In the web UI,
  scraped markdown is cleaned by DOMPurify before it is shown, so a page can't inject script into the UI.

### Every container is locked down

| Container | User | Filesystem | Linux capabilities | Network |
|---|---|---|---|---|
| `hivecrawl` | non-root (distroless `nonroot`) | read-only; only the `hc-data` volume is writable | all dropped, `no-new-privileges` | port bound to `127.0.0.1` only |
| `browser` (Chrome) | `nobody` | read-only; scratch space in memory (`tmpfs`) | all dropped, `no-new-privileges` | its own network — only the node can reach it |
| `searxng` | drops to the `searxng` user | its config is mounted read-only | all dropped except the 4 its entrypoint needs to switch user, `no-new-privileges` | no published port |
| `warp` | — | its own volume | all dropped, `no-new-privileges` | no published port |
| `updater` | root (needs the Docker socket) | the node's folder and a small shared status folder | `no-new-privileges` | no published port; makes outbound calls to GitHub and image registries only |

Even if a malicious page found a bug in Chrome, it would land in a container that can't write files, has no
privileges to escalate, and can't see your network.

### Nothing can reach your machine or home network

Every route that fetches a URL — direct, Cloudflare WARP, the browser (every request a page makes is
intercepted), and fetches for peers — refuses these addresses **at the moment of connecting**, so redirects and
DNS tricks can't get around it:

- `localhost`, `127.0.0.0/8` and the machine itself
- your LAN: `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`, link-local `169.254.0.0/16`, CGNAT `100.64.0.0/10`
- cloud metadata (`169.254.169.254`) and IPv6 private, link-local and unique-local ranges

So a scraped page, a crawl or a peer can never reach your router, NAS, printer, other containers or the node's
own admin endpoints. Home nodes also **publish no port at all**: they only dial out, so there is nothing on your
router for anyone to connect to.

### Only you can use your node

- **There is no open mode.** The UI, the API and MCP all need the node's API key. If you don't set one, the node
  makes a random key on first start. Without it, every request gets `401` and the UI stays on the sign-in screen.
- **The mesh can't touch your stuff.** Other nodes can only send the three messages above. They can't reach
  your AI model, settings, crawls, extract/agent jobs or files. Every mesh message must be signed with the
  sender's ed25519 key *and* carry your network's shared key; anything else is dropped.
- **Your AI credits are yours.** The AI model is used only for requests made with your API key. No mesh route
  leads to it.
- **Secrets stay on the node.** `/data/api-key` and `/data/settings.json` are owner-only files (`0600`). The AI
  provider key is never sent back to the browser (the UI shows only the last four characters, like `sk-…ab12`). The sign-in link carries the
  key after `#`, which browsers never send to servers, and the UI removes it from the address bar at once.

### What other people can see (privacy)

- Peers see the **queries** your node sends them when your own engines are blocked — not who asked, beyond your
  node's ID. Keep `HC_MIN_RESULTS` low, or run a private network with people you trust.
- If you enable `HC_SERVE_SCRAPES`, sites you fetch for peers see your IP, like any visit. It is off by default.
- Webhooks go only to URLs you set yourself in your own API requests.
- The update check sends one unauthenticated request to `api.github.com` twice a day, like any visit to a
  GitHub page. `HC_UPDATE_CHECK=false` turns it off.

### The updater, honestly

The `updater` container is the one part with real power: to rebuild the node it uses the Docker socket, which
is root-level control of the machine. That is why it is kept apart from everything that talks to the network:

- It has **no ports** and reads no input from the internet or from peers. Its only input is a flag file the node
  writes when you press **Check for updates**; the flag can make it check sooner, nothing else.
- It runs fixed commands only (`git fetch`, `git checkout <release tag>`, `docker compose up -d --build`), and
  only installs **release tags** of this repository.
- The node container itself still has no Docker access, no shell and no way to run programs.

What you trust by leaving it on: that a future release of this repository is safe — the same as with any app that
updates itself. If you'd rather review each release first, set `HC_AUTO_UPDATE=false` (it then only tells you)
or remove the service and run `./update.sh` yourself.

### Check it yourself

```bash
grep -rn '"os/exec"' --include=*.go .              # nothing: the node can't start programs
docker compose exec hivecrawl sh                   # fails: there is no shell in the image
docker inspect hivecrawl-browser-1 --format '{{.Config.User}} {{.HostConfig.ReadonlyRootfs}} {{.HostConfig.CapDrop}}'
curl -s -o /dev/null -w '%{http_code}\n' localhost:3002/v1/status   # 401 without the key
go test ./internal/scrape/ ./internal/browser/ -run 'Internal|Private|SSRF|Blocks'   # address checks
```

One honest caveat: `searxng/searxng:latest` is pulled by tag, so you trust the SearXNG project's releases, the
same as anyone running SearXNG. Pin a digest in `docker-compose.yml` if you prefer.

## Limitations

- Search quality is bounded by what SearXNG's engines return to your nodes; Google carries most of it.
- Sites with strong bot protection (CAPTCHA walls, Turnstile challenges) can still block the browser; HiveCrawl
  does not solve CAPTCHAs.
- Engines' terms of service generally prohibit automated querying, and Cloudflare WARP is meant for personal
  device traffic. Running HiveCrawl at scale is your responsibility; a small private network is the intended use.
- Probes cost a node one cached search per new peer per hour.

## Development

```bash
go test -race ./...                       # unit, API/MCP, scraper, mesh and multi-node e2e tests
HC_SIM=1 HC_SIM_HOMES=1000 HC_SIM_PUBLICS=10 go test -run TestSimulateNetwork -v ./internal/sim/
python3 docs/diagrams/make.py && node docs/diagrams/render.mjs   # redraw the diagrams (needs puppeteer-core)
go build -o bin/hivecrawl ./cmd/hivecrawl
HC_SEARXNG_URL=http://127.0.0.1:8888 HC_DATA_DIR=./data ./bin/hivecrawl
```

## License

HiveCrawl is an independent project and is not affiliated with or endorsed by Firecrawl. "Firecrawl" is used
only to describe API compatibility.

HiveCrawl is licensed under the [GNU Affero General Public License v3.0](LICENSE), like SearXNG and Firecrawl.
If you run a modified version as a network service, you must make your changes available to its users.

Bundled third-party software: [marked](https://github.com/markedjs/marked) (MIT) and
[DOMPurify](https://github.com/cure53/DOMPurify) (Apache-2.0 / MPL-2.0) in the web UI; the WARP container
downloads [wgcf](https://github.com/ViRb3/wgcf) (MIT) and [wireproxy](https://github.com/windtf/wireproxy) (ISC)
with pinned checksums; SearXNG runs as its own AGPL-3.0 container.

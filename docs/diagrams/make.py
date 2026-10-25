"""Generates the README diagrams as SVG (docs/assets/diagrams/*.svg).

    python3 docs/diagrams/make.py

PNG copies are rendered from these with headless Chrome (see render.sh).
"""
import os
from html import escape

OUT = os.path.join(os.path.dirname(__file__), "..", "assets", "diagrams")
FONT = "Inter, 'Segoe UI', 'Noto Sans', system-ui, sans-serif"
MONO = "'JetBrains Mono', 'DejaVu Sans Mono', ui-monospace, monospace"

INK = "#1f2a37"
MUTED = "#5b6675"
LINE = "#cdd4de"
BG = "#f5f6f8"
CARD = "#ffffff"
AMBER = "#e2900c"
AMBER_SOFT = "#fef3d9"
AMBER_EDGE = "#f5c66b"
GREEN = "#12897f"
GREEN_SOFT = "#e0f5f3"
BLUE = "#2563eb"
BLUE_SOFT = "#e8effd"
RED = "#c2410c"
RED_SOFT = "#fdeee6"
GREY_SOFT = "#eef1f5"


class SVG:
    def __init__(self, w, h):
        self.w, self.h, self.parts = w, h, []

    def add(self, s):
        self.parts.append(s)

    def text(self, x, y, s, size=15, color=INK, weight=400, anchor="start", font=FONT, italic=False):
        st = ' font-style="italic"' if italic else ""
        self.add(f'<text x="{x}" y="{y}" font-family="{font}" font-size="{size}" font-weight="{weight}" '
                 f'fill="{color}" text-anchor="{anchor}"{st}>{escape(s)}</text>')

    def lines(self, x, y, rows, size=14, color=MUTED, gap=None, anchor="start", weight=400, font=FONT):
        gap = gap or size * 1.45
        for i, r in enumerate(rows):
            self.text(x, y + i * gap, r, size, color, weight, anchor, font)

    def rect(self, x, y, w, h, fill=CARD, stroke=LINE, r=14, sw=1.3, dash=None, shadow=False):
        d = f' stroke-dasharray="{dash}"' if dash else ""
        f = ""  # flat style: no drop shadows
        self.add(f'<rect x="{x}" y="{y}" width="{w}" height="{h}" rx="{r}" fill="{fill}" stroke="{stroke}" '
                 f'stroke-width="{sw}"{d}{f}/>')

    def card(self, x, y, w, h, title, rows=(), fill=CARD, stroke=LINE, tcolor=INK, badge=None, bcolor=AMBER,
             tsize=17, rsize=13.5, shadow=True, dash=None):
        self.rect(x, y, w, h, fill, stroke, shadow=shadow, dash=dash)
        ty = y + 32
        if badge:
            self.pill(x + 18, y + 16, badge, bcolor)
            ty = y + 62
        self.text(x + 18, ty, title, tsize, tcolor, 650)
        self.lines(x + 18, ty + 24, rows, rsize)

    def pill(self, x, y, s, color=AMBER, size=11.5):
        w = len(s) * size * 0.62 + 18
        self.add(f'<rect x="{x}" y="{y}" width="{w:.0f}" height="22" rx="11" fill="{color}" fill-opacity=".14"/>')
        self.text(x + 9, y + 15.5, s, size, color, 700)
        return w

    def arrow(self, pts, color=MUTED, dash=None, sw=1.8, head=True, label=None, lx=None, ly=None,
              lcolor=None, lanchor="middle", both=False):
        d = "M" + " L".join(f"{x},{y}" for x, y in pts)
        da = f' stroke-dasharray="{dash}"' if dash else ""
        mid = color.strip("#")
        me = f' marker-end="url(#a{mid})"' if head else ""
        ms = f' marker-start="url(#s{mid})"' if both else ""
        self.add(f'<path d="{d}" fill="none" stroke="{color}" stroke-width="{sw}" stroke-linecap="round" '
                 f'stroke-linejoin="round"{da}{me}{ms}/>')
        self.markers.add(color)
        if label:
            self.label(lx, ly, label, lcolor or color, lanchor)

    def label(self, x, y, s, color=MUTED, anchor="middle", size=12.5):
        w = len(s) * size * 0.56 + 14
        bx = x - w / 2 if anchor == "middle" else (x if anchor == "start" else x - w)
        self.add(f'<rect x="{bx:.0f}" y="{y - 15}" width="{w:.0f}" height="22" rx="6" fill="{BG}"/>')
        self.text(x if anchor != "start" else x + 7, y, s, size, color, 600, anchor if anchor != "start" else "start")

    markers = set()

    def save(self, name, title):
        defs = ['<filter id="sh" x="-10%" y="-10%" width="120%" height="130%"><feDropShadow dx="0" dy="3" '
                'stdDeviation="5" flood-color="#5a4a20" flood-opacity=".09"/></filter>']
        for c in self.markers:
            m = c.strip("#")
            defs.append(f'<marker id="a{m}" viewBox="0 0 10 10" refX="8.5" refY="5" markerWidth="7" markerHeight="7" '
                        f'orient="auto-start-reverse"><path d="M0,0 L10,5 L0,10 z" fill="{c}"/></marker>')
            defs.append(f'<marker id="s{m}" viewBox="0 0 10 10" refX="1.5" refY="5" markerWidth="7" markerHeight="7" '
                        f'orient="auto-start-reverse"><path d="M0,0 L10,5 L0,10 z" fill="{c}"/></marker>')
        body = "\n".join(self.parts)
        svg = (f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {self.w} {self.h}" width="{self.w}" '
               f'height="{self.h}" role="img" aria-label="{escape(title)}"><title>{escape(title)}</title>'
               f'<defs>{"".join(defs)}</defs><rect width="100%" height="100%" fill="{BG}"/>\n{body}\n</svg>')
        with open(os.path.join(OUT, name + ".svg"), "w") as f:
            f.write(svg)
        SVG.markers = set()


def heading(s, x, y, title, sub):
    s.text(x, y, title, 26, INK, 750)
    s.text(x, y + 28, sub, 15, MUTED)


# ---------------------------------------------------------------- architecture
def architecture():
    s = SVG(1600, 860)
    heading(s, 60, 64, "Inside one HiveCrawl node", "Four containers from docker-compose.yml. Only the node talks to the outside; everything else is private.")

    # clients
    s.text(60, 150, "YOUR APPS", 12, MUTED, 700)
    cl = [("Firecrawl SDKs · cURL", "Python, Node, any HTTP"), ("AI agents over MCP", "Claude Code, Cursor, Desktop"),
          ("Web UI", "signed in with the node key")]
    for i, (t, sub) in enumerate(cl):
        y = 168 + i * 112
        s.card(60, y, 270, 90, t, [sub], tsize=16)
        s.arrow([(330, y + 45), (420, y + 45)], AMBER)
    s.label(195, 530, "API key on every request", AMBER)

    # node
    nx, ny, nw, nh = 430, 130, 700, 650
    s.rect(nx, ny, nw, nh, AMBER_SOFT, AMBER_EDGE, r=22, sw=1.6, shadow=True)
    s.pill(nx + 24, ny + 22, "hivecrawl container · Go, ~10 MB · non-root, read-only")
    s.text(nx + 24, ny + 80, "HiveCrawl node", 22, INK, 750)
    s.rect(nx + 24, ny + 100, nw - 48, 70, CARD, AMBER_EDGE, r=12)
    s.text(nx + 44, ny + 130, "API", 16, INK, 700)
    s.text(nx + 44, ny + 154, "/v2/search  /v2/scrape  /v2/crawl  /v2/map  /v2/extract  /v2/agent  /mcp  /v1/*", 13, MUTED, font=MONO)
    mods = [
        ("Search orchestrator", ["cache → local → retry only", "blocked engines → merge"]),
        ("Scraper", ["Chrome fingerprint → WARP", "→ browser → peer (opt-in)"]),
        ("Crawl & jobs", ["robots.txt, sitemaps,", "webhooks, pagination"]),
        ("AI (owner only)", ["extract, agent, json,", "summary — your model"]),
    ]
    for i, (t, rows) in enumerate(mods):
        cx = nx + 24 + (i % 2) * 336
        cy = ny + 196 + (i // 2) * 126
        s.card(cx, cy, 316, 106, t, rows, shadow=False, tsize=16)
    s.rect(nx + 24, ny + 456, nw - 48, 98, CARD, AMBER_EDGE, r=12)
    s.text(nx + 44, ny + 488, "Mesh", 16, INK, 700)
    s.lines(nx + 44, ny + 512, ["ed25519 identity + network key on every message · gossip · home-node tunnels · relay",
                                "proof of participation · fair-share limits · answer quality scores"], 13.5)
    s.text(nx + 24, ny + 610, "Settings and keys: /data (owner-only files). No shell, no plugins, never runs programs.", 13.5, MUTED, italic=True)

    # right: services
    rx = 1200
    s.text(rx, 150, "PRIVATE CONTAINERS", 12, MUTED, 700)
    svc = [("SearXNG", ["meta-search over 5 engines,", "plus WARP copies"], 168),
           ("Cloudflare WARP", ["SOCKS5 proxy, only inside", "the compose network"], 288),
           ("Headless Chrome", ["own network · nobody user ·", "read-only · no capabilities"], 408)]
    for t, rows, y in svc:
        s.card(rx, y, 340, 100, t, rows, tsize=16)
        s.arrow([(nx + nw, y + 50), (rx - 6, y + 50)], MUTED)
    s.card(rx, 548, 340, 92, "Your AI provider", ["Anthropic, OpenAI, Gemini, DeepSeek,", "OpenRouter, Ollama — you choose"],
           fill=BLUE_SOFT, stroke="#c3d4f7", tsize=16)
    s.arrow([(nx + nw, 594), (rx - 6, 594)], BLUE, dash="6 5")
    s.card(rx, 676, 340, 100, "Other nodes", ["signed searches, probes and", "(if they opt in) page fetches"],
           fill=GREEN_SOFT, stroke="#a9e0da", tsize=16)
    s.arrow([(nx + nw, 722), (rx - 6, 722)], GREEN, both=True)
    s.save("architecture", "Inside one HiveCrawl node")


# ---------------------------------------------------------------- search flow
def search_flow():
    s = SVG(1600, 700)
    heading(s, 60, 64, "How a search is answered", "Most queries finish at step 2. Only engines that were blocked get retried, and each route gets a different part.")
    y = 210
    steps = [("1  Cache", ["same query in the last hour?", "answer instantly"]),
             ("2  This node's engines", ["Google, Brave, DuckDuckGo,", "Startpage, Mojeek via SearXNG"])]
    s.card(60, 140, 230, 110, "Your query", ['"kerala monsoon forecast"'], fill=GREY_SOFT, tsize=16)
    s.arrow([(290, 195), (350, 195)], AMBER)
    s.card(360, 140, 260, 110, *steps[0], tsize=16)
    s.arrow([(620, 195), (680, 195)], AMBER)
    s.card(690, 140, 300, 110, *steps[1], tsize=16)
    s.arrow([(990, 195), (1100, 195)], GREEN, label="enough results", lx=1045, ly=180)
    s.card(1110, 140, 430, 110, "Firecrawl-shaped response", ["body identical to Firecrawl's;", "X-HC-Source, X-HC-Engines, X-HC-Blocked headers"],
           fill=GREEN_SOFT, stroke="#a9e0da", tsize=16)

    # blocked branch
    s.arrow([(840, 250), (840, 330)], RED, label="DuckDuckGo, Brave blocked", lx=840, ly=300, lcolor=RED)
    s.rect(330, 340, 1020, 250, CARD, LINE, r=18, shadow=True)
    s.text(360, 378, "3  Retry only the blocked engines — all routes at once", 17, INK, 700)
    lanes = [("Cloudflare WARP", "DuckDuckGo + Brave from a Cloudflare IP", BLUE, BLUE_SOFT),
             ("Peer: home-laptop", "DuckDuckGo from a home (residential) IP", GREEN, GREEN_SOFT),
             ("Peer: cloud-2", "Brave from another server's IP", GREEN, GREEN_SOFT)]
    for i, (t, sub, c, soft) in enumerate(lanes):
        ly = 402 + i * 60
        s.rect(360, ly, 960, 48, soft, soft, r=10, shadow=False)
        s.text(380, ly + 30, t, 15, c, 700)
        s.text(600, ly + 30, sub, 14, MUTED)
    s.arrow([(1350, 465), (1420, 465), (1420, 260)], AMBER, label="4  merge by rank (RRF), cache", lx=1420, ly=300, lcolor=AMBER)
    s.text(360, 640, "Peers answer only from their own cache and engines — a request is never forwarded again, so one query can't fan out across the network.", 14, MUTED, italic=True)
    s.save("search-flow", "How a search is answered")


# ---------------------------------------------------------------- mesh
def router(s, x, y, label):
    s.rect(x, y, 150, 40, "#eef1f5", LINE, r=10, shadow=False)
    s.text(x + 75, y + 25, label, 13, MUTED, 650, "middle")


def mesh():
    s = SVG(1600, 900)
    heading(s, 60, 64, "Home nodes only dial out", "No port forwarding, no open ports on your router. Home nodes open a tunnel up to public nodes; searches come back down it.")
    s.rect(60, 120, 1480, 290, "#eef1f5", "#dde3ea", r=24, shadow=False)
    s.text(90, 156, "INTERNET", 12, MUTED, 700)
    cx = [340, 800, 1260]  # column centres
    for x, t in zip(cx, ["Public node A", "Public node B", "Public node C"]):
        s.card(x - 120, 250, 240, 96, t, ["VPS · HTTPS"], fill=CARD, stroke=AMBER_EDGE, tsize=17)
    s.arrow([(460, 298), (680, 298)], AMBER, both=True, label="gossip", lx=570, ly=290)
    s.arrow([(920, 298), (1140, 298)], AMBER, both=True, label="gossip", lx=1030, ly=290)

    for x, t in zip(cx, ["Home node · laptop", "Home node · desktop", "Home node · Raspberry Pi"]):
        s.rect(x - 200, 520, 400, 300, "#ffffff", "#dde3ea", r=22, shadow=True)
        s.text(x - 176, 554, "HOME NETWORK", 12, MUTED, 700)
        s.rect(x - 75, 580, 150, 40, "#eef1f5", LINE, r=10)
        s.text(x, 605, "router · NAT", 13, MUTED, 650, "middle")
        s.card(x - 140, 690, 280, 100, t, ["no open ports", "keeps 2 tunnels"], fill=GREEN_SOFT, stroke="#a9e0da", tsize=16, shadow=False)
        s.add(f'<path d="M{x},690 L{x},620" stroke="{GREEN}" stroke-width="1.8" fill="none"/>')
    # tunnels: straight up to the node above, and one diagonal each
    for x in cx:
        s.arrow([(x - 20, 580), (x - 20, 352)], GREEN)
    s.arrow([(cx[0] + 30, 580), (cx[1] - 60, 352)], GREEN)
    s.arrow([(cx[1] + 30, 580), (cx[2] - 60, 352)], GREEN)
    s.arrow([(cx[2] + 30, 580), (cx[2] + 90, 352)], GREEN)
    s.label(cx[0] - 34, 470, "outbound tunnel", GREEN, "end")
    # relay: C -> A over the top, then down the laptop's tunnel
    blue = BLUE.strip("#")
    SVG.markers.add(BLUE)
    s.add(f'<path d="M1260,250 C 1260,180 340,180 340,246" fill="none" stroke="{BLUE}" stroke-width="2" stroke-dasharray="7 6" marker-end="url(#a{blue})"/>')
    s.label(800, 205, "1 · node C needs the laptop: it asks node A, which holds the laptop's tunnel", BLUE)
    s.add(f'<path d="M360,352 L360,574" fill="none" stroke="{BLUE}" stroke-width="2" stroke-dasharray="7 6" marker-end="url(#a{blue})"/>')
    s.label(372, 440, "2 · A sends it down the tunnel", BLUE, "start")
    s.text(60, 868, "Relay choice: pick two public nodes at random, tunnel to the less busy one, so thousands of home nodes spread evenly. Dead tunnels are noticed in under a minute and redialled.", 14, MUTED, italic=True)
    s.save("mesh", "Home nodes only dial out")


# ---------------------------------------------------------------- security
def security():
    s = SVG(1600, 900)
    heading(s, 60, 64, "What can reach what", "Everything that comes from the network is data. Nothing in HiveCrawl runs programs on your machine.")
    s.rect(60, 120, 1010, 720, "#ffffff", "#d5dce5", r=24, shadow=True)
    s.text(90, 158, "YOUR MACHINE", 12, MUTED, 700)
    s.text(90, 182, "Docker Compose · port bound to 127.0.0.1 only · home nodes publish no port at all", 13.5, MUTED)
    # private containers on the left
    s.card(100, 214, 430, 190, "Headless Chrome", [], tsize=18)
    s.lines(122, 274, ["✓ own network: only the node can reach it", "✓ runs as nobody, read-only, no capabilities",
                       "✓ every request a page makes is checked"], 14, INK, 32)
    s.card(100, 430, 205, 130, "WARP proxy", ["SOCKS5, inside the", "compose network only"], tsize=17)
    s.card(325, 430, 205, 130, "SearXNG", ["queries the engines", "no published port"], tsize=17)
    s.arrow([(325, 495), (311, 495)], MUTED)
    # the node on the right, next to the outside
    s.card(590, 214, 440, 346, "hivecrawl", [], fill=AMBER_SOFT, stroke=AMBER_EDGE, tsize=19)
    s.lines(612, 278, ["✓ non-root user, read-only filesystem", "✓ all Linux capabilities dropped", "✓ no shell in the image (distroless)",
                       "✓ never executes programs, no plugins", "✓ API key required for UI, API, MCP", "✓ secrets in owner-only files (/data)",
                       "✓ every outgoing address is checked"], 14.5, INK, 36)
    s.arrow([(590, 300), (536, 300)], MUTED)
    s.arrow([(590, 495), (536, 495)], MUTED)
    # refused list
    s.rect(100, 600, 930, 210, RED_SOFT, "#f6cdb8", r=16, shadow=False)
    s.text(124, 636, "Refused on every route — direct, WARP, browser, peer fetch — checked at connect time:", 15, RED, 700)
    s.lines(124, 670, ["• localhost, 127.0.0.0/8 and this machine", "• your LAN: 10/8, 172.16/12, 192.168/16 · link-local · CGNAT 100.64/10",
                       "• cloud metadata 169.254.169.254 · IPv6 private ranges", "• redirects and DNS rebinding into any of these"], 14, INK, 26)
    s.text(124, 790, "So a scraped page, a crawl or a peer can never reach your router, NAS, other containers or this machine.", 14, MUTED, italic=True)
    # outside
    s.text(1180, 158, "OUTSIDE", 12, MUTED, 700)
    s.card(1180, 190, 360, 130, "You", ["browser, SDKs and MCP clients", "with this node's API key"], tsize=17)
    s.arrow([(1180, 262), (1036, 262)], AMBER, label="full access", lx=1108, ly=250, lcolor=AMBER)
    s.card(1180, 350, 360, 260, "Other nodes in your network", [], fill=GREEN_SOFT, stroke="#a9e0da", tsize=17)
    s.lines(1202, 410, ["may send, signed with node + network key:", "• a search — answered as data", "• a probe search",
                        "• a page fetch, only if you set", "   HC_SERVE_SCRAPES=true", "cannot use your AI, settings, crawls,", "jobs or files, or run anything"], 14, INK, 25)
    s.arrow([(1180, 470), (1036, 470)], GREEN, label="data only", lx=1108, ly=458, lcolor=GREEN)
    s.card(1180, 640, 360, 170, "Everyone else", ["401 without the key", "unsigned mesh requests dropped"], fill=GREY_SOFT, tsize=17)
    s.arrow([(1180, 725), (1080, 725)], RED, label="blocked", lx=1128, ly=712, lcolor=RED)
    s.save("security", "What can reach what")


if __name__ == "__main__":
    os.makedirs(OUT, exist_ok=True)
    architecture()
    search_flow()
    mesh()
    security()
    print("ok")

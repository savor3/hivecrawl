// Package config reads node settings from HC_* environment variables.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	// Listen is the HTTP address for the API, web UI and mesh endpoints.
	Listen string
	// NodeName is a human-friendly name shown in the UI and to peers.
	NodeName string
	// PublicURL is how other nodes reach this one (e.g. https://hc1.example.com).
	// Empty means this node is behind NAT and joins the mesh via tunnels only.
	PublicURL string
	// Peers are public node URLs to connect to (gossip discovers the rest).
	Peers []string
	// NetworkKey is the shared secret every node in the mesh must hold.
	NetworkKey string
	// Network is "community", "private" (own HC_NETWORK_KEY) or "off".
	Network string
	// APIKeys protect /v1/*, /v2/*, /mcp and the web UI. If empty, a key is
	// generated once and kept in DataDir/api-key (see EnsureAPIKey).
	APIKeys []string

	SearxURL string
	// Engines are SearXNG's direct engines (must match its settings.yml).
	Engines []string
	// Warp retries blocked engines through their "<engine> warp" copies.
	Warp bool
	// WarpSocks is the WARP SOCKS5 proxy used to retry blocked scrapes.
	WarpSocks string
	// Browser is headless Chrome's DevTools address ("browser:9222");
	// BrowserTabs bounds concurrent renders.
	Browser     string
	BrowserTabs int
	// Timezone the browser reports; empty = detect from the public IP.
	Timezone string
	// Crawl limits per job, and the secret that signs webhooks.
	CrawlMaxPages, CrawlMaxConcurrency int
	WebhookSecret                      string
	// Any OpenAI-compatible chat API for extract, agent, json and summary.
	LLMBaseURL, LLMAPIKey, LLMModel string
	// LLMProvider is "anthropic" for Claude (Messages API); anything else
	// is OpenAI-compatible.
	LLMProvider string
	// UIURL is where the owner opens the web UI (for the sign-in link).
	UIURL string
	// ServeScrapes lets peers fetch pages through this node (opt-in);
	// PeerScrapes lets this node ask peers when every local route is blocked.
	ServeScrapes, PeerScrapes bool
	// MinResults: fewer local results than this triggers asking peers.
	MinResults int
	CacheTTL   time.Duration
	CacheSize  int
	// PeerRate is how many searches per minute one peer may ask of this node.
	PeerRate int
	// MaxConcurrent bounds peer searches served at once; when full,
	// high-priority (contributing) peers wait and others get 429.
	MaxConcurrent int
	// DataDir holds the node's identity key (default /data in Docker).
	DataDir string
	// Tunnels a NAT'd node keeps to public nodes; MaxTunnels a public node accepts.
	Tunnels, MaxTunnels int
	// DNS servers used for all outbound connections (peers, tunnels).
	DNS []string
	// UpdateCheck asks GitHub now and then whether a newer release is out.
	UpdateCheck bool
	// UpdateDir is shared with the updater service (status and "check now").
	UpdateDir string
}

func Load() (*Config, error) {
	host, _ := os.Hostname()
	c := &Config{
		Listen:              env("HC_LISTEN", ":3002"),
		NodeName:            env("HC_NODE_NAME", host),
		PublicURL:           strings.TrimSuffix(env("HC_PUBLIC_URL", ""), "/"),
		Peers:               list(env("HC_PEERS", "")),
		NetworkKey:          env("HC_NETWORK_KEY", ""),
		Network:             strings.ToLower(env("HC_NETWORK", "community")),
		APIKeys:             list(env("HC_API_KEYS", "")),
		SearxURL:            strings.TrimSuffix(env("HC_SEARXNG_URL", "http://127.0.0.1:8888"), "/"),
		Engines:             list(env("HC_ENGINES", "google cse,brave,duckduckgo,yandex,yahoo,naver,mwmbl,yacy,360search,gmx,fynd")),
		Warp:                env("HC_WARP", "true") == "true",
		WarpSocks:           env("HC_WARP_SOCKS", ""),
		Browser:             env("HC_BROWSER", ""),
		BrowserTabs:         envInt("HC_BROWSER_TABS", 4),
		Timezone:            env("HC_TIMEZONE", ""),
		CrawlMaxPages:       envInt("HC_CRAWL_MAX_PAGES", 5000),
		CrawlMaxConcurrency: envInt("HC_CRAWL_CONCURRENCY", 10),
		WebhookSecret:       env("HC_WEBHOOK_SECRET", ""),
		LLMBaseURL:          env("HC_LLM_BASE_URL", "https://api.openai.com/v1"),
		LLMAPIKey:           env("HC_LLM_API_KEY", ""),
		LLMModel:            env("HC_LLM_MODEL", ""),
		LLMProvider:         env("HC_LLM_PROVIDER", ""),
		UIURL:               strings.TrimSuffix(env("HC_UI_URL", ""), "/"),
		ServeScrapes:        env("HC_SERVE_SCRAPES", "false") == "true",
		PeerScrapes:         env("HC_PEER_SCRAPES", "true") == "true",
		MinResults:          envInt("HC_MIN_RESULTS", 8),
		CacheTTL:            envDuration("HC_CACHE_TTL", time.Hour),
		CacheSize:           envInt("HC_CACHE_SIZE", 5000),
		PeerRate:            envInt("HC_PEER_RATE", 30),
		MaxConcurrent:       envInt("HC_MAX_CONCURRENT", 4),
		DNS:                 list(env("HC_DNS", "1.1.1.1:53,8.8.8.8:53")),
		DataDir:             env("HC_DATA_DIR", "./data"),
		Tunnels:             envInt("HC_TUNNELS", 2),
		MaxTunnels:          envInt("HC_MAX_TUNNELS", 1000),
		UpdateCheck:         env("HC_UPDATE_CHECK", "true") == "true",
		UpdateDir:           env("HC_UPDATE_DIR", ""),
	}
	for i, p := range c.Peers {
		c.Peers[i] = strings.TrimSuffix(p, "/")
	}
	switch {
	case c.NetworkKey != "":
		c.Network = "private" // an own key always means an own network
	case c.Network == "community":
		c.NetworkKey = CommunityKey
		c.Peers = append(c.Peers, CommunityPeers...)
	case c.Network == "off":
		c.Peers = nil
	default:
		return nil, fmt.Errorf(`HC_NETWORK must be "community" or "off" (or set HC_NETWORK_KEY for a private network)`)
	}
	if len(c.Peers) > 0 && c.NetworkKey == "" {
		return nil, fmt.Errorf("HC_PEERS is set but HC_NETWORK_KEY is empty: every mesh node needs the same network key")
	}
	if c.NetworkKey != "" && len(c.NetworkKey) < 16 {
		return nil, fmt.Errorf("HC_NETWORK_KEY must be at least 16 characters")
	}
	return c, nil
}

func env(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if n, err := strconv.Atoi(env(k, "")); err == nil {
		return n
	}
	return def
}

func envDuration(k string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(env(k, "")); err == nil {
		return d
	}
	return def
}

func list(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// EnsureAPIKey makes sure the node is never open: with HC_API_KEYS empty it
// uses a key generated once and kept in DataDir/api-key (owner-only).
// created reports whether the key was just made.
func (c *Config) EnsureAPIKey() (created bool, err error) {
	if len(c.APIKeys) > 0 {
		return false, nil
	}
	p := filepath.Join(c.DataDir, "api-key")
	if b, err := os.ReadFile(p); err == nil && len(strings.TrimSpace(string(b))) > 0 {
		c.APIKeys = []string{strings.TrimSpace(string(b))}
		return false, nil
	}
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return false, err
	}
	key := "hc-" + hex.EncodeToString(buf)
	if err := os.MkdirAll(c.DataDir, 0o700); err != nil {
		return false, err
	}
	if err := os.WriteFile(p, []byte(key+"\n"), 0o600); err != nil {
		return false, err
	}
	c.APIKeys = []string{key}
	return true, nil
}

// LoginURL is a one-click link that signs a browser in to the web UI. The
// key is in the #fragment, which browsers never send to the server.
func (c *Config) LoginURL() string {
	base := c.UIURL
	if base == "" {
		base = c.PublicURL
	}
	if base == "" {
		port := os.Getenv("HC_HOST_PORT") // the published port in Docker
		if port == "" {
			port = c.Listen[strings.LastIndex(c.Listen, ":")+1:]
		}
		base = "http://localhost:" + port
	}
	if len(c.APIKeys) == 0 {
		return base + "/"
	}
	return base + "/#key=" + c.APIKeys[0]
}

// The community network is public on purpose: anyone can join it with no
// setup, so its key is not a secret. Nodes are still authenticated by their
// own ed25519 keys, take-only nodes stay on a trial allowance, and nobody can
// use another node's API or AI key. Set HC_NETWORK_KEY for a private network.
const CommunityKey = "hivecrawl-community-04e6b467233218306412c2ee2c9dce0f66c2a07bb2af1c78"

// CommunityPeers are the public nodes a new community node first contacts;
// gossip and the saved peer list (see mesh) take over from there.
var CommunityPeers = []string{
	"https://hc.168-144-156-16.sslip.io",
	"https://hc.167-172-121-155.sslip.io",
}

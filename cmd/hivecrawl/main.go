// Command hivecrawl runs one HiveCrawl node.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"hivecrawl/internal/api"
	"hivecrawl/internal/browser"
	"hivecrawl/internal/cache"
	"hivecrawl/internal/config"
	"hivecrawl/internal/llm"
	"hivecrawl/internal/mesh"
	"hivecrawl/internal/netx"
	"hivecrawl/internal/node"
	"hivecrawl/internal/scrape"
	"hivecrawl/internal/searx"
	"hivecrawl/internal/settings"
	"hivecrawl/internal/types"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	created, err := cfg.EnsureAPIKey()
	if err != nil {
		log.Fatalf("api key: %v", err)
	}
	// `hivecrawl login` prints the web UI sign-in link and key, then exits.
	if len(os.Args) > 1 && os.Args[1] == "login" {
		fmt.Printf("Open this link in your browser to sign in to the web UI:\n\n  %s\n\nOr paste this API key when the UI asks for it:\n\n  %s\n", cfg.LoginURL(), cfg.APIKeys[0])
		return
	}

	// SearXNG is usually a local container, so it uses the system resolver
	// (Docker service names). Everything that leaves the box uses public DNS.
	sx := searx.New(cfg.SearxURL, &http.Client{Timeout: 15 * time.Second})
	outDialer := netx.PublicDialer(cfg.DNS)
	outClient := netx.Client(outDialer, 15*time.Second)

	id, err := mesh.LoadIdentity(cfg.DataDir)
	if err != nil {
		log.Fatalf("identity: %v", err)
	}
	scraper := &scrape.Scraper{Direct: scrape.StdClient(scrape.Client(outDialer))}
	if cf, err := scrape.ChromeDirect(outDialer); err == nil {
		scraper.Direct = cf // Chrome TLS/HTTP2 fingerprint for plain fetches
	} else {
		log.Printf("chrome fingerprint client unavailable, using Go's: %v", err)
	}
	if cfg.Browser != "" {
		b := browser.New(cfg.Browser, cfg.BrowserTabs, outDialer.Resolver)
		b.Timezone = cfg.Timezone
		if b.Timezone == "" {
			// Match the browser's clock to where this node's IP is.
			b.Timezone = browser.DetectTimezone(context.Background(), outClient)
			log.Printf("browser timezone: %q (from public IP)", b.Timezone)
		}
		scraper.Browser = b
	}
	if cfg.Warp && cfg.WarpSocks != "" {
		if wc, err := scrape.WarpClient(cfg.WarpSocks, outDialer.Resolver); err == nil {
			scraper.Warp = wc
		} else {
			log.Printf("warp scrape fallback disabled: %v", err)
		}
	}
	n := node.New(cfg.NodeName, cache.New(cfg.CacheTTL, cfg.CacheSize), sx, cfg.MinResults)
	n.Engines, n.Warp = cfg.Engines, cfg.Warp
	n.Mesh = mesh.New(mesh.Options{
		Identity: id, Name: cfg.NodeName, Tunnels: cfg.Tunnels, MaxTunnels: cfg.MaxTunnels, PublicURL: cfg.PublicURL, Peers: cfg.Peers, Key: cfg.NetworkKey, DataDir: cfg.DataDir,
		PeerRate: cfg.PeerRate, MaxConcurrent: cfg.MaxConcurrent, Dialer: outDialer, Client: outClient, Local: n.Local,
		ProbeLocal: n.ProbeAnswer,
		Blocked:    n.BlockedEngines,
		// Opt-in: fetch pages for peers, with this node's own routes only.
		ServeScrapes: cfg.ServeScrapes,
		FetchRaw: func(ctx context.Context, u string) (*types.RawPage, error) {
			raw, err := scraper.Fetch(ctx, u)
			if err != nil {
				return nil, err
			}
			return &types.RawPage{Status: raw.Status, URL: raw.URL, ContentType: raw.ContentType, Body: raw.Body}, nil
		},
	})
	if cfg.PeerScrapes {
		scraper.Peers = func(ctx context.Context, u string) (*scrape.Raw, string, error) {
			p, name, err := n.Mesh.FetchViaPeers(ctx, u)
			if err != nil {
				return nil, "", err
			}
			return &scrape.Raw{Body: p.Body, Status: p.Status, URL: p.URL, ContentType: p.ContentType}, name, nil
		}
	}

	mux := http.NewServeMux()
	// LLM: settings saved from the web UI win over .env.
	var llmClient *llm.Client
	var llmCfg *settings.LLM
	if cfg.LLMModel != "" {
		llmCfg = &settings.LLM{Provider: cfg.LLMProvider, BaseURL: cfg.LLMBaseURL, Model: cfg.LLMModel, APIKey: cfg.LLMAPIKey}
	}
	if saved, err := settings.Load(cfg.DataDir); err != nil {
		log.Printf("settings: %v", err)
	} else if saved.LLM != nil {
		llmCfg = saved.LLM
	}
	if llmCfg != nil {
		llmClient = llm.NewFor(llmCfg.Provider, llmCfg.BaseURL, llmCfg.APIKey, llmCfg.Model)
		log.Printf("llm: %s at %s", llmCfg.Model, llmCfg.BaseURL)
	}
	(&api.Server{Node: n, Scraper: scraper, LLM: llmClient, LLMConfig: llmCfg, DataDir: cfg.DataDir, ServeScrapes: cfg.ServeScrapes, Network: cfg.Network, CrawlMaxPages: cfg.CrawlMaxPages, CrawlMaxConcurrency: cfg.CrawlMaxConcurrency,
		WebhookSecret: cfg.WebhookSecret, APIKeys: cfg.APIKeys, PublicURL: cfg.PublicURL, Started: time.Now()}).Register(mux)
	n.Mesh.Register(mux)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	n.Mesh.Start(ctx)

	srv := &http.Server{Addr: cfg.Listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()

	mode := "standalone"
	switch {
	case n.Mesh.Enabled() && cfg.PublicURL != "":
		mode = "public node at " + cfg.PublicURL
	case n.Mesh.Enabled():
		mode = "home node (tunnels to peers)"
	}
	if created {
		log.Printf("generated this node's API key (saved in %s/api-key). Sign in to the web UI with: %s", cfg.DataDir, cfg.LoginURL())
	}
	log.Printf("web UI sign-in link: run `docker compose exec hivecrawl /hivecrawl login`")
	log.Printf("node id %s", id.ID)
	log.Printf("hivecrawl %q listening on %s, %s, searxng %s, warp fallback %v", cfg.NodeName, cfg.Listen, mode, cfg.SearxURL, cfg.Warp)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

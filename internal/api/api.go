// Package api serves the Firecrawl-compatible search API (/v2/search and
// /v1/search), the status endpoint and the embedded web UI.
package api

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"hivecrawl/internal/jobs"
	"hivecrawl/internal/llm"
	"hivecrawl/internal/mesh"
	"hivecrawl/internal/node"
	"hivecrawl/internal/scrape"
	"hivecrawl/internal/settings"
	"hivecrawl/internal/types"
	"hivecrawl/internal/update"
)

//go:embed web
var webFS embed.FS

type Server struct {
	Node *node.Node
	// Scraper fetches pages for /v2/scrape and search scrapeOptions
	// (refuses internal addresses; retries blocked pages via WARP).
	Scraper *scrape.Scraper
	Jobs    *jobs.Store
	LLM     *llm.Client // initial LLM (nil: off); changeable from Settings
	// LLMConfig describes LLM for the Settings page; DataDir is where
	// settings changed in the UI are saved.
	LLMConfig *settings.LLM
	DataDir   string
	// ServeScrapes is shown in the UI (peers may fetch pages through us).
	ServeScrapes bool
	// Network is "community", "private" or "off" (shown in the UI).
	Network string
	// Crawl limits for this node and the secret that signs webhooks.
	CrawlMaxPages       int
	CrawlMaxConcurrency int
	WebhookSecret       string
	APIKeys             []string
	PublicURL           string
	Started             time.Time
	// Updates reports whether a newer release is out (nil: check is off).
	Updates *update.Checker
	// UpdateDir is the folder shared with the updater service ("": none).
	UpdateDir string

	lastCheck atomic.Int64 // unix time of the last "Check for updates"

	llmCur     atomic.Pointer[llm.Client]
	settingsMu sync.Mutex
	llmCfg     *settings.LLM
}

func (s *Server) Register(mux *http.ServeMux) {
	if s.Scraper == nil {
		s.Scraper = &scrape.Scraper{Direct: scrape.StdClient(scrape.Client(&net.Dialer{Timeout: 10 * time.Second}))}
	}
	web, _ := fs.Sub(webFS, "web")
	files := http.FileServerFS(web)
	mux.Handle("GET /", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Always revalidate so an upgraded node never serves a stale UI.
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	}))
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("POST /v2/search", s.auth(s.searchV2))
	mux.HandleFunc("POST /v1/search", s.auth(s.searchV1))
	if s.Jobs == nil {
		s.Jobs = jobs.NewStore()
	}
	s.llmCur.Store(s.LLM)
	s.llmCfg = s.LLMConfig
	mux.HandleFunc("GET /v1/settings", s.auth(s.getSettings))
	mux.HandleFunc("PUT /v1/settings/llm", s.auth(s.saveLLM))
	mux.HandleFunc("DELETE /v1/settings/llm", s.auth(s.deleteLLM))
	mux.HandleFunc("POST /v1/settings/llm/test", s.auth(s.testLLM))
	mux.HandleFunc("POST /v1/settings/llm/models", s.auth(s.listModels))
	mux.HandleFunc("POST /v2/crawl", s.auth(s.startCrawl))
	mux.HandleFunc("GET /v2/crawl/ongoing", s.auth(s.ongoingCrawls))
	mux.HandleFunc("GET /v2/crawl/active", s.auth(s.ongoingCrawls))
	mux.HandleFunc("GET /v2/crawl/{id}", s.auth(s.jobStatus("crawl", "crawl")))
	mux.HandleFunc("DELETE /v2/crawl/{id}", s.auth(s.jobCancel("crawl")))
	mux.HandleFunc("GET /v2/crawl/{id}/errors", s.auth(s.jobErrors("crawl")))
	mux.HandleFunc("POST /v2/batch/scrape", s.auth(s.startBatch))
	mux.HandleFunc("GET /v2/batch/scrape/{id}", s.auth(s.jobStatus("batch_scrape", "batch/scrape")))
	mux.HandleFunc("DELETE /v2/batch/scrape/{id}", s.auth(s.jobCancel("batch_scrape")))
	mux.HandleFunc("GET /v2/batch/scrape/{id}/errors", s.auth(s.jobErrors("batch_scrape")))
	mux.HandleFunc("POST /v2/map", s.auth(s.mapV2))
	mux.HandleFunc("POST /v2/extract", s.auth(s.startExtract))
	mux.HandleFunc("GET /v2/extract/{id}", s.auth(s.llmJobStatus("extract")))
	mux.HandleFunc("POST /v2/agent", s.auth(s.startAgent))
	mux.HandleFunc("GET /v2/agent/{id}", s.auth(s.llmJobStatus("agent")))
	mux.HandleFunc("DELETE /v2/agent/{id}", s.auth(s.jobCancel("agent")))
	mux.HandleFunc("POST /v1/map", s.auth(s.mapV1))
	mux.HandleFunc("POST /v2/scrape", s.auth(s.scrapeV2))
	mux.HandleFunc("POST /v1/scrape", s.auth(s.scrapeV2))
	mux.HandleFunc("GET /v1/status", s.auth(s.handleStatus))
	mux.HandleFunc("POST /v1/update/check", s.auth(s.checkUpdate))
	mux.HandleFunc("PUT /v1/update/auto", s.auth(s.setAutoUpdate))
	mux.HandleFunc("GET /v1/config", s.handleConfig)
	mcpH := s.auth(s.mcpHandler().ServeHTTP)
	mux.HandleFunc("POST /mcp", mcpH)
	mux.HandleFunc("GET /mcp", mcpH)
	mux.HandleFunc("DELETE /mcp", mcpH)
}

func (s *Server) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if len(s.APIKeys) == 0 {
			h(w, r)
			return
		}
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		for _, k := range s.APIKeys {
			if subtle.ConstantTimeCompare([]byte(got), []byte(k)) == 1 {
				h(w, r)
				return
			}
		}
		writeJSON(w, http.StatusUnauthorized, fcError{Error: "Unauthorized: Invalid token"})
	}
}

// results holds one search's groups, by Firecrawl source.
type results struct {
	web    []fcWebResult
	images []fcImageResult
	news   []fcNewsResult
	meta   types.Meta // how web results (or the first group) were answered
	// retry: nothing came back because routes were blocked or rate
	// limited (see types.Meta.Retry).
	retry bool
}

func (r results) empty() bool { return len(r.web)+len(r.images)+len(r.news) == 0 }

// retryAfter is what a client is told to wait when no route could answer.
const retryAfter = 20

// run decodes a Firecrawl body, searches, and sets the X-HC-* headers.
// ok=false means an error response was already written.
func (s *Server) run(w http.ResponseWriter, r *http.Request) (parsed, results, bool) {
	var in fcSearchRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, fcError{Error: "Bad Request: invalid JSON body"})
		return parsed{}, results{}, false
	}
	p, err := parseFirecrawl(in)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, fcError{Error: "Bad Request: " + err.Error()})
		return p, results{}, false
	}
	res, err := s.searchAll(r.Context(), p)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, fcError{Error: err.Error()})
		return p, results{}, false
	}
	h := w.Header()
	h.Set("X-HC-Node", s.Node.Name)
	h.Set("X-HC-Source", res.meta.Source)
	h.Set("X-HC-Took-Ms", strconv.FormatInt(res.meta.TookMs, 10))
	if len(res.meta.Engines) > 0 {
		h.Set("X-HC-Engines", strings.Join(res.meta.Engines, ","))
	}
	if len(res.meta.Blocked) > 0 {
		h.Set("X-HC-Blocked", strings.Join(res.meta.Blocked, ","))
	}
	if res.retry && res.empty() {
		// Like Firecrawl's own rate limit answer: tell the client to come
		// back, instead of a success that looks like "no results".
		h.Set("Retry-After", strconv.Itoa(retryAfter))
		writeJSON(w, http.StatusTooManyRequests, fcError{Error: fmt.Sprintf(
			"Temporarily unable to search: every engine and peer is blocked or rate limited right now. Retry after %d seconds.", retryAfter)})
		return p, res, false
	}
	return p, res, true
}

// searchAll runs every requested source (web, images, news) in parallel.
// Shared by the REST API and MCP.
func (s *Server) searchAll(ctx context.Context, p parsed) (results, error) {
	if p.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(p.timeout)*time.Millisecond)
		defer cancel()
	}
	limit := p.req.Limit
	if limit == 0 {
		limit = 10
	}
	req := p.req
	// Ask for extra results when filtering, so filters do not starve the page.
	if len(p.include)+len(p.exclude) > 0 || p.validURL {
		req.Limit = min(limit*2, 100)
	}

	var (
		out  results
		mu   sync.Mutex
		errs []string
		wg   sync.WaitGroup
	)
	for _, src := range p.sources {
		wg.Go(func() {
			r := req
			if src != "web" {
				r.Category = src
			}
			if src == "web" && len(p.topics) > 0 {
				web, meta, err := s.searchTopics(ctx, p, r, limit)
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					errs = append(errs, "web: "+err.Error())
					return
				}
				out.web, out.meta = web, meta
				return
			}
			resp := s.Node.Search(ctx, r)
			mu.Lock()
			defer mu.Unlock()
			if !resp.Success {
				errs = append(errs, src+": "+resp.Error)
				return
			}
			if src == "web" || out.meta.Source == "" {
				out.meta = resp.Meta
			}
			out.retry = out.retry || resp.Meta.Retry
			switch src {
			case "web":
				out.web = p.toWeb(resp.Data.Web, limit)
			case "images":
				out.images = p.toImages(resp.Data.Web, limit)
			case "news":
				out.news = p.toNews(resp.Data.Web, limit)
			}
		})
	}
	wg.Wait()
	if len(errs) == len(p.sources) {
		return out, errors.New(strings.Join(errs, "; "))
	}
	if p.scrape != nil && len(out.web) > 0 {
		s.scrapeResults(ctx, out.web, *p.scrape)
	}
	return out, nil
}

// searchTopics runs each requested category at once and interleaves their
// results, each tagged with its category, as Firecrawl does.
func (s *Server) searchTopics(ctx context.Context, p parsed, req types.SearchRequest, limit int) ([]fcWebResult, types.Meta, error) {
	lists := make([][]fcWebResult, len(p.topics))
	resps := make([]types.SearchResponse, len(p.topics))
	var wg sync.WaitGroup
	for i, t := range p.topics {
		wg.Go(func() {
			resps[i] = s.Node.Topic(ctx, req, t)
			lists[i] = p.toWeb(resps[i].Data.Web, limit)
			for j := range lists[i] {
				lists[i][j].Category = t
			}
		})
	}
	wg.Wait()

	var meta types.Meta
	var errs, sources []string
	for i, r := range resps {
		if !r.Success {
			errs = append(errs, p.topics[i]+": "+r.Error)
			continue
		}
		if !slices.Contains(sources, r.Meta.Source) {
			sources = append(sources, r.Meta.Source)
		}
		meta.Engines = union(meta.Engines, r.Meta.Engines)
		meta.Blocked = union(meta.Blocked, r.Meta.Blocked)
		meta.TookMs = max(meta.TookMs, r.Meta.TookMs)
		meta.Retry = meta.Retry || r.Meta.Retry
	}
	if len(errs) == len(resps) {
		return nil, meta, errors.New(strings.Join(errs, "; "))
	}
	meta.Source = strings.Join(sources, "+")

	out := []fcWebResult{}
	seen := map[string]bool{}
	for i := 0; len(out) < limit; i++ {
		more := false
		for _, l := range lists {
			if i >= len(l) {
				continue
			}
			more = true
			if r := l[i]; !seen[r.URL] && len(out) < limit {
				seen[r.URL] = true
				r.Position = len(out) + 1
				out = append(out, r)
			}
		}
		if !more {
			break
		}
	}
	return out, meta, nil
}

func union(a, b []string) []string {
	out := slices.Clone(a)
	for _, x := range b {
		if !slices.Contains(out, x) {
			out = append(out, x)
		}
	}
	return out
}

func (s *Server) searchV2(w http.ResponseWriter, r *http.Request) {
	p, res, ok := s.run(w, r)
	if !ok {
		return
	}
	out := fcV2Response{Success: true, ID: newID(), Warning: strings.Join(p.warnings, "; ")}
	for _, src := range p.sources {
		switch src {
		case "web":
			out.Data.Web = nonNil(res.web)
		case "images":
			out.Data.Images = nonNil(res.images)
		case "news":
			out.Data.News = nonNil(res.news)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) searchV1(w http.ResponseWriter, r *http.Request) {
	p, res, ok := s.run(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, fcV1Response{Success: true, Data: nonNil(res.web), Warning: strings.Join(p.warnings, "; ")})
}

func nonNil[T any](xs []T) []T {
	if xs == nil {
		return []T{}
	}
	return xs
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	mode, id, tunnels := "standalone", "", 0
	var peers any = []any{}
	res := mesh.HostResources()
	res.Engines = s.Node.EngineCount(r.Context())
	if s.Node.Mesh != nil && s.Node.Mesh.Enabled() {
		mode = "home (behind NAT, tunnels out)"
		if s.PublicURL != "" {
			mode = "public"
		}
		peers, id, tunnels = s.Node.Mesh.Peers(), s.Node.Mesh.ID(), s.Node.Mesh.TunnelCount()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"node":         s.Node.Name,
		"id":           id,
		"version":      Version,
		"engines":      s.Node.Engines,
		"engineGroups": s.Node.EngineGroups(r.Context()),
		"res":          res,
		"warp":         s.Node.Warp,
		"tunnels":      tunnels,
		"llm":          s.llmName(),
		"browser":      s.Scraper != nil && s.Scraper.Browser != nil,
		"serveScrapes": s.ServeScrapes,
		"network":      s.Network,
		"mode":         mode,
		"publicUrl":    s.PublicURL,
		"uptimeSec":    int(time.Since(s.Started).Seconds()),
		"searxng":      s.Node.SearxStatus(),
		"cache":        s.Node.CacheStats(),
		"peers":        peers,
		"update":       s.updateStatus(),
	})
}

// handleConfig tells the web UI whether it must ask for an API key.
func (s *Server) handleConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"node": s.Node.Name, "keyRequired": len(s.APIKeys) > 0, "llm": s.llmName() != ""})
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// Version is reported to MCP clients and compared with the newest release;
// release builds set it with -ldflags.
var Version = "0.4.5"

func (s *Server) updateStatus() update.Status {
	st := update.Status{Current: Version, Error: "update check is off (HC_UPDATE_CHECK=false)"}
	if s.Updates != nil {
		st = s.Updates.Status()
	}
	st.Updater = update.ReadUpdater(s.UpdateDir)
	return st
}

// setAutoUpdate is the auto-update switch in Settings: {"enabled": bool}.
func (s *Server) setAutoUpdate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<10)).Decode(&req); err != nil || req.Enabled == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": `send {"enabled": true} or {"enabled": false}`})
		return
	}
	if update.ReadUpdater(s.UpdateDir) == nil {
		writeJSON(w, http.StatusConflict, map[string]any{"success": false, "error": "no updater is running for this node"})
		return
	}
	if err := update.SetAuto(s.UpdateDir, *req.Enabled); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "update": s.updateStatus()})
}

// checkUpdate is the "Check for updates" button: it asks GitHub now and tells
// the updater service to check (and install, when auto-update is on).
func (s *Server) checkUpdate(w http.ResponseWriter, r *http.Request) {
	now := time.Now().Unix()
	if last := s.lastCheck.Load(); now-last < 15 {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"success": false, "error": "checked a moment ago; try again in a few seconds"})
		return
	}
	s.lastCheck.Store(now)
	if s.Updates != nil {
		s.Updates.Check(r.Context())
	}
	if update.ReadUpdater(s.UpdateDir) != nil {
		if err := update.RequestCheck(s.UpdateDir); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": err.Error()})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "update": s.updateStatus()})
}

// llmName is the configured model, or "" when there is no LLM.
func (s *Server) llmName() string {
	if s.llm() == nil {
		return ""
	}
	return s.llm().Model
}

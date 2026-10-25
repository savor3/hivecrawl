package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"hivecrawl/internal/crawl"
	"hivecrawl/internal/jobs"
	"hivecrawl/internal/scrape"
	"hivecrawl/internal/types"
)

// Crawl, batch scrape and map, in Firecrawl's v2 shapes.

type fcWebhook struct {
	URL      string            `json:"url"`
	Headers  map[string]string `json:"headers"`
	Metadata map[string]string `json:"metadata"`
	Events   []string          `json:"events"`
}

// parseWebhook accepts a URL string or an object, like Firecrawl.
func parseWebhook(raw json.RawMessage) (*fcWebhook, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var u string
	if json.Unmarshal(raw, &u) == nil {
		raw, _ = json.Marshal(map[string]string{"url": u})
	}
	var w fcWebhook
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil, errors.New("invalid webhook")
	}
	if pu, err := url.Parse(w.URL); err != nil || (pu.Scheme != "http" && pu.Scheme != "https") {
		return nil, errors.New("webhook url must be http(s)")
	}
	if len(w.Events) == 0 {
		w.Events = []string{"started", "page", "completed", "failed"}
	}
	return &w, nil
}

// send posts a Firecrawl-style webhook event (best effort, in the background).
func (s *Server) send(w *fcWebhook, kind, event, id string, success bool, data any, errMsg string) {
	if w == nil || !contains(w.Events, event) {
		return
	}
	if data == nil {
		data = []any{}
	}
	payload := map[string]any{"success": success, "type": kind + "." + event, "id": id, "webhookId": jobs.NewID(), "data": data}
	if errMsg != "" {
		payload["error"] = errMsg
	}
	if len(w.Metadata) > 0 {
		payload["metadata"] = w.Metadata
	}
	body, _ := json.Marshal(payload)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		for k, v := range w.Headers {
			req.Header.Set(k, v)
		}
		if s.WebhookSecret != "" {
			m := hmac.New(sha256.New, []byte(s.WebhookSecret))
			m.Write(body)
			req.Header.Set("X-Firecrawl-Signature", "sha256="+hex.EncodeToString(m.Sum(nil)))
		}
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
}

// baseURL is how clients reach this API (for job status URLs).
func (s *Server) baseURL(r *http.Request) string {
	if s.PublicURL != "" {
		return s.PublicURL
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, fcError{Error: "Bad Request: invalid JSON body"})
		return false
	}
	return true
}

func badRequest(w http.ResponseWriter, msg string) {
	writeJSON(w, http.StatusBadRequest, fcError{Error: "Bad Request: " + msg})
}

// ---- crawl ----

type fcCrawlRequest struct {
	URL                    string           `json:"url"`
	IncludePaths           []string         `json:"includePaths"`
	ExcludePaths           []string         `json:"excludePaths"`
	MaxDiscoveryDepth      *int             `json:"maxDiscoveryDepth"`
	Limit                  int              `json:"limit"`
	CrawlEntireDomain      bool             `json:"crawlEntireDomain"`
	AllowExternalLinks     bool             `json:"allowExternalLinks"`
	AllowSubdomains        bool             `json:"allowSubdomains"`
	IgnoreRobotsTxt        bool             `json:"ignoreRobotsTxt"`
	RobotsUserAgent        string           `json:"robotsUserAgent"`
	Sitemap                string           `json:"sitemap"`
	DeduplicateSimilarURLs *bool            `json:"deduplicateSimilarURLs"`
	IgnoreQueryParameters  bool             `json:"ignoreQueryParameters"`
	RegexOnFullURL         bool             `json:"regexOnFullURL"`
	Delay                  float64          `json:"delay"`
	MaxConcurrency         int              `json:"maxConcurrency"`
	ScrapeOptions          *fcScrapeOptions `json:"scrapeOptions"`
	Webhook                json.RawMessage  `json:"webhook"`
	Prompt                 string           `json:"prompt"`
}

func (s *Server) crawlLimits() (pages, concurrency int) {
	pages, concurrency = s.CrawlMaxPages, s.CrawlMaxConcurrency
	if pages <= 0 {
		pages = 5000
	}
	if concurrency <= 0 {
		concurrency = 10
	}
	return
}

func (s *Server) startCrawl(w http.ResponseWriter, r *http.Request) {
	var in fcCrawlRequest
	if !decode(w, r, &in) {
		return
	}
	job, err := s.newCrawl(in)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	resp := map[string]any{"success": true, "id": job.ID, "url": s.baseURL(r) + "/v2/crawl/" + job.ID}
	if in.Prompt != "" {
		resp["warning"] = "prompt is not supported: set crawl options explicitly"
	}
	writeJSON(w, http.StatusOK, resp)
}

// newCrawl validates a crawl request and starts it in the background.
func (s *Server) newCrawl(in fcCrawlRequest) (*jobs.Job, error) {
	if u, err := url.Parse(in.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("url must be an absolute http(s) URL")
	}
	switch in.Sitemap {
	case "", "include", "skip", "only":
	default:
		return nil, errors.New(`sitemap must be "include", "skip" or "only"`)
	}
	if in.Delay < 0 || in.Delay > 60 {
		return nil, errors.New("delay is in seconds and cannot exceed 60")
	}
	maxPages, maxConc := s.crawlLimits()
	o := crawl.Options{
		IncludePaths: in.IncludePaths, ExcludePaths: in.ExcludePaths, MaxDiscoveryDepth: -1,
		Limit: in.Limit, CrawlEntireDomain: in.CrawlEntireDomain, AllowExternalLinks: in.AllowExternalLinks,
		AllowSubdomains: in.AllowSubdomains, IgnoreRobotsTxt: in.IgnoreRobotsTxt, RobotsUserAgent: in.RobotsUserAgent,
		Sitemap: in.Sitemap, DeduplicateSimilarURLs: true, IgnoreQueryParameters: in.IgnoreQueryParameters,
		RegexOnFullURL: in.RegexOnFullURL, Delay: time.Duration(in.Delay * float64(time.Second)),
		MaxConcurrency: min(max(in.MaxConcurrency, 0), maxConc),
	}
	if in.MaxDiscoveryDepth != nil {
		o.MaxDiscoveryDepth = *in.MaxDiscoveryDepth
	}
	if in.DeduplicateSimilarURLs != nil {
		o.DeduplicateSimilarURLs = *in.DeduplicateSimilarURLs
	}
	if o.Limit <= 0 || o.Limit > maxPages {
		o.Limit = min(max(o.Limit, 0), maxPages)
		if o.Limit == 0 {
			o.Limit = maxPages
		}
	}
	sp := scrapeSpec{opts: scrape.Options{Formats: []string{"markdown"}, OnlyMainContent: true}}
	if in.ScrapeOptions != nil {
		var err error
		if sp, err = in.ScrapeOptions.toOptions(); err != nil {
			return nil, err
		}
	}
	o.Scrape = sp.opts
	// Validate patterns now so the client gets a 400, not a failed job.
	if err := crawl.Validate(in.URL, o); err != nil {
		return nil, err
	}
	hook, err := parseWebhook(in.Webhook)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	job := s.Jobs.Create("crawl", in.URL, in, cancel)
	go func() {
		defer cancel()
		s.send(hook, "crawl", "started", job.ID, true, nil, "")
		err := crawl.Run(ctx, s.Scraper, in.URL, o, crawl.Callbacks{
			Page: func(d *scrape.Document) {
				if sp.json != nil || sp.summary {
					s.applyLLMFormats(ctx, d, sp)
				}
				d.Metadata.ScrapeID = jobs.NewID()
				job.AddDoc(d)
				s.send(hook, "crawl", "page", job.ID, true, []any{d}, "")
			},
			Error:         func(u string, err error) { code, msg := errInfo(err); job.AddError(u, code, msg) },
			RobotsBlocked: job.AddRobotsBlocked,
			Discovered:    job.SetTotal,
		})
		if err != nil && !errors.Is(err, context.Canceled) {
			job.Finish(jobs.Failed, err.Error())
			s.send(hook, "crawl", "failed", job.ID, false, nil, err.Error())
			return
		}
		job.Finish(jobs.Completed, "")
		s.send(hook, "crawl", "completed", job.ID, true, nil, "")
	}()
	return job, nil
}

func errInfo(err error) (string, string) {
	var se *scrape.Error
	if errors.As(err, &se) {
		return se.Code, se.Msg
	}
	return "", err.Error()
}

// jobStatus serves GET /v2/{crawl|batch/scrape}/{id}.
func (s *Server) jobStatus(kind, path string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		job, ok := s.Jobs.Get(r.PathValue("id"), kind)
		if !ok {
			writeJSON(w, http.StatusNotFound, fcError{Error: "Job not found"})
			return
		}
		skip, _ := strconv.Atoi(r.URL.Query().Get("skip"))
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		v := job.View(skip, limit)
		docs := v.Docs
		if docs == nil {
			docs = []*scrape.Document{}
		}
		out := map[string]any{
			"success": v.Status != jobs.Failed, "status": v.Status, "completed": v.Completed, "total": v.Total,
			"creditsUsed": 0, "expiresAt": v.ExpiresAt.UTC().Format(time.RFC3339), "createdAt": v.Created.UTC().Format(time.RFC3339),
			"data": docs,
		}
		if !v.Finished.IsZero() {
			out["completedAt"] = v.Finished.UTC().Format(time.RFC3339)
			out["duration"] = v.Finished.Sub(v.Created).Milliseconds()
		}
		if v.Next > 0 {
			next := s.baseURL(r) + "/v2/" + path + "/" + job.ID + "?skip=" + strconv.Itoa(v.Next)
			if limit > 0 {
				next += "&limit=" + strconv.Itoa(limit)
			}
			out["next"] = next
		}
		if v.Err != "" {
			out["error"] = v.Err
		}
		if len(v.RobotsBlocked) > 0 && v.Status != jobs.Scraping {
			out["warning"] = "One or more pages could not be crawled because the site's robots.txt disallows them. See the robotsBlocked list on GET /v2/" + path + "/" + job.ID + "/errors. Set ignoreRobotsTxt: true to crawl them."
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func (s *Server) jobCancel(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		job, ok := s.Jobs.Get(r.PathValue("id"), kind)
		if !ok {
			writeJSON(w, http.StatusNotFound, fcError{Error: "Job not found"})
			return
		}
		job.Cancel()
		writeJSON(w, http.StatusOK, map[string]any{"success": true, "status": "cancelled"})
	}
}

func (s *Server) jobErrors(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		job, ok := s.Jobs.Get(r.PathValue("id"), kind)
		if !ok {
			writeJSON(w, http.StatusNotFound, fcError{Error: "Job not found"})
			return
		}
		v := job.View(0, 1)
		errs, blocked := v.Errors, v.RobotsBlocked
		if errs == nil {
			errs = []jobs.Error{}
		}
		if blocked == nil {
			blocked = []string{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"errors": errs, "robotsBlocked": blocked})
	}
}

func (s *Server) ongoingCrawls(w http.ResponseWriter, _ *http.Request) {
	crawls := []map[string]any{}
	for _, j := range s.Jobs.Running("crawl") {
		crawls = append(crawls, map[string]any{"id": j.ID, "teamId": s.Node.Name, "url": j.URL,
			"created_at": j.Created.UTC().Format(time.RFC3339), "options": j.Options})
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "crawls": crawls})
}

// ---- batch scrape ----

type fcBatchRequest struct {
	URLs              []string        `json:"urls"`
	IgnoreInvalidURLs *bool           `json:"ignoreInvalidURLs"`
	MaxConcurrency    int             `json:"maxConcurrency"`
	Webhook           json.RawMessage `json:"webhook"`
	fcScrapeOptions
}

func (s *Server) startBatch(w http.ResponseWriter, r *http.Request) {
	var in fcBatchRequest
	if !decode(w, r, &in) {
		return
	}
	if len(in.URLs) == 0 {
		badRequest(w, "urls must contain at least one URL")
		return
	}
	maxPages, maxConc := s.crawlLimits()
	if len(in.URLs) > maxPages {
		badRequest(w, "too many urls (this node allows "+strconv.Itoa(maxPages)+")")
		return
	}
	ignoreInvalid := in.IgnoreInvalidURLs == nil || *in.IgnoreInvalidURLs
	var valid, invalid []string
	for _, raw := range in.URLs {
		if u, err := url.Parse(strings.TrimSpace(raw)); err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" {
			valid = append(valid, u.String())
		} else {
			invalid = append(invalid, raw)
		}
	}
	if len(invalid) > 0 && !ignoreInvalid {
		badRequest(w, "invalid URLs: "+strings.Join(invalid, ", "))
		return
	}
	sp, err := in.fcScrapeOptions.toOptions()
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	hook, err := parseWebhook(in.Webhook)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	conc := in.MaxConcurrency
	if conc <= 0 || conc > maxConc {
		conc = min(5, maxConc)
	}

	ctx, cancel := context.WithCancel(context.Background())
	job := s.Jobs.Create("batch_scrape", "", in, cancel)
	job.SetTotal(len(valid))
	go func() {
		defer cancel()
		s.send(hook, "batch_scrape", "started", job.ID, true, nil, "")
		var wg sync.WaitGroup
		sem := make(chan struct{}, conc)
	loop:
		for _, u := range valid {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				break loop
			}
			wg.Go(func() {
				defer func() { <-sem }()
				doc, err := s.scrapeOne(ctx, u, sp)
				if err != nil {
					code, msg := errInfo(err)
					job.AddError(u, code, msg)
					return
				}
				job.AddDoc(doc)
				s.send(hook, "batch_scrape", "page", job.ID, true, []any{doc}, "")
			})
		}
		wg.Wait()
		job.Finish(jobs.Completed, "")
		s.send(hook, "batch_scrape", "completed", job.ID, true, nil, "")
	}()
	if invalid == nil {
		invalid = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "id": job.ID, "url": s.baseURL(r) + "/v2/batch/scrape/" + job.ID, "invalidURLs": invalid})
}

// ---- map ----

type fcMapRequest struct {
	URL                   string   `json:"url"`
	Search                string   `json:"search"`
	Sitemap               string   `json:"sitemap"`
	IncludeSubdomains     *bool    `json:"includeSubdomains"`
	IgnoreQueryParameters *bool    `json:"ignoreQueryParameters"`
	IgnoreSitemap         bool     `json:"ignoreSitemap"` // v1
	SitemapOnly           bool     `json:"sitemapOnly"`   // v1
	Limit                 int      `json:"limit"`
	Timeout               int      `json:"timeout"`
	IncludePaths          []string `json:"includePaths"`
	ExcludePaths          []string `json:"excludePaths"`
}

func (s *Server) doMap(ctx context.Context, in fcMapRequest) ([]crawl.Link, error) {
	o := crawl.MapOptions{Search: in.Search, Sitemap: in.Sitemap, IncludeSubdomains: true, IgnoreQueryParameters: true,
		Limit: in.Limit, IncludePaths: in.IncludePaths, ExcludePaths: in.ExcludePaths}
	if in.IncludeSubdomains != nil {
		o.IncludeSubdomains = *in.IncludeSubdomains
	}
	if in.IgnoreQueryParameters != nil {
		o.IgnoreQueryParameters = *in.IgnoreQueryParameters
	}
	if in.IgnoreSitemap {
		o.Sitemap = "skip"
	}
	if in.SitemapOnly {
		o.Sitemap = "only"
	}
	if o.Limit > 100000 {
		return nil, errors.New("limit cannot exceed 100000")
	}
	if in.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(in.Timeout)*time.Millisecond)
		defer cancel()
	}
	search := func(ctx context.Context, q string, limit int) ([]crawl.Link, error) {
		resp := s.Node.Search(ctx, types.SearchRequest{Query: q, Limit: limit})
		if !resp.Success {
			return nil, errors.New(resp.Error)
		}
		out := make([]crawl.Link, 0, len(resp.Data.Web))
		for _, x := range resp.Data.Web {
			out = append(out, crawl.Link{URL: x.URL, Title: x.Title, Description: x.Description})
		}
		return out, nil
	}
	return crawl.Map(ctx, s.Scraper, search, in.URL, o)
}

func (s *Server) mapV2(w http.ResponseWriter, r *http.Request) {
	var in fcMapRequest
	if !decode(w, r, &in) {
		return
	}
	links, err := s.doMap(r.Context(), in)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "id": jobs.NewID(), "links": links})
}

// mapV1 returns links as plain strings, like Firecrawl v1.
func (s *Server) mapV1(w http.ResponseWriter, r *http.Request) {
	var in fcMapRequest
	if !decode(w, r, &in) {
		return
	}
	links, err := s.doMap(r.Context(), in)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	out := make([]string, len(links))
	for i, l := range links {
		out[i] = l.URL
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "links": out})
}

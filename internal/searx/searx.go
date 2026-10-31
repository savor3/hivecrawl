// Package searx queries a SearXNG instance's JSON API.
package searx

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"hivecrawl/internal/types"
)

type Client struct {
	base string
	http *http.Client

	mu      sync.Mutex
	blocked map[string]string // engine -> reason it was blocked in its most recent query
	lastOK  time.Time
	lastErr string
	known   map[string]bool // every engine this SearXNG has, enabled or not
	catalog []Engine
	knownAt time.Time
}

// Engine is one engine as SearXNG's /config lists it.
type Engine struct {
	Name       string   `json:"name"`
	Enabled    bool     `json:"enabled"`
	Categories []string `json:"categories"`
}

// knownTTL is how long the engine list from /config is trusted.
const knownTTL = 10 * time.Minute

func New(base string, c *http.Client) *Client {
	return &Client{base: base, http: c, blocked: map[string]string{}}
}

type searxResponse struct {
	Results []struct {
		URL           string   `json:"url"`
		Title         string   `json:"title"`
		Content       string   `json:"content"`
		Engines       []string `json:"engines"`
		PublishedDate string   `json:"publishedDate"`
		ImgSrc        string   `json:"img_src"`
		Thumbnail     string   `json:"thumbnail"`
		ThumbnailSrc  string   `json:"thumbnail_src"`
		Resolution    string   `json:"resolution"`
	} `json:"results"`
	// Each entry is [engine, reason].
	Unresponsive [][]string `json:"unresponsive_engines"`
}

// Outcome is one SearXNG answer. Results are in SearXNG's ranked order.
type Outcome struct {
	Results []types.Result
	Engines []string
	Blocked []string
}

func (c *Client) Search(ctx context.Context, req types.SearchRequest) (Outcome, error) {
	if len(req.Engines) > 0 {
		// SearXNG ignores engine names it does not have, and when none are
		// left it runs the whole category instead. A peer or WARP request
		// for engines only newer nodes have must not turn into that.
		req.Engines = c.Have(ctx, req.Engines)
		if len(req.Engines) == 0 {
			return Outcome{}, nil
		}
	}
	q := url.Values{}
	q.Set("q", req.Query)
	q.Set("format", "json")
	q.Set("language", req.Lang)
	q.Set("safesearch", "0")
	if req.TimeRange != "" {
		q.Set("time_range", req.TimeRange)
	}
	if len(req.Engines) > 0 {
		q.Set("engines", strings.Join(req.Engines, ","))
	}
	if req.Category != "" {
		q.Set("categories", req.Category)
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/search?"+q.Encode(), nil)
	if err != nil {
		return Outcome{}, err
	}
	resp, err := c.http.Do(r)
	if err != nil {
		c.fail(err.Error())
		return Outcome{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		err := fmt.Errorf("searxng status %d: %s", resp.StatusCode, b)
		c.fail(err.Error())
		return Outcome{}, err
	}
	var sr searxResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&sr); err != nil {
		c.fail(err.Error())
		return Outcome{}, fmt.Errorf("decode searxng: %w", err)
	}

	var out Outcome
	seen := map[string]bool{}
	for i, x := range sr.Results {
		res := types.Result{
			URL: x.URL, Title: x.Title, Description: x.Content, Position: i + 1, Engines: x.Engines,
			Date: normalizeDate(x.PublishedDate),
		}
		switch {
		case x.ImgSrc != "":
			res.ImageURL = x.ImgSrc
			res.ImageWidth, res.ImageHeight = parseResolution(x.Resolution)
		case x.Thumbnail != "":
			res.ImageURL = x.Thumbnail
		case x.ThumbnailSrc != "":
			res.ImageURL = x.ThumbnailSrc
		}
		out.Results = append(out.Results, res)
		for _, e := range x.Engines {
			if !seen[e] {
				seen[e] = true
				out.Engines = append(out.Engines, e)
			}
		}
	}
	blocked := map[string]string{}
	for _, u := range sr.Unresponsive {
		if len(u) >= 2 {
			blocked[u[0]] = u[1]
			out.Blocked = append(out.Blocked, u[0])
		}
	}
	c.mu.Lock()
	if len(req.Engines) == 0 {
		c.blocked = blocked
	} else {
		// A query for specific engines only updates those engines' status.
		for _, e := range req.Engines {
			if reason, ok := blocked[e]; ok {
				c.blocked[e] = reason
			} else {
				delete(c.blocked, e)
			}
		}
	}
	c.lastOK = time.Now()
	c.lastErr = ""
	c.mu.Unlock()
	return out, nil
}

// Have returns the engines from names that this SearXNG has. If its engine
// list cannot be read, names are returned unchanged.
func (c *Client) Have(ctx context.Context, names []string) []string {
	known := c.engines(ctx)
	if known == nil {
		return names
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		if known[n] {
			out = append(out, n)
		}
	}
	return out
}

func (c *Client) engines(ctx context.Context) map[string]bool {
	c.mu.Lock()
	known, fresh := c.known, time.Since(c.knownAt) < knownTTL
	c.mu.Unlock()
	if known != nil && fresh {
		return known
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/config", nil)
	if err != nil {
		return known
	}
	resp, err := c.http.Do(r)
	if err != nil {
		return known
	}
	defer resp.Body.Close()
	var cfg struct {
		Engines []Engine `json:"engines"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&cfg) != nil || len(cfg.Engines) == 0 {
		return known
	}
	known = make(map[string]bool, len(cfg.Engines))
	for _, e := range cfg.Engines {
		known[e.Name] = true
	}
	c.mu.Lock()
	c.known, c.catalog, c.knownAt = known, cfg.Engines, time.Now()
	c.mu.Unlock()
	return known
}

// Catalog lists every engine this SearXNG has (nil if unknown yet).
func (c *Client) Catalog(ctx context.Context) []Engine {
	c.engines(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.catalog
}

func (c *Client) fail(msg string) {
	c.mu.Lock()
	c.lastErr = msg
	c.mu.Unlock()
}

// Status is shown on /v1/status.
type Status struct {
	URL     string            `json:"url"`
	LastOK  time.Time         `json:"lastOk,omitzero"`
	LastErr string            `json:"lastError,omitempty"`
	Blocked map[string]string `json:"blocked"`
}

func (c *Client) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	b := make(map[string]string, len(c.blocked))
	for k, v := range c.blocked {
		b[k] = v
	}
	return Status{URL: c.base, LastOK: c.lastOK, LastErr: c.lastErr, Blocked: b}
}

// normalizeDate turns SearXNG's publishedDate into RFC 3339 ("" if unknown).
func normalizeDate(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || strings.HasPrefix(s, "0001") {
		return ""
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02T15:04:05.999999", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC().Format(time.RFC3339)
		}
	}
	return ""
}

// parseResolution reads "1920 x 1080" or "1920x1080".
func parseResolution(s string) (int, int) {
	var w, h int
	s = strings.ReplaceAll(strings.ReplaceAll(s, " ", ""), "×", "x")
	if _, err := fmt.Sscanf(s, "%dx%d", &w, &h); err != nil {
		return 0, 0
	}
	return w, h
}

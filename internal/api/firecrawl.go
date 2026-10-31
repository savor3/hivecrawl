package api

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"hivecrawl/internal/node"
	"hivecrawl/internal/scrape"
	"hivecrawl/internal/types"
)

// The public search API mirrors Firecrawl's /v2/search (and /v1/search)
// request and response bodies, so Firecrawl SDKs and clients work unchanged.
// HiveCrawl-specific details go in X-HC-* response headers, never the body.

// fcSearchRequest is Firecrawl's v2 search body. Fields Firecrawl accepts
// that only make sense for its hosted scraper (scrapeOptions, enterprise…)
// are accepted and ignored with a warning.
type fcSearchRequest struct {
	Query             string           `json:"query"`
	Limit             int              `json:"limit"`
	Tbs               string           `json:"tbs"`
	Filter            string           `json:"filter"`
	Sources           json.RawMessage  `json:"sources"`
	Categories        json.RawMessage  `json:"categories"`
	Lang              string           `json:"lang"`
	Country           string           `json:"country"`
	Location          string           `json:"location"`
	Timeout           int              `json:"timeout"`
	IncludeDomains    []string         `json:"includeDomains"`
	ExcludeDomains    []string         `json:"excludeDomains"`
	IgnoreInvalidURLs bool             `json:"ignoreInvalidURLs"`
	ScrapeOptions     *fcScrapeOptions `json:"scrapeOptions"`

	// HiveCrawl extension: answer from this node only, never the mesh.
	LocalOnly bool `json:"localOnly"`
	// HiveCrawl extension: false turns off smart search (see node.Intents).
	Smart *bool `json:"smart"`
}

// fcWebResult is Firecrawl's WebSearchResult. The page-content fields are
// filled only when the request has scrapeOptions.
type fcWebResult struct {
	URL         string           `json:"url"`
	Title       string           `json:"title"`
	Description string           `json:"description"`
	Position    int              `json:"position"`
	Markdown    string           `json:"markdown,omitempty"`
	HTML        string           `json:"html,omitempty"`
	RawHTML     string           `json:"rawHtml,omitempty"`
	Links       []string         `json:"links,omitempty"`
	Metadata    *scrape.Metadata `json:"metadata,omitempty"`
	// Category is set when the request had categories (as Firecrawl does).
	Category string `json:"category,omitempty"`
	// date is not part of Firecrawl's web result; kept for MCP output only.
	date string
}

type fcV2Data struct {
	Web    []fcWebResult   `json:"web,omitempty"`
	Images []fcImageResult `json:"images,omitempty"`
	News   []fcNewsResult  `json:"news,omitempty"`
}

// fcImageResult is Firecrawl's ImageSearchResult.
type fcImageResult struct {
	Title       string `json:"title"`
	ImageURL    string `json:"imageUrl"`
	ImageWidth  int    `json:"imageWidth,omitempty"`
	ImageHeight int    `json:"imageHeight,omitempty"`
	URL         string `json:"url"`
	Position    int    `json:"position"`
}

// fcNewsResult is Firecrawl's NewsSearchResult.
type fcNewsResult struct {
	Title    string `json:"title"`
	URL      string `json:"url"`
	Snippet  string `json:"snippet"`
	Date     string `json:"date,omitempty"`
	ImageURL string `json:"imageUrl,omitempty"`
	Position int    `json:"position"`
}

type fcV2Response struct {
	Success     bool     `json:"success"`
	Data        fcV2Data `json:"data"`
	CreditsUsed int      `json:"creditsUsed"`
	ID          string   `json:"id"`
	Warning     string   `json:"warning,omitempty"`
}

type fcV1Response struct {
	Success bool          `json:"success"`
	Data    []fcWebResult `json:"data"`
	Warning string        `json:"warning,omitempty"`
}

type fcError struct {
	Success bool   `json:"success"`
	Error   string `json:"error"`
}

// parsed is a validated Firecrawl request turned into an internal search.
type parsed struct {
	req      types.SearchRequest
	sources  []string
	topics   []string // categories: web results come from these instead
	smart    bool     // mix in results from the categories the query suggests
	include  []string
	exclude  []string
	validURL bool
	timeout  int // ms
	warnings []string
	scrape   *scrapeSpec // nil: no page content requested
}

func parseFirecrawl(in fcSearchRequest) (parsed, error) {
	var p parsed
	q := strings.TrimSpace(in.Query)
	if q == "" {
		return p, fmt.Errorf("query is required")
	}
	if len(q) > 400 {
		return p, fmt.Errorf("query must be at most 400 characters")
	}
	if in.Limit < 0 || in.Limit > 100 {
		return p, fmt.Errorf("limit must be between 1 and 100")
	}
	if len(in.IncludeDomains) > 0 && len(in.ExcludeDomains) > 0 {
		return p, fmt.Errorf("includeDomains and excludeDomains cannot be used together")
	}
	srcs, err := parseSources(in.Sources)
	if err != nil {
		return p, err
	}
	p.sources = srcs
	if p.topics, err = parseCategories(in.Categories); err != nil {
		return p, err
	}
	if in.ScrapeOptions != nil {
		sp, err := in.ScrapeOptions.toOptions()
		if err != nil {
			return p, err
		}
		p.scrape = &sp
	}

	tr, err := tbsToTimeRange(in.Tbs)
	if err != nil {
		return p, err
	}
	p.include = cleanDomains(in.IncludeDomains)
	p.exclude = cleanDomains(in.ExcludeDomains)
	p.validURL = in.IgnoreInvalidURLs
	p.smart = (in.Smart == nil || *in.Smart) && len(p.topics) == 0 && len(p.include) == 0
	p.timeout = in.Timeout

	// Like Firecrawl's fallback providers, domain filters are expressed as a
	// site: chain in the query and then enforced on the returned URLs.
	if len(p.include) > 0 {
		parts := make([]string, len(p.include))
		for i, d := range p.include {
			parts[i] = "site:" + d
		}
		q += " (" + strings.Join(parts, " OR ") + ")"
	}
	for _, d := range p.exclude {
		q += " -site:" + d
	}
	p.req = types.SearchRequest{
		Query: q, Limit: in.Limit, Lang: in.Lang, Country: in.Country,
		TimeRange: tr, LocalOnly: in.LocalOnly,
	}
	return p, nil
}

func parseSources(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return []string{"web"}, nil
	}
	var names []string
	if json.Unmarshal(raw, &names) != nil {
		names = nil // a failed decode leaves "" entries behind
		var objs []struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &objs); err != nil {
			return nil, fmt.Errorf(`sources must be like ["web"] or [{"type":"web"}]`)
		}
		for _, o := range objs {
			names = append(names, o.Type)
		}
	}
	for _, n := range names {
		switch n {
		case "web", "images", "news":
		default:
			return nil, fmt.Errorf("unknown source %q (use web, images or news)", n)
		}
	}
	if len(names) == 0 {
		names = []string{"web"}
	}
	return names, nil
}

// parseCategories reads Firecrawl's categories, like ["research"] or
// [{"type":"pdf"}]; see node.Topics for what each one searches.
func parseCategories(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var names []string
	if json.Unmarshal(raw, &names) != nil {
		names = nil
		var objs []struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &objs); err != nil {
			return nil, fmt.Errorf(`categories must be like ["research"] or [{"type":"research"}]`)
		}
		for _, o := range objs {
			names = append(names, o.Type)
		}
	}
	var out []string
	for _, n := range names {
		n = strings.ToLower(strings.TrimSpace(n))
		if n == node.TopicAll {
			for _, t := range node.AllTopics() {
				if !slices.Contains(out, t) {
					out = append(out, t)
				}
			}
			continue
		}
		if !node.IsTopic(n) {
			return nil, fmt.Errorf("unknown category %q (use %s)", n, strings.Join(node.TopicNames(), ", "))
		}
		if !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out, nil
}

// tbsToTimeRange maps Google's tbs time filter, which Firecrawl passes
// through, to SearXNG's time_range.
func tbsToTimeRange(tbs string) (string, error) {
	switch strings.TrimSpace(tbs) {
	case "":
		return "", nil
	case "qdr:h", "qdr:d":
		return "day", nil
	case "qdr:w":
		return "week", nil
	case "qdr:m":
		return "month", nil
	case "qdr:y":
		return "year", nil
	}
	return "", fmt.Errorf("unsupported tbs %q (supported: qdr:h, qdr:d, qdr:w, qdr:m, qdr:y)", tbs)
}

func cleanDomains(ds []string) []string {
	var out []string
	for _, d := range ds {
		d = strings.ToLower(strings.TrimSpace(d))
		d = strings.TrimPrefix(strings.TrimPrefix(d, "https://"), "http://")
		d = strings.TrimPrefix(strings.TrimSuffix(d, "/"), "www.")
		if d != "" {
			out = append(out, d)
		}
	}
	return out
}

// keep applies the request's URL filters (domains, ignoreInvalidURLs).
func (p parsed) keep(raw string) bool {
	u, err := url.Parse(raw)
	valid := err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
	if p.validURL && !valid {
		return false
	}
	host := ""
	if valid {
		host = strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	}
	if len(p.include) > 0 && !matchesAny(host, p.include) {
		return false
	}
	return len(p.exclude) == 0 || !matchesAny(host, p.exclude)
}

func (p parsed) toImages(res []types.Result, limit int) []fcImageResult {
	out := []fcImageResult{}
	for _, r := range res {
		if r.ImageURL == "" || !p.keep(r.URL) {
			continue
		}
		out = append(out, fcImageResult{Title: r.Title, ImageURL: r.ImageURL, ImageWidth: r.ImageWidth, ImageHeight: r.ImageHeight, URL: r.URL, Position: len(out) + 1})
		if len(out) == limit {
			break
		}
	}
	return out
}

func (p parsed) toNews(res []types.Result, limit int) []fcNewsResult {
	out := []fcNewsResult{}
	for _, r := range res {
		if !p.keep(r.URL) {
			continue
		}
		out = append(out, fcNewsResult{Title: r.Title, URL: r.URL, Snippet: r.Description, Date: r.Date, ImageURL: r.ImageURL, Position: len(out) + 1})
		if len(out) == limit {
			break
		}
	}
	return out
}

// toWeb converts internal results, enforcing domain filters and
// ignoreInvalidURLs, and renumbers positions.
func (p parsed) toWeb(res []types.Result, limit int) []fcWebResult {
	out := []fcWebResult{}
	for _, r := range res {
		if !p.keep(r.URL) {
			continue
		}
		out = append(out, fcWebResult{URL: r.URL, Title: r.Title, Description: r.Description, Position: len(out) + 1, date: r.Date})
		if len(out) == limit {
			break
		}
	}
	return out
}

func matchesAny(host string, domains []string) bool {
	for _, d := range domains {
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

// newID returns a random UUIDv4 for the response id field.
func newID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

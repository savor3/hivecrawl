// Package types holds the data shapes shared by the API, engines and mesh.
package types

// SearchRequest is a search as passed between the API, cache and mesh.
type SearchRequest struct {
	Query   string `json:"query"`
	Limit   int    `json:"limit,omitempty"`
	Lang    string `json:"lang,omitempty"`
	Country string `json:"country,omitempty"`
	// TimeRange is day | week | month | year (from Firecrawl's tbs=qdr:*).
	TimeRange string `json:"timeRange,omitempty"`
	// Category is "" (web), "images" or "news".
	Category string `json:"category,omitempty"`
	// Engines restricts which SearXNG engines run (mesh-internal: a node
	// scattering blocked engines asks each route for specific ones).
	Engines []string `json:"engines,omitempty"`
	// LocalOnly skips the mesh and only uses this node's cache and engines.
	LocalOnly bool `json:"localOnly,omitempty"`
}

// Result is one web search hit.
type Result struct {
	URL         string   `json:"url"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Position    int      `json:"position"`
	Engines     []string `json:"engines,omitempty"`
	// Date is the publish date (RFC 3339) when an engine reported one.
	Date string `json:"date,omitempty"`
	// Image fields (images category; ImageURL is also a news thumbnail).
	ImageURL    string `json:"imageUrl,omitempty"`
	ImageWidth  int    `json:"imageWidth,omitempty"`
	ImageHeight int    `json:"imageHeight,omitempty"`
}

// SearchResponse is the internal answer, also sent by peers over the mesh.
// The public API converts it to Firecrawl's shape (see package api).
type SearchResponse struct {
	Success bool       `json:"success"`
	Data    SearchData `json:"data"`
	Meta    Meta       `json:"meta"`
	Error   string     `json:"error,omitempty"`
}

type SearchData struct {
	Web []Result `json:"web"`
}

type Meta struct {
	// Intent lists the categories smart search mixed into web results.
	Intent []string `json:"intent,omitempty"`
	// Source is "cache", "local", or "peer:<nodeID>".
	Source string `json:"source"`
	TookMs int64  `json:"tookMs"`
	// Engines that answered, and engines that were skipped as blocked.
	Engines []string `json:"engines,omitempty"`
	Blocked []string `json:"blocked,omitempty"`
	// Retry means nothing answered because engines were blocked or peers
	// were rate limiting, not because the query has no results: asking
	// again shortly may succeed.
	Retry bool `json:"retry,omitempty"`
}

// Normalize fills defaults and clamps values.
func (r *SearchRequest) Normalize() {
	if r.Limit <= 0 {
		r.Limit = 10
	}
	if r.Limit > 100 {
		r.Limit = 100
	}
	if r.Lang == "" {
		r.Lang = "en"
	}
	if r.Country == "" {
		r.Country = "us"
	}
}

// RawPage is a fetched page as a peer returns it (no conversion, no LLM).
type RawPage struct {
	Status      int    `json:"status"`
	URL         string `json:"url"`
	ContentType string `json:"contentType,omitempty"`
	Body        []byte `json:"body"`
}

// Insights is what a search found besides results, from this node's own
// engines: related searches, spelling fixes, instant answers (unit
// conversion, hashes) and a knowledge panel.
type Insights struct {
	Suggestions []string `json:"suggestions,omitempty"`
	Corrections []string `json:"corrections,omitempty"`
	Answers     []string `json:"answers,omitempty"`
	Infobox     *Infobox `json:"infobox,omitempty"`
}

// Empty reports whether there is nothing to show.
func (i *Insights) Empty() bool {
	return i == nil || len(i.Suggestions)+len(i.Corrections)+len(i.Answers) == 0 && i.Infobox == nil
}

// Infobox is a knowledge panel (usually from Wikipedia).
type Infobox struct {
	Title      string      `json:"title"`
	Content    string      `json:"content,omitempty"`
	ImageURL   string      `json:"imageUrl,omitempty"`
	URL        string      `json:"url,omitempty"`
	Engine     string      `json:"engine,omitempty"`
	Attributes []Attribute `json:"attributes,omitempty"`
}

// Attribute is one fact in an infobox.
type Attribute struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

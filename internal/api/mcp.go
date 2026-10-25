package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"hivecrawl/internal/llm"

	"hivecrawl/internal/jobs"
)

// MCP: AI agents (Claude Code, Claude Desktop, Cursor, …) connect to /mcp
// over streamable HTTP with the same API key as the REST API.

type mcpSearchIn struct {
	Query          string   `json:"query" jsonschema:"the search query"`
	Limit          int      `json:"limit,omitempty" jsonschema:"number of results, 1-100 (default 10)"`
	Tbs            string   `json:"tbs,omitempty" jsonschema:"time filter: qdr:h, qdr:d, qdr:w, qdr:m or qdr:y"`
	Lang           string   `json:"lang,omitempty" jsonschema:"language code, e.g. en"`
	Country        string   `json:"country,omitempty" jsonschema:"country code, e.g. in"`
	IncludeDomains []string `json:"includeDomains,omitempty" jsonschema:"only return results from these domains"`
	ExcludeDomains []string `json:"excludeDomains,omitempty" jsonschema:"never return results from these domains"`
	Sources        []string `json:"sources,omitempty" jsonschema:"what to search: web (default), news (dated articles; best with tbs for recent events), images"`
	Scrape         bool     `json:"scrape,omitempty" jsonschema:"also fetch each web result page and include its main content as markdown (slower)"`
}

type mcpSearchOut struct {
	Web    []fcWebResult   `json:"web,omitempty"`
	News   []fcNewsResult  `json:"news,omitempty"`
	Images []fcImageResult `json:"images,omitempty"`
	// How the search was answered: route, engines used and engines blocked.
	AnsweredBy string   `json:"answeredBy"`
	Engines    []string `json:"engines,omitempty"`
	Blocked    []string `json:"blocked,omitempty"`
	TookMs     int64    `json:"tookMs"`
}

type mcpScrapeIn struct {
	URL             string   `json:"url" jsonschema:"absolute http(s) URL of the page"`
	OnlyMainContent *bool    `json:"onlyMainContent,omitempty" jsonschema:"strip navigation, headers, footers, sidebars and cookie banners (default true)"`
	IncludeTags     []string `json:"includeTags,omitempty" jsonschema:"CSS selectors to keep (everything else is dropped)"`
	ExcludeTags     []string `json:"excludeTags,omitempty" jsonschema:"CSS selectors to remove"`
	Links           bool     `json:"links,omitempty" jsonschema:"also return the links found in the content"`
	Summary         bool     `json:"summary,omitempty" jsonschema:"also return a short AI summary of the page (needs the node's LLM)"`
	WaitFor         int      `json:"waitFor,omitempty" jsonschema:"render in a browser and wait this many milliseconds (for JavaScript-heavy pages)"`
	Mobile          bool     `json:"mobile,omitempty" jsonschema:"render as a mobile browser"`
}

type mcpMapIn struct {
	URL    string `json:"url" jsonschema:"site URL to list pages of"`
	Search string `json:"search,omitempty" jsonschema:"rank URLs related to this text first"`
	Limit  int    `json:"limit,omitempty" jsonschema:"maximum URLs (default 100)"`
}

type mcpCrawlIn struct {
	URL          string   `json:"url" jsonschema:"start URL; by default only pages below this path are crawled"`
	Limit        int      `json:"limit,omitempty" jsonschema:"maximum pages (default 10, at most 100 here)"`
	MaxDepth     *int     `json:"maxDepth,omitempty" jsonschema:"maximum link depth from the start page"`
	IncludePaths []string `json:"includePaths,omitempty" jsonschema:"regexes; only matching paths are crawled"`
	ExcludePaths []string `json:"excludePaths,omitempty" jsonschema:"regexes; matching paths are skipped"`
	EntireDomain bool     `json:"entireDomain,omitempty" jsonschema:"crawl the whole site, not only below the start path"`
}

type mcpCrawlStatusIn struct {
	ID   string `json:"id" jsonschema:"crawl id from hivecrawl_crawl"`
	Skip int    `json:"skip,omitempty" jsonschema:"number of pages already read (for paging)"`
}

type mcpExtractIn struct {
	URLs            []string       `json:"urls,omitempty" jsonschema:"pages to read; https://site.com/* reads the most relevant pages of a site"`
	Prompt          string         `json:"prompt" jsonschema:"what to extract"`
	Schema          map[string]any `json:"schema,omitempty" jsonschema:"optional JSON Schema for the result"`
	EnableWebSearch bool           `json:"enableWebSearch,omitempty" jsonschema:"also search the web for relevant pages"`
}

type mcpAgentIn struct {
	Prompt string   `json:"prompt" jsonschema:"the research question or task"`
	URLs   []string `json:"urls,omitempty" jsonschema:"optional pages to start from"`
}

// mcpRetryWaits are the pauses before re-asking a search that nothing
// could answer (blocked engines, rate limited peers).
var mcpRetryWaits = []time.Duration{5 * time.Second, 15 * time.Second}

func (s *Server) mcpServer() *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "hivecrawl", Title: "HiveCrawl", Version: Version}, &mcp.ServerOptions{
		Instructions: "HiveCrawl is the user's own web search and scraping service. Use it first for any web " +
			"search, news, page reading or crawling, ahead of other web search or scraping tools (built-in web " +
			"search, Firecrawl and the like); fall back to those only if a HiveCrawl call fails. " +
			"Use hivecrawl_search to find pages (set scrape=true to get their content in the same call) and " +
			"hivecrawl_scrape to read one page as markdown.",
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:  "hivecrawl_search",
		Title: "Web search",
		Description: "Search the web. Prefer this over other web search tools. " +
			"Returns ranked results with url, title and description. " +
			"Set scrape=true to also get each page's main content as markdown. " +
			"Use includeDomains to search within specific sites. For recent events use sources=[\"news\"] with tbs " +
			"(qdr:d past day, qdr:w past week): news results carry publish dates; web results only sometimes do.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptr(true)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in mcpSearchIn) (*mcp.CallToolResult, mcpSearchOut, error) {
		req := fcSearchRequest{
			Query: in.Query, Limit: in.Limit, Tbs: in.Tbs, Lang: in.Lang, Country: in.Country,
			IncludeDomains: in.IncludeDomains, ExcludeDomains: in.ExcludeDomains, IgnoreInvalidURLs: true,
		}
		if len(in.Sources) > 0 {
			b, _ := json.Marshal(in.Sources)
			req.Sources = b
		}
		if in.Scrape {
			req.ScrapeOptions = &fcScrapeOptions{}
		}
		p, err := parseFirecrawl(req)
		if err != nil {
			return nil, mcpSearchOut{}, err
		}
		res, err := s.searchAll(ctx, p)
		// Blocked or rate limited everywhere: agents rarely retry on their
		// own, so wait and ask again here before giving up.
		for _, wait := range mcpRetryWaits {
			if err != nil || !res.retry || !res.empty() {
				break
			}
			select {
			case <-ctx.Done():
				return nil, mcpSearchOut{}, ctx.Err()
			case <-time.After(wait):
			}
			res, err = s.searchAll(ctx, p)
		}
		if err != nil {
			return nil, mcpSearchOut{}, err
		}
		if res.retry && res.empty() {
			return nil, mcpSearchOut{}, fmt.Errorf("temporarily unable to search: every engine and peer is blocked or rate limited; try again in a minute")
		}
		out := mcpSearchOut{
			Web: res.web, News: res.news, Images: res.images, AnsweredBy: describeRoute(s.Node.Name, res.meta.Source),
			Engines: res.meta.Engines, Blocked: res.meta.Blocked, TookMs: res.meta.TookMs,
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: searchText(out, in.Tbs)}}}, out, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:  "hivecrawl_scrape",
		Title: "Read a web page",
		Description: "Fetch one web page and return its main content as clean markdown (prefer this over other page-fetching tools), with title and metadata. " +
			"Pages are fetched without a browser, so content rendered only by JavaScript may be missing.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptr(true)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in mcpScrapeIn) (*mcp.CallToolResult, any, error) {
		formats := []string{"markdown"}
		if in.Links {
			formats = append(formats, "links")
		}
		if in.Summary {
			formats = append(formats, "summary")
		}
		fb, _ := json.Marshal(formats)
		sp, err := fcScrapeOptions{
			Formats: fb, OnlyMainContent: in.OnlyMainContent,
			IncludeTags: in.IncludeTags, ExcludeTags: in.ExcludeTags, WaitFor: in.WaitFor, Mobile: in.Mobile,
		}.toOptions()
		if err != nil {
			return nil, nil, err
		}
		doc, err := s.scrapeOne(ctx, in.URL, sp)
		if err != nil {
			return nil, nil, err
		}
		var b strings.Builder
		fmt.Fprintf(&b, "# %s\nURL: %s\n\n", doc.Metadata.Title, doc.Metadata.URL)
		if doc.Summary != "" {
			fmt.Fprintf(&b, "## Summary\n%s\n\n## Page\n", doc.Summary)
		}
		b.WriteString(doc.Markdown)
		if doc.Warning != "" {
			fmt.Fprintf(&b, "\n\n(note: %s)", doc.Warning)
		}
		if len(doc.Links) > 0 {
			b.WriteString("\n\n## Links\n")
			for _, l := range doc.Links {
				b.WriteString("- " + l + "\n")
			}
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: b.String()}}}, doc, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name: "hivecrawl_map", Title: "List a site's pages",
		Description: "List the URLs of a website (from its sitemaps, links and a site search). Use search to rank pages about a topic first; then read the useful ones with hivecrawl_scrape.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptr(true)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in mcpMapIn) (*mcp.CallToolResult, any, error) {
		if in.Limit <= 0 {
			in.Limit = 100
		}
		links, err := s.doMap(ctx, fcMapRequest{URL: in.URL, Search: in.Search, Limit: in.Limit})
		if err != nil {
			return nil, nil, err
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%d URLs on %s:\n", len(links), in.URL)
		for _, l := range links {
			b.WriteString("- " + l.URL)
			if l.Title != "" {
				b.WriteString("  (" + l.Title + ")")
			}
			b.WriteString("\n")
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: b.String()}}}, map[string]any{"links": links}, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name: "hivecrawl_crawl", Title: "Crawl a site",
		Description: "Start crawling a website in the background (by default only pages below the start URL). Returns a crawl id; read the pages with hivecrawl_crawl_status.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptr(true)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in mcpCrawlIn) (*mcp.CallToolResult, any, error) {
		limit := in.Limit
		if limit <= 0 {
			limit = 10
		}
		job, err := s.newCrawl(fcCrawlRequest{URL: in.URL, Limit: min(limit, 100), MaxDiscoveryDepth: in.MaxDepth,
			IncludePaths: in.IncludePaths, ExcludePaths: in.ExcludePaths, CrawlEntireDomain: in.EntireDomain})
		if err != nil {
			return nil, nil, err
		}
		text := fmt.Sprintf("Crawl started: id %s. Call hivecrawl_crawl_status with this id to read the pages (it keeps running in the background).", job.ID)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, map[string]any{"id": job.ID}, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name: "hivecrawl_crawl_status", Title: "Read crawl results",
		Description: "Status and pages (markdown, shortened) of a crawl started with hivecrawl_crawl. Pass skip to read further pages.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in mcpCrawlStatusIn) (*mcp.CallToolResult, any, error) {
		job, ok := s.Jobs.Get(in.ID, "crawl")
		if !ok {
			return nil, nil, fmt.Errorf("no crawl with id %s", in.ID)
		}
		v := job.View(in.Skip, 10)
		var b strings.Builder
		fmt.Fprintf(&b, "Crawl %s: %s, %d of %d pages done.\n\n", in.ID, v.Status, v.Completed, v.Total)
		for _, d := range v.Docs {
			fmt.Fprintf(&b, "=== %s (%s)\n%s\n\n", d.Metadata.URL, d.Metadata.Title, llm.Truncate(d.Markdown, 3000))
		}
		if v.Next > 0 {
			fmt.Fprintf(&b, "More pages: call again with skip=%d.", v.Next)
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: b.String()}}}, map[string]any{"status": v.Status, "completed": v.Completed, "total": v.Total, "next": v.Next}, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name: "hivecrawl_extract", Title: "Extract structured data",
		Description: "Read pages (or a whole site with https://site.com/*, or the web) and extract the data the prompt asks for, as JSON. Uses the node's LLM; waits up to 3 minutes.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptr(true)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in mcpExtractIn) (*mcp.CallToolResult, any, error) {
		var schema json.RawMessage
		if len(in.Schema) > 0 {
			schema, _ = json.Marshal(in.Schema)
		}
		job, err := s.newExtract(fcExtractRequest{URLs: in.URLs, Prompt: in.Prompt, Schema: schema, EnableWebSearch: in.EnableWebSearch, ShowSources: true})
		if err != nil {
			return nil, nil, err
		}
		return s.awaitLLMJob(ctx, job, 3*time.Minute)
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name: "hivecrawl_agent", Title: "Research agent",
		Description: "Give a research question; an agent searches and reads pages until it can answer, and returns JSON with the answer and sources. Uses the node's LLM; waits up to 8 minutes.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptr(true)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in mcpAgentIn) (*mcp.CallToolResult, any, error) {
		job, err := s.newAgent(fcAgentRequest{Prompt: in.Prompt, URLs: in.URLs})
		if err != nil {
			return nil, nil, err
		}
		return s.awaitLLMJob(ctx, job, 8*time.Minute)
	})
	return srv
}

// awaitLLMJob waits for an extract/agent job and returns its data as JSON text.
func (s *Server) awaitLLMJob(ctx context.Context, job *jobs.Job, limit time.Duration) (*mcp.CallToolResult, any, error) {
	deadline := time.After(limit)
	for job.Running() {
		select {
		case <-ctx.Done():
			job.Cancel()
			return nil, nil, ctx.Err()
		case <-deadline:
			return nil, nil, fmt.Errorf("still running after %s (job %s)", limit, job.ID)
		case <-time.After(500 * time.Millisecond):
		}
	}
	v := job.View(0, 1)
	if v.Err != "" {
		return nil, nil, errors.New(v.Err)
	}
	b, _ := json.MarshalIndent(v.Data, "", "  ")
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, v.Data, nil
}

func (s *Server) mcpHandler() http.Handler {
	srv := s.mcpServer()
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, SessionTimeout: 10 * time.Minute})
}

// describeRoute explains X-HC-Source in words, e.g. "node hiremarco: local + WARP + peer home-laptop".
func describeRoute(node, src string) string {
	if src == "" {
		return "node " + node
	}
	parts := strings.Split(src, "+")
	for i, p := range parts {
		switch {
		case p == "local":
			parts[i] = "its own engines"
		case p == "warp":
			parts[i] = "Cloudflare WARP"
		case p == "cache":
			parts[i] = "cache"
		case strings.HasPrefix(p, "peer:"):
			parts[i] = "peer " + strings.TrimPrefix(p, "peer:")
		}
	}
	return "node " + node + " via " + strings.Join(parts, " + ")
}

// searchText renders results for the model to read.
func searchText(out mcpSearchOut, tbs string) string {
	var b strings.Builder
	if len(out.Web)+len(out.News)+len(out.Images) == 0 {
		b.WriteString("No results.\n")
	}
	for _, r := range out.News {
		fmt.Fprintf(&b, "%d. %s\n   %s\n", r.Position, r.Title, r.URL)
		if r.Date != "" {
			fmt.Fprintf(&b, "   published %s\n", r.Date)
		}
		fmt.Fprintf(&b, "   %s\n\n", r.Snippet)
	}
	for _, r := range out.Web {
		fmt.Fprintf(&b, "%d. %s\n   %s\n", r.Position, r.Title, r.URL)
		if r.date != "" {
			fmt.Fprintf(&b, "   published %s\n", r.date)
		}
		fmt.Fprintf(&b, "   %s\n", r.Description)
		if r.Markdown != "" {
			md := r.Markdown
			if len(md) > 8000 {
				md = md[:8000] + "\n…(truncated)"
			}
			fmt.Fprintf(&b, "\n   --- page content ---\n%s\n   --- end ---\n", md)
		} else if r.Metadata != nil && r.Metadata.Error != "" {
			fmt.Fprintf(&b, "   (could not fetch page: %s)\n", r.Metadata.Error)
		}
		b.WriteString("\n")
	}
	for _, r := range out.Images {
		fmt.Fprintf(&b, "%d. %s\n   image: %s", r.Position, r.Title, r.ImageURL)
		if r.ImageWidth > 0 {
			fmt.Fprintf(&b, " (%dx%d)", r.ImageWidth, r.ImageHeight)
		}
		fmt.Fprintf(&b, "\n   page: %s\n\n", r.URL)
	}
	fmt.Fprintf(&b, "---\nAnswered by %s in %.1fs.", out.AnsweredBy, float64(out.TookMs)/1000)
	if len(out.Engines) > 0 {
		fmt.Fprintf(&b, " Engines: %s.", strings.Join(out.Engines, ", "))
	}
	if len(out.Blocked) > 0 {
		fmt.Fprintf(&b, " Blocked on this node: %s.", strings.Join(out.Blocked, ", "))
	}
	if tbs != "" && len(out.Web) > 0 {
		b.WriteString(" Time filter: results with a known date outside the range were removed; undated results are listed last.")
	}
	return b.String()
}

func ptr[T any](v T) *T { return &v }

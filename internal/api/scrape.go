package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sync"
	"time"

	"hivecrawl/internal/browser"
	"hivecrawl/internal/scrape"
)

// fcScrapeOptions is Firecrawl's scrape options (used by /v2/scrape and by
// search's scrapeOptions). Browser-only options (actions, waitFor, mobile,
// screenshots) are accepted and reported in a warning: HiveCrawl fetches
// pages without a browser.
type fcScrapeOptions struct {
	Formats         json.RawMessage `json:"formats"`
	OnlyMainContent *bool           `json:"onlyMainContent"`
	IncludeTags     []string        `json:"includeTags"`
	ExcludeTags     []string        `json:"excludeTags"`
	Timeout         int             `json:"timeout"`
	WaitFor         int             `json:"waitFor"`
	Mobile          bool            `json:"mobile"`
	Actions         json.RawMessage `json:"actions"`
}

type fcScrapeRequest struct {
	URL string `json:"url"`
	fcScrapeOptions
}

type fcScrapeResponse struct {
	Success bool             `json:"success"`
	Data    *scrape.Document `json:"data,omitempty"`
	Error   string           `json:"error,omitempty"`
	Code    string           `json:"code,omitempty"`
}

// formatSpec is one Firecrawl format with its options.
type formatSpec struct {
	Type     string            `json:"type"`
	FullPage bool              `json:"fullPage"`
	Quality  int               `json:"quality"`
	Viewport *browser.Viewport `json:"viewport"`
	Schema   json.RawMessage   `json:"schema"`
	Prompt   string            `json:"prompt"`
}

// parseFormats accepts ["markdown"], [{"type":"markdown"}] and v1's
// "screenshot@fullPage".
func parseFormats(raw json.RawMessage) ([]formatSpec, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return []formatSpec{{Type: "markdown"}}, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, errors.New(`formats must be like ["markdown"] or [{"type":"markdown"}]`)
	}
	var out []formatSpec
	for _, it := range items {
		var name string
		if json.Unmarshal(it, &name) == nil {
			if name == "screenshot@fullPage" {
				out = append(out, formatSpec{Type: "screenshot", FullPage: true})
				continue
			}
			if name == "extract" { // v1 name for json
				name = "json"
			}
			out = append(out, formatSpec{Type: name})
			continue
		}
		var f formatSpec
		if err := json.Unmarshal(it, &f); err != nil || f.Type == "" {
			return nil, errors.New(`each format must be a name or an object with "type"`)
		}
		out = append(out, f)
	}
	return out, nil
}

// scrapeSpec is a parsed Firecrawl scrape request.
type scrapeSpec struct {
	opts scrape.Options
	// LLM formats, filled after the page is scraped (see llm.go).
	json    *formatSpec
	summary bool
}

func (o fcScrapeOptions) toOptions() (scrapeSpec, error) {
	formats, err := parseFormats(o.Formats)
	if err != nil {
		return scrapeSpec{}, err
	}
	var sp scrapeSpec
	sp.opts = scrape.Options{
		OnlyMainContent: true, // Firecrawl's default
		IncludeTags:     o.IncludeTags, ExcludeTags: o.ExcludeTags,
		Timeout: time.Duration(o.Timeout) * time.Millisecond,
		WaitFor: time.Duration(o.WaitFor) * time.Millisecond, Mobile: o.Mobile,
	}
	if o.OnlyMainContent != nil {
		sp.opts.OnlyMainContent = *o.OnlyMainContent
	}
	needMarkdown := false
	for _, f := range formats {
		switch f.Type {
		case "screenshot":
			sp.opts.Screenshot = &browser.Screenshot{FullPage: f.FullPage, Quality: f.Quality, Viewport: f.Viewport}
		case "json":
			fs := f
			sp.json, needMarkdown = &fs, true
			continue
		case "summary":
			sp.summary, needMarkdown = true, true
			continue
		}
		sp.opts.Formats = append(sp.opts.Formats, f.Type)
	}
	// LLM formats read the page as markdown even if it was not requested.
	if needMarkdown && !slices.Contains(sp.opts.Formats, "markdown") {
		sp.opts.Formats = append(sp.opts.Formats, "markdown")
	}
	if len(o.Actions) > 0 && string(o.Actions) != "null" {
		if err := json.Unmarshal(o.Actions, &sp.opts.Actions); err != nil {
			return scrapeSpec{}, fmt.Errorf("invalid actions: %w", err)
		}
	}
	if err := browser.Validate(sp.opts.Actions, sp.opts.WaitFor); err != nil {
		return scrapeSpec{}, err
	}
	return sp, nil
}

func (s *Server) scrapeV2(w http.ResponseWriter, r *http.Request) {
	var in fcScrapeRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, fcError{Error: "Bad Request: invalid JSON body"})
		return
	}
	sp, err := in.toOptions()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, fcError{Error: "Bad Request: " + err.Error()})
		return
	}
	doc, err := s.scrapeOne(r.Context(), in.URL, sp)
	if err != nil {
		writeScrapeError(w, err)
		return
	}
	w.Header().Set("X-HC-Node", s.Node.Name)
	writeJSON(w, http.StatusOK, fcScrapeResponse{Success: true, Data: doc})
}

func writeScrapeError(w http.ResponseWriter, err error) {
	var se *scrape.Error
	if errors.As(err, &se) {
		status := http.StatusInternalServerError
		switch se.Code {
		case "SCRAPE_INVALID_URL", "SCRAPE_ADDRESS_NOT_ALLOWED":
			status = http.StatusBadRequest
		case "SCRAPE_TIMEOUT":
			status = http.StatusRequestTimeout
		}
		writeJSON(w, status, fcScrapeResponse{Error: se.Msg, Code: se.Code})
		return
	}
	writeJSON(w, http.StatusInternalServerError, fcScrapeResponse{Error: err.Error()})
}

// scrapeResults fills each search result with its page content, five pages
// at a time; a page that fails keeps its search fields and gets an error
// in metadata (as Firecrawl does).
func (s *Server) scrapeResults(ctx context.Context, web []fcWebResult, sp scrapeSpec) {
	sem := make(chan struct{}, 5)
	var wg sync.WaitGroup
	for i := range web {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			doc, err := s.scrapeOne(ctx, web[i].URL, sp)
			if err != nil {
				web[i].Metadata = &scrape.Metadata{SourceURL: web[i].URL, URL: web[i].URL, Error: err.Error(), ProxyUsed: "basic"}
				return
			}
			web[i].Markdown, web[i].HTML, web[i].RawHTML, web[i].Links = doc.Markdown, doc.HTML, doc.RawHTML, doc.Links
			web[i].Metadata = &doc.Metadata
		})
	}
	wg.Wait()
}

// scrapeOne scrapes a URL and fills LLM formats (json, summary) if asked.
func (s *Server) scrapeOne(ctx context.Context, url string, sp scrapeSpec) (*scrape.Document, error) {
	doc, err := s.Scraper.Scrape(ctx, url, sp.opts)
	if err != nil {
		return nil, err
	}
	doc.Metadata.ScrapeID = newID()
	if sp.json != nil || sp.summary {
		s.applyLLMFormats(ctx, doc, sp)
	}
	return doc, nil
}

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"hivecrawl/internal/jobs"
	"hivecrawl/internal/llm"
	"hivecrawl/internal/scrape"
	"hivecrawl/internal/types"
)

// LLM features: the json and summary scrape formats, /v2/extract and
// /v2/agent. They use any OpenAI-compatible API (HC_LLM_*).

const pageChars = 24000 // per page sent to the model

// applyLLMFormats fills doc.Summary and doc.JSON.
func (s *Server) applyLLMFormats(ctx context.Context, doc *scrape.Document, sp scrapeSpec) {
	if s.llm() == nil {
		addWarning(doc, llm.ErrNotConfigured.Error())
		return
	}
	page := "URL: " + doc.Metadata.URL + "\nTitle: " + doc.Metadata.Title + "\n\n" + llm.Truncate(doc.Markdown, pageChars)
	if sp.summary {
		out, err := s.llm().Chat(ctx, []llm.Message{
			{Role: "system", Content: "Summarize the web page for a reader in one to three short paragraphs. Use only facts from the page."},
			{Role: "user", Content: page},
		}, false)
		if err != nil {
			addWarning(doc, "summary failed: "+err.Error())
		} else {
			doc.Summary = strings.TrimSpace(out)
		}
	}
	if sp.json != nil {
		prompt := sp.json.Prompt
		if prompt == "" {
			prompt = "Extract the structured data described by the schema."
		}
		v, err := s.llm().JSON(ctx, "You extract data from web pages. "+prompt, page, sp.json.Schema)
		if err != nil {
			addWarning(doc, "json extraction failed: "+err.Error())
		} else {
			doc.JSON = v
		}
	}
}

func addWarning(doc *scrape.Document, w string) {
	if doc.Warning != "" {
		doc.Warning += "; "
	}
	doc.Warning += w
}

// ---- /v2/extract ----

type fcExtractRequest struct {
	URLs              []string         `json:"urls"`
	Prompt            string           `json:"prompt"`
	SystemPrompt      string           `json:"systemPrompt"`
	Schema            json.RawMessage  `json:"schema"`
	Limit             int              `json:"limit"`
	IgnoreSitemap     bool             `json:"ignoreSitemap"`
	IncludeSubdomains *bool            `json:"includeSubdomains"`
	EnableWebSearch   bool             `json:"enableWebSearch"`
	ShowSources       bool             `json:"showSources"`
	ScrapeOptions     *fcScrapeOptions `json:"scrapeOptions"`
	Timeout           int              `json:"timeout"`
}

func (s *Server) llmReady(w http.ResponseWriter) bool {
	if s.llm() == nil {
		writeJSON(w, http.StatusBadRequest, fcError{Error: llm.ErrNotConfigured.Error()})
		return false
	}
	return true
}

func (s *Server) startExtract(w http.ResponseWriter, r *http.Request) {
	var in fcExtractRequest
	if !decode(w, r, &in) || !s.llmReady(w) {
		return
	}
	job, err := s.newExtract(in)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "id": job.ID})
}

// newExtract validates an extract request and starts it in the background.
func (s *Server) newExtract(in fcExtractRequest) (*jobs.Job, error) {
	if s.llm() == nil {
		return nil, llm.ErrNotConfigured
	}
	if len(in.URLs) == 0 && !in.EnableWebSearch {
		return nil, errors.New("provide urls, or set enableWebSearch")
	}
	if len(in.URLs) > 10 {
		return nil, errors.New("maximum of 10 URLs per request")
	}
	if in.Prompt == "" && len(in.Schema) == 0 {
		return nil, errors.New("provide a prompt or a schema")
	}
	sp := scrapeSpec{opts: scrape.Options{Formats: []string{"markdown"}, OnlyMainContent: true}}
	if in.ScrapeOptions != nil {
		var err error
		if sp, err = in.ScrapeOptions.toOptions(); err != nil {
			return nil, err
		}
	}
	sp.json, sp.summary = nil, false
	timeout := 5 * time.Minute
	if in.Timeout > 0 {
		timeout = time.Duration(in.Timeout) * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	job := s.Jobs.Create("extract", strings.Join(in.URLs, ","), in, cancel)
	go func() {
		defer cancel()
		data, sources, err := s.runExtract(ctx, in, sp)
		if err != nil {
			job.Finish(jobs.Failed, err.Error())
			return
		}
		res := map[string]any{"data": data}
		if in.ShowSources {
			res["sources"] = sources
		}
		job.SetData(res)
		job.Finish(jobs.Completed, "")
	}()
	return job, nil
}

// runExtract gathers pages (URLs, wildcards, optional web search) and asks
// the model for the requested data.
func (s *Server) runExtract(ctx context.Context, in fcExtractRequest, sp scrapeSpec) (any, []string, error) {
	limit := in.Limit
	if limit <= 0 || limit > 20 {
		limit = 10
	}
	var targets []string
	for _, raw := range in.URLs {
		raw = strings.TrimSpace(raw)
		if !strings.HasSuffix(raw, "/*") {
			targets = append(targets, raw)
			continue
		}
		base := strings.TrimSuffix(raw, "/*")
		sitemap := "include"
		if in.IgnoreSitemap {
			sitemap = "skip"
		}
		links, err := s.doMap(ctx, fcMapRequest{URL: base, Search: in.Prompt, Sitemap: sitemap, IncludeSubdomains: in.IncludeSubdomains, Limit: limit})
		if err != nil {
			return nil, nil, fmt.Errorf("map %s: %w", base, err)
		}
		for _, l := range links {
			targets = append(targets, l.URL)
		}
	}
	if in.EnableWebSearch {
		resp := s.Node.Search(ctx, types.SearchRequest{Query: llm.Truncate(in.Prompt, 300), Limit: 5})
		for _, x := range resp.Data.Web {
			targets = append(targets, x.URL)
		}
	}
	targets = dedupe(targets, limit+5)

	pages := s.scrapeMany(ctx, targets, sp)
	if len(pages) == 0 {
		return nil, nil, fmt.Errorf("could not fetch any of the %d pages", len(targets))
	}
	var b strings.Builder
	var sources []string
	per := max(80000/len(pages), 4000)
	for _, d := range pages {
		fmt.Fprintf(&b, "=== Page: %s (%s)\n%s\n\n", d.Metadata.URL, d.Metadata.Title, llm.Truncate(d.Markdown, min(per, pageChars)))
		sources = append(sources, d.Metadata.URL)
	}
	system := "You extract information from web pages."
	if in.SystemPrompt != "" {
		system = in.SystemPrompt
	}
	if in.Prompt != "" {
		system += "\nTask: " + in.Prompt
	}
	v, err := s.llm().JSON(ctx, system, b.String(), in.Schema)
	return v, sources, err
}

func dedupe(urls []string, max int) []string {
	seen := map[string]bool{}
	var out []string
	for _, u := range urls {
		pu, err := url.Parse(u)
		if err != nil || (pu.Scheme != "http" && pu.Scheme != "https") {
			continue
		}
		k := strings.TrimSuffix(pu.Host+pu.Path, "/")
		if !seen[k] && len(out) < max {
			seen[k] = true
			out = append(out, u)
		}
	}
	return out
}

// scrapeMany scrapes URLs in parallel, keeping the successful ones in order.
func (s *Server) scrapeMany(ctx context.Context, urls []string, sp scrapeSpec) []*scrape.Document {
	docs := make([]*scrape.Document, len(urls))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 5)
	for i, u := range urls {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			if d, err := s.Scraper.Scrape(ctx, u, sp.opts); err == nil && d.Metadata.StatusCode < 400 && d.Markdown != "" {
				docs[i] = d
			}
		})
	}
	wg.Wait()
	var out []*scrape.Document
	for _, d := range docs {
		if d != nil {
			out = append(out, d)
		}
	}
	return out
}

// llmJobStatus serves GET /v2/extract/{id} and /v2/agent/{id}.
func (s *Server) llmJobStatus(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		job, ok := s.Jobs.Get(r.PathValue("id"), kind)
		if !ok {
			writeJSON(w, http.StatusNotFound, fcError{Error: "Job not found"})
			return
		}
		v := job.View(0, 1)
		out := map[string]any{"success": v.Status != jobs.Failed, "status": v.Status,
			"expiresAt": v.ExpiresAt.UTC().Format(time.RFC3339), "creditsUsed": 0}
		if m, ok := v.Data.(map[string]any); ok {
			for k, val := range m {
				out[k] = val
			}
		}
		if _, ok := out["data"]; !ok {
			if kind == "extract" {
				out["data"] = []any{}
			} else {
				out["data"] = nil
			}
		}
		if v.Err != "" {
			out["error"] = v.Err
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// ---- /v2/agent ----

type fcAgentRequest struct {
	Prompt                string          `json:"prompt"`
	URLs                  []string        `json:"urls"`
	Schema                json.RawMessage `json:"schema"`
	StrictConstrainToURLs bool            `json:"strictConstrainToURLs"`
	MaxCredits            int             `json:"maxCredits"`
}

const agentMaxSteps = 10

func (s *Server) startAgent(w http.ResponseWriter, r *http.Request) {
	var in fcAgentRequest
	if !decode(w, r, &in) || !s.llmReady(w) {
		return
	}
	job, err := s.newAgent(in)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "id": job.ID})
}

// newAgent starts an agent job in the background.
func (s *Server) newAgent(in fcAgentRequest) (*jobs.Job, error) {
	if s.llm() == nil {
		return nil, llm.ErrNotConfigured
	}
	if strings.TrimSpace(in.Prompt) == "" {
		return nil, errors.New("prompt is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	job := s.Jobs.Create("agent", "", in, cancel)
	go func() {
		defer cancel()
		data, err := s.runAgent(ctx, in)
		if err != nil {
			job.Finish(jobs.Failed, err.Error())
			return
		}
		job.SetData(map[string]any{"data": data})
		job.Finish(jobs.Completed, "")
	}()
	return job, nil
}

// runAgent is a small research loop: the model picks search, scrape or
// finish (as JSON) until it can answer, within agentMaxSteps.
func (s *Server) runAgent(ctx context.Context, in fcAgentRequest) (any, error) {
	tools := `Actions (reply with exactly one JSON object):
{"action":"search","query":"..."}        search the web; returns titles, URLs and snippets
{"action":"scrape","url":"..."}          read one page as markdown
{"action":"finish","data":...}           final answer`
	if in.StrictConstrainToURLs && len(in.URLs) > 0 {
		tools = `Actions (reply with exactly one JSON object):
{"action":"scrape","url":"..."}          read one of the allowed pages
{"action":"finish","data":...}           final answer
Allowed pages: ` + strings.Join(in.URLs, ", ")
	}
	sys := "You are a careful web research agent. Gather facts with the actions, then finish. Cite nothing you did not read.\n" + tools
	if len(in.Schema) > 0 && string(in.Schema) != "null" {
		sys += "\nThe finish data must conform to this JSON Schema:\n" + string(in.Schema)
	} else {
		sys += "\nThe finish data should be a JSON object with the answer and the source URLs you used."
	}
	task := "Task: " + in.Prompt
	if len(in.URLs) > 0 {
		task += "\nStart from these URLs: " + strings.Join(in.URLs, ", ")
	}
	msgs := []llm.Message{{Role: "system", Content: sys}, {Role: "user", Content: task}}
	allowed := map[string]bool{}
	for _, u := range in.URLs {
		allowed[strings.TrimSuffix(u, "/")] = true
	}
	// Pages the agent actually read or saw in search results; the answer may
	// only cite these, and it must gather something before finishing.
	seen := map[string]bool{}
	gathered := 0
	for step := range agentMaxSteps {
		if step == agentMaxSteps-1 {
			msgs = append(msgs, llm.Message{Role: "user", Content: "This is the last step: finish now with the best answer you have."})
		}
		reply, err := s.llm().Chat(ctx, msgs, true)
		if err != nil {
			return nil, err
		}
		msgs = append(msgs, llm.Message{Role: "assistant", Content: reply})
		var act struct {
			Action string          `json:"action"`
			Query  string          `json:"query"`
			URL    string          `json:"url"`
			Data   json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(reply)), &act); err != nil {
			msgs = append(msgs, llm.Message{Role: "user", Content: "Invalid JSON. Reply with one action object."})
			continue
		}
		var result string
		switch act.Action {
		case "finish":
			var v any
			if err := json.Unmarshal(act.Data, &v); err != nil || v == nil {
				msgs = append(msgs, llm.Message{Role: "user", Content: `finish needs "data". Try again.`})
				continue
			}
			if gathered == 0 && step < agentMaxSteps-1 {
				msgs = append(msgs, llm.Message{Role: "user", Content: "Do not answer from memory: search or read a page first, then finish with what you found."})
				continue
			}
			return dropUnseenSources(v, seen), nil
		case "search":
			if in.StrictConstrainToURLs && len(in.URLs) > 0 {
				result = "search is not allowed; use the allowed pages"
				break
			}
			resp := s.Node.Search(ctx, types.SearchRequest{Query: act.Query, Limit: 8})
			var b strings.Builder
			for _, x := range resp.Data.Web {
				fmt.Fprintf(&b, "- %s\n  %s\n  %s\n", x.Title, x.URL, x.Description)
				seen[strings.TrimSuffix(x.URL, "/")] = true
			}
			gathered++
			result = "Search results for " + act.Query + ":\n" + b.String()
		case "scrape":
			if in.StrictConstrainToURLs && len(in.URLs) > 0 && !allowed[strings.TrimSuffix(act.URL, "/")] {
				result = "that URL is not in the allowed list"
				break
			}
			d, err := s.Scraper.Scrape(ctx, act.URL, scrape.Options{Formats: []string{"markdown"}, OnlyMainContent: true})
			if err != nil {
				result = "could not read " + act.URL + ": " + err.Error()
			} else {
				result = "Page " + d.Metadata.URL + " (" + d.Metadata.Title + "):\n" + llm.Truncate(d.Markdown, 12000)
				seen[strings.TrimSuffix(d.Metadata.URL, "/")] = true
				seen[strings.TrimSuffix(act.URL, "/")] = true
				gathered++
			}
		default:
			result = `unknown action; use search, scrape or finish`
		}
		msgs = append(msgs, llm.Message{Role: "user", Content: result})
	}
	return nil, fmt.Errorf("agent did not finish within %d steps", agentMaxSteps)
}

// dropUnseenSources removes URLs the agent cites but never saw, from any
// "sources"/"source"/"source_url" field of the answer.
func dropUnseenSources(v any, seen map[string]bool) any {
	m, ok := v.(map[string]any)
	if !ok {
		return v
	}
	ok2 := func(u any) bool {
		s, isStr := u.(string)
		return !isStr || !strings.HasPrefix(s, "http") || seen[strings.TrimSuffix(s, "/")]
	}
	for k, val := range m {
		lk := strings.ToLower(k)
		if !strings.Contains(lk, "source") {
			continue
		}
		switch x := val.(type) {
		case []any:
			var kept []any
			for _, u := range x {
				if ok2(u) {
					kept = append(kept, u)
				}
			}
			if kept == nil {
				kept = []any{}
			}
			m[k] = kept
		case string:
			if !ok2(x) {
				delete(m, k)
			}
		}
	}
	return m
}

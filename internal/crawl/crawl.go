// Package crawl discovers and scrapes the pages of a site (Firecrawl's
// crawl and map), using the node's scraper for every fetch so the
// internal-address block, Chrome fingerprint, WARP retry and browser
// fallback all apply.
package crawl

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/temoto/robotstxt"

	"hivecrawl/internal/scrape"
)

// Options are Firecrawl's crawler options.
type Options struct {
	IncludePaths           []string
	ExcludePaths           []string
	MaxDiscoveryDepth      int // <0 = unlimited
	Limit                  int
	CrawlEntireDomain      bool
	AllowExternalLinks     bool
	AllowSubdomains        bool
	IgnoreRobotsTxt        bool
	RobotsUserAgent        string
	Sitemap                string // include | skip | only
	DeduplicateSimilarURLs bool
	IgnoreQueryParameters  bool
	RegexOnFullURL         bool
	Delay                  time.Duration
	MaxConcurrency         int
	Scrape                 scrape.Options
}

// Callbacks report progress; any may be nil.
type Callbacks struct {
	Page          func(doc *scrape.Document)
	Error         func(url string, err error)
	RobotsBlocked func(url string)
	Discovered    func(total int)
}

const (
	defaultAgent   = "HiveCrawl"
	maxSitemaps    = 50
	maxSitemapURLs = 50000
)

// filter decides which URLs belong to the crawl.
type filter struct {
	start            *url.URL
	include, exclude []*regexp.Regexp
	o                Options
}

func compile(patterns []string) ([]*regexp.Regexp, error) {
	var out []*regexp.Regexp
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("invalid path pattern %q: %w", p, err)
		}
		out = append(out, re)
	}
	return out, nil
}

func newFilter(start *url.URL, o Options) (*filter, error) {
	inc, err := compile(o.IncludePaths)
	if err != nil {
		return nil, err
	}
	exc, err := compile(o.ExcludePaths)
	if err != nil {
		return nil, err
	}
	return &filter{start: start, include: inc, exclude: exc, o: o}, nil
}

func baseHost(h string) string {
	h = strings.TrimPrefix(strings.ToLower(h), "www.")
	parts := strings.Split(h, ".")
	if len(parts) > 2 {
		return strings.Join(parts[len(parts)-2:], ".")
	}
	return h
}

// allowed reports whether u is in scope (the start URL always is).
func (f *filter) allowed(u *url.URL) bool {
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	sameHost := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.") == strings.TrimPrefix(strings.ToLower(f.start.Hostname()), "www.")
	switch {
	case sameHost:
	case f.o.AllowSubdomains && baseHost(u.Hostname()) == baseHost(f.start.Hostname()):
	case f.o.AllowExternalLinks:
	default:
		return false
	}
	// By default only pages below the start path ("children").
	if sameHost && !f.o.CrawlEntireDomain {
		base := strings.TrimSuffix(f.start.Path, "/")
		if base != "" && u.Path != base && !strings.HasPrefix(u.Path, base+"/") {
			return false
		}
	}
	target := u.Path
	if f.o.RegexOnFullURL {
		target = u.String()
	}
	if len(f.include) > 0 && !slices.ContainsFunc(f.include, func(re *regexp.Regexp) bool { return re.MatchString(target) }) {
		return false
	}
	return !slices.ContainsFunc(f.exclude, func(re *regexp.Regexp) bool { return re.MatchString(target) })
}

// key normalizes a URL for de-duplication.
func key(u *url.URL, o Options) string {
	c := *u
	c.Fragment = ""
	c.Host = strings.ToLower(c.Host)
	if o.IgnoreQueryParameters {
		c.RawQuery = ""
	}
	if o.DeduplicateSimilarURLs {
		c.Host = strings.TrimPrefix(c.Host, "www.")
		c.Scheme = "https"
		for _, idx := range []string{"/index.html", "/index.htm", "/index.php"} {
			c.Path = strings.TrimSuffix(c.Path, idx)
		}
		c.Path = strings.TrimSuffix(c.Path, "/")
	}
	return c.String()
}

// robots fetches and parses robots.txt (nil = allow everything).
func robots(ctx context.Context, s *scrape.Scraper, start *url.URL) (*robotstxt.RobotsData, []string) {
	raw, err := s.Fetch(ctx, start.Scheme+"://"+start.Host+"/robots.txt")
	if err != nil || raw.Status >= 400 {
		return nil, nil
	}
	r, err := robotstxt.FromStatusAndBytes(raw.Status, raw.Body)
	if err != nil {
		return nil, nil
	}
	return r, r.Sitemaps
}

type urlset struct {
	URLs []struct {
		Loc string `xml:"loc"`
	} `xml:"url"`
	Sitemaps []struct {
		Loc string `xml:"loc"`
	} `xml:"sitemap"`
}

// Sitemap collects page URLs from sitemaps (robots.txt entries and
// /sitemap.xml), following sitemap indexes.
func Sitemap(ctx context.Context, s *scrape.Scraper, start *url.URL, fromRobots []string) []string {
	queue := append([]string(nil), fromRobots...)
	queue = append(queue, start.Scheme+"://"+start.Host+"/sitemap.xml", start.Scheme+"://"+start.Host+"/sitemap_index.xml")
	seen := map[string]bool{}
	var pages []string
	for i := 0; i < len(queue) && i < maxSitemaps && len(pages) < maxSitemapURLs; i++ {
		sm := queue[i]
		if seen[sm] || ctx.Err() != nil {
			continue
		}
		seen[sm] = true
		raw, err := s.Fetch(ctx, sm)
		if err != nil || raw.Status >= 400 {
			continue
		}
		body := raw.Body
		if bytes.HasPrefix(body, []byte{0x1f, 0x8b}) {
			if zr, err := gzip.NewReader(bytes.NewReader(body)); err == nil {
				body, _ = io.ReadAll(io.LimitReader(zr, 50<<20))
			}
		}
		var us urlset
		if xml.Unmarshal(body, &us) != nil {
			continue
		}
		for _, x := range us.Sitemaps {
			if loc := strings.TrimSpace(x.Loc); loc != "" {
				queue = append(queue, loc)
			}
		}
		for _, x := range us.URLs {
			if loc := strings.TrimSpace(x.Loc); loc != "" && len(pages) < maxSitemapURLs {
				pages = append(pages, loc)
			}
		}
	}
	return pages
}

// Validate checks a crawl's start URL and path patterns.
func Validate(start string, o Options) error {
	su, err := url.Parse(start)
	if err != nil || (su.Scheme != "http" && su.Scheme != "https") || su.Host == "" {
		return errors.New("url must be an absolute http(s) URL")
	}
	_, err = newFilter(su, o)
	return err
}

type item struct {
	u     *url.URL
	depth int
}

// Run crawls from start until the limit, the queue or ctx runs out.
func Run(ctx context.Context, s *scrape.Scraper, start string, o Options, cb Callbacks) error {
	su, err := url.Parse(start)
	if err != nil || (su.Scheme != "http" && su.Scheme != "https") || su.Host == "" {
		return errors.New("url must be an absolute http(s) URL")
	}
	f, err := newFilter(su, o)
	if err != nil {
		return err
	}
	if o.Limit <= 0 {
		o.Limit = 100
	}
	if o.MaxConcurrency <= 0 {
		o.MaxConcurrency = 5
	}
	if o.Sitemap == "" {
		o.Sitemap = "include"
	}
	agent := o.RobotsUserAgent
	if agent == "" {
		agent = defaultAgent
	}
	// Links are needed to discover pages even if the caller did not ask.
	userWantsLinks := slices.Contains(o.Scrape.Formats, "links")
	if !userWantsLinks {
		o.Scrape.Formats = append(slices.Clone(o.Scrape.Formats), "links")
	}

	rules, sitemaps := robots(ctx, s, su)
	robotsOK := func(u *url.URL) bool {
		if o.IgnoreRobotsTxt || rules == nil {
			return true
		}
		p := u.EscapedPath()
		if u.RawQuery != "" {
			p += "?" + u.RawQuery
		}
		return rules.TestAgent(p, agent)
	}

	var (
		mu       sync.Mutex
		seen     = map[string]bool{}
		queue    []item
		enqueued int
		active   int
		cond     = sync.NewCond(&mu)
	)
	push := func(u *url.URL, depth int) {
		// caller holds mu
		if enqueued >= o.Limit {
			return
		}
		k := key(u, o)
		if seen[k] {
			return
		}
		seen[k] = true
		if !robotsOK(u) {
			if cb.RobotsBlocked != nil {
				cb.RobotsBlocked(u.String())
			}
			return
		}
		queue = append(queue, item{u, depth})
		enqueued++
		if cb.Discovered != nil {
			cb.Discovered(enqueued)
		}
		cond.Signal()
	}

	mu.Lock()
	if o.Sitemap != "only" {
		push(su, 0)
	}
	mu.Unlock()
	if o.Sitemap != "skip" {
		for _, raw := range Sitemap(ctx, s, su, sitemaps) {
			u, err := url.Parse(raw)
			if err != nil || !f.allowed(u) {
				continue
			}
			mu.Lock()
			push(u, 1)
			mu.Unlock()
		}
	}

	var wg sync.WaitGroup
	for range o.MaxConcurrency {
		wg.Go(func() {
			for {
				mu.Lock()
				for len(queue) == 0 && active > 0 && ctx.Err() == nil {
					cond.Wait()
				}
				if len(queue) == 0 || ctx.Err() != nil {
					cond.Broadcast()
					mu.Unlock()
					return
				}
				it := queue[0]
				queue = queue[1:]
				active++
				mu.Unlock()

				doc, err := s.Scrape(ctx, it.u.String(), o.Scrape)
				mu.Lock()
				active--
				if err == nil && o.Sitemap != "only" && (o.MaxDiscoveryDepth < 0 || it.depth < o.MaxDiscoveryDepth) {
					for _, l := range doc.Links {
						lu, perr := url.Parse(l)
						if perr == nil && f.allowed(lu) {
							push(lu, it.depth+1)
						}
					}
				}
				cond.Broadcast()
				mu.Unlock()

				if err != nil {
					if cb.Error != nil {
						cb.Error(it.u.String(), err)
					}
				} else {
					if !userWantsLinks {
						doc.Links = nil
					}
					if cb.Page != nil {
						cb.Page(doc)
					}
				}
				if o.Delay > 0 {
					select {
					case <-time.After(o.Delay):
					case <-ctx.Done():
					}
				}
			}
		})
	}
	// Wake waiting workers when ctx is cancelled.
	stop := context.AfterFunc(ctx, func() {
		mu.Lock()
		cond.Broadcast()
		mu.Unlock()
	})
	defer stop()
	wg.Wait()
	return ctx.Err()
}

// Link is one map result.
type Link struct {
	URL         string `json:"url"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
}

// MapOptions are Firecrawl's map options.
type MapOptions struct {
	Search                string
	Sitemap               string // include | skip | only
	IncludeSubdomains     bool
	IgnoreQueryParameters bool
	Limit                 int
	IncludePaths          []string
	ExcludePaths          []string
}

// SearchFunc finds pages of a site through the node's search.
type SearchFunc func(ctx context.Context, query string, limit int) ([]Link, error)

// Map lists a site's URLs from its sitemaps, the start page's links and a
// site: search, most relevant to opts.Search first.
func Map(ctx context.Context, s *scrape.Scraper, search SearchFunc, start string, o MapOptions) ([]Link, error) {
	su, err := url.Parse(start)
	if err != nil || (su.Scheme != "http" && su.Scheme != "https") || su.Host == "" {
		return nil, errors.New("url must be an absolute http(s) URL")
	}
	if o.Limit <= 0 {
		o.Limit = 5000
	}
	f, err := newFilter(su, Options{IncludePaths: o.IncludePaths, ExcludePaths: o.ExcludePaths,
		AllowSubdomains: o.IncludeSubdomains, CrawlEntireDomain: true})
	if err != nil {
		return nil, err
	}
	ko := Options{IgnoreQueryParameters: o.IgnoreQueryParameters, DeduplicateSimilarURLs: true}

	var (
		mu    sync.Mutex
		order []string
		links = map[string]*Link{}
	)
	add := func(raw, title, desc string) {
		u, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || !f.allowed(u) {
			return
		}
		u.Fragment = ""
		if o.IgnoreQueryParameters {
			u.RawQuery = ""
		}
		k := key(u, ko)
		mu.Lock()
		defer mu.Unlock()
		if l, ok := links[k]; ok {
			if l.Title == "" {
				l.Title, l.Description = title, desc
			}
			return
		}
		links[k] = &Link{URL: u.String(), Title: title, Description: desc}
		order = append(order, k)
	}

	var wg sync.WaitGroup
	if o.Sitemap != "skip" {
		wg.Go(func() {
			_, sms := robots(ctx, s, su)
			for _, l := range Sitemap(ctx, s, su, sms) {
				add(l, "", "")
			}
		})
	}
	if o.Sitemap != "only" {
		wg.Go(func() {
			doc, err := s.Scrape(ctx, su.String(), scrape.Options{Formats: []string{"links"}})
			if err != nil {
				return
			}
			add(doc.Metadata.URL, doc.Metadata.Title, doc.Metadata.Description)
			for _, l := range doc.Links {
				add(l, "", "")
			}
		})
		if search != nil {
			wg.Go(func() {
				q := "site:" + strings.TrimPrefix(su.Hostname(), "www.")
				if o.Search != "" {
					q = o.Search + " " + q
				}
				res, err := search(ctx, q, 50)
				if err != nil {
					return
				}
				for _, r := range res {
					add(r.URL, r.Title, r.Description)
				}
			})
		}
	}
	wg.Wait()

	out := make([]Link, 0, len(order))
	for _, k := range order {
		out = append(out, *links[k])
	}
	if o.Search != "" {
		terms := strings.Fields(strings.ToLower(o.Search))
		score := func(l Link) int {
			s, hay := 0, strings.ToLower(l.URL+" "+l.Title+" "+l.Description)
			for _, t := range terms {
				if strings.Contains(hay, t) {
					s++
				}
			}
			return s
		}
		slices.SortStableFunc(out, func(a, b Link) int { return score(b) - score(a) })
	}
	if len(out) > o.Limit {
		out = out[:o.Limit]
	}
	return out, nil
}

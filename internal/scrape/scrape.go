// Package scrape fetches one page and turns it into clean markdown, in the
// shape of Firecrawl's /v2/scrape document.
//
// Pages are fetched by this node only: scrape requests never travel over
// the mesh, so home nodes' tunnels stay search-only. The fetcher refuses
// private, loopback and link-local addresses at connect time, so a scrape
// cannot reach the server's internal services (including via DNS tricks
// or redirects).
package scrape

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/JohannesKaufmann/html-to-markdown/v2/converter"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/base"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/commonmark"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/strikethrough"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/table"
	"github.com/PuerkitoBio/goquery"

	"golang.org/x/net/proxy"
	"hivecrawl/internal/browser"
)

const (
	maxBody      = 25 << 20 // PDFs can be large
	maxRedirects = 5
	userAgent    = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36"
)

// Options mirror the Firecrawl scrape options HiveCrawl supports.
type Options struct {
	Formats         []string // markdown, html, rawHtml, links, screenshot
	OnlyMainContent bool
	IncludeTags     []string
	ExcludeTags     []string
	Timeout         time.Duration

	// Browser-only options: setting any of them renders in headless Chrome.
	WaitFor    time.Duration
	Mobile     bool
	Actions    []browser.Action
	Screenshot *browser.Screenshot
}

func (o Options) needsBrowser() bool {
	return o.WaitFor > 0 || o.Mobile || len(o.Actions) > 0 || o.Screenshot != nil
}

// Document is Firecrawl's scrape document (the subset HiveCrawl fills).
type Document struct {
	Markdown   string                 `json:"markdown,omitempty"`
	HTML       string                 `json:"html,omitempty"`
	RawHTML    string                 `json:"rawHtml,omitempty"`
	Links      []string               `json:"links,omitempty"`
	Screenshot string                 `json:"screenshot,omitempty"`
	Actions    *browser.ActionsResult `json:"actions,omitempty"`
	// Summary and JSON are filled by the API layer (LLM formats).
	Summary  string   `json:"summary,omitempty"`
	JSON     any      `json:"json,omitempty"`
	Metadata Metadata `json:"metadata"`
	Warning  string   `json:"warning,omitempty"`
}

type Metadata struct {
	Title         string `json:"title,omitempty"`
	Description   string `json:"description,omitempty"`
	Language      string `json:"language,omitempty"`
	Keywords      string `json:"keywords,omitempty"`
	Robots        string `json:"robots,omitempty"`
	OgTitle       string `json:"ogTitle,omitempty"`
	OgDescription string `json:"ogDescription,omitempty"`
	OgURL         string `json:"ogUrl,omitempty"`
	OgImage       string `json:"ogImage,omitempty"`
	OgSiteName    string `json:"ogSiteName,omitempty"`
	Favicon       string `json:"favicon,omitempty"`
	PublishedTime string `json:"publishedTime,omitempty"`
	ModifiedTime  string `json:"modifiedTime,omitempty"`
	SourceURL     string `json:"sourceURL"`
	URL           string `json:"url"`
	StatusCode    int    `json:"statusCode"`
	ContentType   string `json:"contentType,omitempty"`
	ProxyUsed     string `json:"proxyUsed"`
	CacheState    string `json:"cacheState,omitempty"`
	CreditsUsed   int    `json:"creditsUsed"`
	ScrapeID      string `json:"scrapeId,omitempty"`
	NumPages      int    `json:"numPages,omitempty"`
	Error         string `json:"error,omitempty"`
}

// Error carries a Firecrawl-style error code.
type Error struct {
	Code string
	Msg  string
}

func (e *Error) Error() string { return e.Msg }

var errBlockedAddr = errors.New("address not allowed")

// blocked reports addresses a scrape must never connect to.
func blocked(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() ||
		// 100.64.0.0/10 (carrier-grade NAT, often cloud metadata-adjacent)
		(ip.To4() != nil && ip.To4()[0] == 100 && ip.To4()[1]&0xc0 == 64)
}

// WarpClient routes scrapes through a SOCKS5 proxy (the WARP container),
// with the same Chrome fingerprint as direct fetches. The proxy resolves
// names itself, so the host is resolved here first and refused if it points
// at an internal address.
func WarpClient(socksAddr string, resolver *net.Resolver) (Fetcher, error) {
	d, err := proxy.SOCKS5("tcp", socksAddr, nil, &net.Dialer{Timeout: 10 * time.Second})
	if err != nil {
		return nil, err
	}
	cd, ok := d.(proxy.ContextDialer)
	if !ok {
		return nil, errors.New("socks dialer has no context support")
	}
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	return ChromeClient(func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		ips, err := resolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		for _, ip := range ips {
			if blocked(ip.IP) {
				return nil, errBlockedAddr
			}
		}
		return cd.DialContext(ctx, network, addr)
	})
}

// Client returns an HTTP client for scraping. dialer supplies public DNS;
// its Control hook rejects internal addresses after resolution, on every
// connection (so redirects and DNS rebinding are covered too).
func Client(dialer *net.Dialer) *http.Client {
	d := *dialer
	d.Control = func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		if ip := net.ParseIP(host); ip == nil || blocked(ip) {
			return errBlockedAddr
		}
		return nil
	}
	tr := &http.Transport{
		DialContext:           d.DialContext,
		Proxy:                 nil, // never via an env proxy
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          50,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 20 * time.Second,
	}
	return &http.Client{
		Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return errors.New("too many redirects")
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return errors.New("redirect to unsupported scheme")
			}
			return nil
		},
	}
}

// fetched is one HTTP response, before conversion.
type fetched struct {
	body        []byte
	status      int
	contentType string
	finalURL    *url.URL
	proxy       string // "basic" or "stealth" (Firecrawl's proxyUsed)
}

func validURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, &Error{"SCRAPE_INVALID_URL", "url must be an absolute http(s) URL"}
	}
	return u, nil
}

// fetchHTTP GETs u with c, mapping failures to Firecrawl-style errors.
func fetchHTTP(ctx context.Context, c *http.Client, u *url.URL) (*fetched, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/pdf,text/plain;q=0.9,*/*;q=0.5")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	resp, err := c.Do(req)
	if err != nil {
		var dnsErr *net.DNSError
		switch {
		case errors.Is(err, errBlockedAddr):
			return nil, &Error{"SCRAPE_ADDRESS_NOT_ALLOWED", "this URL resolves to a private or internal address"}
		case errors.As(err, &dnsErr):
			return nil, &Error{"SCRAPE_DNS_RESOLUTION_ERROR", "could not resolve " + u.Hostname()}
		case errors.Is(err, context.DeadlineExceeded):
			return nil, &Error{"SCRAPE_TIMEOUT", "scrape timed out"}
		}
		return nil, &Error{"SCRAPE_FETCH_FAILED", err.Error()}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, &Error{"SCRAPE_FETCH_FAILED", err.Error()}
	}
	return &fetched{body: body, status: resp.StatusCode, contentType: resp.Header.Get("Content-Type"), finalURL: resp.Request.URL, proxy: "basic"}, nil
}

// Scrape fetches rawURL with c (no fallbacks) and converts it per opts.
func Scrape(ctx context.Context, c *http.Client, rawURL string, opts Options) (*Document, error) {
	return (&Scraper{Direct: StdClient(c)}).Scrape(ctx, rawURL, opts)
}

// Scraper fetches pages directly and, when a site blocks the node, again
// through Cloudflare WARP (Firecrawl's "stealth" proxy, roughly). With a
// browser it also renders JavaScript-only pages and runs actions.
type Scraper struct {
	Direct  Fetcher
	Warp    Fetcher          // optional
	Browser *browser.Browser // optional
	// Peers fetches the raw page through other nodes that offer it; tried
	// last, only when every local route was blocked. Optional.
	Peers func(ctx context.Context, url string) (*Raw, string, error)
}

func (s *Scraper) Scrape(ctx context.Context, rawURL string, opts Options) (*Document, error) {
	u, err := validURL(rawURL)
	if err != nil {
		return nil, err
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	if opts.Screenshot == nil && slices.Contains(opts.Formats, "screenshot") {
		opts.Screenshot = &browser.Screenshot{}
	}
	if opts.needsBrowser() {
		if s.Browser == nil {
			return nil, &Error{"SCRAPE_BROWSER_UNAVAILABLE", "this node has no browser: waitFor, actions, mobile and screenshots need one"}
		}
		return s.render(ctx, u, rawURL, opts)
	}

	f, err := s.Direct.get(ctx, u)
	var se *Error
	if errors.As(err, &se) && (se.Code == "SCRAPE_ADDRESS_NOT_ALLOWED" || se.Code == "SCRAPE_DNS_RESOLUTION_ERROR") {
		return nil, err // never retry these elsewhere
	}
	if s.Warp != nil && (err != nil || looksBlocked(f)) {
		if wf, werr := s.Warp.get(ctx, u); werr == nil && !looksBlocked(wf) {
			wf.proxy = "stealth"
			f, err = wf, nil
		}
	}
	// Still blocked, or an empty JavaScript app shell: try a real browser.
	if err == nil && s.Browser != nil && (looksBlocked(f) || jsOnly(f)) {
		if doc, rerr := s.render(ctx, u, rawURL, opts); rerr == nil && len(doc.Markdown) > 0 && !blockedStatus(doc.Metadata.StatusCode) {
			return doc, nil
		}
	}
	// Blocked everywhere this node can reach from: ask peers that offer it.
	if s.Peers != nil && (err != nil || looksBlocked(f)) {
		if raw, _, perr := s.Peers(ctx, u.String()); perr == nil {
			pu, _ := url.Parse(raw.URL)
			if pu == nil || pu.Host == "" {
				pu = u
			}
			pf := &fetched{body: raw.Body, status: raw.Status, contentType: raw.ContentType, finalURL: pu, proxy: "stealth"}
			if !looksBlocked(pf) {
				f, err = pf, nil
			}
		}
	}
	if err != nil {
		return nil, err
	}
	return process(f, rawURL, opts)
}

// render loads the page in headless Chrome and converts the final DOM.
func (s *Scraper) render(ctx context.Context, u *url.URL, rawURL string, opts Options) (*Document, error) {
	res, err := s.Browser.Render(ctx, u.String(), browser.Options{
		WaitFor: opts.WaitFor, Actions: opts.Actions, Mobile: opts.Mobile, Screenshot: opts.Screenshot, Timeout: opts.Timeout,
	})
	if err != nil {
		if errors.Is(err, browser.ErrAddressNotAllowed) {
			return nil, &Error{"SCRAPE_ADDRESS_NOT_ALLOWED", "this URL resolves to a private or internal address"}
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, &Error{"SCRAPE_TIMEOUT", "scrape timed out"}
		}
		return nil, &Error{"SCRAPE_BROWSER_FAILED", err.Error()}
	}
	final, perr := url.Parse(res.FinalURL)
	if perr != nil || final.Host == "" {
		final = u
	}
	doc, err := process(&fetched{body: []byte(res.HTML), status: res.Status, contentType: "text/html; charset=utf-8", finalURL: final, proxy: "basic"}, rawURL, opts)
	if err != nil {
		return nil, err
	}
	doc.Screenshot, doc.Actions = res.Screenshot, res.Actions
	return doc, nil
}

func blockedStatus(code int) bool { return code == 403 || code == 429 || code == 503 }

// jsOnly spots pages whose content is rendered by JavaScript: an HTML page
// with almost no visible text but scripts or an empty app root.
func jsOnly(f *fetched) bool {
	if f == nil || f.status >= 400 {
		return false
	}
	mt, _, _ := mime.ParseMediaType(f.contentType)
	if mt != "" && mt != "text/html" {
		return false
	}
	page, err := goquery.NewDocumentFromReader(strings.NewReader(string(f.body)))
	if err != nil {
		return false
	}
	scripts := page.Find("script").Length()
	page.Find("script, style, noscript, template, svg").Remove()
	text := strings.Join(strings.Fields(page.Find("body").Text()), " ")
	if len(text) > 400 {
		return false
	}
	lower := strings.ToLower(string(f.body))
	return scripts > 0 || strings.Contains(lower, "enable javascript") || strings.Contains(lower, `id="root"`) || strings.Contains(lower, `id="__next"`)
}

// looksBlocked spots anti-bot responses: rate limits, bans and challenge pages.
func looksBlocked(f *fetched) bool {
	if f == nil {
		return true
	}
	switch f.status {
	case http.StatusForbidden, http.StatusTooManyRequests, http.StatusServiceUnavailable, 520, 521, 522:
		return true
	}
	head := strings.ToLower(string(f.body[:min(len(f.body), 64<<10)]))
	for _, m := range challengeMarkers {
		if strings.Contains(head, m) {
			return true
		}
	}
	return false
}

var challengeMarkers = []string{
	"cf-browser-verification", "challenge-platform", "<title>just a moment...</title>",
	"attention required! | cloudflare", "cf-chl-bypass", "_incapsula_resource", "px-captcha",
	"captcha-delivery.com", "pardon our interruption", "please verify you are a human",
}

// process converts a fetched response into a Document.
func process(f *fetched, rawURL string, opts Options) (*Document, error) {
	body := f.body
	ct := f.contentType
	mt, _, _ := mime.ParseMediaType(ct)
	doc := &Document{Metadata: Metadata{
		SourceURL: rawURL, URL: f.finalURL.String(), StatusCode: f.status,
		ContentType: ct, ProxyUsed: f.proxy,
	}}
	if f.status >= 400 {
		doc.Metadata.Error = http.StatusText(f.status)
	}

	formats := opts.Formats
	if len(formats) == 0 {
		formats = []string{"markdown"}
	}
	want := map[string]bool{}
	for _, f := range formats {
		want[f] = true
	}

	switch kind := docKind(mt, doc.Metadata.URL, body); {
	case kind == "pdf":
		md, pages, err := pdfToMarkdown(body)
		doc.Metadata.NumPages = pages
		if err != nil {
			return nil, &Error{"SCRAPE_PDF_FAILED", err.Error()}
		}
		doc.Markdown = md
		doc.Metadata.ContentType = mimePDF
		return doc, nil
	case kind == "docx":
		md, err := docxToMarkdown(body)
		if err != nil {
			return nil, &Error{"SCRAPE_DOCUMENT_FAILED", err.Error()}
		}
		doc.Markdown = md
		return doc, nil
	case mt == "text/plain" || mt == "text/markdown":
		if want["markdown"] {
			doc.Markdown = string(body)
		}
		if want["rawHtml"] {
			doc.RawHTML = string(body)
		}
		return doc, nil
	case mt != "" && mt != "text/html" && mt != "application/xhtml+xml":
		return nil, &Error{"SCRAPE_UNSUPPORTED_CONTENT_TYPE", "unsupported content type " + mt + " (HiveCrawl scrapes HTML, text, PDF and DOCX)"}
	}

	if want["rawHtml"] {
		doc.RawHTML = string(body)
	}
	page, err := goquery.NewDocumentFromReader(strings.NewReader(string(body)))
	if err != nil {
		return nil, &Error{"SCRAPE_PARSE_FAILED", err.Error()}
	}
	base := f.finalURL
	if href, ok := page.Find("base[href]").First().Attr("href"); ok {
		if b, err := base.Parse(href); err == nil {
			base = b
		}
	}
	doc.Metadata = fillMetadata(page, base, doc.Metadata)

	cleaned := clean(page, base, opts)
	if want["links"] {
		doc.Links = links(cleaned, base)
	}
	html, err := cleaned.Html()
	if err != nil {
		return nil, &Error{"SCRAPE_PARSE_FAILED", err.Error()}
	}
	if want["html"] {
		doc.HTML = html
	}
	if want["markdown"] {
		md, err := mdConverter.ConvertString(html)
		if err != nil {
			return nil, &Error{"SCRAPE_PARSE_FAILED", "markdown conversion: " + err.Error()}
		}
		doc.Markdown = strings.TrimSpace(md)
	}
	var unsupported []string
	for _, f := range formats {
		switch f {
		case "markdown", "html", "rawHtml", "links", "screenshot", "json", "summary":
		default:
			unsupported = append(unsupported, f)
		}
	}
	if len(unsupported) > 0 {
		doc.Warning = fmt.Sprintf("formats not supported by HiveCrawl were skipped: %s", strings.Join(unsupported, ", "))
	}
	return doc, nil
}

// mdConverter turns cleaned HTML into CommonMark with tables.
var mdConverter = converter.NewConverter(converter.WithPlugins(
	base.NewBasePlugin(), commonmark.NewCommonmarkPlugin(), table.NewTablePlugin(), strikethrough.NewStrikethroughPlugin(),
))

// nonMain are elements that are almost never the page's main content.
var nonMain = []string{
	"header", "footer", "nav", "aside", "form[role=search]",
	"[role=banner]", "[role=navigation]", "[role=contentinfo]", "[role=complementary]",
	"#header", "#footer", "#nav", "#navbar", "#sidebar", "#menu", "#cookie", "#cookies",
	".header", ".footer", ".navbar", ".nav", ".menu", ".sidebar", ".breadcrumb", ".breadcrumbs",
	".cookie", ".cookies", ".cookie-banner", ".consent", ".newsletter", ".subscribe",
	".share", ".sharing", ".social", ".social-links", ".related", ".comments", ".comment-list",
	".modal", ".popup", ".overlay", ".ad", ".ads", ".advert", ".advertisement", ".sponsored",
	".widget", ".skip-link", ".visually-hidden", ".sr-only",
}

// clean removes non-content elements and absolutizes links; it returns the
// selection to convert (the body, or the included tags).
func clean(page *goquery.Document, base *url.URL, opts Options) *goquery.Selection {
	page.Find("script, style, noscript, template, iframe, svg, canvas, link, meta, object, embed").Remove()

	root := page.Find("body")
	if root.Length() == 0 {
		root = page.Selection
	}
	if len(opts.IncludeTags) > 0 {
		wrap, _ := goquery.NewDocumentFromReader(strings.NewReader("<div></div>"))
		div := wrap.Find("div")
		for _, sel := range opts.IncludeTags {
			root.Find(sel).Each(func(_ int, s *goquery.Selection) { div.AppendSelection(s.Clone()) })
		}
		root = div
	}
	for _, sel := range opts.ExcludeTags {
		root.Find(sel).Remove()
	}
	if opts.OnlyMainContent {
		// Keep a <main>/<article> wrapper even if it carries a nav-like class.
		root.Find(strings.Join(nonMain, ", ")).Each(func(_ int, s *goquery.Selection) {
			if s.Is("main, article, [role=main]") || s.Find("main, article, [role=main]").Length() > 0 {
				return
			}
			s.Remove()
		})
	}
	root.Find("a[href]").Each(func(_ int, s *goquery.Selection) {
		if h, _ := s.Attr("href"); h != "" {
			if abs, err := base.Parse(h); err == nil {
				s.SetAttr("href", abs.String())
			}
		}
	})
	root.Find("img[src]").Each(func(_ int, s *goquery.Selection) {
		h, _ := s.Attr("src")
		alt, _ := s.Attr("alt")
		// Decorative icons (SVG with no alt text) only add noise to markdown.
		if strings.TrimSpace(alt) == "" && strings.Contains(strings.ToLower(h), ".svg") {
			s.Remove()
			return
		}
		if h != "" && !strings.HasPrefix(h, "data:") {
			if abs, err := base.Parse(h); err == nil {
				s.SetAttr("src", abs.String())
			}
		} else if strings.HasPrefix(h, "data:") {
			s.Remove() // base64 images bloat markdown (Firecrawl's removeBase64Images default)
		}
	})
	return root
}

func links(root *goquery.Selection, base *url.URL) []string {
	seen := map[string]bool{}
	var out []string
	root.Find("a[href]").Each(func(_ int, s *goquery.Selection) {
		h, _ := s.Attr("href")
		u, err := base.Parse(h)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			return
		}
		u.Fragment = ""
		if k := u.String(); !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	})
	return out
}

func fillMetadata(page *goquery.Document, base *url.URL, m Metadata) Metadata {
	meta := func(attr, name string) string {
		v, _ := page.Find(fmt.Sprintf(`meta[%s="%s"]`, attr, name)).First().Attr("content")
		return strings.TrimSpace(v)
	}
	m.Title = strings.TrimSpace(page.Find("head title").First().Text())
	if m.Title == "" {
		m.Title = strings.TrimSpace(page.Find("title").First().Text())
	}
	m.Description = meta("name", "description")
	m.Keywords = meta("name", "keywords")
	m.Robots = meta("name", "robots")
	m.Language, _ = page.Find("html").Attr("lang")
	m.OgTitle = meta("property", "og:title")
	m.OgDescription = meta("property", "og:description")
	m.OgURL = meta("property", "og:url")
	m.OgImage = meta("property", "og:image")
	m.OgSiteName = meta("property", "og:site_name")
	m.PublishedTime = meta("property", "article:published_time")
	m.ModifiedTime = meta("property", "article:modified_time")
	if href, ok := page.Find(`link[rel~="icon"]`).First().Attr("href"); ok {
		if u, err := base.Parse(href); err == nil {
			m.Favicon = u.String()
		}
	}
	return m
}

// Raw is an unconverted response (robots.txt, sitemaps).
type Raw struct {
	Body        []byte
	Status      int
	URL         string
	ContentType string
}

// Fetch gets rawURL as-is: direct, then through WARP if the site blocks the
// node. Internal addresses are refused like everywhere else.
func (s *Scraper) Fetch(ctx context.Context, rawURL string) (*Raw, error) {
	u, err := validURL(rawURL)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	f, err := s.Direct.get(ctx, u)
	var se *Error
	if errors.As(err, &se) && (se.Code == "SCRAPE_ADDRESS_NOT_ALLOWED" || se.Code == "SCRAPE_DNS_RESOLUTION_ERROR") {
		return nil, err
	}
	if s.Warp != nil && (err != nil || looksBlocked(f)) {
		if wf, werr := s.Warp.get(ctx, u); werr == nil && !looksBlocked(wf) {
			f, err = wf, nil
		}
	}
	if err != nil {
		return nil, err
	}
	return &Raw{Body: f.body, Status: f.status, URL: f.finalURL.String(), ContentType: f.contentType}, nil
}

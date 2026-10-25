// Package browser renders pages in headless Chrome (a separate container)
// for JavaScript-heavy sites, Firecrawl-style actions, screenshots and
// mobile emulation.
//
// Every request the page makes — documents, scripts, images, XHR — is
// intercepted and refused if it targets a private, loopback or link-local
// address, so a page cannot be used to reach the server's internal services.
package browser

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
	"github.com/chromedp/chromedp/remote"
)

const (
	MaxActions   = 50
	MaxTotalWait = 60 * time.Second
)

// Action is one Firecrawl action.
type Action struct {
	Type         string    `json:"type"`
	Milliseconds int       `json:"milliseconds,omitempty"`
	Selector     string    `json:"selector,omitempty"`
	All          bool      `json:"all,omitempty"`
	FullPage     bool      `json:"fullPage,omitempty"`
	Quality      int       `json:"quality,omitempty"`
	Viewport     *Viewport `json:"viewport,omitempty"`
	Text         string    `json:"text,omitempty"`
	Key          string    `json:"key,omitempty"`
	Direction    string    `json:"direction,omitempty"`
	Script       string    `json:"script,omitempty"`
	Landscape    bool      `json:"landscape,omitempty"`
	Scale        float64   `json:"scale,omitempty"`
	Format       string    `json:"format,omitempty"`
}

type Viewport struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

// Screenshot asks for a screenshot of the final page (the screenshot format).
type Screenshot struct {
	FullPage bool
	Quality  int
	Viewport *Viewport
}

type Options struct {
	WaitFor    time.Duration
	Actions    []Action
	Mobile     bool
	Screenshot *Screenshot
	Timeout    time.Duration
}

// ActionsResult is Firecrawl's document.actions.
type ActionsResult struct {
	Screenshots       []string       `json:"screenshots"`
	Scrapes           []ActionScrape `json:"scrapes"`
	JavascriptReturns []JSReturn     `json:"javascriptReturns"`
	PDFs              []string       `json:"pdfs"`
}

type ActionScrape struct {
	URL  string `json:"url"`
	HTML string `json:"html"`
}

type JSReturn struct {
	Type  string `json:"type"`
	Value any    `json:"value"`
}

type Result struct {
	HTML       string
	Status     int
	FinalURL   string
	Screenshot string // data URL
	Actions    *ActionsResult
	// Blocked lists requests the page tried to make to internal addresses.
	Blocked []string
}

// Browser talks to Chrome's DevTools endpoint (e.g. "browser:9222").
type Browser struct {
	addr     string
	resolver *net.Resolver
	sem      chan struct{}
	http     *http.Client
	// Timezone the page sees (IANA name); set it to the node IP's zone.
	Timezone string

	verOnce     sync.Once
	major, full string
}

func New(addr string, maxTabs int, resolver *net.Resolver) *Browser {
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	return &Browser{addr: addr, resolver: resolver, sem: make(chan struct{}, max(maxTabs, 1)), http: &http.Client{Timeout: 5 * time.Second}}
}

// Validate checks actions before any work starts.
func Validate(actions []Action, waitFor time.Duration) error {
	if len(actions) > MaxActions {
		return fmt.Errorf("number of actions cannot exceed %d", MaxActions)
	}
	total := waitFor
	for i, a := range actions {
		switch a.Type {
		case "wait":
			if (a.Milliseconds > 0) == (a.Selector != "") {
				return fmt.Errorf("action %d: wait needs either milliseconds or selector, not both", i)
			}
			total += time.Duration(a.Milliseconds) * time.Millisecond
		case "click", "scroll", "scrape", "screenshot", "pdf":
		case "write":
			if a.Text == "" {
				return fmt.Errorf("action %d: write needs text", i)
			}
		case "press":
			if a.Key == "" {
				return fmt.Errorf("action %d: press needs key", i)
			}
		case "executeJavascript":
			if a.Script == "" {
				return fmt.Errorf("action %d: executeJavascript needs script", i)
			}
		default:
			return fmt.Errorf("action %d: unknown type %q", i, a.Type)
		}
		if a.Type == "click" && a.Selector == "" {
			return fmt.Errorf("action %d: click needs selector", i)
		}
	}
	if total > MaxTotalWait {
		return fmt.Errorf("total wait time (waitFor + wait actions) cannot exceed %s", MaxTotalWait)
	}
	return nil
}

// devtoolsHost resolves the DevTools address to ip:port: DevTools refuses
// Host headers that are not IPs.
func (b *Browser) devtoolsHost(ctx context.Context) (string, error) {
	host, port, err := net.SplitHostPort(b.addr)
	if err != nil {
		return "", err
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil || len(ips) == 0 {
		return "", fmt.Errorf("browser %s: %v", b.addr, err)
	}
	return net.JoinHostPort(ips[0].IP.String(), port), nil
}

// wsURL finds Chrome's browser websocket.
func (b *Browser) wsURL(ctx context.Context) (string, error) {
	hp, err := b.devtoolsHost(ctx)
	if err != nil {
		return "", err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+hp+"/json/version", nil)
	resp, err := b.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("browser unreachable: %w", err)
	}
	defer resp.Body.Close()
	var v struct {
		WS string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&v); err != nil || v.WS == "" {
		return "", errors.New("browser: no websocket URL")
	}
	u, err := url.Parse(v.WS)
	if err != nil {
		return "", err
	}
	u.Host = hp
	return u.String(), nil
}

// Available reports whether Chrome answers.
func (b *Browser) Available(ctx context.Context) bool {
	_, err := b.wsURL(ctx)
	return err == nil
}

// allowed reports whether the page may load rawURL.
func (b *Browser) allowed(ctx context.Context, rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	switch u.Scheme {
	case "data", "blob", "about":
		return true
	case "http", "https", "ws", "wss":
	default:
		return false
	}
	host := u.Hostname()
	if ip := net.ParseIP(host); ip != nil {
		return !Blocked(ip)
	}
	ips, err := b.resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return false
	}
	for _, ip := range ips {
		if Blocked(ip.IP) {
			return false
		}
	}
	return true
}

// Blocked mirrors scrape's internal-address rule.
func Blocked(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() ||
		(ip.To4() != nil && ip.To4()[0] == 100 && ip.To4()[1]&0xc0 == 64)
}

// ErrAddressNotAllowed means the page itself points at an internal address.
var ErrAddressNotAllowed = errors.New("address not allowed")

// Render loads rawURL, runs actions, and returns the final HTML.
func (b *Browser) Render(ctx context.Context, rawURL string, opts Options) (*Result, error) {
	if !b.allowed(ctx, rawURL) {
		return nil, ErrAddressNotAllowed
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 45 * time.Second
	}
	select {
	case b.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-b.sem }()

	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	ws, err := b.wsURL(ctx)
	if err != nil {
		return nil, err
	}
	actx, acancel := remote.NewAllocator(ctx, ws, remote.NoModifyURL)
	defer acancel()
	tctx, tcancel := chromedp.NewContext(actx)
	defer tcancel()

	// Attach to a new tab, then subscribe before anything loads.
	if err := chromedp.Do(tctx); err != nil {
		return nil, fmt.Errorf("browser: %w", err)
	}
	paused := chromedp.Events(tctx, fetch.RequestPaused)
	responses := chromedp.Events(tctx, network.ResponseReceived)

	res := &Result{}
	var mu sync.Mutex
	go func() {
		for e, err := range paused {
			if err != nil {
				return
			}
			go func() {
				if b.allowed(tctx, e.Request.URL) {
					chromedp.Call(tctx, fetch.ContinueRequest, fetch.ContinueRequestParams{RequestID: e.RequestID})
				} else {
					chromedp.Call(tctx, fetch.FailRequest, fetch.FailRequestParams{RequestID: e.RequestID, ErrorReason: network.ErrorReasonBlockedByClient})
					mu.Lock()
					res.Blocked = append(res.Blocked, e.Request.URL)
					mu.Unlock()
				}
			}()
		}
	}()
	go func() {
		for e, err := range responses {
			if err != nil {
				return
			}
			if e.Type == network.ResourceTypeDocument && e.Response != nil {
				mu.Lock()
				if res.Status == 0 {
					res.Status = int(e.Response.Status) // first document response = the page
				}
				mu.Unlock()
			}
		}
	}()

	if _, err := chromedp.Call(tctx, fetch.Enable, fetch.EnableParams{Patterns: []*fetch.RequestPattern{{URLPattern: "*"}}}); err != nil {
		return nil, fmt.Errorf("browser: %w", err)
	}
	chromedp.Call(tctx, network.Enable, network.EnableParams{})
	if err := b.harden(tctx, b.profile(ctx, opts.Mobile)); err != nil {
		return nil, fmt.Errorf("browser setup: %w", err)
	}
	if err := chromedp.Do(tctx, chromedp.Navigate(rawURL)); err != nil {
		return nil, fmt.Errorf("browser navigate: %w", err)
	}
	settle := 800 * time.Millisecond // let client-side rendering finish
	if opts.WaitFor > 0 {
		settle = opts.WaitFor
	}
	chromedp.Do(tctx, chromedp.Sleep(settle))

	if len(opts.Actions) > 0 {
		res.Actions = &ActionsResult{Screenshots: []string{}, Scrapes: []ActionScrape{}, JavascriptReturns: []JSReturn{}, PDFs: []string{}}
		for i, a := range opts.Actions {
			if err := runAction(tctx, a, res.Actions); err != nil {
				return nil, fmt.Errorf("action %d (%s): %w", i, a.Type, err)
			}
		}
	}
	if opts.Screenshot != nil {
		shot, err := screenshot(tctx, opts.Screenshot.FullPage, opts.Screenshot.Quality, opts.Screenshot.Viewport)
		if err != nil {
			return nil, fmt.Errorf("screenshot: %w", err)
		}
		res.Screenshot = shot
	}
	html, err := chromedp.Run(tctx, chromedp.OuterHTML(chromedp.CSS("html")))
	if err != nil {
		return nil, fmt.Errorf("read page: %w", err)
	}
	loc, _ := chromedp.Run(tctx, chromedp.Location())
	mu.Lock()
	res.HTML, res.FinalURL = html, loc
	if res.Status == 0 {
		res.Status = 200
	}
	mu.Unlock()
	return res, nil
}

var keys = map[string]string{
	"enter": kb.Enter, "tab": kb.Tab, "escape": kb.Escape, "esc": kb.Escape, "backspace": kb.Backspace,
	"delete": kb.Delete, "arrowup": kb.ArrowUp, "arrowdown": kb.ArrowDown, "arrowleft": kb.ArrowLeft,
	"arrowright": kb.ArrowRight, "pageup": kb.PageUp, "pagedown": kb.PageDown, "home": kb.Home, "end": kb.End,
	"space": " ",
}

func runAction(ctx context.Context, a Action, out *ActionsResult) error {
	switch a.Type {
	case "wait":
		if a.Selector != "" {
			return chromedp.Do(ctx, chromedp.WaitVisible(chromedp.CSS(a.Selector)))
		}
		return chromedp.Do(ctx, chromedp.Sleep(time.Duration(a.Milliseconds)*time.Millisecond))
	case "click":
		if a.All {
			_, err := chromedp.Run(ctx, chromedp.Evaluate[int](fmt.Sprintf(`(() => { const els = document.querySelectorAll(%q); els.forEach(e => e.click()); return els.length; })()`, a.Selector)))
			return err
		}
		return chromedp.Do(ctx, chromedp.Click(chromedp.CSS(a.Selector), chromedp.NodeVisible))
	case "write":
		return chromedp.Do(ctx, chromedp.KeyEvent(a.Text))
	case "press":
		k, ok := keys[strings.ToLower(a.Key)]
		if !ok {
			k = a.Key
		}
		return chromedp.Do(ctx, chromedp.KeyEvent(k))
	case "scroll":
		dir := 1
		if a.Direction == "up" {
			dir = -1
		}
		target := "window"
		if a.Selector != "" {
			target = fmt.Sprintf("document.querySelector(%q)", a.Selector)
		}
		_, err := chromedp.Run(ctx, chromedp.Evaluate[bool](fmt.Sprintf(`(() => { const t = %s; if (!t) return false; t.scrollBy(0, %d * (t.innerHeight || t.clientHeight || 800)); return true; })()`, target, dir)))
		return err
	case "screenshot":
		shot, err := screenshot(ctx, a.FullPage, a.Quality, a.Viewport)
		if err != nil {
			return err
		}
		out.Screenshots = append(out.Screenshots, shot)
	case "scrape":
		html, err := chromedp.Run(ctx, chromedp.OuterHTML(chromedp.CSS("html")))
		if err != nil {
			return err
		}
		loc, _ := chromedp.Run(ctx, chromedp.Location())
		out.Scrapes = append(out.Scrapes, ActionScrape{URL: loc, HTML: html})
	case "executeJavascript":
		v, err := chromedp.Run(ctx, chromedp.Evaluate[any](a.Script, chromedp.EvalAwaitPromise))
		if err != nil {
			return err
		}
		out.JavascriptReturns = append(out.JavascriptReturns, JSReturn{Type: jsType(v), Value: v})
	case "pdf":
		opts := []chromedp.PDFOption{chromedp.PDFPaper(paper(a.Format)), chromedp.PDFPrintBackground()}
		if a.Landscape {
			opts = append(opts, chromedp.PDFLandscape())
		}
		if a.Scale > 0 {
			opts = append(opts, chromedp.PDFScale(a.Scale))
		}
		buf, err := chromedp.Run(ctx, chromedp.PrintToPDF(opts...))
		if err != nil {
			return err
		}
		out.PDFs = append(out.PDFs, "data:application/pdf;base64,"+base64.StdEncoding.EncodeToString(buf))
	}
	return nil
}

func screenshot(ctx context.Context, fullPage bool, quality int, vp *Viewport) (string, error) {
	if vp != nil && vp.Width > 0 && vp.Height > 0 {
		if err := chromedp.Do(ctx, chromedp.EmulateViewport(int64(vp.Width), int64(vp.Height))); err != nil {
			return "", err
		}
	}
	jpeg := quality > 0 && quality < 100
	mime := "image/png"
	if jpeg {
		mime = "image/jpeg"
	}
	var buf []byte
	var err error
	if fullPage {
		q := quality
		if !jpeg {
			q = 100 // FullScreenshot: 100 means PNG
		}
		buf, err = chromedp.Run(ctx, chromedp.FullScreenshot(q))
	} else {
		params := page.CaptureScreenshotParams{Format: page.CaptureScreenshotFormatPng}
		if jpeg {
			q := int64(quality)
			params = page.CaptureScreenshotParams{Format: page.CaptureScreenshotFormatJpeg, Quality: &q}
		}
		var r page.CaptureScreenshotResult
		r, err = chromedp.Call(ctx, page.CaptureScreenshot, params)
		buf = r.Data
	}
	if err != nil {
		return "", err
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(buf), nil
}

// paper maps Firecrawl's pdf formats to sizes in inches.
func paper(f string) chromedp.PaperSize {
	switch strings.ToUpper(f) {
	case "A0":
		return chromedp.PaperSize{Width: 33.11, Height: 46.81}
	case "A1":
		return chromedp.PaperSize{Width: 23.39, Height: 33.11}
	case "A2":
		return chromedp.PaperSize{Width: 16.54, Height: 23.39}
	case "A3":
		return chromedp.PaperA3
	case "A4":
		return chromedp.PaperA4
	case "A5":
		return chromedp.PaperA5
	case "A6":
		return chromedp.PaperSize{Width: 4.13, Height: 5.83}
	case "LEGAL":
		return chromedp.PaperLegal
	case "TABLOID":
		return chromedp.PaperTabloid
	case "LEDGER":
		return chromedp.PaperSize{Width: 17, Height: 11}
	}
	return chromedp.PaperLetter
}

func jsType(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case float64, int:
		return "number"
	case bool:
		return "boolean"
	case []any:
		return "array"
	}
	return "object"
}

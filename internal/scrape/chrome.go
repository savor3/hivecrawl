package scrape

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	fhttp "github.com/bogdanfinn/fhttp"
	tlsclient "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

// Fetcher is one way of fetching a page: a standard Go client (tests) or a
// client that looks like Chrome on the wire.
type Fetcher interface {
	get(ctx context.Context, u *url.URL) (*fetched, error)
}

type stdFetcher struct{ c *http.Client }

// StdClient wraps a net/http client.
func StdClient(c *http.Client) Fetcher { return stdFetcher{c} }

func (s stdFetcher) get(ctx context.Context, u *url.URL) (*fetched, error) {
	return fetchHTTP(ctx, s.c, u)
}

// Plain fetches present one consistent Chrome: the TLS handshake, HTTP/2
// settings and header order of Chrome 152 (tls-client's profile), with the
// user agent and client hints of the same version. A Chrome user agent over
// Go's own TLS fingerprint is an easy bot signal.
const chromeMajor = "152"

var chromeProfile = profiles.Chrome_152

var chromeHeaders = [][2]string{
	{"sec-ch-ua", `"Chromium";v="` + chromeMajor + `", "Google Chrome";v="` + chromeMajor + `", "Not_A Brand";v="24"`},
	{"sec-ch-ua-mobile", "?0"},
	{"sec-ch-ua-platform", `"Linux"`},
	{"upgrade-insecure-requests", "1"},
	{"user-agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/" + chromeMajor + ".0.0.0 Safari/537.36"},
	{"accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7"},
	{"sec-fetch-site", "none"},
	{"sec-fetch-mode", "navigate"},
	{"sec-fetch-user", "?1"},
	{"sec-fetch-dest", "document"},
	{"accept-encoding", "gzip, deflate, br, zstd"},
	{"accept-language", "en-US,en;q=0.9"},
	{"priority", "u=0, i"},
}

type chromeFetcher struct{ c tlsclient.HttpClient }

// ChromeClient builds a Chrome-fingerprint fetcher whose every connection
// goes through dial (which must refuse internal addresses). HTTP/3 is off:
// QUIC would bypass dial.
func ChromeClient(dial func(ctx context.Context, network, addr string) (net.Conn, error)) (Fetcher, error) {
	c, err := tlsclient.NewHttpClient(tlsclient.NewNoopLogger(),
		tlsclient.WithClientProfile(chromeProfile),
		tlsclient.WithDialContext(dial),
		tlsclient.WithDisableHttp3(),
		tlsclient.WithTimeoutSeconds(60),
		tlsclient.WithCustomRedirectFunc(func(req *fhttp.Request, via []*fhttp.Request) error {
			if len(via) >= maxRedirects {
				return errors.New("too many redirects")
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return errors.New("redirect to unsupported scheme")
			}
			return nil
		}),
	)
	if err != nil {
		return nil, err
	}
	return chromeFetcher{c}, nil
}

func (f chromeFetcher) get(ctx context.Context, u *url.URL) (*fetched, error) {
	req, err := fhttp.NewRequestWithContext(ctx, fhttp.MethodGet, u.String(), nil)
	if err != nil {
		return nil, &Error{"SCRAPE_INVALID_URL", err.Error()}
	}
	order := make([]string, 0, len(chromeHeaders))
	for _, h := range chromeHeaders {
		req.Header.Set(h[0], h[1])
		order = append(order, h[0])
	}
	req.Header[fhttp.HeaderOrderKey] = order
	resp, err := f.c.Do(req)
	if err != nil {
		var dnsErr *net.DNSError
		switch {
		case errors.Is(err, errBlockedAddr) || strings.Contains(err.Error(), errBlockedAddr.Error()):
			return nil, &Error{"SCRAPE_ADDRESS_NOT_ALLOWED", "this URL resolves to a private or internal address"}
		case (errors.As(err, &dnsErr) && dnsErr.IsNotFound) || strings.Contains(err.Error(), "no such host"):
			return nil, &Error{"SCRAPE_DNS_RESOLUTION_ERROR", "could not resolve " + u.Hostname()}
		case errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil:
			return nil, &Error{"SCRAPE_TIMEOUT", "scrape timed out"}
		}
		return nil, &Error{"SCRAPE_FETCH_FAILED", err.Error()}
	}
	defer resp.Body.Close()
	body := io.ReadCloser(resp.Body)
	if !resp.Uncompressed && resp.Header.Get("Content-Encoding") != "" {
		body = fhttp.DecompressBody(resp)
	}
	data, err := io.ReadAll(io.LimitReader(body, maxBody))
	if err != nil {
		return nil, &Error{"SCRAPE_FETCH_FAILED", err.Error()}
	}
	final := u
	if resp.Request != nil && resp.Request.URL != nil {
		final = resp.Request.URL
	}
	return &fetched{body: data, status: resp.StatusCode, contentType: resp.Header.Get("Content-Type"), finalURL: final, proxy: "basic"}, nil
}

// guardedDial wraps a dialer so connections to internal addresses fail.
func guardedDial(d *net.Dialer) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return Client(d).Transport.(*http.Transport).DialContext
}

// ChromeDirect is the node's default page fetcher.
func ChromeDirect(d *net.Dialer) (Fetcher, error) { return ChromeClient(guardedDial(d)) }

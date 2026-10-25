package scrape

import (
	"context"
	"encoding/json"
	"net"
	"net/url"
	"os"
	"strings"
	"testing"
)

func TestChromeClientRefusesInternalAddresses(t *testing.T) {
	f, err := ChromeDirect(&net.Dialer{})
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"http://127.0.0.1:9/", "https://169.254.169.254/latest/", "http://localhost:9/"} {
		u, _ := url.Parse(raw)
		_, err := f.get(context.Background(), u)
		var se *Error
		if !asErr(err, &se) || se.Code != "SCRAPE_ADDRESS_NOT_ALLOWED" {
			t.Errorf("%s: want SCRAPE_ADDRESS_NOT_ALLOWED, got %v", raw, err)
		}
	}
}

// Network test: HC_NET_TESTS=1 go test -run TestChromeFingerprint -v ./internal/scrape/
func TestChromeFingerprint(t *testing.T) {
	if os.Getenv("HC_NET_TESTS") == "" {
		t.Skip("set HC_NET_TESTS=1")
	}
	f, err := ChromeDirect(&net.Dialer{})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse("https://tls.peet.ws/api/all")
	got, err := f.get(context.Background(), u)
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		HTTPVersion string `json:"http_version"`
		UserAgent   string `json:"user_agent"`
		TLS         struct {
			JA4 string `json:"ja4"`
		} `json:"tls"`
		HTTP2 struct {
			Akamai string `json:"akamai_fingerprint"`
		} `json:"http2"`
	}
	if err := json.Unmarshal(got.body, &d); err != nil {
		t.Fatalf("decode (decompression broken?): %v %.200s", err, got.body)
	}
	t.Logf("http=%s ja4=%s akamai=%s ua=%s", d.HTTPVersion, d.TLS.JA4, d.HTTP2.Akamai, d.UserAgent)
	if d.HTTPVersion != "h2" || !strings.HasPrefix(d.TLS.JA4, "t13d") || !strings.Contains(d.UserAgent, "Chrome/"+chromeMajor) {
		t.Fatal("does not look like Chrome")
	}
	// Go's own client: "Go-http-client" UA, akamai 1:65536;... differs from Chrome's 1:65536;2:0;4:6291456;6:262144
	if !strings.HasPrefix(d.HTTP2.Akamai, "1:65536;2:0;4:6291456;6:262144") {
		t.Fatalf("HTTP/2 settings are not Chrome's: %s", d.HTTP2.Akamai)
	}
}

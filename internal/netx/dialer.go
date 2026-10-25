// Package netx builds HTTP clients whose DNS lookups go to public resolvers
// instead of the ISP or cloud provider's resolver.
package netx

import (
	"context"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

// PublicDialer returns a dialer that resolves hostnames via the given DNS
// servers ("1.1.1.1:53"), rotating between them. If servers is empty the
// system resolver is used.
func PublicDialer(servers []string) *net.Dialer {
	d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	if len(servers) == 0 {
		return d
	}
	var next atomic.Uint32
	d.Resolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			srv := servers[int(next.Add(1))%len(servers)]
			var nd net.Dialer
			return nd.DialContext(ctx, network, srv)
		},
	}
	return d
}

// Client returns an HTTP client using the given dialer.
func Client(d *net.Dialer, timeout time.Duration) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = d.DialContext
	tr.MaxIdleConnsPerHost = 8
	return &http.Client{Transport: tr, Timeout: timeout}
}

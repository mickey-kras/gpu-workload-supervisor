// Package httptransport provides private transports for trusted runtime endpoints.
package httptransport

import (
	"net"
	"net/http"
	"time"
)

// DefaultResponseHeaderTimeout allows model loading and non-streaming inference
// while bounding requests to an upstream that never returns response headers.
const DefaultResponseHeaderTimeout = 10 * time.Minute

// NewDirect returns a transport that never inherits ambient HTTP proxies or the
// mutable http.DefaultTransport. Response bodies have no total deadline:
// execution can stream indefinitely; health clients set their own timeout.
func NewDirect() *http.Transport {
	return &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: DefaultResponseHeaderTimeout,
		ExpectContinueTimeout: time.Second,
	}
}

// Package httptransport provides private transports for trusted runtime endpoints.
package httptransport

import (
	"net"
	"net/http"
	"time"
)

// NewDirect returns a transport that never inherits ambient HTTP proxies or the
// mutable http.DefaultTransport. Response headers and bodies have no total
// deadline: execution can stream indefinitely; health clients set their own timeout.
func NewDirect() *http.Transport {
	return &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}

package proxy

import (
	"io"
	"net/http"
)

// ReverseProxy normalizes empty bodies to nil, enabling transport replay even
// for executing GET routes. Restore a non-rewindable body after that step.
type executionTransport struct{ base http.RoundTripper }

func (t executionTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	forwarded := request.Clone(request.Context())
	forwarded.GetBody = nil
	forwarded.Header.Del("Idempotency-Key")
	forwarded.Header.Del("X-Idempotency-Key")
	if forwarded.Body == nil || forwarded.Body == http.NoBody {
		forwarded.Body = io.NopCloser(http.NoBody)
		// A known-empty body needs no chunk terminator for upstream to read.
		forwarded.TransferEncoding = []string{"identity"}
	}
	return t.base.RoundTrip(forwarded)
}

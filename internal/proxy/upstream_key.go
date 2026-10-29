package proxy

import (
	"errors"
	"net/http"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/vllmkey"
)

var errUpstreamKey = errors.New("native model API key is unavailable")

// upstreamKeyTransport sends the vLLM API key to the loopback upstream. It
// reads the private key file for each request, so a new key applies without a
// restart, and it replaces any Authorization header from the client.
type upstreamKeyTransport struct {
	keyFile string
	base    http.RoundTripper
}

func (t upstreamKeyTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	key, err := vllmkey.Read(t.keyFile)
	if err != nil {
		if request.Body != nil {
			_ = request.Body.Close()
		}
		return nil, errUpstreamKey
	}
	forwarded := request.Clone(request.Context())
	forwarded.Header = request.Header.Clone()
	forwarded.Header.Set("Authorization", "Bearer "+key)
	return t.base.RoundTrip(forwarded)
}

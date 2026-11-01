package proxy

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/vllmkey"
)

var errUpstreamKey = errors.New("native model API key is not sent")

// Test hook.
var checkUpstreamListener = vllmkey.CheckListener

// upstreamKeyTransport sends the vLLM API key to the loopback upstream. It
// reads the private key file for each request, so a new key applies without a
// restart, and it replaces any Authorization header from the client. It sends
// the key only when the desktop user holds the upstream port.
type upstreamKeyTransport struct {
	keyFile string
	base    http.RoundTripper
}

func (t upstreamKeyTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	key, err := vllmkey.Read(t.keyFile)
	if err == nil {
		err = checkUpstreamListener("http://" + request.URL.Host)
	}
	if err != nil {
		if request.Body != nil {
			_ = request.Body.Close()
		}
		return nil, fmt.Errorf("%w: %w", errUpstreamKey, err)
	}
	// A shallow copy keeps the request's trailer map, which the ingress
	// transport fills while the body is read.
	forwarded := request.WithContext(request.Context())
	forwarded.Header = request.Header.Clone()
	forwarded.Header.Set("Authorization", "Bearer "+key)
	return t.base.RoundTrip(forwarded)
}

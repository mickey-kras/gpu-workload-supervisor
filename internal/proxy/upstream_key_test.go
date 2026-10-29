package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/vllmkey"
)

var proxyTestKey = strings.Repeat("p", 43)

// proxyKeyFile writes the private vLLM API key file of a test binding.
func proxyKeyFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "vllm.env")
	if err := os.WriteFile(path, []byte(vllmkey.Name+"="+proxyTestKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

type recordingTransport struct{ headers []http.Header }

func (r *recordingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	r.headers = append(r.headers, request.Header.Clone())
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("{}")), Header: http.Header{}, Request: request}, nil
}

func TestUpstreamKeyTransportReplacesClientAuthorization(t *testing.T) {
	base := &recordingTransport{}
	transport := upstreamKeyTransport{keyFile: proxyKeyFile(t), base: base}
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:9000/v1/models", nil)
	request.Header.Set("Authorization", "Bearer client-secret")
	if _, err := transport.RoundTrip(request); err != nil {
		t.Fatal(err)
	}
	if len(base.headers) != 1 || base.headers[0].Get("Authorization") != "Bearer "+proxyTestKey {
		t.Fatalf("upstream headers = %v", base.headers)
	}
	if request.Header.Get("Authorization") != "Bearer client-secret" {
		t.Fatal("the original request changed")
	}
}

func TestUpstreamKeyTransportFailsClosedWithoutKey(t *testing.T) {
	base := &recordingTransport{}
	transport := upstreamKeyTransport{keyFile: filepath.Join(t.TempDir(), "missing.env"), base: base}
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:9000/v1/completions", strings.NewReader("{}"))
	if _, err := transport.RoundTrip(request); !errors.Is(err, errUpstreamKey) {
		t.Fatalf("error = %v", err)
	}
	if len(base.headers) != 0 {
		t.Fatal("request without the key reached the upstream")
	}
}

func TestNativeReadRouteSendsUpstreamKey(t *testing.T) {
	var seen string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{"data":[{"id":"selected"}]}`)
	}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	s := &nativeStore{fakeStore: fakeStore{state: admittedState(control.OwnerSupervisor)}, catalog: control.CatalogSnapshot{Revision: "one", Catalog: control.Catalog{Version: 1, Profiles: []control.WorkloadProfile{{ID: "media", NativeModel: &control.NativeModel{Runtime: "vllm", Model: "selected", Endpoint: upstream.URL, APIKeyFile: proxyKeyFile(t)}}}}}}
	h, err := NewWithContext(context.Background(), s, Config{Upstream: u, Workload: "media", ExecutionRoutes: []Route{{Method: "POST", Path: "/v1/completions"}}, ReadOnlyRoutes: []Route{{Method: "GET", Path: "/v1/models"}}})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("Authorization", "Bearer client-secret")
	response := httptest.NewRecorder()
	h.ServeHTTP(response, request)
	if response.Code != http.StatusOK || seen != "Bearer "+proxyTestKey {
		t.Fatalf("status %d, upstream Authorization %q", response.Code, seen)
	}
}

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

// Read routes copy request trailers while the body streams, so the key
// transport must keep the trailer map of the request that it forwards.
func TestAdapterNativeProxyKeepsTrailersAndSendsOnlyTheKey(t *testing.T) {
	var authorization, trailer string
	fixture := newTerminalFixtureWith(t, "vllm", "/v1/completions", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		authorization, trailer = r.Header.Get("Authorization"), r.Trailer.Get("X-Result")
		_, _ = io.WriteString(w, `{"data":[{"id":"selected"}]}`)
	}))
	target, _ := url.Parse(fixture.upstream.URL)
	config := adapterConfig(target)
	config.ExecutionRoutes = []Route{{"POST", "/v1/completions"}}
	config.ReadOnlyRoutes, config.PassthroughRoutes = []Route{{"GET", "/v1/models"}}, nil
	handler, err := NewWithContext(context.Background(), fixture.store, config)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	request, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/models", strings.NewReader("payload"))
	request.ContentLength = -1
	request.Header.Set(ExecutionCredentialHeader, executionCredential)
	request.Header.Set("Authorization", "Bearer client-secret")
	request.Trailer = http.Header{"X-Result": {"safe-input"}}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || authorization != "Bearer "+proxyTestKey || trailer != "safe-input" {
		t.Fatalf("status %d, upstream Authorization %q, trailer %q", response.StatusCode, authorization, trailer)
	}
}

func TestNativeProxyRequiresKeyFileAtStart(t *testing.T) {
	upstream := httptest.NewServer(http.NotFoundHandler())
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	s := &nativeStore{fakeStore: fakeStore{state: admittedState(control.OwnerSupervisor)}, catalog: control.CatalogSnapshot{Revision: "one", Catalog: control.Catalog{Version: 1, Profiles: []control.WorkloadProfile{{ID: "media", NativeModel: &control.NativeModel{Runtime: "vllm", Model: "selected", Endpoint: upstream.URL}}}}}}
	if _, err := NewWithContext(context.Background(), s, Config{Upstream: u, Workload: "media", ExecutionRoutes: []Route{{Method: "POST", Path: "/v1/completions"}}}); err == nil || !strings.Contains(err.Error(), "run setup again") {
		t.Fatalf("binding without a key file accepted: %v", err)
	}
}

// nativeKeyHandler returns a handler for a native binding with the upstream
// endpoint and key file.
func nativeKeyHandler(t *testing.T, endpoint, keyFile string) *Handler {
	t.Helper()
	u, _ := url.Parse(endpoint)
	s := &nativeStore{fakeStore: fakeStore{state: admittedState(control.OwnerSupervisor)}, catalog: control.CatalogSnapshot{Revision: "one", Catalog: control.Catalog{Version: 1, Profiles: []control.WorkloadProfile{{ID: "media", NativeModel: &control.NativeModel{Runtime: "vllm", Model: "selected", Endpoint: endpoint, EnvironmentFile: keyFile}}}}}}
	h, err := NewWithContext(context.Background(), s, Config{Upstream: u, Workload: "media", ExecutionRoutes: []Route{{Method: "POST", Path: "/v1/completions"}}, ReadOnlyRoutes: []Route{{Method: "GET", Path: "/v1/models"}}})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestNativeReadRouteSendsUpstreamKey(t *testing.T) {
	var seen string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{"data":[{"id":"selected"}]}`)
	}))
	defer upstream.Close()
	h := nativeKeyHandler(t, upstream.URL, proxyKeyFile(t))
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("Authorization", "Bearer client-secret")
	response := httptest.NewRecorder()
	h.ServeHTTP(response, request)
	if response.Code != http.StatusOK || seen != "Bearer "+proxyTestKey {
		t.Fatalf("status %d, upstream Authorization %q", response.Code, seen)
	}
}

// A custom transport would skip the peer check, so a native binding refuses it.
func TestNativeProxyRefusesCustomTransport(t *testing.T) {
	u, _ := url.Parse("http://127.0.0.1:9000")
	s := &nativeStore{fakeStore: fakeStore{state: admittedState(control.OwnerSupervisor)}, catalog: control.CatalogSnapshot{Revision: "one", Catalog: control.Catalog{Version: 1, Profiles: []control.WorkloadProfile{{ID: "media", NativeModel: &control.NativeModel{Runtime: "vllm", Model: "selected", Endpoint: u.String(), EnvironmentFile: proxyKeyFile(t)}}}}}}
	_, err := NewWithContext(context.Background(), s, Config{Upstream: u, Workload: "media", ExecutionRoutes: []Route{{Method: "POST", Path: "/v1/completions"}}, Transport: &recordingTransport{}})
	if err == nil || !strings.Contains(err.Error(), "built-in upstream transport") {
		t.Fatalf("custom transport accepted: %v", err)
	}
}

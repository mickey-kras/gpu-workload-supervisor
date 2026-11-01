package setup

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func probeServer(t *testing.T, responses map[string]string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.RawQuery != "" {
			t.Errorf("mutating discovery request: %s %s", r.Method, r.URL)
			w.WriteHeader(400)
			return
		}
		body, ok := responses[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server
}
func TestProbeNativeInventories(t *testing.T) {
	tests := []struct {
		app       string
		responses map[string]string
		count     int
		inventory string
	}{
		{"comfyui", map[string]string{"/system_stats": `{"system":{"comfyui_version":"0.3.60"},"devices":[]}`}, 0, "not-applicable"},
		{"vllm", map[string]string{"/version": `{"version":"0.10.0"}`, "/v1/models": `{"data":[{"id":"alias-a","root":"/models/a"},{"id":"alias-b","root":"/models/a"}]}`}, 1, "available"},
	}
	for _, tt := range tests {
		t.Run(tt.app, func(t *testing.T) {
			s := probeServer(t, tt.responses)
			got, err := Probe(context.Background(), ProbeRequest{App: tt.app, Endpoint: s.URL})
			if err != nil {
				t.Fatal(err)
			}
			if got.InventoryStatus != tt.inventory || len(got.Models) != tt.count || got.LifecycleControl != "unverified" {
				t.Fatalf("%+v", got)
			}
			if tt.app == "vllm" && len(got.Models[0].Aliases) != 2 {
				t.Fatal(got.Models)
			}
		})
	}
}
func TestProbeUnknownInventoryAndInvalidRequests(t *testing.T) {
	for _, body := range []string{`{}`, `{"data":null}`, `{"data":[{}]}`, `{"data":[]}{}`, `bad`, strings.Repeat(" ", 1048577)} {
		s := probeServer(t, map[string]string{"/version": `{"version":"0.12.0"}`, "/v1/models": body})
		got, err := Probe(context.Background(), ProbeRequest{App: "vllm", Endpoint: s.URL})
		if err != nil || got.InventoryStatus != "invalid" {
			t.Fatalf("%+v %v", got, err)
		}
	}
	for _, request := range []ProbeRequest{{App: "other", Endpoint: "http://127.0.0.1"}, {App: "ollama", Endpoint: "http://127.0.0.1:11434"}, {App: "llama.cpp", Endpoint: "http://127.0.0.1:8080"}, {App: "vllm", Endpoint: "http://example.com"}, {App: "vllm", Endpoint: "http://user@127.0.0.1"}, {App: "vllm", Endpoint: "http://127.0.0.1/?reload=1"}, {App: "vllm", Reference: "relative", ReferenceKind: "model-file"}, {App: "vllm", Endpoint: "http://127.0.0.1", Reference: "/a", ReferenceKind: "model-file"}} {
		if _, err := Probe(context.Background(), request); err == nil {
			t.Fatalf("accepted %+v", request)
		}
	}
}
func TestProbeReferencesDoNotReadOrWriteModels(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "model")
	before := []byte("not actually a model")
	if err := os.WriteFile(file, before, 0600); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []struct{ path, kind string }{{dir, "model-directory"}, {file, "configuration"}, {file, "application"}} {
		got, err := Probe(context.Background(), ProbeRequest{App: "vllm", Reference: ref.path, ReferenceKind: ref.kind})
		status := "candidate"
		if ref.kind == "application" {
			status = "inspection-failed"
		}
		if err != nil || got.InstanceStatus != status || got.LifecycleControl != "unverified" {
			t.Fatalf("%+v %v", got, err)
		}
	}
	data, _ := os.ReadFile(file)
	if string(data) != string(before) {
		t.Fatal("model changed")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatal("filesystem changed")
	}
	got, err := Probe(context.Background(), ProbeRequest{App: "vllm", Reference: filepath.Join(dir, "missing"), ReferenceKind: "model-directory"})
	if err != nil || got.InstanceStatus != "missing" {
		t.Fatalf("%+v %v", got, err)
	}
}
func TestProbeCancellationAndRedirect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := Probe(ctx, ProbeRequest{App: "vllm", Endpoint: server.URL})
	if err == nil || time.Since(start) > time.Second {
		t.Fatal("cancellation not propagated", err)
	}
	hits := 0
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	defer destination.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, destination.URL, http.StatusFound) }))
	defer redirect.Close()
	got, err := Probe(context.Background(), ProbeRequest{App: "vllm", Endpoint: redirect.URL})
	if err != nil || hits != 0 || got.InventoryStatus == "available" {
		t.Fatalf("redirect followed: %+v %v hits=%d", got, err, hits)
	}
}
func TestDecodeProbeBounds(t *testing.T) {
	for _, raw := range []string{`{}`, `{"app":"vllm","endpoint":"http://127.0.0.1","extra":true}`, `{"app":"vllm","endpoint":"http://127.0.0.1"}{}`, strings.Repeat(" ", 16385)} {
		if _, err := DecodeProbe(strings.NewReader(raw)); err == nil {
			t.Fatal("invalid probe accepted")
		}
	}
	raw, _ := json.Marshal(ProbeRequest{App: "vllm", Endpoint: "http://127.0.0.1:8000"})
	if _, err := DecodeProbe(strings.NewReader(string(raw))); err != nil {
		t.Fatal(err)
	}
}

func TestProbeOlderAndOptionalAPIs(t *testing.T) {
	tests := []struct {
		app                 string
		responses           map[string]string
		inventory, instance string
		models              int
	}{
		{"vllm", map[string]string{"/version": `{"version":"1"}`, "/v1/models": `{}`}, "invalid", "available", 0},
		{"vllm", map[string]string{"/version": `{"version":"1"}`}, "unsupported", "unsupported", 0},
		{"vllm", map[string]string{}, "unsupported", "unsupported", 0},
		{"comfyui", map[string]string{"/system_stats": `{}`}, "invalid", "candidate", 0},
	}
	for _, tt := range tests {
		t.Run(tt.app+tt.inventory, func(t *testing.T) {
			s := probeServer(t, tt.responses)
			got, err := Probe(context.Background(), ProbeRequest{App: tt.app, Endpoint: s.URL})
			if err != nil || got.InventoryStatus != tt.inventory || got.InstanceStatus != tt.instance || len(got.Models) != tt.models {
				t.Fatalf("%+v %v", got, err)
			}
		})
	}
}
func TestProbeFailurePreservesReferenceAndUnknownLoadedState(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) }))
	got, err := Probe(context.Background(), ProbeRequest{App: "vllm", Endpoint: s.URL})
	s.Close()
	if err != nil || got.InstanceStatus != "unreachable" || got.Endpoint != s.URL {
		t.Fatalf("%+v %v", got, err)
	}
	got, err = Probe(context.Background(), ProbeRequest{App: "vllm", Endpoint: s.URL})
	if err != nil || got.InventoryStatus != "unknown" || got.Endpoint != s.URL {
		t.Fatalf("%+v %v", got, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Probe(ctx, ProbeRequest{App: "vllm", Endpoint: s.URL}); err == nil {
		t.Fatal("cancelled probe proceeded")
	}
	localhost := probeServer(t, map[string]string{"/system_stats": `{"system":{"comfyui_version":"1"}}`})
	got, err = Probe(context.Background(), ProbeRequest{App: "comfyui", Endpoint: strings.Replace(localhost.URL, "127.0.0.1", "localhost", 1)})
	if err != nil || got.InstanceStatus != "available" {
		t.Fatalf("localhost: %+v %v", got, err)
	}
}
func TestProbeReferenceTypesAndSymlinks(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "config")
	if err := os.WriteFile(file, []byte("malicious executable config contents"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []struct{ path, kind string }{{file, "model-directory"}, {link, "configuration"}} {
		got, err := Probe(context.Background(), ProbeRequest{App: "vllm", Reference: ref.path, ReferenceKind: ref.kind})
		if err != nil || got.InstanceStatus != "invalid" {
			t.Fatalf("%+v %v", got, err)
		}
	}
	got, err := Probe(context.Background(), ProbeRequest{App: "comfyui", Reference: file, ReferenceKind: "configuration"})
	if err != nil || got.InventoryStatus != "not-applicable" || len(got.Models) != 0 {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := Probe(context.Background(), ProbeRequest{App: "comfyui", Reference: file, ReferenceKind: "unknown"}); err == nil {
		t.Fatal("unknown reference accepted")
	}
}

func TestApplicationAliasSelectionDefersToExecutableTrust(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "vllm")
	aliasDir := filepath.Join(dir, "aliases")
	alias := filepath.Join(aliasDir, "vllm")
	if err := os.WriteFile(target, []byte("not executed"), 0755); err != nil {
		t.Fatal(err)
	}
	// World-write is the real danger the production validator must refuse.
	if err := os.Chmod(target, 0777); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(aliasDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, alias); err != nil {
		t.Fatal(err)
	}
	r := ProbeRequest{App: "vllm", Reference: alias, ReferenceKind: "application"}
	// Simulate an authoritative trusted-alias verdict without requiring a
	// root-owned fixture. Production uses the full executable trust validator.
	calls := 0
	validated := func(app, path string) error {
		calls++
		if app != "vllm" || path != alias {
			t.Fatal("selected application identity changed before trust validation")
		}
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatal("alias did not reach executable validation")
		}
		return nil
	}
	got, err := probeReferenceWithExecutableValidator(context.Background(), r, candidate(r), validated)
	if err != nil || calls != 1 || !got.Recognized || got.SourceKind != "owned" || got.InstanceStatus != "installed" || got.Binding == nil || got.Binding.Owned.Executable != alias || got.LifecycleControl != "unverified" {
		t.Fatalf("qualified alias rejected or changed: %+v %v calls=%d", got, err, calls)
	}
	// The actual validator refuses this writable/unprivileged alias fixture.
	got, err = Probe(context.Background(), r)
	if err != nil || got.Recognized || got.Binding != nil || got.InstanceStatus != "inspection-failed" {
		t.Fatalf("unsafe executable alias admitted or disclosed: %+v %v", got, err)
	}
	for _, kind := range []string{"configuration", "application-directory", "model-directory"} {
		r.ReferenceKind = kind
		got, err := probeReferenceWithExecutableValidator(context.Background(), r, candidate(r), validated)
		if err != nil || got.InstanceStatus != "invalid" || got.Binding != nil || calls != 1 {
			t.Fatalf("generic alias reached executable validator: %s %+v %v calls=%d", kind, got, err, calls)
		}
	}
	r.ReferenceKind = "application"
	r.Reference = filepath.Join(dir, "missing")
	got, err = probeReferenceWithExecutableValidator(context.Background(), r, candidate(r), validated)
	if err != nil || got.InstanceStatus != "missing" || calls != 1 {
		t.Fatalf("missing selection behavior changed: %+v %v calls=%d", got, err, calls)
	}
}

func TestProbeKeyProtectedVLLMKeepsModelsUnknown(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/version" {
			_, _ = w.Write([]byte(`{"version":"0.14.1"}`))
			return
		}
		if r.Header.Get("Authorization") != "" {
			t.Error("discovery sent credentials")
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer s.Close()
	got, err := Probe(context.Background(), ProbeRequest{App: "vllm", Endpoint: s.URL})
	if err != nil || got.InstanceStatus != "available" || got.InventoryStatus != "unknown" || got.Version != "0.14.1" || len(got.Models) != 0 || !strings.Contains(got.NextStep, "API key") {
		t.Fatalf("%+v %v", got, err)
	}
}

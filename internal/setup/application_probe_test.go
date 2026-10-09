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
		{"ollama", map[string]string{"/api/version": `{"version":"0.12.0"}`, "/api/tags": `{"models":[{"name":"local:latest","digest":"abc"},{"name":"cloud","remote_host":"https://ollama.com","remote_model":"cloud"}]}`, "/api/ps": `{"models":[{"name":"local:latest","digest":"abc"}]}`}, 2, "available"},
		{"llama.cpp", map[string]string{"/models": `{"data":[{"id":"preset","path":"/models/a.gguf","status":{"value":"loaded"}}]}`}, 1, "available"},
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
			if tt.app == "ollama" && (got.Models[0].Loaded != "yes" || got.Models[1].Locality != "non-local") {
				t.Fatal(got.Models)
			}
			if tt.app == "vllm" && len(got.Models[0].Aliases) != 2 {
				t.Fatal(got.Models)
			}
		})
	}
}
func TestProbeUnknownInventoryAndInvalidRequests(t *testing.T) {
	for _, body := range []string{`{}`, `{"models":null}`, `{"models":[{}]}`, `{"models":[]}{}`, `bad`, strings.Repeat(" ", 1048577)} {
		s := probeServer(t, map[string]string{"/api/version": `{"version":"0.12.0"}`, "/api/tags": body})
		got, err := Probe(context.Background(), ProbeRequest{App: "ollama", Endpoint: s.URL})
		if err != nil || got.InventoryStatus != "invalid" {
			t.Fatalf("%+v %v", got, err)
		}
	}
	for _, request := range []ProbeRequest{{App: "other", Endpoint: "http://127.0.0.1"}, {App: "ollama", Endpoint: "http://example.com"}, {App: "ollama", Endpoint: "http://user@127.0.0.1"}, {App: "ollama", Endpoint: "http://127.0.0.1/?reload=1"}, {App: "ollama", Reference: "relative", ReferenceKind: "model-file"}, {App: "ollama", Endpoint: "http://127.0.0.1", Reference: "/a", ReferenceKind: "model-file"}} {
		if _, err := Probe(context.Background(), request); err == nil {
			t.Fatalf("accepted %+v", request)
		}
	}
}
func TestProbeReferencesDoNotReadOrWriteModels(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "model.gguf")
	before := []byte("not actually a model")
	if err := os.WriteFile(file, before, 0600); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []struct{ path, kind string }{{file, "model-file"}, {dir, "model-directory"}, {file, "configuration"}, {file, "application"}} {
		got, err := Probe(context.Background(), ProbeRequest{App: "llama.cpp", Reference: ref.path, ReferenceKind: ref.kind})
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
	got, err := Probe(context.Background(), ProbeRequest{App: "llama.cpp", Reference: filepath.Join(dir, "missing"), ReferenceKind: "model-file"})
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
	_, err := Probe(ctx, ProbeRequest{App: "ollama", Endpoint: server.URL})
	if err == nil || time.Since(start) > time.Second {
		t.Fatal("cancellation not propagated", err)
	}
	hits := 0
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	defer destination.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, destination.URL, http.StatusFound) }))
	defer redirect.Close()
	got, err := Probe(context.Background(), ProbeRequest{App: "ollama", Endpoint: redirect.URL})
	if err != nil || hits != 0 || got.InventoryStatus == "available" {
		t.Fatalf("redirect followed: %+v %v hits=%d", got, err, hits)
	}
}
func TestDecodeProbeBounds(t *testing.T) {
	for _, raw := range []string{`{}`, `{"app":"ollama","endpoint":"http://127.0.0.1","extra":true}`, `{"app":"ollama","endpoint":"http://127.0.0.1"}{}`, strings.Repeat(" ", 16385)} {
		if _, err := DecodeProbe(strings.NewReader(raw)); err == nil {
			t.Fatal("invalid probe accepted")
		}
	}
	raw, _ := json.Marshal(ProbeRequest{App: "ollama", Endpoint: "http://127.0.0.1:11434"})
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
		{"llama.cpp", map[string]string{"/v1/models": `{"data":[{"id":"old-model","owned_by":"llamacpp"}]}`}, "available", "available", 1},
		{"llama.cpp", map[string]string{}, "unsupported", "unsupported", 0},
		{"llama.cpp", map[string]string{"/v1/models": `{"data":[]}`}, "invalid", "candidate", 0},
		{"llama.cpp", map[string]string{"/v1/models": `{"data":null}`}, "invalid", "candidate", 0},
		{"llama.cpp", map[string]string{"/v1/models": `{"data":[{"id":"other","owned_by":"unknown"}]}`}, "invalid", "candidate", 0},
		{"llama.cpp", map[string]string{"/models": `{"data":null}`}, "invalid", "candidate", 0},
		{"llama.cpp", map[string]string{"/models": `{"data":[{"id":"a","status":{"value":"unloaded"}},{"id":"b","status":{"value":"sleeping"}},{"id":"c","status":{"value":"loading"}},{"id":"d","status":{"value":"downloading"}}]}`}, "available", "available", 4},
		{"llama.cpp", map[string]string{"/models": `{"data":[{"id":"a","status":{"value":"new-state"}}]}`}, "invalid", "candidate", 0},
		{"llama.cpp", map[string]string{"/models": `{"data":[{"id":""}]}`}, "invalid", "candidate", 0},
		{"ollama", map[string]string{"/api/version": `{"version":"older"}`, "/api/tags": `{"models":[{"model":"a"}]}`}, "available", "available", 1},
		{"ollama", map[string]string{"/api/version": `{}`}, "invalid", "candidate", 0},
		{"ollama", map[string]string{"/api/version": `{"version":"1"}`, "/api/tags": `{"models":[{"name":"a"}]}`, "/api/ps": `{"models":[{}]}`}, "available", "available", 1},
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
			if tt.app == "ollama" && len(got.Models) > 0 && got.Models[0].Loaded != "unknown" {
				t.Fatal("optional API failure reported model unloaded")
			}
		})
	}
}
func TestProbeFailurePreservesReferenceAndUnknownLoadedState(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) }))
	got, err := Probe(context.Background(), ProbeRequest{App: "ollama", Endpoint: s.URL})
	s.Close()
	if err != nil || got.InstanceStatus != "unreachable" || got.Endpoint != s.URL {
		t.Fatalf("%+v %v", got, err)
	}
	got, err = Probe(context.Background(), ProbeRequest{App: "ollama", Endpoint: s.URL})
	if err != nil || got.InventoryStatus != "unknown" || got.Endpoint != s.URL {
		t.Fatalf("%+v %v", got, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Probe(ctx, ProbeRequest{App: "ollama", Endpoint: s.URL}); err == nil {
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
	for _, ref := range []struct{ path, kind string }{{dir, "model-file"}, {file, "model-directory"}, {link, "configuration"}} {
		got, err := Probe(context.Background(), ProbeRequest{App: "llama.cpp", Reference: ref.path, ReferenceKind: ref.kind})
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
	target := filepath.Join(dir, "ollama")
	aliasDir := filepath.Join(dir, "aliases")
	alias := filepath.Join(aliasDir, "ollama")
	if err := os.WriteFile(target, []byte("not executed"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(aliasDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, alias); err != nil {
		t.Fatal(err)
	}
	r := ProbeRequest{App: "ollama", Reference: alias, ReferenceKind: "application"}
	// Simulate an authoritative trusted-alias verdict without requiring a
	// root-owned fixture. Production uses the full executable trust validator.
	calls := 0
	validated := func(app, path string) error {
		calls++
		if app != "ollama" || path != alias {
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
	if err != nil || got.Recognized || got.Binding != nil || got.InstanceStatus != "inspection-failed" || strings.Contains(got.NextStep, alias) {
		t.Fatalf("unsafe executable alias admitted or disclosed: %+v %v", got, err)
	}
	for _, kind := range []string{"configuration", "model-file", "application-directory", "model-directory"} {
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

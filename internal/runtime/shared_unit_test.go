package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

// The fixture canonicalizes untagged names to :latest exactly like a real
// Ollama daemon, 404s unknown models, and applies keep_alive sign semantics:
// -1 persists, 0 unloads, a positive value loads with an expiry.
type sharedOllamaServer struct {
	mu          sync.Mutex
	known       map[string]bool
	loaded      map[string]int
	pending     map[string]int
	unloadDelay int
	requests    []string
	psCalls     int
}

func canonicalModel(model string) string {
	return control.NativeModel{Runtime: "ollama", Model: model}.ComparisonModel()
}

func (s *sharedOllamaServer) load(model string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loaded[canonicalModel(model)] = -1
}

func (s *sharedOllamaServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		switch {
		case r.Method == "GET" && r.URL.Path == "/health":
			w.WriteHeader(200)
		case r.Method == "GET" && r.URL.Path == "/api/ps":
			s.psCalls++
			for model, left := range s.pending {
				if left <= 1 {
					delete(s.loaded, model)
					delete(s.pending, model)
				} else {
					s.pending[model] = left - 1
				}
			}
			models := []map[string]string{}
			for model := range s.loaded {
				models = append(models, map[string]string{"name": model, "model": model})
			}
			json.NewEncoder(w).Encode(map[string]any{"models": models})
		case r.Method == "POST" && r.URL.Path == "/api/show":
			var body struct {
				Model string `json:"model"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil || !s.known[canonicalModel(body.Model)] {
				w.WriteHeader(404)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"model_info": map[string]any{"k": "v"}})
		case r.Method == "POST" && r.URL.Path == "/api/generate":
			var body struct {
				Model     string `json:"model"`
				KeepAlive int    `json:"keep_alive"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil {
				w.WriteHeader(400)
				return
			}
			s.requests = append(s.requests, fmt.Sprintf("%s:%d", body.Model, body.KeepAlive))
			model := canonicalModel(body.Model)
			if !s.known[model] {
				w.WriteHeader(404)
				return
			}
			switch {
			case body.KeepAlive == 0:
				if s.unloadDelay > 0 {
					if _, ok := s.loaded[model]; ok {
						s.pending[model] = s.unloadDelay
					}
				} else {
					delete(s.loaded, model)
				}
			default:
				s.loaded[model] = body.KeepAlive
			}
			w.WriteHeader(200)
		default:
			w.WriteHeader(404)
		}
	})
}

func sharedOllamaFixture(t *testing.T, mutate ...func(*control.Catalog)) (*SystemdManager, *fakeRunner, *sharedOllamaServer, string) {
	t.Helper()
	backend := &sharedOllamaServer{
		known:   map[string]bool{"alpha:latest": true, "beta:latest": true, "gamma:latest": true},
		loaded:  map[string]int{},
		pending: map[string]int{},
	}
	server := httptest.NewServer(backend.handler())
	t.Cleanup(server.Close)
	data := nativeLaunchFixture(t, "ollama", server.URL, "alpha")
	path := filepath.Join(t.TempDir(), "ollama.service")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	hash := fmt.Sprintf("%x", sha256.Sum256(data))
	profile := func(id, model string) control.WorkloadProfile {
		return control.WorkloadProfile{
			ID: control.Workload(id), Label: id, Adapter: "systemd", Unit: "ollama.service",
			Cgroup: "/workloads/ollama.service", HealthURL: server.URL + "/health",
			NativeModel: &control.NativeModel{Runtime: "ollama", Instance: "local", Model: model, Endpoint: server.URL, LaunchFile: path, LaunchSHA256: hash},
		}
	}
	c := control.Catalog{Version: 1, Profiles: []control.WorkloadProfile{
		profile("alpha", "alpha"),
		profile("beta", "beta"),
		{ID: "chat", Label: "chat", Adapter: "systemd", Unit: "chat.service", Cgroup: "/workloads/chat.service", HealthURL: server.URL + "/health"},
	}}
	for _, fn := range mutate {
		fn(&c)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	r := &fakeRunner{outputs: map[string][]byte{}}
	cfg := testConfig()
	cfg.Catalog = &c
	m, err := newSystemdManager(cfg, r, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	m.nativeExecutableValidator = fixtureExecutableValidator
	showCmd := "/usr/bin/true --user show --property=LoadState --property=ActiveState --property=SubState --property=ControlGroup -- ollama.service"
	r.outputs[showCmd] = []byte("LoadState=loaded\nActiveState=active\nSubState=running\nControlGroup=/workloads/ollama.service\n")
	r.outputs["/usr/bin/true --user show --property=LoadState --property=ActiveState --property=SubState --property=ControlGroup -- chat.service"] = stoppedOutput()
	r.outputs["/usr/bin/true --user show --property=FragmentPath --property=DropInPaths --property=NeedDaemonReload -- ollama.service"] = []byte("FragmentPath=" + path + "\nDropInPaths=\nNeedDaemonReload=no\n")
	fixtureCgroups(t, m)
	return m, r, backend, showCmd
}

func TestSharedUnitObserveIsModelLevel(t *testing.T) {
	m, r, backend, showCmd := sharedOllamaFixture(t)
	backend.load("alpha")
	snap, err := m.Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !snap.Workloads["alpha"].Active || snap.Workloads["beta"].Active {
		t.Fatalf("observation = %#v", snap.Workloads)
	}
	r.outputs[showCmd] = stoppedOutput()
	calls := backend.psCalls
	snap, err = m.Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snap.Workloads["alpha"].Active || snap.Workloads["beta"].Active || backend.psCalls != calls {
		t.Fatal("dead unit must observe inactive without API calls")
	}
}

func TestSharedUnitStopUnloadsOnlyOwnModel(t *testing.T) {
	m, r, backend, _ := sharedOllamaFixture(t)
	backend.load("alpha")
	backend.load("gamma")
	if err := m.Stop(context.Background(), "alpha"); err != nil {
		t.Fatal(err)
	}
	if len(backend.requests) != 1 || backend.requests[0] != "alpha:0" {
		t.Fatalf("stop requests = %#v", backend.requests)
	}
	if len(backend.loaded) != 1 || backend.loaded["gamma:latest"] != -1 {
		t.Fatalf("stop touched foreign models: %#v", backend.loaded)
	}
	// The goal state already holds: an absent model is not loaded just to be
	// unloaded, which a real daemon would 404.
	if err := m.Stop(context.Background(), "beta"); err != nil {
		t.Fatal(err)
	}
	if len(backend.requests) != 1 {
		t.Fatalf("stop loaded an absent model to stop it: %#v", backend.requests)
	}
	for _, call := range r.calls {
		if strings.Contains(call, "stop -- ollama.service") {
			t.Fatal("shared stop terminated the unit")
		}
	}
}

type failingTransport struct{}

func (failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("api unreachable")
}

func TestSharedUnitStopUnverifiedUnloadFails(t *testing.T) {
	m, _, backend, _ := sharedOllamaFixture(t)
	backend.load("alpha")
	broken := *m
	broken.client = &http.Client{Transport: failingTransport{}}
	if err := broken.stopSharedOllama(context.Background(), m.config.Catalog.Profiles[0]); err == nil {
		t.Fatal("unload state unreadable accepted as stopped")
	}
	if len(backend.requests) != 0 {
		t.Fatal("unload attempted without loaded-model evidence")
	}
}

func TestSharedUnitStopPollFailsFastOnIdentityError(t *testing.T) {
	garbage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	}))
	defer garbage.Close()
	m := &SystemdManager{client: garbage.Client()}
	n := control.NativeModel{Runtime: "ollama", Endpoint: garbage.URL, Model: "alpha"}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := m.pollOllamaModels(ctx, n, func(map[string]bool) bool { return true }); !errors.Is(err, ErrModelIdentity) {
		t.Fatalf("permanent identity error retried: %v", err)
	}
	broken := &SystemdManager{client: &http.Client{Transport: failingTransport{}}}
	short, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	err := broken.pollOllamaModels(short, n, func(map[string]bool) bool { return true })
	if !errors.Is(err, ErrUnloadUnverified) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("transient transport failure not retried to the deadline: %v", err)
	}
}

func TestSharedUnitStopTimesOutWhenModelStaysLoaded(t *testing.T) {
	m, _, backend, _ := sharedOllamaFixture(t)
	backend.load("alpha")
	stubborn := *m
	stubborn.client = &http.Client{Transport: keepLoadedTransport{backend: backend}}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	err := stubborn.stopSharedOllama(ctx, m.config.Catalog.Profiles[0])
	if !errors.Is(err, ErrUnloadUnverified) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
}

type keepLoadedTransport struct{ backend *sharedOllamaServer }

func (k keepLoadedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == "POST" && r.URL.Path == "/api/generate" {
		return &http.Response{StatusCode: 200, Body: http.NoBody, Header: make(http.Header)}, nil
	}
	k.backend.mu.Lock()
	defer k.backend.mu.Unlock()
	body := `{"models":[{"name":"alpha:latest","model":"alpha:latest"}]}`
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
}

func TestSharedUnitReleasedForIsWholeListAndTargetAware(t *testing.T) {
	m, r, backend, showCmd := sharedOllamaFixture(t)
	ctx := context.Background()
	backend.load("gamma")
	for _, target := range []control.Workload{"beta", "chat", control.WorkloadIdle} {
		if err := m.ReleasedFor(ctx, target); err == nil {
			t.Fatalf("foreign loaded model accepted for %s", target)
		}
	}
	backend.mu.Lock()
	backend.loaded = map[string]int{}
	backend.mu.Unlock()
	backend.load("alpha")
	if err := m.ReleasedFor(ctx, "beta"); err == nil {
		t.Fatal("loaded opposing model accepted as released")
	}
	for _, target := range []control.Workload{"chat", control.WorkloadIdle} {
		if err := m.ReleasedFor(ctx, target); err == nil {
			t.Fatalf("shared model accepted for non-shared target %s", target)
		}
	}
	backend.load("beta")
	if err := m.ReleasedFor(ctx, "beta"); err == nil {
		t.Fatal("outgoing sibling model accepted as released")
	}
	backend.mu.Lock()
	delete(backend.loaded, "alpha:latest")
	backend.mu.Unlock()
	if err := m.ReleasedFor(ctx, "beta"); err != nil {
		t.Fatal("sibling switch rejected its own preloaded model", err)
	}
	r.outputs[showCmd] = stoppedOutput()
	backend.load("alpha")
	calls := backend.psCalls
	if err := m.ReleasedFor(ctx, "beta"); err != nil || backend.psCalls != calls {
		t.Fatalf("dead unit must prove release from cgroup evidence without API calls: %v", err)
	}
	writeEvents(t, m.cgroups.root, "workloads/ollama.service", "populated 1\n")
	if err := m.ReleasedFor(ctx, "beta"); err == nil {
		t.Fatal("populated cgroup of a dead unit accepted as released")
	}
}

func TestSharedUnitStartEvictsLeftoversBeforePreload(t *testing.T) {
	m, _, backend, _ := sharedOllamaFixture(t)
	backend.unloadDelay = 2
	backend.load("gamma")
	if err := m.Start(context.Background(), "beta"); err != nil {
		t.Fatal(err)
	}
	if len(backend.requests) != 2 || backend.requests[0] != "gamma:latest:0" || backend.requests[1] != "beta:-1" {
		t.Fatalf("requests = %#v", backend.requests)
	}
	if len(backend.loaded) != 1 || backend.loaded["beta:latest"] != -1 {
		t.Fatalf("loaded = %#v", backend.loaded)
	}
}

func TestSharedUnitStartOnStoppedUnitSkipsEviction(t *testing.T) {
	m, r, backend, showCmd := sharedOllamaFixture(t)
	r.outputs[showCmd] = stoppedOutput()
	if err := m.Start(context.Background(), "beta"); err != nil {
		t.Fatal(err)
	}
	if len(backend.requests) != 1 || backend.requests[0] != "beta:-1" {
		t.Fatalf("stopped unit holds no models to evict: %#v", backend.requests)
	}
}

func TestSharedUnitStartEvictsBeforeCapacityProbe(t *testing.T) {
	m, r, backend, _ := sharedOllamaFixture(t, func(c *control.Catalog) {
		c.Profiles[1].RequiredMiB = 1024
	})
	r.outputs[gpuFreeCommand] = []byte("8192\n")
	backend.load("gamma")
	probe := &capacityProbeRunner{inner: r, backend: backend}
	m.runner = probe
	if err := m.Start(context.Background(), "beta"); err != nil {
		t.Fatal(err)
	}
	if !probe.ran {
		t.Fatal("capacity probe did not run")
	}
	if probe.leftover {
		t.Fatal("capacity probe ran before evictable leftovers were unloaded")
	}
}

type capacityProbeRunner struct {
	inner    *fakeRunner
	backend  *sharedOllamaServer
	ran      bool
	leftover bool
}

func (p *capacityProbeRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := p.inner.Run(ctx, name, args...)
	if strings.Join(append([]string{name}, args...), " ") == gpuFreeCommand {
		p.backend.mu.Lock()
		_, p.leftover = p.backend.loaded["gamma:latest"]
		p.ran = true
		p.backend.mu.Unlock()
	}
	return out, err
}

func TestOllamaLoadedModelsRejectsBadPayloads(t *testing.T) {
	for _, body := range []string{
		"not json",
		`{"models":[{"name":"","model":""}]}`,
		`{"models":[{"name":"","model":"m:latest"}]}`,
		`{"models":[{"name":"m:latest","model":""}]}`,
	} {
		t.Run(body, func(t *testing.T) {
			if _, err := ollamaLoadedModels([]byte(body)); !errors.Is(err, ErrModelIdentity) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestSharedUnitModelLoadedPropagatesAPIFailures(t *testing.T) {
	m, _, _, _ := sharedOllamaFixture(t)
	n := *m.config.Catalog.Profiles[0].NativeModel
	broken := *m
	broken.client = &http.Client{Transport: failingTransport{}}
	if _, err := broken.ollamaModelLoaded(context.Background(), n); err == nil {
		t.Fatal("unreachable API accepted")
	}
	garbage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	}))
	defer garbage.Close()
	n.Endpoint = garbage.URL
	if _, err := m.ollamaModelLoaded(context.Background(), n); !errors.Is(err, ErrModelIdentity) {
		t.Fatalf("garbage model list: err = %v", err)
	}
}

func TestSetOllamaKeepAliveRejectsBadEndpoint(t *testing.T) {
	m, _, _, _ := sharedOllamaFixture(t)
	if err := m.setOllamaKeepAlive(context.Background(), "http://\x7f", "alpha", 0); err == nil {
		t.Fatal("invalid endpoint accepted")
	}
}

func TestSharedUnitStopRequiresActiveRunningUnit(t *testing.T) {
	m, r, backend, showCmd := sharedOllamaFixture(t)
	backend.load("alpha")
	p := m.config.Catalog.Profiles[0]
	r.errs = map[string]error{showCmd: errors.New("systemd down")}
	if err := m.stopSharedOllama(context.Background(), p); err == nil {
		t.Fatal("unit state failure accepted")
	}
	r.errs = nil
	r.outputs[showCmd] = stoppedOutput()
	if err := m.stopSharedOllama(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	r.outputs[showCmd] = []byte("LoadState=loaded\nActiveState=activating\nSubState=start\nControlGroup=/workloads/ollama.service\n")
	if err := m.stopSharedOllama(context.Background(), p); err == nil {
		t.Fatal("transitional unit state accepted")
	}
	if len(backend.requests) != 0 {
		t.Fatal("unload attempted on non-running unit")
	}
}

func TestSharedUnitReleasedForPropagatesFailures(t *testing.T) {
	m, r, _, showCmd := sharedOllamaFixture(t)
	r.errs = map[string]error{showCmd: errors.New("systemd down")}
	if err := m.ReleasedFor(context.Background(), "beta"); err == nil {
		t.Fatal("unit state failure accepted")
	}
	r.errs = nil
	broken := *m
	broken.client = &http.Client{Transport: failingTransport{}}
	if err := broken.ReleasedFor(context.Background(), "beta"); err == nil {
		t.Fatal("unreachable API accepted as released")
	}
}

func TestSharedUnitEvictPropagatesFailures(t *testing.T) {
	m, _, backend, _ := sharedOllamaFixture(t)
	n := *m.config.Catalog.Profiles[0].NativeModel
	broken := *m
	broken.client = &http.Client{Transport: failingTransport{}}
	if err := broken.evictOtherOllamaModels(context.Background(), n); err == nil {
		t.Fatal("unreachable API accepted")
	}
	garbage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	}))
	defer garbage.Close()
	bad := n
	bad.Endpoint = garbage.URL
	if err := m.evictOtherOllamaModels(context.Background(), bad); !errors.Is(err, ErrModelIdentity) {
		t.Fatalf("garbage model list: err = %v", err)
	}
	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/api/generate" {
			w.WriteHeader(500)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"models": []map[string]string{{"name": "gamma:latest", "model": "gamma:latest"}}})
	}))
	defer refusing.Close()
	stuck := n
	stuck.Endpoint = refusing.URL
	if err := m.evictOtherOllamaModels(context.Background(), stuck); err == nil {
		t.Fatal("refused eviction accepted")
	}
	backend.unloadDelay = 100
	backend.load("gamma")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := m.evictOtherOllamaModels(ctx, n); !errors.Is(err, ErrUnloadUnverified) {
		t.Fatalf("unverified eviction accepted: %v", err)
	}
}

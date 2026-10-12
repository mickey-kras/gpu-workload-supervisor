package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestNativeOllamaVersionedPSReplay(t *testing.T) {
	loaded, err := os.ReadFile("testdata/ollama-v0.40.3/api-ps-loaded.json")
	if err != nil {
		t.Fatal(err)
	}
	empty, err := os.ReadFile("testdata/ollama-v0.40.3/api-ps-empty.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name      string
		body      string
		ready     bool
		listValid bool
	}{
		{"loaded", string(loaded), true, true},
		{"empty", string(empty), false, true},
		{"wrong-tag", strings.ReplaceAll(string(loaded), "selected:latest", "selected:other"), false, true},
		{"wrong-case", strings.ReplaceAll(string(loaded), "selected:latest", "Selected:latest"), false, true},
		{"contradictory", `{"models":[{"name":"selected:latest","model":"other:latest"}]}`, false, true},
		{"multiple", `{"models":[{"name":"selected:latest","model":"selected:latest"},{"name":"other:latest","model":"other:latest"}]}`, false, true},
		{"missing", `{}`, false, false},
		{"null", `{"models":null}`, false, false},
		{"object", `{"models":{}}`, false, false},
		{"wrong-type", `{"models":[{"name":17,"model":"selected:latest"}]}`, false, false},
		{"duplicate-key", `{"models":[{"name":"other:latest","name":"selected:latest","model":"selected:latest"}]}`, false, false},
		{"root-case-collision", `{"models":[{"name":"foreign:latest","model":"foreign:latest"}],"Models":[]}`, false, false},
		{"root-case-alias", `{"Models":[]}`, false, false},
		{"root-unicode-alias", `{"modelſ":[]}`, false, false},
		{"name-case-collision", `{"models":[{"name":"other:latest","Name":"selected:latest","model":"selected:latest"}]}`, false, false},
		{"model-case-alias", `{"models":[{"name":"selected:latest","Model":"selected:latest"}]}`, false, false},
		{"unsupported-data", `{"data":[{"id":"selected"}]}`, false, false},
		{"trailing", string(loaded) + ` {}`, false, false},
		{"invalid", `{"models":[`, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/api/ps" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				fmt.Fprint(w, tc.body)
			}))
			defer s.Close()
			m := &SystemdManager{client: s.Client()}
			n := control.NativeModel{Runtime: "ollama", Model: "selected", Endpoint: s.URL}
			if err := m.nativeReady(context.Background(), n); (err == nil) != tc.ready {
				t.Fatalf("ready = %v, err = %v", tc.ready, err)
			}
			body, err := m.fetchModelList(context.Background(), n)
			if err != nil {
				t.Fatal(err)
			}
			models, err := ollamaLoadedModels(body)
			if (err == nil) != tc.listValid {
				t.Fatalf("listValid = %v, err = %v", tc.listValid, err)
			}
			if tc.ready && !models["selected:latest"] {
				t.Fatal("selected model missing from loaded list")
			}
		})
	}
}

func TestNativeLlamaVersionedModelsReplay(t *testing.T) {
	loaded, err := os.ReadFile("testdata/llama.cpp-v0.6.0/models-loaded.json")
	if err != nil {
		t.Fatal(err)
	}
	failed, err := os.ReadFile("testdata/llama.cpp-v0.6.0/models-failed.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		body  string
		ready bool
	}{
		{"loaded", string(loaded), true},
		{"failed", string(failed), false},
		{"wrong-id-with-selected-alias", strings.Replace(strings.Replace(string(loaded), `"id": "selected"`, `"id": "other"`, 1), `"selected-alias"`, `"selected"`, 1), false},
		{"wrong-case", strings.Replace(string(loaded), `"id": "selected"`, `"id": "Selected"`, 1), false},
		{"multiple", `{"data":[{"id":"selected","status":{"value":"loaded"}},{"id":"other","status":{"value":"loaded"}}]}`, false},
		{"loading", strings.Replace(string(loaded), `"value": "loaded"`, `"value": "loading"`, 1), false},
		{"unloaded", strings.Replace(string(loaded), `"value": "loaded"`, `"value": "unloaded"`, 1), false},
		{"downloading", strings.Replace(string(loaded), `"value": "loaded"`, `"value": "downloading"`, 1), false},
		{"downloaded", strings.Replace(string(loaded), `"value": "loaded"`, `"value": "downloaded"`, 1), false},
		{"sleeping", strings.Replace(string(loaded), `"value": "loaded"`, `"value": "sleeping"`, 1), false},
		{"unknown-status", strings.Replace(string(loaded), `"value": "loaded"`, `"value": "ready"`, 1), false},
		{"loaded-but-failed", `{"data":[{"id":"selected","status":{"value":"loaded","failed":true}}]}`, false},
		{"loaded-with-exit-code", `{"data":[{"id":"selected","status":{"value":"loaded","exit_code":1}}]}`, false},
		{"loaded-with-zero-exit", `{"data":[{"id":"selected","status":{"value":"loaded","failed":false,"exit_code":0}}]}`, true},
		{"null-status", `{"data":[{"id":"selected","status":null}]}`, false},
		{"string-status", `{"data":[{"id":"selected","status":"loaded"}]}`, false},
		{"empty-status", `{"data":[{"id":"selected","status":{}}]}`, false},
		{"wrong-failed-type", `{"data":[{"id":"selected","status":{"value":"loaded","failed":"false"}}]}`, false},
		{"null-failed", `{"data":[{"id":"selected","status":{"value":"loaded","failed":null}}]}`, false},
		{"wrong-exit-type", `{"data":[{"id":"selected","status":{"value":"loaded","exit_code":"0"}}]}`, false},
		{"null-exit", `{"data":[{"id":"selected","status":{"value":"loaded","exit_code":null}}]}`, false},
		{"duplicate-id", `{"data":[{"id":"other","id":"selected"}]}`, false},
		{"duplicate-status", `{"data":[{"id":"selected","status":{"value":"unloaded","value":"loaded"}}]}`, false},
		{"data-case-alias", `{"Data":[{"id":"selected"}]}`, false},
		{"id-case-collision", `{"data":[{"id":"other","ID":"selected"}]}`, false},
		{"status-case-collision", `{"data":[{"id":"selected","status":{"value":"unloaded"},"Status":{"value":"loaded"}}]}`, false},
		{"status-case-alias", `{"data":[{"id":"selected","Status":{"value":"loaded"}}]}`, false},
		{"status-unicode-alias", `{"data":[{"id":"selected","ſtatus":{"value":"loaded"}}]}`, false},
		{"value-case-collision", `{"data":[{"id":"selected","status":{"value":"unloaded","Value":"loaded"}}]}`, false},
		{"failure-case-collision", `{"data":[{"id":"selected","status":{"value":"loaded","failed":true,"Failed":false,"exit_code":1,"Exit_Code":0}}]}`, false},
		{"failed-case-alias", `{"data":[{"id":"selected","status":{"value":"loaded","Failed":false}}]}`, false},
		{"exit-case-collision", `{"data":[{"id":"selected","status":{"value":"loaded","exit_code":1,"Exit_Code":0}}]}`, false},
		{"exit-case-alias", `{"data":[{"id":"selected","status":{"value":"loaded","Exit_Code":0}}]}`, false},
		{"missing", `{}`, false},
		{"null-data", `{"data":null}`, false},
		{"empty-data", `{"data":[]}`, false},
		{"object-data", `{"data":{}}`, false},
		{"unsupported-models", `{"models":[{"name":"selected"}]}`, false},
		{"invalid", `{"data":[`, false},
		{"trailing", string(loaded) + ` {}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/models" {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				fmt.Fprint(w, tc.body)
			}))
			defer s.Close()
			resp, err := s.Client().Get(s.URL + "/models")
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if err := verifyModelList(body, control.NativeModel{Runtime: "llama.cpp", Model: "selected"}); (err == nil) != tc.ready {
				t.Fatalf("ready = %v, err = %v", tc.ready, err)
			}
		})
	}
}

func TestNativeDirectServerReadinessPaths(t *testing.T) {
	for _, family := range []string{"llama.cpp", "vllm"} {
		t.Run(family, func(t *testing.T) {
			var mu sync.Mutex
			var paths []string
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				paths = append(paths, r.URL.Path)
				mu.Unlock()
				switch r.URL.Path {
				case "/health":
					fmt.Fprint(w, `{"status":"ok"}`)
				case "/v1/models":
					fmt.Fprint(w, `{"data":[{"id":"selected"}]}`)
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer s.Close()
			m := &SystemdManager{client: s.Client()}
			if err := m.nativeReady(context.Background(), control.NativeModel{Runtime: family, Model: "selected", Endpoint: s.URL}); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			defer mu.Unlock()
			if strings.Join(paths, ",") != "/health,/v1/models" {
				t.Fatalf("paths = %v", paths)
			}
		})
	}
}

func TestNativeIdentityRejectsHealthyWrongModel(t *testing.T) {
	for _, family := range []string{"ollama", "llama.cpp", "vllm"} {
		t.Run(family, func(t *testing.T) {
			body := `{"data":[{"id":"other"}],"models":[{"name":"other:latest","model":"other:latest"}]}`
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
			defer server.Close()
			m := &SystemdManager{client: server.Client()}
			n := control.NativeModel{Runtime: family, Model: "selected", Endpoint: server.URL}
			if m.nativeReady(context.Background(), n) == nil {
				t.Fatal("wrong model accepted")
			}
			body = `{"data":[{"id":"selected"}],"models":[{"name":"selected:latest","model":"selected:latest"}]}`
			if err := m.nativeReady(context.Background(), n); err != nil {
				t.Fatal(err)
			}
			body = `{"data":[{"id":"selected"},{"id":"other"}],"models":[{"name":"selected:latest","model":"selected:latest"},{"name":"other:latest","model":"other:latest"}]}`
			if m.nativeReady(context.Background(), n) == nil {
				t.Fatal("multiple loaded models accepted")
			}
		})
	}
}
func TestNativeLaunchDrift(t *testing.T) {
	path := filepath.Join(t.TempDir(), "model.service")
	data := nativeLaunchFixture(t, "ollama", "http://localhost:11434", "a")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	n := control.NativeModel{Runtime: "ollama", Endpoint: "http://localhost:11434", LaunchFile: path, LaunchSHA256: fmt.Sprintf("%x", sha256.Sum256(data))}
	if err := verifyNativeLaunchWithValidator(n, fixtureExecutableValidator); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("other"), 0600); err != nil {
		t.Fatal(err)
	}
	if verifyNativeLaunchWithValidator(n, validateNativeExecutable) == nil {
		t.Fatal("changed launch file accepted")
	}
}

func nativeFixture(t *testing.T, family, endpoint string) (*SystemdManager, *fakeRunner, control.WorkloadProfile, string) {
	t.Helper()
	c := acceptanceCatalog()
	p := c.Profiles[2]
	path := filepath.Join(t.TempDir(), "speech.service")
	data := nativeLaunchFixture(t, family, endpoint, "selected")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	p.HealthURL = endpoint
	p.NativeModel = &control.NativeModel{Runtime: family, Instance: "local", Model: "selected", Endpoint: endpoint, LaunchFile: path, LaunchSHA256: fmt.Sprintf("%x", sha256.Sum256(data))}
	c.Profiles[2] = p
	m, r := acceptanceManager(t, c)
	m.nativeExecutableValidator = fixtureExecutableValidator
	cmd := "/usr/bin/true --user show --property=FragmentPath --property=DropInPaths --property=NeedDaemonReload --property=ExecStartPre --property=ExecStart -- speech.service"
	launch, _ := parseLaunchUnit(data, family)
	r.outputs[cmd] = []byte("ExecStart={ path=" + strings.Fields(launch.execStart)[0] + " ; argv[]=" + launch.execStart + " ; }\nFragmentPath=" + path + "\nDropInPaths=\nNeedDaemonReload=no\n")
	return m, r, p, cmd
}
func TestNativeLaunchBindingVerification(t *testing.T) {
	m, r, p, cmd := nativeFixture(t, "vllm", "http://localhost:9000")
	if err := m.verifyNativeBinding(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	good := r.outputs[cmd]
	for _, bad := range []string{"FragmentPath=/other\nNeedDaemonReload=no\n", "FragmentPath=" + p.NativeModel.LaunchFile + "\nNeedDaemonReload=yes\n", "FragmentPath=" + p.NativeModel.LaunchFile + "\nDropInPaths=/override\nNeedDaemonReload=no\n", "FragmentPath=a\nFragmentPath=b\n"} {
		r.outputs[cmd] = []byte(bad)
		if m.verifyNativeBinding(context.Background(), p) == nil {
			t.Fatal("changed manager binding accepted")
		}
	}
	r.outputs[cmd] = good
	r.errs = map[string]error{cmd: errors.New("unavailable")}
	if m.verifyNativeBinding(context.Background(), p) == nil {
		t.Fatal("manager failure ignored")
	}
	r.errs = nil
	if err := os.WriteFile(p.NativeModel.LaunchFile, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	calls := len(r.calls)
	if m.Start(context.Background(), p.ID) == nil {
		t.Fatal("drift permitted start")
	}
	for _, c := range r.calls[calls:] {
		if strings.Contains(c, " --user start ") {
			t.Fatal("started despite drift")
		}
	}
	if m.Preflight(context.Background()) == nil {
		t.Fatal("preflight drift ignored")
	}
	if m.Healthy(context.Background(), p.ID) == nil {
		t.Fatal("healthy despite drift")
	}
}
func TestNativeStartPreloadsAndVerifiesSelectedModel(t *testing.T) {
	for _, family := range []string{"ollama", "llama.cpp", "vllm"} {
		t.Run(family, func(t *testing.T) {
			preloads := 0
			identity := true
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/show" {
					fmt.Fprint(w, `{"model_info":{"general.architecture":"llama"}}`)
					return
				}
				if r.Method == "POST" {
					preloads++
					var v map[string]any
					if json.NewDecoder(r.Body).Decode(&v) != nil || v["model"] != "selected" || v["keep_alive"] != float64(-1) {
						t.Error("invalid preload")
					}
					fmt.Fprint(w, `{"done":true}`)
					return
				}
				if r.URL.Path == "/api/ps" || r.URL.Path == "/v1/models" {
					if !identity {
						w.WriteHeader(503)
						return
					}
					fmt.Fprint(w, `{"data":[{"id":"selected"}],"models":[{"name":"selected:latest","model":"selected:latest"}]}`)
				}
			}))
			defer server.Close()
			m, _, p, _ := nativeFixture(t, family, server.URL)
			if err := m.Preflight(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := m.Start(context.Background(), p.ID); err != nil {
				t.Fatal(err)
			}
			if err := m.Healthy(context.Background(), p.ID); err != nil {
				t.Fatal(err)
			}
			expected := 0
			if family == "ollama" {
				expected = 1
			}
			if preloads != expected {
				t.Fatal(preloads)
			}
			identity = false
			if m.Healthy(context.Background(), p.ID) == nil {
				t.Fatal("identity failure accepted")
			}
		})
	}
}
func TestNativeReadinessMalformedAndCanceled(t *testing.T) {
	for _, body := range []string{"invalid", strings.Repeat("x", 65537), `{"data":[]}`, `{"models":[]}`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
		m := &SystemdManager{client: server.Client()}
		if m.nativeReady(context.Background(), control.NativeModel{Runtime: "vllm", Endpoint: server.URL, Model: "a"}) == nil {
			t.Fatal("malformed identity accepted")
		}
		server.Close()
	}
	m := &SystemdManager{client: http.DefaultClient}
	if m.nativeReady(context.Background(), control.NativeModel{Endpoint: "://invalid"}) == nil {
		t.Fatal("invalid URL accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if m.nativeReady(ctx, control.NativeModel{Endpoint: "http://127.0.0.1:1"}) == nil {
		t.Fatal("cancel ignored")
	}
	p := control.WorkloadProfile{HealthURL: "http://127.0.0.1:1", NativeModel: &control.NativeModel{Runtime: "ollama"}}
	if m.startNative(ctx, p) == nil {
		t.Fatal("preload wait cancellation ignored")
	}
}
func TestNativeLaunchMissingDirectoryAndOversize(t *testing.T) {
	for _, path := range []string{filepath.Join(t.TempDir(), "missing"), t.TempDir()} {
		if verifyNativeLaunchWithValidator(control.NativeModel{LaunchFile: path}, validateNativeExecutable) == nil {
			t.Fatal("invalid launch file accepted")
		}
	}
	path := filepath.Join(t.TempDir(), "large")
	if err := os.WriteFile(path, make([]byte, 1<<20+1), 0600); err != nil {
		t.Fatal(err)
	}
	if verifyNativeLaunchWithValidator(control.NativeModel{LaunchFile: path}, validateNativeExecutable) == nil {
		t.Fatal("oversized launch accepted")
	}
}
func TestNativeLaunchFIFOIsRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pipe")
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- verifyNativeLaunchWithValidator(control.NativeModel{LaunchFile: path}, validateNativeExecutable)
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("FIFO accepted")
		}
	case <-time.After(time.Second):
		t.Fatal("FIFO blocked launch verification")
	}
}
func TestNativeOllamaContradictoryIdentity(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"models":[{"name":"selected","model":"other"}]}`)
	}))
	defer s.Close()
	m := &SystemdManager{client: s.Client()}
	if m.nativeReady(context.Background(), control.NativeModel{Runtime: "ollama", Endpoint: s.URL, Model: "selected"}) == nil {
		t.Fatal("contradictory identity accepted")
	}
}
func TestOllamaPreloadRequiresLocalModel(t *testing.T) {
	for _, body := range []string{`{"remote_host":"https://ollama.com","remote_model":"cloud"}`, `{}`, `garbage`} {
		t.Run(body, func(t *testing.T) {
			loads := 0
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/show" {
					fmt.Fprint(w, body)
				} else if r.URL.Path == "/api/generate" {
					loads++
				}
			}))
			defer s.Close()
			m := &SystemdManager{client: s.Client()}
			p := control.WorkloadProfile{HealthURL: s.URL, NativeModel: &control.NativeModel{Runtime: "ollama", Endpoint: s.URL, Model: "selected"}}
			if m.startNative(context.Background(), p) == nil || loads != 0 {
				t.Fatal("nonlocal model preloaded")
			}
		})
	}
}

func nativeLaunchFixture(t *testing.T, family, endpoint, model string) []byte {
	t.Helper()
	base := "llama-server"
	if family == "ollama" {
		base = "ollama"
	}
	if family == "vllm" {
		base = "vllm"
	}
	dir, err := os.MkdirTemp("", "native-launch-test-")
	if err != nil {
		t.Fatal(err)
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	exe := filepath.Join(dir, base)
	if err := os.WriteFile(exe, []byte("#!/bin/false\n"), 0700); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	if family == "ollama" {
		return []byte("[Service]\nEnvironment=OLLAMA_NO_CLOUD=1\nEnvironment=OLLAMA_HOST=" + u.Host + "\nExecStart=" + exe + " serve\n")
	}
	local := filepath.Join(dir, "model")
	command := ""
	if family == "vllm" {
		if err := os.Mkdir(local, 0700); err != nil {
			t.Fatal(err)
		}
		command = exe + " serve " + local + " --served-model-name " + model
	} else {
		if err := os.WriteFile(local, []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
		command = exe + " --model " + local + " --alias " + model
	}
	return []byte("[Service]\nExecStart=" + command + " --host " + u.Hostname() + " --port " + u.Port() + "\n")
}

// Real filesystem trust is tested independently; these fixtures model installed
// runtime executables without requiring root to write a trusted system path.
func fixtureExecutableValidator(path string) error {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return ErrLaunchUnsupported
	}
	return nil
}

package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

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
	cmd := "/usr/bin/true --user show --property=FragmentPath --property=DropInPaths --property=NeedDaemonReload -- speech.service"
	r.outputs[cmd] = []byte("FragmentPath=" + path + "\nDropInPaths=\nNeedDaemonReload=no\n")
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

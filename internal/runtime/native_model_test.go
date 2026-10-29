package runtime

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
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

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

func TestNativeModelListIdentity(t *testing.T) {
	loaded := `{"object":"list","data":[{"id":"selected","object":"model","owned_by":"vllm","root":"/models/selected","parent":null,"max_model_len":4096}]}`
	for _, tc := range []struct {
		name  string
		body  string
		ready bool
	}{
		{"loaded", loaded, true},
		{"minimal", `{"data":[{"id":"selected"}]}`, true},
		{"wrong-id", strings.Replace(loaded, `"id":"selected"`, `"id":"other"`, 1), false},
		{"wrong-case", strings.Replace(loaded, `"id":"selected"`, `"id":"Selected"`, 1), false},
		{"multiple", `{"data":[{"id":"selected"},{"id":"other"}]}`, false},
		{"duplicate-id", `{"data":[{"id":"other","id":"selected"}]}`, false},
		{"data-case-alias", `{"Data":[{"id":"selected"}]}`, false},
		{"id-case-collision", `{"data":[{"id":"other","ID":"selected"}]}`, false},
		{"missing", `{}`, false},
		{"null-data", `{"data":null}`, false},
		{"empty-data", `{"data":[]}`, false},
		{"object-data", `{"data":{}}`, false},
		{"unsupported-models", `{"models":[{"name":"selected"}]}`, false},
		{"invalid", `{"data":[`, false},
		{"trailing", loaded + ` {}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := verifyModelList([]byte(tc.body), control.NativeModel{Runtime: "vllm", Model: "selected"}); (err == nil) != tc.ready {
				t.Fatalf("ready = %v, err = %v", tc.ready, err)
			}
		})
	}
}

func TestNativeDirectServerReadinessPaths(t *testing.T) {
	for _, family := range []string{"vllm"} {
		t.Run(family, func(t *testing.T) {
			var mu sync.Mutex
			var paths []string
			s := httptest.NewServer(servingAPI(func(w http.ResponseWriter, r *http.Request) {
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
			if err := m.nativeReady(context.Background(), servingBinding(t, s.URL, "selected")); err != nil {
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
	for _, family := range []string{"vllm"} {
		t.Run(family, func(t *testing.T) {
			body := `{"data":[{"id":"other"}]}`
			server := httptest.NewServer(servingAPI(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
			defer server.Close()
			m := &SystemdManager{client: server.Client()}
			n := servingBinding(t, server.URL, "selected")
			if m.nativeReady(context.Background(), n) == nil {
				t.Fatal("wrong model accepted")
			}
			body = `{"data":[{"id":"selected"}]}`
			if err := m.nativeReady(context.Background(), n); err != nil {
				t.Fatal(err)
			}
			body = `{"data":[{"id":"selected"},{"id":"other"}]}`
			if m.nativeReady(context.Background(), n) == nil {
				t.Fatal("multiple loaded models accepted")
			}
		})
	}
}
func TestNativeLaunchDrift(t *testing.T) {
	path := filepath.Join(t.TempDir(), "model.service")
	n := control.NativeModel{Runtime: "vllm", Model: "a", Endpoint: "http://127.0.0.1:8080", LaunchFile: path}
	data := nativeLaunchFixture(t, &n)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	n.LaunchSHA256 = fmt.Sprintf("%x", sha256.Sum256(data))
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
	n := control.NativeModel{Runtime: family, Instance: "local", Model: "selected", Endpoint: endpoint, LaunchFile: path}
	data := nativeLaunchFixture(t, &n)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	n.LaunchSHA256 = fmt.Sprintf("%x", sha256.Sum256(data))
	p.HealthURL = endpoint
	p.NativeModel = &n
	c.Profiles[2] = p
	m, r := acceptanceManager(t, c)
	m.nativeExecutableValidator = fixtureExecutableValidator
	cmd := "/usr/bin/true --user show --property=FragmentPath --property=DropInPaths --property=NeedDaemonReload --property=ExecStartPre --property=ExecStart --property=Environment --property=EnvironmentFiles -- speech.service"
	launch, _ := parseLaunchUnit(data)
	r.outputs[cmd] = []byte("ExecStart={ path=" + strings.Fields(launch.execStart)[0] + " ; argv[]=" + launch.execStart + " ; }\nFragmentPath=" + path + "\nDropInPaths=\nNeedDaemonReload=no\nEnvironment=HF_HUB_OFFLINE=1\nEnvironmentFiles=" + n.APIKeyFile + " (ignore_errors=no)\n")
	return m, r, p, cmd
}
func TestNativeLaunchBindingVerification(t *testing.T) {
	m, r, p, cmd := nativeFixture(t, "vllm", "http://127.0.0.1:9000")
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
func TestNativeStartVerifiesSelectedModelWithoutRequests(t *testing.T) {
	for _, family := range []string{"vllm"} {
		t.Run(family, func(t *testing.T) {
			posts := 0
			identity := true
			server := httptest.NewServer(servingAPI(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					posts++
					return
				}
				if r.URL.Path == "/v1/models" {
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
			if posts != 0 {
				t.Fatalf("start sent %d model requests", posts)
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
func nativeLaunchFixture(t *testing.T, n *control.NativeModel) []byte {
	t.Helper()
	if n.Runtime != "vllm" {
		t.Fatalf("unsupported fixture runtime %q", n.Runtime)
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
	exe := filepath.Join(dir, "vllm")
	if err := os.WriteFile(exe, []byte("#!/bin/false\n"), 0700); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(n.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(dir, "model")
	if err := os.Mkdir(local, 0700); err != nil {
		t.Fatal(err)
	}
	hardened := newHardenedFixture(t)
	*n = hardened.bind(*n)
	command := exe + " serve " + local + " --served-model-name " + n.Model
	return []byte("[Service]\n" + hardened.lines() + "ExecStart=" + command + " --host " + u.Hostname() + " --port " + u.Port() + hardened.args() + "\n")
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

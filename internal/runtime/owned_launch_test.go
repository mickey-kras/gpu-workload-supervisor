package runtime

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

func ownedRenderProfile(runtimeName, id string, owned *control.OwnedLaunch) control.WorkloadProfile {
	model := "selected"
	endpoint := fmt.Sprintf("http://127.0.0.1:%d", owned.Port)
	return control.WorkloadProfile{
		ID:   control.Workload(id),
		Unit: control.OwnedUnitName(runtimeName, "local", control.Workload(id)),
		NativeModel: &control.NativeModel{
			Runtime:    runtimeName,
			Instance:   "local",
			Model:      model,
			Endpoint:   endpoint,
			LaunchFile: "/home/u/.config/systemd/user/" + control.OwnedUnitName(runtimeName, "local", control.Workload(id)),
			Owned:      owned,
		},
	}
}

func TestRenderOwnedUnitGolden(t *testing.T) {
	cases := map[string]struct {
		profile control.WorkloadProfile
		want    string
		wantErr error
	}{
		"ollama": {
			profile: ownedRenderProfile("ollama", "chat", &control.OwnedLaunch{Port: 11434}),
			want: "[Unit]\nDescription=Supervisor-owned ollama launch for instance local\n\n[Service]\nType=simple\nRestart=no\n" +
				"Environment=OLLAMA_NO_CLOUD=1\nEnvironment=OLLAMA_HOST=127.0.0.1:11434\nEnvironment=OLLAMA_MAX_LOADED_MODELS=1\n" +
				"ExecStart=/usr/bin/ollama serve\n",
		},
		"llama full": {
			profile: ownedRenderProfile("llama.cpp", "vision", &control.OwnedLaunch{ModelPath: "/models/v.gguf", Port: 9100, CtxSize: 8192, GPULayers: 99, Alias: "selected"}),
			want: "[Unit]\nDescription=Supervisor-owned llama.cpp launch for vision\n\n[Service]\nType=simple\nRestart=no\n" +
				"ExecStart=/usr/bin/llama-server -m /models/v.gguf --host 127.0.0.1 --port 9100 --ctx-size 8192 --n-gpu-layers 99 --alias selected\n",
		},
		"llama minimal": {
			profile: ownedRenderProfile("llama.cpp", "vision", &control.OwnedLaunch{ModelPath: "/models/v.gguf", Port: 9100}),
			want: "[Unit]\nDescription=Supervisor-owned llama.cpp launch for vision\n\n[Service]\nType=simple\nRestart=no\n" +
				"ExecStart=/usr/bin/llama-server -m /models/v.gguf --host 127.0.0.1 --port 9100\n",
		},
		"vllm full": {
			profile: ownedRenderProfile("vllm", "vision", &control.OwnedLaunch{ModelPath: "/models/v", Port: 9100, MaxModelLen: 4096, Alias: "selected"}),
			want: "[Unit]\nDescription=Supervisor-owned vllm launch for vision\n\n[Service]\nType=simple\nRestart=no\n" +
				"ExecStart=/usr/bin/vllm serve /models/v --host 127.0.0.1 --port 9100 --max-model-len 4096 --served-model-name selected\n",
		},
		"external ollama": {
			profile: control.WorkloadProfile{ID: "chat", NativeModel: &control.NativeModel{
				Runtime: "ollama", Instance: "external", Model: "selected", Endpoint: "http://127.0.0.1:11434",
			}},
			wantErr: ErrOwnedRender,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			raw, err := RenderOwnedUnit(tc.profile)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) || len(raw) != 0 {
					t.Fatalf("external unit rendered: raw=%q err=%v", raw, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if string(raw) != tc.want {
				t.Fatalf("render drift:\n%s", raw)
			}
			if _, err := RenderOwnedUnit(tc.profile); err != nil || string(raw) != tc.want {
				t.Fatal("render not deterministic")
			}
		})
	}
}

func TestRenderOwnedUnitRoundTripQualifies(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"ollama", "llama-server", "vllm"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/false\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	saved := ownedBinaryDirectory
	ownedBinaryDirectory = dir
	t.Cleanup(func() { ownedBinaryDirectory = saved })
	gguf := filepath.Join(dir, "v.gguf")
	if err := os.WriteFile(gguf, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	modelDir := filepath.Join(dir, "v")
	if err := os.Mkdir(modelDir, 0700); err != nil {
		t.Fatal(err)
	}
	defaultAlias := ownedRenderProfile("llama.cpp", "vision", &control.OwnedLaunch{ModelPath: gguf, Port: 9100})
	defaultAlias.NativeModel.Model = gguf
	for _, tc := range []control.WorkloadProfile{
		ownedRenderProfile("ollama", "chat", &control.OwnedLaunch{Port: 11434}),
		ownedRenderProfile("llama.cpp", "vision", &control.OwnedLaunch{ModelPath: gguf, Port: 9100, CtxSize: 8192, GPULayers: 99, Alias: "selected"}),
		defaultAlias,
		ownedRenderProfile("vllm", "vision", &control.OwnedLaunch{ModelPath: modelDir, Port: 9100, MaxModelLen: 4096, Alias: "selected"}),
	} {
		raw, err := RenderOwnedUnit(tc)
		if err != nil {
			t.Fatal(err)
		}
		if err := qualifyFixtureLaunch(raw, *tc.NativeModel); err != nil {
			t.Fatalf("%s render does not qualify: %v", tc.NativeModel.Runtime, err)
		}
	}
}

func TestRenderOwnedUnitRejectsUnrenderable(t *testing.T) {
	if _, err := RenderOwnedUnit(control.WorkloadProfile{ID: "x"}); !errors.Is(err, ErrOwnedRender) {
		t.Fatalf("missing spec = %v", err)
	}
	p := ownedRenderProfile("llama.cpp", "vision", &control.OwnedLaunch{ModelPath: "/models/v.gguf", Port: 9100})
	p.NativeModel.Owned = nil
	if _, err := RenderOwnedUnit(p); !errors.Is(err, ErrOwnedRender) {
		t.Fatalf("nil owned = %v", err)
	}
	p = ownedRenderProfile("ollama", "chat", &control.OwnedLaunch{Port: 11434})
	p.NativeModel.Runtime = "comfyui"
	if _, err := RenderOwnedUnit(p); !errors.Is(err, ErrOwnedRender) {
		t.Fatalf("unknown runtime = %v", err)
	}
}

func TestRenderMustQualifyFailsClosed(t *testing.T) {
	p := ownedRenderProfile("ollama", "chat", &control.OwnedLaunch{Port: 11434})
	raw, err := RenderOwnedUnit(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := renderMustQualify(raw, *p.NativeModel); !errors.Is(err, ErrOwnedRender) {
		t.Fatalf("untrusted packaged binary accepted: %v", err)
	}
	if err := renderMustQualify([]byte("[Service]\nExecStart=/bin/sh -c x\n"), *p.NativeModel); !errors.Is(err, ErrOwnedRender) {
		t.Fatalf("unsupported render accepted: %v", err)
	}
}

func TestVerifyOwnedSpec(t *testing.T) {
	if err := verifyOwnedSpec(control.WorkloadProfile{}); err != nil {
		t.Fatal("non-owned profile rejected")
	}
	p := ownedRenderProfile("ollama", "chat", &control.OwnedLaunch{Port: 11434})
	raw, err := RenderOwnedUnit(p)
	if err != nil {
		t.Fatal(err)
	}
	p.NativeModel.LaunchSHA256 = fmt.Sprintf("%x", sha256.Sum256(raw))
	p.NativeModel.Owned.Port = 12000
	if err := verifyOwnedSpec(p); !errors.Is(err, ErrOwnedRender) {
		t.Fatalf("spec/hash skew = %v", err)
	}
}

func TestOwnedCgroupDerivation(t *testing.T) {
	p := ownedRenderProfile("ollama", "chat", &control.OwnedLaunch{Port: 11434})
	if got := OwnedCgroup("/user.slice/user-1000.slice/user@1000.service", p); got != "/user.slice/user-1000.slice/user@1000.service/app.slice/gws-owned-ollama-local.service" {
		t.Fatalf("cgroup %q", got)
	}
}

func TestPreflightRejectsSkewedOwnedSpec(t *testing.T) {
	config := testConfig()
	owned := ownedRenderProfile("ollama", "chat", &control.OwnedLaunch{Port: 11434})
	owned.Label = "Chat"
	owned.Adapter = "systemd"
	owned.Cgroup = "/user.slice/user-1000.slice/user@1000.service/app.slice/" + owned.Unit
	owned.HealthURL = "http://127.0.0.1:11434/api/tags"
	owned.NativeModel.LaunchSHA256 = fmt.Sprintf("%x", sha256.Sum256([]byte("stale rendering")))
	catalog := control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{owned}}
	config.Catalog = &catalog
	config.OwnedUnitDir = t.TempDir()
	r := stoppedRunner()
	m, err := newSystemdManager(config, r, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Preflight(t.Context()); !errors.Is(err, ErrOwnedRender) {
		t.Fatalf("skewed owned spec = %v", err)
	}
	if len(r.calls) != 0 {
		t.Fatalf("preflight caused runtime effects: %v", r.calls)
	}
}

func TestPreflightFlagsOrphanedOwnedUnit(t *testing.T) {
	r := stoppedRunner()
	r.outputs[textShowCommand] = []byte("LoadState=loaded\nActiveState=active\nSubState=running\nControlGroup=/workloads/text.service\n")
	m := strictManager(t, r)
	root := fixtureCgroups(t, m)
	writeEvents(t, root, "text.service", "populated 1\n")
	dir := t.TempDir()
	m.config.OwnedUnitDir = dir
	if err := os.WriteFile(filepath.Join(dir, "gws-owned-stale.service"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.Preflight(t.Context()); !errors.Is(err, ErrOrphanedOwnedUnit) {
		t.Fatalf("orphan passed preflight: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, "gws-owned-stale.service")); err != nil {
		t.Fatal(err)
	}
	if err := m.Preflight(t.Context()); err != nil {
		t.Fatalf("clean scan rejected: %v", err)
	}
}

// TestOwnedCgroupMustDeriveFromManagerRoot rejects hand-edited owned profiles
// whose cgroup carries a foreign or nested prefix: only the exact derivation
// from the discovered manager root is admissible.
func TestOwnedCgroupMustDeriveFromManagerRoot(t *testing.T) {
	owned := ownedRenderProfile("ollama", "chat", &control.OwnedLaunch{Port: 11434})
	owned.Label = "Chat"
	owned.Adapter = "systemd"
	owned.HealthURL = "http://127.0.0.1:11434/api/tags"
	owned.NativeModel.LaunchSHA256 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	manager := func(cgroup string) *SystemdManager {
		owned.Cgroup = cgroup
		catalog := control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{owned}}
		config := testConfig()
		config.Catalog = &catalog
		m, err := newSystemdManager(config, stoppedRunner(), http.DefaultClient)
		if err != nil {
			t.Fatal(err)
		}
		fixtureCgroups(t, m)
		return m
	}
	// A cgroup missing the app.slice suffix never reaches this check: catalog
	// validation rejects it at load. These cases carry the right suffix but a
	// wrong prefix, which only the discovered manager root can rule out.
	for name, cgroup := range map[string]string{
		"foreign prefix":   "/elsewhere/app.slice/" + owned.Unit,
		"nested injection": "/workloads/other.slice/app.slice/" + owned.Unit,
	} {
		t.Run(name, func(t *testing.T) {
			if err := manager(cgroup).verifyManagerCgroup(t.Context()); err == nil {
				t.Fatal("foreign owned cgroup accepted")
			}
		})
	}
	t.Run("exact derivation accepted", func(t *testing.T) {
		if err := manager("/workloads/app.slice/" + owned.Unit).verifyManagerCgroup(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
}

func TestPreflightOrphanScan(t *testing.T) {
	dir := t.TempDir()
	m := &SystemdManager{config: SystemdConfig{OwnedUnitDir: dir}}
	if err := m.preflightOwned(t.Context(), dir); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(dir, "other.service")
	if err := os.WriteFile(foreign, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.preflightOwned(t.Context(), dir); err != nil {
		t.Fatal("foreign-prefix unit flagged")
	}
	orphan := filepath.Join(dir, "gws-owned-stale.service")
	if err := os.WriteFile(orphan, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.preflightOwned(t.Context(), dir); !errors.Is(err, ErrOrphanedOwnedUnit) {
		t.Fatalf("orphan accepted: %v", err)
	}
	owned := ownedRenderProfile("ollama", "chat", &control.OwnedLaunch{Port: 11434})
	catalog := control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{owned}}
	m.config.Catalog = &catalog
	if err := os.Rename(orphan, filepath.Join(dir, owned.Unit)); err != nil {
		t.Fatal(err)
	}
	if err := m.preflightOwned(t.Context(), dir); err != nil {
		t.Fatalf("managed unit flagged: %v", err)
	}
	if err := m.preflightOwned(t.Context(), filepath.Join(dir, "missing")); err != nil {
		t.Fatal("missing directory rejected")
	}
	if err := m.preflightOwned(t.Context(), ""); err != nil {
		t.Fatal("empty unit directory rejected")
	}
	unreadable := filepath.Join(dir, "blocked")
	if err := os.WriteFile(unreadable, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.preflightOwned(t.Context(), unreadable); err == nil {
		t.Fatal("unreadable unit directory accepted")
	}
	m.config.OwnedUnitDir = ""
	got, err := m.ownedUnitDirectory()
	if err != nil || got == "" {
		t.Fatalf("default unit directory unresolved: %v", err)
	}
}

// TestQualifyOwnedUnitRejectsUnqualifiedHosts proves the pre-commit
// qualification gate: a render whose packaged executable is absent or whose
// launch cannot be parsed by the shared qualifier fails loudly.
func TestQualifyOwnedUnitRejectsUnqualifiedHosts(t *testing.T) {
	p := ownedRenderProfile("ollama", "chat", &control.OwnedLaunch{Port: 11434})
	restore := ownedBinaryDirectory
	ownedBinaryDirectory = filepath.Join(t.TempDir(), "empty")
	defer func() { ownedBinaryDirectory = restore }()
	if err := QualifyOwnedUnit(p); !errors.Is(err, ErrOwnedRender) {
		t.Fatalf("missing packaged executable qualified: %v", err)
	}
	adopted := p
	adopted.NativeModel.Owned = nil
	if err := QualifyOwnedUnit(adopted); err != nil {
		t.Fatal(err)
	}
}

// TestPreflightAccountsAdoptedOwnedUnitBinding covers owned→adopted
// conversion: an exact binding (launch path + proven fingerprint) accounts for
// the unit file, while a drifted fingerprint stays fail-closed.
func TestPreflightAccountsAdoptedOwnedUnitBinding(t *testing.T) {
	dir := t.TempDir()
	name := "gws-owned-vision.service"
	raw := []byte("[Unit]\nDescription=converted\n")
	if err := os.WriteFile(filepath.Join(dir, name), raw, 0600); err != nil {
		t.Fatal(err)
	}
	adopted := control.WorkloadProfile{
		ID:        "vision",
		Label:     "Vision",
		Adapter:   "systemd",
		Unit:      name,
		Cgroup:    "/workloads/" + name,
		HealthURL: "http://127.0.0.1:9100/health",
		NativeModel: &control.NativeModel{
			Runtime:      "llama.cpp",
			Instance:     "second",
			Model:        "vision",
			Endpoint:     "http://127.0.0.1:9100",
			LaunchFile:   filepath.Join(dir, name),
			LaunchSHA256: fmt.Sprintf("%x", sha256.Sum256(raw)),
		},
	}
	manager := func() *SystemdManager {
		catalog := control.Catalog{Version: 1, Profiles: []control.WorkloadProfile{adopted}}
		config := testConfig()
		config.Catalog = &catalog
		m, err := newSystemdManager(config, stoppedRunner(), http.DefaultClient)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	if err := manager().preflightOwned(t.Context(), dir); err != nil {
		t.Fatalf("exact adopted binding treated as orphan: %v", err)
	}
	m := manager()
	m.config.Catalog.Profiles[0].NativeModel.LaunchSHA256 = strings.Repeat("0", 64)
	if err := m.preflightOwned(t.Context(), dir); !errors.Is(err, ErrOrphanedOwnedUnit) {
		t.Fatalf("drifted fingerprint accounted: %v", err)
	}
	// An unreferenced gws-owned file is still an orphan.
	if err := os.WriteFile(filepath.Join(dir, "gws-owned-stray.service"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := manager().preflightOwned(t.Context(), dir); !errors.Is(err, ErrOrphanedOwnedUnit) {
		t.Fatalf("stray unit accounted: %v", err)
	}
}

// TestPreflightFailsClosedWhenAccountUnresolvable proves the orphan scan is
// never silently skipped: when the account record cannot be resolved the
// preflight errors instead of disabling the scan.
func TestPreflightFailsClosedWhenAccountUnresolvable(t *testing.T) {
	m := strictManager(t, stoppedRunner())
	m.config.OwnedUnitDir = ""
	restore := lookupAccountID
	lookupAccountID = func(string) (*user.User, error) { return nil, errors.New("no account record") }
	defer func() { lookupAccountID = restore }()
	if err := m.Preflight(t.Context()); err == nil {
		t.Fatal("unresolvable account silently disabled the orphan scan")
	}
}

// TestPreflightDerivesHomeFromAccountRecord ignores the ambient HOME: the scan
// directory comes from the passwd entry for the euid, same as setup.
func TestPreflightDerivesHomeFromAccountRecord(t *testing.T) {
	m := strictManager(t, stoppedRunner())
	m.config.OwnedUnitDir = ""
	t.Setenv("HOME", "/nonexistent-home")
	account, err := user.LookupId(strconv.Itoa(os.Geteuid()))
	if err != nil || account.HomeDir == "" {
		t.Skip("no account record for euid")
	}
	got, err := m.ownedUnitDirectory()
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(account.HomeDir, ".config/systemd/user") {
		t.Fatalf("scan derived from ambient HOME: %s", got)
	}
}

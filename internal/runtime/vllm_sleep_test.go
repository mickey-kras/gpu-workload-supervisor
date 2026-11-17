package runtime

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

func newSleepTestManager(t *testing.T, endpoint string, sleepEnabled bool, runner *fakeRunner) *SystemdManager {
	t.Helper()
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "vllm.env")
	if err := os.WriteFile(keyFile, []byte("VLLM_API_KEY=secret-test-token-1234567890\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tplFile := filepath.Join(dir, "chat_template.jinja")
	if err := os.WriteFile(tplFile, []byte("{{ messages }}"), 0o600); err != nil {
		t.Fatal(err)
	}
	tplHash, err := ChatTemplateSHA256(tplFile)
	if err != nil {
		t.Fatal(err)
	}
	vllmBin := filepath.Join(dir, "vllm")
	if err := os.WriteFile(vllmBin, []byte("#!/bin/false\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	p := control.WorkloadProfile{
		ID:        "text",
		Label:     "Text",
		Adapter:   "systemd",
		Unit:      "gws-owned-text.service",
		Cgroup:    "/user.test/app.slice/gws-owned-text.service",
		HealthURL: endpoint + "/health",
		NativeModel: &control.NativeModel{
			Runtime:            "vllm",
			Instance:           "local",
			Endpoint:           endpoint,
			Model:              dir,
			GPUUUID:            "GPU-11111111-2222-3333-4444-555555555555",
			LaunchFile:         filepath.Join(dir, "gws-owned-text.service"),
			ChatTemplate:       tplFile,
			ChatTemplateSHA256: tplHash,
			EnvironmentFile:    keyFile,
			Owned: &control.OwnedLaunch{
				Executable: vllmBin,
				ModelPath:  dir,
				Port:       8000,
				Serving:    control.ServingOptions{Version: 1, SleepMode: sleepEnabled},
			},
		},
	}
	rawUnit, err := RenderOwnedUnit(p)
	if err != nil {
		t.Fatal(err)
	}
	p.NativeModel.LaunchSHA256 = fmt.Sprintf("%x", sha256.Sum256(rawUnit))
	if err := os.WriteFile(p.NativeModel.LaunchFile, rawUnit, 0o600); err != nil {
		t.Fatal(err)
	}
	cat := &control.Catalog{Version: control.CatalogVersionServing, Profiles: []control.WorkloadProfile{p}}
	m, err := newSystemdManager(SystemdConfig{
		Catalog:       cat,
		HealthTimeout: 2 * time.Second,
		GPUUUID:       "GPU-11111111-2222-3333-4444-555555555555",
		NvidiaSMIPath: "/usr/bin/true",
		SystemctlPath: "/usr/bin/true",
	}, runner, &http.Client{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	m.cgroups = cgroupFS{root: t.TempDir(), verify: func(int) error { return nil }}
	return m
}

func TestVLLMSleepWakeAndReleasedViaSleep(t *testing.T) {
	oldCheck := checkKeyListener
	checkKeyListener = func(string) error { return nil }
	t.Cleanup(func() { checkKeyListener = oldCheck })

	var sleeping atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret-test-token-1234567890" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/sleep":
			sleeping.Store(true)
			w.WriteHeader(http.StatusOK)
		case "/wake_up":
			sleeping.Store(false)
			w.WriteHeader(http.StatusOK)
		case "/is_sleeping":
			if sleeping.Load() {
				_, _ = w.Write([]byte(`{"is_sleeping":true}`))
			} else {
				_, _ = w.Write([]byte(`{"is_sleeping":false}`))
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	showKey := "/usr/bin/true --user show --property=LoadState --property=ActiveState --property=SubState --property=ControlGroup -- gws-owned-text.service"
	runner := &fakeRunner{outputs: map[string][]byte{
		showKey: []byte("LoadState=loaded\nActiveState=active\nSubState=running\nControlGroup=/user.test/app.slice/gws-owned-text.service\n"),
	}}
	m := newSleepTestManager(t, srv.URL, true, runner)
	if got := m.gpuDeviceTarget(); got != "GPU-11111111-2222-3333-4444-555555555555" {
		t.Fatalf("gpuDeviceTarget = %q", got)
	}
	if err := m.Sleep(context.Background(), "unknown"); err == nil {
		t.Fatal("expected error for unknown workload in Sleep")
	}
	if err := m.Wake(context.Background(), "unknown"); err == nil {
		t.Fatal("expected error for unknown workload in Wake")
	}
	if err := m.Sleep(context.Background(), "text"); err != nil {
		t.Fatalf("Sleep failed: %v", err)
	}
	if !sleeping.Load() {
		t.Fatal("expected vLLM /sleep endpoint to mark server sleeping")
	}
	snap, err := m.Observe(context.Background())
	if err != nil || snap.Workloads["text"].Active {
		t.Fatalf("Observe = %+v, %v", snap, err)
	}
	// Build binding verification outputs so ReleasedFor(idle) and Wake("text") pass full verification
	p := m.config.Catalog.Profiles[0]
	rawUnit, _ := os.ReadFile(p.NativeModel.LaunchFile)
	execLine := ""
	for _, ln := range strings.Split(string(rawUnit), "\n") {
		if after, ok := strings.CutPrefix(ln, "ExecStart="); ok {
			execLine = after
		}
	}
	fields := strings.Fields(execLine)
	runner.outputs["/usr/bin/true --query-gpu=uuid --format=csv,noheader"] = []byte("GPU-11111111-2222-3333-4444-555555555555\n")
	runner.outputs["/usr/bin/true --query-gpu=memory.free,memory.total --format=csv,noheader,nounits -i GPU-11111111-2222-3333-4444-555555555555"] = []byte("30000, 32768\n")
	runner.outputs["/usr/bin/true --user show --property=FragmentPath --property=DropInPaths --property=NeedDaemonReload --property=ExecStartPre --property=ExecStart --property=Environment --property=EnvironmentFiles -- gws-owned-text.service"] = []byte(
		"FragmentPath=" + p.NativeModel.LaunchFile + "\nDropInPaths=\nNeedDaemonReload=no\nExecStartPre=\n" +
			"ExecStart={ path=" + fields[0] + " ; argv[]=" + execLine + " ; ignore_errors=no ; }\n" +
			"Environment=HF_HUB_OFFLINE=1\nEnvironmentFiles=" + p.NativeModel.EnvironmentFile + " (ignore_errors=no)\n",
	)
	if err := m.ReleasedFor(context.Background(), control.WorkloadIdle); err != nil {
		t.Fatalf("ReleasedFor(idle) while sleeping failed: %v", err)
	}
	if err := m.Wake(context.Background(), "text"); err != nil {
		t.Fatalf("Wake failed: %v", err)
	}
	if sleeping.Load() {
		t.Fatal("expected Wake to resume vLLM from sleep")
	}
}

func TestVLLMSleepFallsBackToStopWhenDisabledOrFailing(t *testing.T) {
	oldCheck := checkKeyListener
	checkKeyListener = func(string) error { return nil }
	t.Cleanup(func() { checkKeyListener = oldCheck })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	showKey := "/usr/bin/true --user show --property=LoadState --property=ActiveState --property=SubState --property=ControlGroup -- gws-owned-text.service"
	for _, enabled := range []bool{false, true} {
		runner := &fakeRunner{outputs: map[string][]byte{
			showKey: []byte("LoadState=loaded\nActiveState=inactive\nSubState=dead\nControlGroup=/user.test/app.slice/gws-owned-text.service\n"),
			"/usr/bin/true --user show --property=LoadState --property=ActiveState --property=SubState --property=ControlGroup -- -.slice": []byte("LoadState=loaded\nActiveState=active\nSubState=active\nControlGroup=/user.test\n"),
			"/usr/bin/true --user stop -- gws-owned-text.service":                                                                          []byte(""),
		}}
		m := newSleepTestManager(t, srv.URL, enabled, runner)
		writeEvents(t, m.cgroups.root, "app.slice/gws-owned-text.service", "populated 0\n")
		if err := os.MkdirAll(filepath.Join(m.cgroups.root, "user.test"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := m.Sleep(context.Background(), "text"); err != nil {
			t.Fatalf("Sleep (enabled=%v) failed: %v", enabled, err)
		}
	}
	if _, err := newSystemdManager(SystemdConfig{
		Catalog:       &control.Catalog{Version: 1},
		HealthTimeout: time.Second,
		GPUUUID:       "bad-uuid",
		SystemctlPath: "/usr/bin/true",
	}, &fakeRunner{}, &http.Client{}); err == nil {
		t.Fatal("expected invalid GPUUUID error")
	}
}

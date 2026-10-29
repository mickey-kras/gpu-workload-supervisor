package runtime

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

func limitedOwnedProfile(t *testing.T) (control.WorkloadProfile, []byte) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "vllm"), []byte("#!/bin/false\n"), 0700); err != nil {
		t.Fatal(err)
	}
	saved := ownedBinaryDirectory
	ownedBinaryDirectory = dir
	t.Cleanup(func() { ownedBinaryDirectory = saved })
	modelDir := filepath.Join(dir, "v")
	if err := os.Mkdir(modelDir, 0700); err != nil {
		t.Fatal(err)
	}
	limits := control.ResourceLimits{Version: 1, MemoryHighMiB: 4096, MemoryMaxMiB: 8192, TasksMax: 4096, CPUQuotaPercent: 400,
		IODevice: "/dev/nvme0n1", IODeviceNumber: "259:0", IOReadBandwidthMax: 1 << 30}
	p := ownedRenderProfile("vllm", "vision", &control.OwnedLaunch{ModelPath: modelDir, Port: 9100, Alias: "selected", Conflicts: "gws-owned-alpha.service", Limits: limits})
	raw, err := RenderOwnedUnit(p)
	if err != nil {
		t.Fatal(err)
	}
	p.NativeModel.LaunchSHA256 = fmt.Sprintf("%x", sha256.Sum256(raw))
	return p, raw
}

func TestRenderOwnedUnitWithLimitsGolden(t *testing.T) {
	p, raw := limitedOwnedProfile(t)
	want := "[Unit]\nDescription=Supervisor-owned vllm launch for vision\nConflicts=gws-owned-alpha.service\nAfter=gws-owned-alpha.service\n" +
		"\n[Service]\nType=simple\nRestart=no\n" +
		"OOMPolicy=stop\nOOMScoreAdjust=500\nMemoryHigh=4096M\nMemoryMax=8192M\nMemorySwapMax=0M\nTasksMax=4096\nCPUQuota=400%\n" +
		"IOReadBandwidthMax=/dev/nvme0n1 1073741824\n" +
		"ExecStart=" + ownedBinaryDirectory + "/vllm serve " + p.NativeModel.Owned.ModelPath + " --host 127.0.0.1 --port 9100 --served-model-name selected\n"
	if string(raw) != want {
		t.Fatalf("render drift:\n%s", raw)
	}
	// Removing the limits restores the exact legacy bytes and fingerprint.
	legacy := p
	n := *p.NativeModel
	o := *n.Owned
	o.Limits = control.ResourceLimits{}
	n.Owned = &o
	legacy.NativeModel = &n
	plain, err := RenderOwnedUnit(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plain), "OOMPolicy") || string(plain) != strings.Replace(want, p.NativeModel.Owned.Limits.RenderLines(), "", 1) {
		t.Fatalf("legacy render drift:\n%s", plain)
	}
}

// ownedUnitWithoutLimits renders p with its limits removed.
func ownedUnitWithoutLimits(t *testing.T, p control.WorkloadProfile) (control.WorkloadProfile, []byte) {
	t.Helper()
	n := *p.NativeModel
	o := *n.Owned
	o.Limits = control.ResourceLimits{}
	n.Owned = &o
	p.NativeModel = &n
	raw, err := RenderOwnedUnit(p)
	if err != nil {
		t.Fatal(err)
	}
	p.NativeModel.LaunchSHA256 = fmt.Sprintf("%x", sha256.Sum256(raw))
	return p, raw
}

func TestOwnedLimitsParseOnlyAtRenderedPosition(t *testing.T) {
	p, raw := limitedOwnedProfile(t)
	n := *p.NativeModel
	if err := qualifyFixtureLaunch(raw, n); err != nil {
		t.Fatalf("rendered limits do not qualify: %v", err)
	}
	block := n.Owned.Limits.RenderLines()
	exec := raw[strings.Index(string(raw), "ExecStart="):]
	for name, edited := range map[string]string{
		"moved after ExecStart":  strings.Replace(string(raw), block, "", 1) + block,
		"changed value":          strings.Replace(string(raw), "TasksMax=4096", "TasksMax=8192", 1),
		"directive before block": strings.Replace(string(raw), "Restart=no\n", "Restart=no\nTasksMax=64\n", 1),
		"duplicated block":       strings.Replace(string(raw), block, block+block, 1),
		"missing block":          strings.Replace(string(raw), block, "", 1),
		"extra limit line":       strings.Replace(string(raw), string(exec), "CPUWeight=50\n"+string(exec), 1),
		"second service header":  string(raw) + "\n[Service]\nType=simple\nRestart=no\n",
	} {
		t.Run(name, func(t *testing.T) {
			if err := qualifyFixtureLaunch([]byte(edited), n); err == nil {
				t.Fatal("edited limits qualified")
			}
		})
	}
	if _, err := removeOwnedLimitLines([]byte("[Service]\nExecStart=/x\n"), n.Owned.Limits); !errors.Is(err, ErrLaunchUnsupported) {
		t.Fatalf("missing header = %v", err)
	}
	// The single-header guard holds on its own, before the shared parser runs.
	twice := strings.Replace(string(raw), ownedServiceHeader, ownedServiceHeader+block+ownedServiceHeader, 1)
	if _, err := removeOwnedLimitLines([]byte(twice), n.Owned.Limits); !errors.Is(err, ErrLaunchUnsupported) {
		t.Fatalf("second service header = %v", err)
	}
}

func TestOnlyOwnedGrammarRejectsLimitDirectives(t *testing.T) {
	n := control.NativeModel{Runtime: "vllm", Endpoint: "http://127.0.0.1:9100", Model: "/models/v"}
	p, raw := limitedOwnedProfile(t)
	exec := string(raw[strings.Index(string(raw), "ExecStart="):])
	for _, line := range []string{"OOMPolicy=stop", "OOMScoreAdjust=500", "MemoryHigh=1G", "MemoryMax=2G", "MemorySwapMax=0",
		"TasksMax=64", "CPUWeight=50", "CPUQuota=100%", "IOWeight=100", "IOReadBandwidthMax=/dev/sda 1M",
		"IOWriteBandwidthMax=/dev/sda 1M", "IOReadIOPSMax=/dev/sda 100", "IOWriteIOPSMax=/dev/sda 100",
		"MemoryLimit=2G", "CPUShares=512", "BlockIOWeight=100", "StartupCPUWeight=50", "StartupMemoryMax=2G", "MemoryZSwapMax=0"} {
		// The adopted and imported grammar stays as it was: these settings
		// are harmless there because adopted units get no supervisor limits.
		adopted := "[Service]\nType=simple\n" + line + "\nExecStart=/usr/bin/vllm serve /models/v --host 127.0.0.1 --port 9100\n"
		if _, err := parseOwnedLaunchUnit([]byte(adopted), n); err != nil {
			t.Fatalf("adopted %s rejected: %v", line, err)
		}
		owned := strings.Replace(string(raw), exec, line+"\n"+exec, 1)
		if err := qualifyFixtureLaunch([]byte(owned), *p.NativeModel); !errors.Is(err, ErrLaunchUnsupported) || !strings.Contains(err.Error(), strings.Split(line, "=")[0]) {
			t.Fatalf("owned %s = %v", line, err)
		}
	}
	// An owned unit without limits rejects the same lines: the ban does not
	// depend on a limits block.
	plain, plainRaw := ownedUnitWithoutLimits(t, p)
	plainExec := string(plainRaw[strings.Index(string(plainRaw), "ExecStart="):])
	if err := qualifyFixtureLaunch(plainRaw, *plain.NativeModel); err != nil {
		t.Fatalf("owned unit without limits does not qualify: %v", err)
	}
	for _, line := range []string{"MemoryMax=2G", "CPUQuota=10%", "OOMPolicy=continue", "StartupCPUWeight=50"} {
		owned := strings.Replace(string(plainRaw), plainExec, line+"\n"+plainExec, 1)
		if err := qualifyFixtureLaunch([]byte(owned), *plain.NativeModel); !errors.Is(err, ErrLaunchUnsupported) || !strings.Contains(err.Error(), strings.Split(line, "=")[0]) {
			t.Fatalf("owned unit without limits accepted %s: %v", line, err)
		}
	}
	if resourceLimitDirective("Description") || resourceLimitDirective("StartupDescription") {
		t.Fatal("unrelated directive classified as a limit")
	}
}

// limitedManager adds a verifiable owned unit with limits next to the
// stopped text and media fixtures.
func limitedManager(t *testing.T) (*SystemdManager, *fakeRunner, control.WorkloadProfile) {
	t.Helper()
	p, raw := limitedOwnedProfile(t)
	dir := t.TempDir()
	p.NativeModel.LaunchFile = filepath.Join(dir, p.Unit)
	if err := os.WriteFile(p.NativeModel.LaunchFile, raw, 0600); err != nil {
		t.Fatal(err)
	}
	p.Label, p.Adapter, p.Cgroup, p.HealthURL = "Vision", "systemd", "/workloads/app.slice/"+p.Unit, "http://127.0.0.1:9100/health"
	r := stoppedRunner()
	m := strictManager(t, r)
	catalog := m.config.Catalog.Clone()
	catalog.Version = control.CatalogVersionLimits
	catalog.Profiles = append(catalog.Profiles, p)
	m.config.Catalog = &catalog
	m.config.OwnedUnitDir = dir
	m.nativeExecutableValidator = fixtureExecutableValidator
	launch, err := parseOwnedLaunchUnit(raw, *p.NativeModel)
	if err != nil {
		t.Fatal(err)
	}
	show := "/usr/bin/true --user show --property=FragmentPath --property=DropInPaths --property=NeedDaemonReload --property=ExecStartPre --property=ExecStart -- " + p.Unit
	r.outputs[show] = []byte("ExecStart={ path=" + strings.Fields(launch.execStart)[0] + " ; argv[]=" + launch.execStart + " ; }\nFragmentPath=" + p.NativeModel.LaunchFile + "\nDropInPaths=\nNeedDaemonReload=no\n")
	r.outputs["/usr/bin/true --user show --property=LoadState --property=ActiveState --property=SubState --property=ControlGroup -- "+p.Unit] = stoppedOutput()
	for _, group := range []string{"text.service", "media.service", "app.slice/" + p.Unit} {
		writeEvents(t, filepath.Join(m.cgroups.root, "workloads"), group, "populated 0\n")
	}
	return m, r, p
}

func TestUnitWithLimitsDoesNotLaunchUntilEnforced(t *testing.T) {
	m, r, p := limitedManager(t)
	ctx := context.Background()
	// The gate covers only the limited profile's own start and health.
	// Preflight, release for any target and other profiles stay available,
	// so setup can still remove the limits.
	if err := m.verifyNativeBinding(ctx, p); err != nil {
		t.Fatalf("binding verification of a limited unit = %v", err)
	}
	if err := m.Preflight(ctx); err != nil {
		t.Fatalf("preflight with a limited unit = %v", err)
	}
	for _, target := range []control.Workload{control.WorkloadIdle, control.WorkloadText, p.ID} {
		if err := m.ReleasedFor(ctx, target); err != nil {
			t.Fatalf("release for %s with a limited unit = %v", target, err)
		}
	}
	calls := len(r.calls)
	for name, call := range map[string]func() error{
		"start":  func() error { return m.Start(ctx, p.ID) },
		"health": func() error { return m.Healthy(ctx, p.ID) },
	} {
		err := call()
		if !errors.Is(err, ErrLimitsNotEnforced) || !strings.Contains(err.Error(), string(p.ID)) || !strings.Contains(err.Error(), p.Unit) {
			t.Fatalf("%s of a limited unit = %v", name, err)
		}
	}
	for _, call := range r.calls[calls:] {
		if strings.Contains(call, " start ") {
			t.Fatalf("limited unit started: %v", r.calls[calls:])
		}
	}
	// Another profile still starts and gets a health check without the gate.
	calls = len(r.calls)
	if err := m.Start(ctx, control.WorkloadText); errors.Is(err, ErrLimitsNotEnforced) {
		t.Fatalf("start of an unlimited profile = %v", err)
	}
	if !slices.ContainsFunc(r.calls[calls:], func(call string) bool { return strings.HasSuffix(call, " start -- text.service") }) {
		t.Fatalf("unlimited profile not started: %v", r.calls[calls:])
	}
	if err := m.Healthy(ctx, control.WorkloadText); errors.Is(err, ErrLimitsNotEnforced) {
		t.Fatalf("health of an unlimited profile = %v", err)
	}
	plain := ownedRenderProfile("vllm", "vision", &control.OwnedLaunch{ModelPath: "/models/v", Port: 9100})
	if err := requireEnforceableLimits(plain); err != nil {
		t.Fatalf("unit without limits blocked: %v", err)
	}
	if err := requireEnforceableLimits(control.WorkloadProfile{}); err != nil {
		t.Fatalf("profile without a native model blocked: %v", err)
	}
}

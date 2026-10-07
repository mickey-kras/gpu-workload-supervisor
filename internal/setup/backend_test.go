package setup

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/deployment"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/lock"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

type idleRuntime struct{ err error }

func (r idleRuntime) Observe(context.Context) (gpuruntime.Snapshot, error) {
	return gpuruntime.Snapshot{Workloads: map[control.Workload]gpuruntime.WorkloadObservation{}}, r.err
}
func (r idleRuntime) Start(context.Context, control.Workload) error   { return r.err }
func (r idleRuntime) Stop(context.Context, control.Workload) error    { return r.err }
func (r idleRuntime) StopForRecovery(context.Context) error           { return r.err }
func (r idleRuntime) Healthy(context.Context, control.Workload) error { return r.err }
func (r idleRuntime) ReleasedFor(context.Context, control.Workload) error {
	return r.err
}
func (r idleRuntime) Preflight(context.Context) error { return r.err }
func fixture(t *testing.T) (Backend, string, Request) {
	t.Helper()
	home := t.TempDir()
	r := Request{Version: 1, ConfirmQuiesced: true, Profile: Profile{Version: 1, StatePath: filepath.Join(home, "state/state.db"), SystemctlPath: "/usr/bin/systemctl", NvidiaSMIPath: "/usr/bin/nvidia-smi"}, Catalog: control.Catalog{Version: 1, Profiles: []control.WorkloadProfile{{ID: "text", Label: "Text", Adapter: "systemd", Unit: "text.service", Cgroup: "/user.slice/text", HealthURL: "http://127.0.0.1:8000/health", BootPolicy: "stop-to-idle"}}}}
	backend := SystemBackend()
	backend.makeRuntime = func(Request) (gpuruntime.Manager, error) { return idleRuntime{}, nil }
	backend.runCommand = func(context.Context, string, ...string) ([]byte, error) {
		return []byte("text.service disabled\nmedia.service disabled\n"), nil
	}
	backend.binaryDirectory = t.TempDir()
	backend.packageBinaryUID = uint32(os.Geteuid())
	for _, name := range binaries {
		if err := os.WriteFile(filepath.Join(backend.binaryDirectory, name), []byte("binary-"+name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	return backend, home, r
}
func TestEnableReconciliationErrorExcludesSubprocessOutput(t *testing.T) {
	backend, home, r := fixture(t)
	backend.runCommand = func(context.Context, string, ...string) ([]byte, error) {
		return []byte("sentinel-output\x1b[31m\n"), errors.New("exit status 1")
	}
	if err := os.MkdirAll(filepath.Join(home, ".config/gpu-workload-supervisor"), 0700); err != nil {
		t.Fatal(err)
	}
	err := backend.enableReconciliation(context.Background(), home, r.Profile.SystemctlPath)
	if err == nil {
		t.Fatal("expected enable failure")
	}
	if strings.Contains(err.Error(), "sentinel") || strings.ContainsAny(err.Error(), "\x1b\n") {
		t.Fatalf("unsafe error: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "exit status 1") {
		t.Fatalf("lost exit status: %v", err)
	}
}

func TestApplyFreshRepeatUpgradeDowngradeAndBackup(t *testing.T) {
	backend, home, r := fixture(t)
	ctx := context.Background()
	prior := deployment.Release
	deployment.Release = "0.9.0"
	t.Cleanup(func() { deployment.Release = prior })
	if err := backend.Apply(ctx, home, r); err != nil {
		t.Fatal(err)
	}
	if err := deployment.Check(r.Profile.StatePath, deployment.Release); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(ctx, r.Profile.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, _ := s.Catalog(ctx)
	state, _ := s.State(ctx)
	s.Close()
	if state.ActiveWorkload != control.WorkloadIdle || state.Owner != control.OwnerSupervisor {
		t.Fatal(state)
	}
	r.ExpectedRevision = snapshot.Revision
	if err := backend.Apply(ctx, home, r); err != nil {
		t.Fatal(err)
	}
	deployment.Release = "1.0.0"
	if err := backend.Apply(ctx, home, r); err != nil {
		t.Fatal(err)
	}
	deployment.Release = "0.8.0"
	if err := backend.Apply(ctx, home, r); err == nil {
		t.Fatal("downgrade accepted")
	}
}
func TestApplyFailuresAndMaintenanceResume(t *testing.T) {
	backend, home, r := fixture(t)
	ctx := context.Background()
	r.ConfirmQuiesced = false
	if err := backend.Apply(ctx, home, r); err == nil {
		t.Fatal("implicit activation")
	}
	r.ConfirmQuiesced = true
	gate, err := lock.TryAcquire(r.Profile.StatePath + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.Apply(ctx, home, r); err == nil {
		t.Fatal("gate contention accepted")
	}
	gate.Close()
	backend.makeRuntime = func(Request) (gpuruntime.Manager, error) { return idleRuntime{err: errors.New("GPU busy")}, nil }
	if err := backend.Apply(ctx, home, r); err == nil {
		t.Fatal("live runtime accepted")
	}
	backend.makeRuntime = func(Request) (gpuruntime.Manager, error) { return idleRuntime{}, nil }
	backend.runCommand = func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New("unit manager unavailable")
	}
	if err := backend.Apply(ctx, home, r); err == nil {
		t.Fatal("enable failed silently")
	}
	if err := deployment.Check(r.Profile.StatePath, ""); err == nil {
		t.Fatal("maintenance fence absent")
	}
	changed := r
	changed.Catalog.Profiles = append([]control.WorkloadProfile(nil), r.Catalog.Profiles...)
	changed.Catalog.Profiles[0].Label = "Changed"
	if err := backend.Apply(ctx, home, changed); err == nil {
		t.Fatal("interrupted plan replaced")
	}
	backend.runCommand = func(context.Context, string, ...string) ([]byte, error) { return nil, nil }
	if err := backend.Apply(ctx, home, r); err != nil {
		t.Fatal(err)
	}
	if err := deployment.Check(r.Profile.StatePath, deployment.Release); err != nil {
		t.Fatal(err)
	}
}
func TestDecodePlanDiscoverAndValidation(t *testing.T) {
	backend, home, r := fixture(t)
	data, _ := json.Marshal(r)
	if _, err := Decode(strings.NewReader(string(data))); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"", string(data) + " {}", strings.Repeat(" ", 262145), `{"unknown":true}`, `{"version":2}`} {
		if _, err := Decode(strings.NewReader(text)); err == nil {
			t.Fatal("bad input accepted")
		}
	}
	if _, err := backend.Plan(home, r); err != nil {
		t.Fatal(err)
	}
	if _, err := Home(); err != nil {
		t.Fatal(err)
	}
	bad := r
	bad.Profile.StatePath = "relative"
	if err := Validate(bad); err == nil {
		t.Fatal("relative path")
	}
	bad = r
	bad.Profile.GPUIndex = -1
	if err := Validate(bad); err == nil {
		t.Fatal("negative GPU")
	}
	d, err := backend.Discover(context.Background(), home)
	if err != nil || len(d.Units) != 0 {
		t.Fatalf("%+v %v", d, err)
	}
	if err := backend.Apply(context.Background(), home, r); err != nil {
		t.Fatal(err)
	}
	d, err = backend.Discover(context.Background(), home)
	if err != nil || d.Request.ExpectedRevision == "" {
		t.Fatalf("%+v %v", d, err)
	}
	if err := backend.Reconcile(context.Background(), home); err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{{"1.1.0", "1.0.9"}, {"v2.0.0", "1.9.9"}} {
		if !newer(pair[0], pair[1]) {
			t.Fatal(pair)
		}
	}
	for _, pair := range [][2]string{{"dev", "1.0.0"}, {"1.0.0", "dev"}, {"1.0.0", "1.0.0"}, {"1.-1.0", "1.0.0"}, {"1.0.0", "2.0.0"}} {
		if newer(pair[0], pair[1]) {
			t.Fatal(pair)
		}
	}
}

func TestOwnedIntegrationRemovalPreservesUserData(t *testing.T) {
	backend, home, r := fixture(t)
	ctx := context.Background()
	if err := backend.Apply(ctx, home, r); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home, ".config/systemd/user/default.target.wants", reconcileUnit)
	if err := os.Symlink("/usr/lib/systemd/user/"+reconcileUnit, link); err != nil {
		t.Fatal(err)
	}
	if err := RemoveIntegration(home); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(r.Profile.StatePath); err != nil {
		t.Fatal("state removed", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".config/gpu-workload-supervisor/operator.json")); err != nil {
		t.Fatal("profile removed", err)
	}
	if err := RemoveIntegration(home); err != nil {
		t.Fatal(err)
	}
	if err := backend.enableReconciliation(ctx, home, r.Profile.SystemctlPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/some/user.service", link); err != nil {
		t.Fatal(err)
	}
	if err := RemoveIntegration(home); err == nil {
		t.Fatal("foreign link removed")
	}
	if err := backend.enableReconciliation(ctx, home, r.Profile.SystemctlPath); err == nil {
		t.Fatal("foreign link overwritten")
	}
	os.Remove(link)
	userUnit := filepath.Join(home, ".config/systemd/user", reconcileUnit)
	os.WriteFile(userUnit, []byte("user"), 0600)
	if err := backend.enableReconciliation(ctx, home, r.Profile.SystemctlPath); err == nil {
		t.Fatal("user unit overwritten")
	}
}

func TestApplyEnablesAndRemovesIdleTimerSymmetrically(t *testing.T) {
	backend, home, r := fixture(t)
	ctx := context.Background()
	var enabled []string
	backend.runCommand = func(_ context.Context, name string, args ...string) ([]byte, error) {
		if len(args) == 3 && args[0] == "--user" && args[1] == "enable" {
			enabled = append(enabled, args[2])
		}
		return []byte("text.service disabled\n"), nil
	}
	if err := backend.Apply(ctx, home, r); err != nil {
		t.Fatal(err)
	}
	if len(enabled) != 2 || enabled[0] != reconcileUnit || enabled[1] != idleTimerUnit {
		t.Fatalf("enabled units: %v", enabled)
	}
	timerLink := filepath.Join(home, ".config/systemd/user/timers.target.wants", idleTimerUnit)
	reconcileLink := filepath.Join(home, ".config/systemd/user/default.target.wants", reconcileUnit)
	for _, link := range []string{reconcileLink, timerLink} {
		if err := os.Symlink("/usr/lib/systemd/user/"+filepath.Base(link), link); err != nil {
			t.Fatal(err)
		}
	}
	if err := RemoveIntegration(home); err != nil {
		t.Fatal(err)
	}
	for _, link := range []string{reconcileLink, timerLink} {
		if _, err := os.Lstat(link); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("link survived removal: %s", link)
		}
	}
	// A foreign timer link is preserved, and removal reports it.
	if err := backend.enableIdleTimer(ctx, home, r.Profile.SystemctlPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/some/foreign.timer", timerLink); err != nil {
		t.Fatal(err)
	}
	if err := RemoveIntegration(home); err == nil {
		t.Fatal("foreign timer link removed")
	}
	if _, err := os.Lstat(timerLink); err != nil {
		t.Fatal("foreign timer link lost", err)
	}
	// A user-provided timer override is refused.
	os.Remove(timerLink)
	if err := os.WriteFile(filepath.Join(home, ".config/systemd/user", idleTimerUnit), []byte("user"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := backend.enableIdleTimer(ctx, home, r.Profile.SystemctlPath); err == nil {
		t.Fatal("user timer override overwritten")
	}
}

func TestPolicyTickEvaluatesThePersistedProfile(t *testing.T) {
	backend, home, r := fixture(t)
	ctx := context.Background()
	// The fixture profile uses a non-default state path under the temp home;
	// a tick that ignored operator.json would never find this state.
	if err := backend.Apply(ctx, home, r); err != nil {
		t.Fatal(err)
	}
	if err := backend.PolicyTick(ctx, home); err != nil {
		t.Fatalf("tick with off policy: %v", err)
	}
	// Enable the policy on a stable running workload: without a qualified
	// evidence provider the tick must fail closed, never idle silently.
	s, err := store.Open(ctx, r.Profile.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	state, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	state.DesiredWorkload = control.WorkloadText
	state.ActiveWorkload = control.WorkloadText
	state.Admission = control.AdmissionOpen
	state, err = s.UpdateState(ctx, state.Version, state)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := s.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	settings, err := s.Settings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	e := control.SettingsPrecondition{
		Incarnation: state.LeaseFence.Incarnation, Version: state.Version, Owner: state.Owner,
		ConfigurationRevision: snap.Revision, SettingsRevision: settings.SettingsRevision,
	}
	if _, err := s.SetIdlePolicy(ctx, e, control.IdlePolicy{TimeoutMinutes: 5}, true); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := backend.PolicyTick(ctx, home); err == nil || !strings.Contains(err.Error(), "evidence") {
		t.Fatalf("enabled tick without evidence: %v", err)
	}
}

func TestPolicyTickFailsLoudlyWithoutOperatorProfile(t *testing.T) {
	backend, home, _ := fixture(t)
	if err := backend.PolicyTick(context.Background(), home); err == nil {
		t.Fatal("tick without operator.json silently used defaults")
	}
}

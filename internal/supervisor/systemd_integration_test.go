//go:build systemd_integration

package supervisor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

// This opt-in suite never skips: selecting the tag requires real user systemd
// and the host unified cgroup hierarchy. No production services are touched.
type systemdFixture struct {
	t          *testing.T
	manager    *gpuruntime.SystemdManager
	store      *store.Store
	controller *Controller
	units      []string
	path       string
	unitDir    string
	catalog    control.Catalog
	revision   string
}

func systemdCommand(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "systemctl", append([]string{"--user"}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("systemctl %v: %v: %s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func newSystemdFixture(t *testing.T) *systemdFixture {
	t.Helper()
	anchor := systemdCommand(t, "show", "--property=ControlGroup", "--value", "--", "-.slice")
	if !strings.HasPrefix(anchor, "/") || strings.ContainsAny(anchor, "\r\n") {
		t.Fatalf("real user systemd prerequisite missing: invalid root cgroup %q", anchor)
	}
	f := &systemdFixture{t: t, path: filepath.Join(t.TempDir(), "state.db"), unitDir: t.TempDir()}
	unitDir := f.unitDir
	prefix := fmt.Sprintf("gws-qualification-%d-%d", os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() {
		for _, unit := range f.units {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = exec.CommandContext(ctx, "systemctl", "--user", "kill", "--kill-whom=all", "--signal=KILL", unit).Run()
			cancel()
			for _, action := range []string{"stop", "disable"} {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				output, err := exec.CommandContext(ctx, "systemctl", "--user", action, unit).CombinedOutput()
				cancel()
				if err != nil {
					t.Errorf("cleanup %s %s: %v: %s", action, unit, err, output)
				}
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if output, err := exec.CommandContext(ctx, "systemctl", "--user", "daemon-reload").CombinedOutput(); err != nil {
			t.Errorf("cleanup reload: %v: %s", err, output)
		}
	})
	for _, kind := range []string{"text", "media"} {
		unit := prefix + "-" + kind + ".service"
		f.units = append(f.units, unit)
		data := "[Unit]\nDescription=Isolated workload qualification fixture\n[Service]\nType=exec\nExecStart=/usr/bin/sleep infinity\nKillMode=control-group\nTimeoutStopSec=2\n"
		path := filepath.Join(unitDir, unit)
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		systemdCommand(t, "link", path)
	}

	systemdCommand(t, "daemon-reload")
	groups := make([]string, 2)
	for i, unit := range f.units {
		systemdCommand(t, "start", unit)
		groups[i] = systemdCommand(t, "show", "--property=ControlGroup", "--value", unit)
		if groups[i] == "" {
			t.Fatal("real unit has no ControlGroup")
		}
		systemdCommand(t, "stop", unit)
	}
	health := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	t.Cleanup(health.Close)
	executable, err := exec.LookPath("systemctl")
	if err != nil {
		t.Fatal(err)
	}
	catalog := control.Catalog{Version: 1, Profiles: []control.WorkloadProfile{
		{ID: control.WorkloadText, Label: "text", Adapter: "systemd", Unit: f.units[0], Cgroup: groups[0], HealthURL: health.URL},
		{ID: control.WorkloadMedia, Label: "media", Adapter: "systemd", Unit: f.units[1], Cgroup: groups[1], HealthURL: health.URL},
	}}
	f.catalog = catalog
	f.manager, err = gpuruntime.NewSystemdManager(gpuruntime.SystemdConfig{
		Catalog: &catalog, SystemctlPath: executable, HealthTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.reopen(false)
	t.Cleanup(func() {
		if f.store != nil {
			if err := f.store.Close(); err != nil {
				t.Error(err)
			}
		}
	})
	return f
}

func (f *systemdFixture) reopen(restored bool) {
	f.t.Helper()
	if f.store != nil {
		if err := f.store.Close(); err != nil {
			f.t.Fatal(err)
		}
	}
	var err error
	if restored {
		f.store, err = store.OpenRestored(context.Background(), f.path)
	} else {
		f.store, err = store.Open(context.Background(), f.path)
	}
	if err != nil {
		f.t.Fatal(err)
	}
	snapshot, err := f.store.Catalog(context.Background())
	if err != nil {
		f.t.Fatal(err)
	}
	if snapshot.Revision == "" {
		snapshot, err = f.store.ReplaceCatalog(context.Background(), "", f.catalog)
		if err != nil {
			f.t.Fatal(err)
		}
	}
	f.revision = snapshot.Revision
	f.controller, err = New(f.store, f.manager, Config{Catalog: &snapshot, DrainTimeout: time.Second, VerifyTimeout: time.Second, ActionTimeout: 5 * time.Second, CleanupTimeout: 5 * time.Second, FinalizeTimeout: time.Second, PollInterval: 10 * time.Millisecond})
	if err != nil {
		f.t.Fatal(err)
	}
}

func (f *systemdFixture) assertState(state control.State, owner control.Owner, target control.Workload) {
	f.t.Helper()
	admission := control.AdmissionClosed
	if owner == control.OwnerSupervisor && target != control.WorkloadIdle {
		admission = control.AdmissionOpen
	}
	if state.Owner != owner || state.ActiveWorkload != target || state.DesiredWorkload != target || state.Admission != admission || state.Health != control.HealthHealthy || state.Phase != control.PhaseStable {
		f.t.Fatalf("unexpected state: %#v", state)
	}
	snapshot, err := f.manager.Observe(context.Background())
	if err != nil {
		f.t.Fatal(err)
	}
	if snapshot.Workloads[control.WorkloadText].Active != (target == control.WorkloadText) || snapshot.Workloads[control.WorkloadMedia].Active != (target == control.WorkloadMedia) {
		f.t.Fatalf("real units disagree with state: %#v", snapshot)
	}
}

func TestSystemdDirectedTransitionsAndOwnership(t *testing.T) {
	f := newSystemdFixture(t)
	ctx := context.Background()
	initial, err := f.store.State(ctx)
	if err != nil || initial.Admission != control.AdmissionClosed {
		t.Fatalf("cold admission: %#v %v", initial, err)
	}
	if _, err = f.controller.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	workloads := []control.Workload{control.WorkloadIdle, control.WorkloadText, control.WorkloadMedia}
	for _, owner := range []control.Owner{control.OwnerSupervisor, control.OwnerUser} {
		if owner == control.OwnerUser {
			if _, err = f.controller.TransferToUser(ctx, control.WorkloadIdle, "qualification"); err != nil {
				t.Fatal(err)
			}
		}
		change := f.controller.Switch
		reject := f.controller.SwitchUser
		wantError := ErrSupervisorOwned
		if owner == control.OwnerUser {
			change = f.controller.SwitchUser
			reject = f.controller.Switch
			wantError = ErrUserOwned
		}
		for _, source := range workloads {
			for _, target := range workloads {
				if source == target {
					continue
				}
				t.Run(string(owner)+"/"+string(source)+"-to-"+string(target), func(t *testing.T) {
					if _, err := change(ctx, source, "qualification"); err != nil {
						t.Fatal(err)
					}
					before, err := f.store.State(ctx)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := reject(ctx, target, "wrong-owner"); !errors.Is(err, wantError) {
						t.Fatalf("wrong owner: %v", err)
					}
					afterReject, err := f.store.State(ctx)
					if err != nil || afterReject != before {
						t.Fatalf("rejection mutated state: %v", err)
					}
					state, err := change(ctx, target, "qualification")
					if err != nil {
						t.Fatal(err)
					}
					f.assertState(state, owner, target)
					if _, err := f.store.AdmitWorkToken(ctx, "stale", "", control.WorkloadText, before.LeaseFence); !errors.Is(err, store.ErrStaleFence) {
						t.Fatalf("stale fence accepted: %v", err)
					}
				})
			}
		}
	}
	state, err := f.controller.TransferToSupervisor(ctx, control.WorkloadIdle, "qualification")
	if err != nil {
		t.Fatal(err)
	}
	f.assertState(state, control.OwnerSupervisor, control.WorkloadIdle)
	if err := f.manager.ReleasedFor(ctx, control.WorkloadIdle); err != nil {
		t.Fatal(err)
	}
}

func TestSystemdRestartPreservesUserAndDoesNotRestartStoppedWork(t *testing.T) {
	f := newSystemdFixture(t)
	ctx := context.Background()
	if _, err := f.controller.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	before, err := f.controller.TransferToUser(ctx, control.WorkloadText, "qualification")
	if err != nil {
		t.Fatal(err)
	}
	// Save a closed SQLite snapshot while the earlier user fence is current.
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	backup, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	f.store = nil
	f.reopen(false)
	systemdCommand(t, "stop", f.units[0])
	f.reopen(false)
	state, err := f.controller.Reconcile(ctx)
	if !errors.Is(err, ErrUserOwned) || state.Owner != control.OwnerUser || state.Admission != control.AdmissionClosed {
		t.Fatalf("restart lost owner: %#v %v", state, err)
	}
	snapshot, err := f.manager.Observe(ctx)
	if err != nil || snapshot.AnyActive() {
		t.Fatalf("stopped work restarted: %#v %v", snapshot, err)
	}
	state, err = f.controller.RecoverUser(ctx, control.WorkloadIdle, "qualification")
	if err != nil {
		t.Fatal(err)
	}
	f.assertState(state, control.OwnerUser, control.WorkloadIdle)
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	f.store = nil
	f.path = filepath.Join(t.TempDir(), "restored.db")
	if err := os.WriteFile(f.path, backup, 0600); err != nil {
		t.Fatal(err)
	}
	f.reopen(true)
	// Match restore-state: validating/opening a snapshot never rotates by itself.
	state, err = f.store.RotateIncarnation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if state.LeaseFence.Incarnation == before.LeaseFence.Incarnation || state.LeaseFence.Epoch != 1 || state.Admission != control.AdmissionClosed || state.Owner != control.OwnerSupervisor {
		t.Fatalf("restored fence/admission: %#v", state)
	}
	if _, err := f.store.AdmitWorkToken(ctx, "restored-stale", "", control.WorkloadText, before.LeaseFence); !errors.Is(err, store.ErrStaleFence) {
		t.Fatalf("restored stale fence: %v", err)
	}
}

// Faults are confined to the manager interface; starts, stops, observation and
// release otherwise execute against real systemd and kernel cgroup evidence.
type qualificationFault struct {
	*gpuruntime.SystemdManager
	failure string
}

func (m *qualificationFault) Healthy(ctx context.Context, workload control.Workload) error {
	if m.failure == "health" {
		return errors.New("injected health failure")
	}
	return m.SystemdManager.Healthy(ctx, workload)
}
func (m *qualificationFault) ReleasedFor(ctx context.Context, target control.Workload) error {
	if m.failure == "release" {
		return errors.New("injected release timeout")
	}
	return m.SystemdManager.ReleasedFor(ctx, target)
}
func (m *qualificationFault) Observe(ctx context.Context) (gpuruntime.Snapshot, error) {
	if m.failure == "inspection" {
		return gpuruntime.Snapshot{}, errors.New("injected inspection failure")
	}
	return m.SystemdManager.Observe(ctx)
}

func TestSystemdFailuresLatchAndRequireExplicitRecovery(t *testing.T) {
	for _, failure := range []string{"health", "release", "inspection"} {
		t.Run(failure, func(t *testing.T) {
			f := newSystemdFixture(t)
			ctx := context.Background()
			if _, err := f.controller.Reconcile(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := f.controller.TransferToUser(ctx, control.WorkloadText, "qualification"); err != nil {
				t.Fatal(err)
			}
			f.controller.runtime = &qualificationFault{SystemdManager: f.manager, failure: failure}
			state, err := f.controller.SwitchUser(ctx, control.WorkloadMedia, "qualification")
			if err == nil || state.Owner != control.OwnerUser || state.Admission != control.AdmissionClosed || state.Health != control.HealthError {
				t.Fatalf("fault not latched: %#v %v", state, err)
			}
			if _, err := f.controller.SwitchUser(ctx, control.WorkloadText, "qualification"); !errors.Is(err, ErrRecoveryRequired) {
				t.Fatalf("latched failure bypass: %v", err)
			}
			snapshot, err := f.manager.Observe(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if failure != "inspection" && snapshot.Workloads[control.WorkloadText].Active {
				t.Fatal("failure restarted stopped user work")
			}
			systemdCommand(t, "stop", f.units[0], f.units[1])
			f.reopen(false)
			if _, err := f.controller.Reconcile(ctx); !errors.Is(err, ErrUserOwned) {
				t.Fatalf("restart forgot error: %v", err)
			}
			state, err = f.controller.RecoverUser(ctx, control.WorkloadIdle, "qualification")
			if err != nil {
				t.Fatal(err)
			}
			f.assertState(state, control.OwnerUser, control.WorkloadIdle)
		})
	}
}

func TestSystemdSurvivingDescendantBlocksRelease(t *testing.T) {
	f := newSystemdFixture(t)
	ctx := context.Background()
	data := "[Service]\nType=exec\nExecStart=/bin/sh -c 'sleep infinity & wait'\nKillMode=process\nTimeoutStopSec=2\n"
	if err := os.WriteFile(filepath.Join(f.unitDir, f.units[0]), []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	systemdCommand(t, "daemon-reload")
	systemdCommand(t, "start", f.units[0])
	group := systemdCommand(t, "show", "--property=ControlGroup", "--value", f.units[0])
	deadline := time.Now().Add(5 * time.Second)
	for {
		pids, err := os.ReadFile(filepath.Join("/sys/fs/cgroup", group, "cgroup.procs"))
		if err != nil {
			t.Fatal(err)
		}
		if len(strings.Fields(string(pids))) >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fixture descendant did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	systemdCommand(t, "stop", f.units[0])
	if err := f.manager.ReleasedFor(ctx, control.WorkloadIdle); err == nil {
		t.Fatal("surviving descendant qualified as released")
	}
	systemdCommand(t, "kill", "--kill-whom=all", "--signal=KILL", f.units[0])
	deadline = time.Now().Add(5 * time.Second)
	for {
		if err := f.manager.ReleasedFor(ctx, control.WorkloadIdle); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("release did not clear after descendant exit")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSystemdAdmissionDrainAndConcurrentCommands(t *testing.T) {
	f := newSystemdFixture(t)
	ctx := context.Background()
	if _, err := f.controller.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	before, err := f.controller.Switch(ctx, control.WorkloadText, "qualification")
	if err != nil {
		t.Fatal(err)
	}
	token, err := f.store.AdmitWorkToken(ctx, "running", "", control.WorkloadText, before.LeaseFence)
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		state control.State
		err   error
	}
	done := make(chan result, 1)
	go func() {
		state, err := f.controller.TransferToUser(ctx, control.WorkloadMedia, "qualification")
		done <- result{state, err}
	}()
	deadline := time.Now().Add(time.Second)
	for {
		state, err := f.store.State(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if state.Phase == control.PhaseDraining {
			if _, err := f.store.AdmitWorkToken(ctx, "late", "", control.WorkloadText, state.LeaseFence); !errors.Is(err, store.ErrAdmissionClosed) {
				t.Fatalf("admission raced drain: %v", err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("drain did not start")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := f.controller.Switch(ctx, control.WorkloadIdle, "concurrent"); !errors.Is(err, ErrTransitionRunning) {
		t.Fatalf("concurrent command: %v", err)
	}
	snapshot, err := f.manager.Observe(ctx)
	if err != nil || !snapshot.Workloads[control.WorkloadText].Active || snapshot.Workloads[control.WorkloadMedia].Active {
		t.Fatalf("runtime changed before drain: %#v %v", snapshot, err)
	}
	if err := f.store.FinishWorkToken(ctx, "running", control.WorkloadText, before.LeaseFence, token, store.WorkCompleted); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatal(got.err)
		}
		f.assertState(got.state, control.OwnerUser, control.WorkloadMedia)
	case <-time.After(10 * time.Second):
		t.Fatal("drained handoff did not finish")
	}
}

func TestSystemdInterruptedJournalPhases(t *testing.T) {
	for _, phase := range []control.Phase{control.PhaseDraining, control.PhaseUnloading, control.PhaseLoading, control.PhaseVerifying} {
		t.Run(string(phase), func(t *testing.T) {
			f := newSystemdFixture(t)
			ctx := context.Background()
			before, err := f.controller.Reconcile(ctx)
			if err != nil {
				t.Fatal(err)
			}
			target := before
			target.DesiredWorkload = control.WorkloadText
			state, err := f.store.StartTransition(ctx, before.Version, store.Transition{ID: "interrupted", Source: before, Target: target, Previous: before, Initiator: "qualification", Deadline: time.Now().Add(time.Minute), ConfigurationRevision: f.revision})
			if err != nil {
				t.Fatal(err)
			}
			if phase != control.PhaseDraining {
				if _, err = f.store.SetTransitionPhase(ctx, "interrupted", state.Version, phase); err != nil {
					t.Fatal(err)
				}
			}
			f.reopen(false)
			state, err = f.controller.Reconcile(ctx)
			if !errors.Is(err, ErrRecoveryRequired) || state.Admission != control.AdmissionClosed || state.Health != control.HealthError {
				t.Fatalf("interrupted journal reopened: %#v %v", state, err)
			}
			snapshot, err := f.manager.Observe(ctx)
			if err != nil || snapshot.AnyActive() {
				t.Fatalf("interrupted journal restarted work: %#v %v", snapshot, err)
			}
			state, err = f.controller.Recover(ctx)
			if err != nil {
				t.Fatal(err)
			}
			f.assertState(state, control.OwnerSupervisor, control.WorkloadIdle)
		})
	}
}

func TestSystemdPreflightAllowsFailedRemovedCgroupRecovery(t *testing.T) {
	f := newSystemdFixture(t)
	ctx := context.Background()
	if _, err := f.controller.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	before, err := f.controller.Switch(ctx, control.WorkloadText, "qualification")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.AdmitWorkToken(ctx, "crashed-work", "", control.WorkloadText, before.LeaseFence); err != nil {
		t.Fatal(err)
	}
	group := systemdCommand(t, "show", "--property=ControlGroup", "--value", f.units[0])
	systemdCommand(t, "kill", "--kill-whom=all", "--signal=KILL", f.units[0])
	deadline := time.Now().Add(5 * time.Second)
	for {
		active := systemdCommand(t, "show", "--property=ActiveState", "--value", f.units[0])
		sub := systemdCommand(t, "show", "--property=SubState", "--value", f.units[0])
		_, statErr := os.Stat(filepath.Join("/sys/fs/cgroup", group))
		if active == "failed" && sub == "failed" && errors.Is(statErr, os.ErrNotExist) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("failed unit cgroup not removed: %s/%s %v", active, sub, statErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := f.manager.Preflight(ctx); err != nil {
		t.Fatalf("failed unit wedged preflight: %v", err)
	}
	if err := f.manager.ReleasedFor(ctx, control.WorkloadIdle); err == nil {
		t.Fatal("failed unit qualified as released")
	}
	state, err := f.controller.Reconcile(ctx)
	if err == nil || state.Admission != control.AdmissionClosed || state.Health != control.HealthError {
		t.Fatalf("crash not latched: %#v %v", state, err)
	}
	state, count, err := f.controller.ResolveUnfinishedWork(ctx, "qualification crashed workload")
	if err != nil || count != 1 || state.Admission != control.AdmissionClosed {
		t.Fatalf("resolve crashed work: %#v %d %v", state, count, err)
	}
	state, err = f.controller.Recover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	f.assertState(state, control.OwnerSupervisor, control.WorkloadIdle)
}

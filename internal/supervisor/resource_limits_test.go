package supervisor

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

func limitedCatalog() control.Catalog {
	catalog := testCatalog()
	catalog.Version = control.CatalogVersionLimits
	catalog.Profiles = append(catalog.Profiles, control.WorkloadProfile{
		ID: "vision", Label: "Vision", Adapter: "systemd", Unit: "gws-owned-vision.service",
		Cgroup:    "/user.slice/user-1000.slice/user@1000.service/app.slice/gws-owned-vision.service",
		HealthURL: "http://127.0.0.1:9102/health",
		NativeModel: &control.NativeModel{
			Runtime: "vllm", Instance: "local", Model: "vision-q8", Endpoint: "http://127.0.0.1:9102",
			LaunchFile:   "/home/u/.config/systemd/user/gws-owned-vision.service",
			LaunchSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			Owned: &control.OwnedLaunch{ModelPath: "/models/vision-q8", Port: 9102, Alias: "vision-q8",
				Limits: control.ResourceLimits{Version: 1, MemoryMaxMiB: 8192, TasksMax: 4096}},
		},
	})
	return catalog
}

// limitedRuntime runs the vision unit from limitedCatalog next to the
// fake text and media units. launchErr, startErr, healthErr and
// protectionErr stand in for the runtime limit checks. alsoActive lists
// more running units, and protectionErrs gives the check result per unit.
type limitedRuntime struct {
	fakeRuntime
	visionActive   bool
	alsoActive     []control.Workload
	protectionErrs map[control.Workload]error
	launchErr      error
	launchChecks   int
	visionStartErr error
	visionStopErr  error
	healthErr      error
	// healthErrOnce clears healthErr after the first health check.
	healthErrOnce bool
	healthCalls   int
	protectionErr error
	protections   int
	// probeDeadlines records the deadline of each limit probe context.
	probeDeadlines []time.Time
	// onHealth runs in each health check of vision, before it returns.
	onHealth func()
	// stopCtxErrs records the context error of each stop of vision.
	stopCtxErrs []error
	// statePath is the database of the controller.
	statePath string
}

// recordDeadline keeps the deadline of a probe context, or the zero time.
func (r *limitedRuntime) recordDeadline(ctx context.Context) {
	deadline, _ := ctx.Deadline()
	r.probeDeadlines = append(r.probeDeadlines, deadline)
}

func (r *limitedRuntime) VerifyLaunchLimits(ctx context.Context, w control.Workload) error {
	r.recordDeadline(ctx)
	if w == "vision" {
		r.launchChecks++
	}
	return r.launchErr
}

func (r *limitedRuntime) VerifyLimitProtection(ctx context.Context, w control.Workload) error {
	r.recordDeadline(ctx)
	if w == "vision" {
		r.protections++
	}
	if err, ok := r.protectionErrs[w]; ok {
		return err
	}
	return r.protectionErr
}

func (r *limitedRuntime) Observe(ctx context.Context) (gpuruntime.Snapshot, error) {
	snapshot, err := r.fakeRuntime.Observe(ctx)
	if err == nil {
		snapshot.Workloads["vision"] = gpuruntime.WorkloadObservation{Active: r.visionActive, Exclusive: true}
		for _, w := range r.alsoActive {
			snapshot.Workloads[w] = gpuruntime.WorkloadObservation{Active: true, Exclusive: true}
		}
	}
	return snapshot, err
}

func (r *limitedRuntime) Start(ctx context.Context, w control.Workload) error {
	if w == "vision" {
		r.calls = append(r.calls, "start vision")
		if r.visionStartErr != nil {
			return r.visionStartErr
		}
		r.visionActive = true
		return nil
	}
	return r.fakeRuntime.Start(ctx, w)
}

func (r *limitedRuntime) Stop(ctx context.Context, w control.Workload) error {
	if w == "vision" {
		r.calls = append(r.calls, "stop vision")
		r.stopCtxErrs = append(r.stopCtxErrs, ctx.Err())
		if r.visionStopErr != nil {
			return r.visionStopErr
		}
		r.visionActive = false
		return nil
	}
	return r.fakeRuntime.Stop(ctx, w)
}

func (r *limitedRuntime) Healthy(ctx context.Context, w control.Workload) error {
	if w == "vision" {
		r.healthCalls++
		if r.onHealth != nil {
			r.onHealth()
		}
		err := r.healthErr
		if r.healthErrOnce {
			r.healthErr = nil
		}
		return err
	}
	return r.fakeRuntime.Healthy(ctx, w)
}

func limitedController(t *testing.T, catalog control.Catalog) (*Controller, *limitedRuntime, *store.Store) {
	t.Helper()
	statePath := filepath.Join(t.TempDir(), "state.db")
	s := openStoreAt(t, statePath)
	if _, err := s.ReplaceCatalog(context.Background(), "", catalog); err != nil {
		t.Fatal(err)
	}
	r := &limitedRuntime{fakeRuntime: fakeRuntime{active: control.WorkloadText, mediaReady: true}, statePath: statePath}
	c := testController(t, s, r)
	n := 0
	c.id = func() (string, error) { n++; return fmt.Sprintf("11111111-1111-4111-8111-%012d", n), nil }
	return c, r, s
}

func TestSwitchStartsLimitedTargetThroughRuntimeChecks(t *testing.T) {
	ctx := context.Background()
	c, r, _ := limitedController(t, limitedCatalog())
	if _, err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	state, err := c.Switch(ctx, "vision", "test")
	if err != nil || state.ActiveWorkload != "vision" || state.Health != control.HealthHealthy {
		t.Fatalf("switch to a limited target = %+v, %v", state, err)
	}
	if !slices.Contains(r.calls, "start vision") || r.healthCalls == 0 {
		t.Fatalf("limited target not started and checked: %v", r.calls)
	}
}

// A failed limit check after start ends the verification at once. The
// failed transition stops the unit through its rollback, restarts text and
// latches the error.
func TestLimitFailureAfterStartRollsBack(t *testing.T) {
	for name, tc := range map[string]struct {
		edit func(*limitedRuntime)
		want error
	}{
		"drift at health": {func(r *limitedRuntime) {
			r.healthErr = fmt.Errorf("%w: %w", gpuruntime.ErrLimitCheckFailed, gpuruntime.ErrLimitDrift)
		}, gpuruntime.ErrLimitDrift},
		// The runtime tags a failed read of a limit file of the running unit.
		"tagged readback at health": {func(r *limitedRuntime) {
			r.healthErr = fmt.Errorf("%w: %w", gpuruntime.ErrLimitCheckFailed, gpuruntime.ErrLimitReadback)
		}, gpuruntime.ErrLimitReadback},
		"memory at start": {func(r *limitedRuntime) { r.visionStartErr = gpuruntime.ErrLimitMemory }, gpuruntime.ErrLimitMemory},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			c, r, s := limitedController(t, limitedCatalog())
			if _, err := c.Reconcile(ctx); err != nil {
				t.Fatal(err)
			}
			c.config.VerifyTimeout = 2 * time.Second
			tc.edit(r)
			_, err := c.Switch(ctx, "vision", "test")
			if err == nil || !errors.Is(err, tc.want) || errors.Is(err, ErrVerifyTimeout) || errors.Is(err, ErrLimitedTarget) || r.healthCalls > 1 || r.launchChecks != 1 {
				t.Fatalf("switch = %v after %d health checks", err, r.healthCalls)
			}
			state, stateErr := s.State(ctx)
			if stateErr != nil || state.ActiveWorkload != control.WorkloadText || state.Health != control.HealthError ||
				state.Phase != control.PhaseReconciling || state.Admission != control.AdmissionClosed || r.visionActive || r.fakeRuntime.active != control.WorkloadText {
				t.Fatalf("state after a limit failure = %+v, %v; calls %v", state, stateErr, r.calls)
			}
			if !slices.Contains(r.calls, "stop vision") {
				t.Fatalf("vision not stopped by the rollback: %v", r.calls)
			}
		})
	}
}

// An untagged readback failure at health proves nothing, so the switch
// retries it and settles when a later check passes.
func TestUntaggedReadbackAtHealthRetries(t *testing.T) {
	ctx := context.Background()
	c, r, _ := limitedController(t, limitedCatalog())
	if _, err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	c.config.VerifyTimeout = 5 * time.Second
	r.healthErr, r.healthErrOnce = gpuruntime.ErrLimitReadback, true
	state, err := c.Switch(ctx, "vision", "test")
	if err != nil || state.ActiveWorkload != "vision" || state.Health != control.HealthHealthy || r.healthCalls < 2 {
		t.Fatalf("switch with one readback failure = %+v, %v after %d health checks", state, err, r.healthCalls)
	}
}

// A launch check that fails before any unload refuses the switch: the
// current workload keeps running and the state does not change.
func TestSwitchRefusesLimitedTargetBeforeAnyEffect(t *testing.T) {
	ctx := context.Background()
	c, r, s := limitedController(t, limitedCatalog())
	if _, err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	before, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	r.calls = nil
	r.launchErr = gpuruntime.ErrHostCap
	// Text keeps running, so the steps switch to Idle before setup removes
	// the limits.
	if _, err := c.Switch(ctx, "vision", "test"); !errors.Is(err, ErrLimitedTarget) || !errors.Is(err, gpuruntime.ErrHostCap) ||
		!strings.HasSuffix(err.Error(), "; next steps: "+toIdle+"run `gpu-mode -target idle switch`") {
		t.Fatalf("switch without host cap = %v", err)
	}
	// A cause that names its fix keeps it. The healthy state needs no
	// recovery.
	r.launchErr = limitCause
	if _, err := c.Switch(ctx, "vision", "test"); !errors.Is(err, ErrLimitedTarget) || !strings.HasSuffix(err.Error(), causeRemedy+"; next steps: "+toIdle+"run `gpu-mode -target idle switch`") ||
		strings.Contains(err.Error(), "recover") {
		t.Fatalf("switch with a limit cause = %v", err)
	}
	r.launchErr = gpuruntime.ErrHostCap
	if _, err := c.ActivateWorkload(ctx, "vision", control.OperatorPrecondition{Owner: control.OwnerSupervisor}); !errors.Is(err, ErrLimitedTarget) {
		t.Fatalf("activation without host cap = %v", err)
	}
	after, err := s.State(ctx)
	if err != nil || !reflect.DeepEqual(after, before) || r.fakeRuntime.active != control.WorkloadText || len(r.calls) != 0 {
		t.Fatalf("refused switch changed something: %+v, %v; calls %v", after, err, r.calls)
	}
	// A limited profile does not block other targets.
	if state, err := c.Switch(ctx, control.WorkloadMedia, "test"); err != nil || state.ActiveWorkload != control.WorkloadMedia {
		t.Fatalf("switch to an unlimited target = %+v, %v", state, err)
	}
	// A runtime that cannot run the checks refuses the unit.
	c.runtime = &fakeRuntime{}
	if _, err := c.Switch(ctx, "vision", "test"); !errors.Is(err, ErrLimitedTarget) || !errors.Is(err, gpuruntime.ErrLimitUnchecked) ||
		!strings.Contains(err.Error(), "vision (gws-owned-vision.service)") {
		t.Fatalf("switch with a runtime without limit checks = %v", err)
	}
}

func TestReconcileRetainsHealthyLimitedUnit(t *testing.T) {
	ctx := context.Background()
	catalog := limitedCatalog()
	catalog.Profiles[2].BootPolicy = "retain"
	c, r, _ := limitedController(t, catalog)
	r.fakeRuntime.active = control.WorkloadIdle
	r.visionActive = true
	state, err := c.Reconcile(ctx)
	if err != nil || state.ActiveWorkload != "vision" || !r.visionActive || r.healthCalls == 0 || r.protections != 1 {
		t.Fatalf("reconcile with a retained limited unit = %+v, %v", state, err)
	}
}

// Status never stops a workload. A limit failure latches the error state
// and closes admission; the unit keeps running.
func TestStatusOnlyLatchesLimitFailure(t *testing.T) {
	c, r, s := limitedController(t, limitedCatalog())
	ctx := context.Background()
	if _, err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Switch(ctx, "vision", "test"); err != nil {
		t.Fatal(err)
	}
	r.calls = nil
	r.healthErr = fmt.Errorf("%w: %w", gpuruntime.ErrLimitCheckFailed, gpuruntime.ErrLimitDrift)
	if _, err := c.Status(ctx); !errors.Is(err, gpuruntime.ErrLimitDrift) || !errors.Is(err, ErrHealthCheck) {
		t.Fatalf("status with drift = %v", err)
	}
	state, err := s.State(ctx)
	if err != nil || state.Health != control.HealthError || state.Admission != control.AdmissionClosed || !r.visionActive || len(r.calls) != 0 {
		t.Fatalf("status with drift = %+v, %v; calls %v", state, err, r.calls)
	}
}

// Reconciliation keeps a running unit with limits only after its checks
// pass. A definite failure stops it, settles on idle with the failure
// latched in the same write, and reports why. A transient failure latches
// without a stop.
func TestReconcileStopsRetainedLimitedUnitThatFailsChecks(t *testing.T) {
	for _, cause := range []error{gpuruntime.ErrHostCap, gpuruntime.ErrLimitDrift} {
		ctx := context.Background()
		catalog := limitedCatalog()
		catalog.Profiles[2].BootPolicy = "retain"
		c, r, s := limitedController(t, catalog)
		r.fakeRuntime.active = control.WorkloadIdle
		r.visionActive = true
		before, err := s.State(ctx)
		if err != nil {
			t.Fatal(err)
		}
		r.protectionErr = cause
		state, err := c.Reconcile(ctx)
		if !errors.Is(err, ErrLimitedUnitRejected) || !errors.Is(err, cause) || state.ActiveWorkload != control.WorkloadIdle ||
			state.Health != control.HealthError || state.Admission != control.AdmissionClosed || r.visionActive || r.protections != 1 ||
			r.healthCalls != 0 || !slices.Contains(r.calls, "stop vision") {
			t.Fatalf("reconcile with %v = %+v, %v; calls %v", cause, state, err, r.calls)
		}
		// The stored state holds the latch. The entry into reconciliation
		// and one settle write are the only writes, so no healthy settle
		// was ever stored before the latch.
		stored, err := s.State(ctx)
		if err != nil || stored.Health != control.HealthError || stored.Admission != control.AdmissionClosed ||
			stored.Phase != control.PhaseReconciling || stored.ActiveWorkload != control.WorkloadIdle || stored.Version != before.Version+2 {
			t.Fatalf("stored state after %v = %+v (before %+v), %v", cause, stored, before, err)
		}
		// The failure stays latched: a second reconcile still fails.
		if _, err := c.Reconcile(ctx); !errors.Is(err, ErrRecoveryRequired) {
			t.Fatalf("second reconcile after %v = %v", cause, err)
		}
	}
	ctx := context.Background()
	catalog := limitedCatalog()
	catalog.Profiles[2].BootPolicy = "retain"
	c, r, s := limitedController(t, catalog)
	r.fakeRuntime.active = control.WorkloadIdle
	r.visionActive = true
	transient := errors.New("bus timeout")
	r.protectionErr = transient
	if _, err := c.Reconcile(ctx); !errors.Is(err, transient) || errors.Is(err, ErrLimitedUnitRejected) || r.protections != 1 {
		t.Fatalf("reconcile with a transient failure = %v after %d checks", err, r.protections)
	}
	if state, err := s.State(ctx); err != nil || state.Health != control.HealthError || !r.visionActive || slices.Contains(r.calls, "stop vision") {
		t.Fatalf("reconcile with a transient failure = %+v, %v", state, err)
	}
	// A runtime that cannot run the checks fails them.
	c.runtime = &fakeRuntime{}
	if err := c.checkRetainedLimits(ctx, catalog.Profiles[2]); !errors.Is(err, gpuruntime.ErrLimitUnchecked) || c.checkRetainedLimits(ctx, catalog.Profiles[0]) != nil {
		t.Fatalf("checks without a checker = %v", err)
	}
}

// Recover stops a running retained unit with limits that fails a definite
// check and settles without a new latch. Recover reports the rejection, and
// the next reconcile and setup see a recovered state.
func TestRecoverStopsRejectedLimitedUnitWithoutLatch(t *testing.T) {
	ctx := context.Background()
	catalog := limitedCatalog()
	catalog.Profiles[2].BootPolicy = "retain"
	c, r, s := limitedController(t, catalog)
	r.fakeRuntime.active = control.WorkloadIdle
	r.visionActive = true
	// A definite check failure refuses the running unit with limits.
	rejected := fmt.Errorf("%w: %w", gpuruntime.ErrLimitCheckFailed, limitCause)
	r.protectionErr = rejected
	// Reconcile latches the error state, so recover comes before setup.
	if _, err := c.Reconcile(ctx); !errors.Is(err, ErrLimitedUnitRejected) || !strings.HasSuffix(err.Error(), causeRemedy+"; next steps: first, run `gpu-mode recover`") {
		t.Fatalf("reconcile = %v", err)
	}
	// The unit runs again, for example after a manual start, while the
	// error state is latched.
	r.visionActive = true
	r.calls = nil
	state, err := c.Recover(ctx)
	if !errors.Is(err, ErrLimitedUnitRejected) || !errors.Is(err, gpuruntime.ErrLimitReserve) || r.visionActive ||
		!slices.Contains(r.calls, "stop vision") || !strings.HasSuffix(err.Error(), causeRemedy) || strings.Contains(err.Error(), "gpu-mode recover") {
		t.Fatalf("recover with a rejected unit = %+v, %v; calls %v", state, err, r.calls)
	}
	// This is the state that setup accepts for the removal of the limits.
	stored, err := s.State(ctx)
	if err != nil || stored != state || stored.Health != control.HealthHealthy || stored.Phase != control.PhaseStable ||
		stored.Admission != control.AdmissionClosed || stored.ActiveWorkload != control.WorkloadIdle || stored.DesiredWorkload != control.WorkloadIdle ||
		stored.Owner != control.OwnerSupervisor {
		t.Fatalf("stored state after recover = %+v, %v", stored, err)
	}
	// One recover is enough: reconcile now succeeds.
	if state, err := c.Reconcile(ctx); err != nil || state.Health != control.HealthHealthy {
		t.Fatalf("reconcile after recover = %+v, %v", state, err)
	}
}

// A definite rejection stays in the error when a later retained unit with
// limits fails a transient check. The transient failure latches without a
// stop.
func TestTransientRetainedFailureKeepsEarlierRejection(t *testing.T) {
	reserve := fmt.Errorf("%w: %w", gpuruntime.ErrLimitCheckFailed, limitCause)
	for cause, steps := range map[error]string{
		// The workload that stays is unknown, so the steps also switch to
		// Idle.
		gpuruntime.ErrLimitDrift: "; next steps: first, run `gpu-mode recover`; " + afterRecovery + "\n",
		reserve:                  causeRemedy + "; next steps: first, run `gpu-mode recover`; " + afterRecovery + "\n",
	} {
		transientAfterRejection(t, cause, steps)
	}
}

func transientAfterRejection(t *testing.T, cause error, steps string) {
	t.Helper()
	ctx := context.Background()
	catalog := limitedCatalog()
	catalog.Profiles[2].BootPolicy = "retain"
	second := catalog.Profiles[2]
	model := *second.NativeModel
	second.NativeModel = &model
	second.NativeModel.Owned = &control.OwnedLaunch{ModelPath: "/models/vision-q4", Port: 9103, Alias: "vision-q4",
		Limits: control.ResourceLimits{Version: 1, MemoryMaxMiB: 4096, TasksMax: 2048}}
	second.ID, second.Label, second.Unit = "vision2", "Vision 2", "gws-owned-vision2.service"
	second.Cgroup = "/user.slice/user-1000.slice/user@1000.service/app.slice/gws-owned-vision2.service"
	second.HealthURL, second.NativeModel.Endpoint = "http://127.0.0.1:9103/health", "http://127.0.0.1:9103"
	second.NativeModel.Model, second.NativeModel.Instance = "vision-q4", "local2"
	second.NativeModel.LaunchFile = "/home/u/.config/systemd/user/gws-owned-vision2.service"
	catalog.Profiles = append(catalog.Profiles, second)
	c, r, s := limitedController(t, catalog)
	r.fakeRuntime.active = control.WorkloadIdle
	r.visionActive = true
	r.alsoActive = []control.Workload{"vision2"}
	transient := errors.New("bus timeout")
	r.protectionErrs = map[control.Workload]error{"vision": cause, "vision2": transient}
	_, err := c.Reconcile(ctx)
	if !errors.Is(err, ErrLimitedUnitRejected) || !errors.Is(err, cause) || !errors.Is(err, transient) {
		t.Fatalf("reconcile with a rejected and a transient unit = %v", err)
	}
	// The transient failure names its unit, and the rejected unit keeps its
	// next step.
	if !strings.Contains(err.Error(), "vision2 (gws-owned-vision2.service): bus timeout") || !strings.Contains(err.Error(), steps) {
		t.Fatalf("reconcile error does not name the unit or the next step: %v", err)
	}
	if state, err := s.State(ctx); err != nil || state.Health != control.HealthError || !r.visionActive || slices.Contains(r.calls, "stop vision") {
		t.Fatalf("state = %+v, %v; calls %v", state, err, r.calls)
	}
}

// A command, read or context failure before unload decides nothing: it is
// not a definite refusal, and nothing changes.
func TestTransientLaunchCheckFailureIsNotARefusal(t *testing.T) {
	ctx := context.Background()
	c, r, s := limitedController(t, limitedCatalog())
	if _, err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	before, _ := s.State(ctx)
	r.calls = nil
	for _, cause := range []error{context.DeadlineExceeded, gpuruntime.ErrLimitReadback, errors.New("bus")} {
		r.launchErr = cause
		_, err := c.Switch(ctx, "vision", "test")
		if !errors.Is(err, cause) || errors.Is(err, ErrLimitedTarget) || !strings.HasPrefix(err.Error(), "vision (gws-owned-vision.service): ") {
			t.Fatalf("switch with %v = %v", cause, err)
		}
	}
	for _, binding := range []error{gpuruntime.ErrLaunchChanged, gpuruntime.ErrLaunchUnsupported} {
		r.launchErr = binding
		if _, err := c.Switch(ctx, "vision", "test"); !errors.Is(err, ErrLimitedTarget) || !errors.Is(err, binding) {
			t.Fatalf("switch with %v = %v", binding, err)
		}
	}
	if after, err := s.State(ctx); err != nil || !reflect.DeepEqual(after, before) || len(r.calls) != 0 {
		t.Fatalf("failed checks changed something: %+v, %v; calls %v", after, err, r.calls)
	}
}

// The stale catalog check runs before the limit checks call the runtime.
func TestStaleCatalogWinsOverLimitedRefusal(t *testing.T) {
	ctx := context.Background()
	c, r, s := limitedController(t, limitedCatalog())
	snap, err := s.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	plain := limitedCatalog()
	plain.Profiles[2].NativeModel.Owned.Limits = control.ResourceLimits{}
	plain.Version = control.LowestCatalogVersion(plain)
	if _, err := s.ReplaceCatalog(ctx, snap.Revision, plain); err != nil {
		t.Fatal(err)
	}
	r.launchErr = gpuruntime.ErrHostCap
	if _, err := c.Switch(ctx, "vision", "test"); !errors.Is(err, store.ErrVersionConflict) || r.launchChecks != 0 {
		t.Fatalf("switch with a stale catalog = %v", err)
	}
	if _, err := c.ActivateWorkload(ctx, "vision", control.OperatorPrecondition{Owner: control.OwnerSupervisor}); !errors.Is(err, store.ErrVersionConflict) || r.launchChecks != 0 {
		t.Fatalf("activation with a stale catalog = %v", err)
	}
}

// A failed rollback start of a limited previous workload latches the error
// state. The runtime test of Start shows that Start stops the unit.
func TestFailedRollbackStartOfLimitedUnit(t *testing.T) {
	ctx := context.Background()
	c, r, s := limitedController(t, limitedCatalog())
	if _, err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Switch(ctx, "vision", "test"); err != nil {
		t.Fatal(err)
	}
	r.startErr = errors.New("media failed")
	r.visionStartErr = gpuruntime.ErrLimitCheckFailed
	if _, err := c.Switch(ctx, control.WorkloadMedia, "test"); !errors.Is(err, gpuruntime.ErrLimitCheckFailed) {
		t.Fatalf("switch with a failed rollback = %v", err)
	}
	// The runtime Start stops the unit it started when its check fails;
	// TestStartStopsUnitWhenReadbackFails in the runtime covers that stop.
	state, err := s.State(ctx)
	if err != nil || state.Health != control.HealthError || state.Admission != control.AdmissionClosed || state.ActiveWorkload != control.WorkloadUnknown {
		t.Fatalf("state after a failed rollback = %+v, %v", state, err)
	}
}

// uncheckedRuntime runs vision but cannot run the limit checks.
type uncheckedRuntime struct {
	fakeRuntime
	visionActive bool
}

func (r *uncheckedRuntime) Observe(ctx context.Context) (gpuruntime.Snapshot, error) {
	snapshot, err := r.fakeRuntime.Observe(ctx)
	if err == nil {
		snapshot.Workloads["vision"] = gpuruntime.WorkloadObservation{Active: r.visionActive, Exclusive: true}
	}
	return snapshot, err
}

func (r *uncheckedRuntime) Stop(ctx context.Context, w control.Workload) error {
	if w == "vision" {
		r.calls = append(r.calls, "stop vision")
		r.visionActive = false
		return nil
	}
	return r.fakeRuntime.Stop(ctx, w)
}

// Reconciliation does not keep a running unit with limits when the runtime
// cannot check them: it stops the unit.
func TestReconcileStopsLimitedUnitWithoutChecker(t *testing.T) {
	ctx := context.Background()
	catalog := limitedCatalog()
	catalog.Profiles[2].BootPolicy = "retain"
	s := openStoreAt(t, filepath.Join(t.TempDir(), "state.db"))
	if _, err := s.ReplaceCatalog(ctx, "", catalog); err != nil {
		t.Fatal(err)
	}
	r := &uncheckedRuntime{visionActive: true}
	c := testController(t, s, r)
	state, err := c.Reconcile(ctx)
	if !errors.Is(err, ErrLimitedUnitRejected) || !errors.Is(err, gpuruntime.ErrLimitUnchecked) || state.ActiveWorkload != control.WorkloadIdle || r.visionActive {
		t.Fatalf("reconcile without a checker = %+v, %v; calls %v", state, err, r.calls)
	}
	// The rejection latch applies on this path too.
	if state.Health != control.HealthError || state.Admission != control.AdmissionClosed || state.Phase != control.PhaseReconciling {
		t.Fatalf("latch without a checker = %+v", state)
	}
}

// When reconciliation keeps another retained unit and rejects a unit with
// limits, it stops the rejected unit, also from a stable state, and records
// the kept unit as active with the failure latched.
func TestReconcileKeepsRetainedUnitAndStopsRejectedOne(t *testing.T) {
	ctx := context.Background()
	catalog := limitedCatalog()
	catalog.Profiles[0].BootPolicy = "retain"
	catalog.Profiles[2].BootPolicy = "retain"
	c, r, _ := limitedController(t, catalog)
	// The first reconcile keeps text and settles stable and healthy.
	if state, err := c.Reconcile(ctx); err != nil || state.ActiveWorkload != control.WorkloadText {
		t.Fatalf("first reconcile = %+v, %v", state, err)
	}
	r.visionActive = true
	r.protectionErr = gpuruntime.ErrLimitDrift
	r.calls = nil
	state, err := c.Reconcile(ctx)
	if !errors.Is(err, ErrLimitedUnitRejected) || r.visionActive || !slices.Contains(r.calls, "stop vision") ||
		r.fakeRuntime.active != control.WorkloadText || state.ActiveWorkload != control.WorkloadText ||
		state.Health != control.HealthError || state.Admission != control.AdmissionClosed {
		t.Fatalf("reconcile with a rejected unit = %+v, %v; calls %v", state, err, r.calls)
	}
	// Recover keeps text, so the state is stable and healthy with open
	// admission. The steps switch to Idle before setup removes the limits.
	r.visionActive = true
	r.protectionErr = fmt.Errorf("%w: %w", gpuruntime.ErrLimitCheckFailed, limitCause)
	state, err = c.Recover(ctx)
	if !errors.Is(err, ErrLimitedUnitRejected) || r.visionActive || state.ActiveWorkload != control.WorkloadText ||
		state.Phase != control.PhaseStable || state.Health != control.HealthHealthy || state.Admission != control.AdmissionOpen ||
		!strings.HasSuffix(err.Error(), causeRemedy+"; next steps: "+toIdle+"run `gpu-mode -target idle switch`") {
		t.Fatalf("recover that keeps text = %+v, %v", state, err)
	}
}

// Recover after a rejection needs no next step.
func TestLimitRemedyOfRecoverWithoutLatch(t *testing.T) {
	drift := fmt.Errorf("%w: %w", ErrLimitedUnitRejected, gpuruntime.ErrLimitDrift)
	if err := limitRemedy(drift, remedyState{kept: control.WorkloadIdle}); err != drift || limitRemedy(nil, remedyState{latched: true}) != nil {
		t.Fatalf("remedy = %v", err)
	}
}

// A retained unit with limits that fails a definite check stays named in
// the error when reconciliation then finds two kept units.
func TestInvariantFailureKeepsRejectedUnits(t *testing.T) {
	ctx := context.Background()
	catalog := limitedCatalog()
	vision := catalog.Profiles[2]
	vision.BootPolicy = "retain"
	catalog.Profiles = []control.WorkloadProfile{vision, catalog.Profiles[0], catalog.Profiles[1]}
	catalog.Profiles[1].BootPolicy, catalog.Profiles[2].BootPolicy = "retain", "retain"
	c, r, _ := limitedController(t, catalog)
	r.fakeRuntime.active = control.WorkloadText
	r.visionActive = true
	r.alsoActive = []control.Workload{control.WorkloadMedia}
	r.protectionErr = gpuruntime.ErrLimitDrift
	_, err := c.Reconcile(ctx)
	if !errors.Is(err, ErrInvariant) || !errors.Is(err, ErrLimitedUnitRejected) || !strings.Contains(err.Error(), "vision (gws-owned-vision.service)") {
		t.Fatalf("reconcile with two kept units and a rejected one = %v", err)
	}
}

// After a latched rejection, a limit cause refusal says to recover first:
// setup refuses a latched state.
func TestLimitCauseRefusalOfLatchedStateNamesRecover(t *testing.T) {
	ctx := context.Background()
	catalog := limitedCatalog()
	catalog.Profiles[2].BootPolicy = "retain"
	c, r, s := limitedController(t, catalog)
	r.fakeRuntime.active = control.WorkloadIdle
	r.visionActive = true
	r.protectionErr = fmt.Errorf("%w: %w", gpuruntime.ErrLimitCheckFailed, limitCause)
	if _, err := c.Reconcile(ctx); !errors.Is(err, ErrLimitedUnitRejected) {
		t.Fatalf("reconcile = %v", err)
	}
	if state, err := s.State(ctx); err != nil || state.Health != control.HealthError || state.ActiveWorkload != control.WorkloadIdle {
		t.Fatalf("state after the rejection = %+v, %v", state, err)
	}
	r.launchErr = limitCause
	_, err := c.Switch(ctx, "vision", "test")
	if !errors.Is(err, ErrLimitedTarget) || !strings.HasSuffix(err.Error(), causeRemedy+"; next steps: first, run `gpu-mode recover`") {
		t.Fatalf("switch with a limit cause from a latched state = %v", err)
	}
}

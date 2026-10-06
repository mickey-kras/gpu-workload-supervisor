package supervisor

import (
	"context"
	"errors"
	"fmt"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
	"testing"
	"time"
)

type acceptanceRuntime struct {
	fakeRuntime
	failStart control.Workload
}

func (r *acceptanceRuntime) Observe(context.Context) (gpuruntime.Snapshot, error) {
	s := gpuruntime.Snapshot{Workloads: map[control.Workload]gpuruntime.WorkloadObservation{}}
	for _, id := range []control.Workload{"text", "media", "speech"} {
		s.Workloads[id] = gpuruntime.WorkloadObservation{Active: r.active == id, Exclusive: true}
	}
	return s, nil
}
func (r *acceptanceRuntime) Start(_ context.Context, id control.Workload) error {
	r.calls = append(r.calls, "start "+string(id))
	if id == r.failStart {
		return errors.New("injected start failure")
	}
	r.active = id
	return nil
}
func acceptanceController(t *testing.T) (*Controller, *store.Store, *acceptanceRuntime, *control.CatalogSnapshot) {
	t.Helper()
	ctx := context.Background()
	s := openStore(t)
	cat := control.Catalog{Version: 1}
	for _, id := range []control.Workload{"text", "media", "speech"} {
		cat.Profiles = append(cat.Profiles, control.WorkloadProfile{ID: id, Label: string(id), Adapter: "systemd", Unit: string(id) + ".service", Cgroup: "/workloads/" + string(id), HealthURL: "http://localhost:9000"})
	}
	snap, err := s.ReplaceCatalog(ctx, "", cat)
	if err != nil {
		t.Fatal(err)
	}
	r := &acceptanceRuntime{}
	n := 0
	c, err := newController(s, r, Config{Catalog: &snap, DrainTimeout: time.Second, VerifyTimeout: time.Second, ActionTimeout: time.Second, CleanupTimeout: time.Second, FinalizeTimeout: time.Second, PollInterval: time.Millisecond}, time.Now, func() (string, error) { n++; return fmt.Sprintf("transition-%d", n), nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	return c, s, r, &snap
}
func TestCatalogRecoveryAfterFailedSwitchDrainsToIdle(t *testing.T) {
	ctx := context.Background()
	c, s, r, _ := acceptanceController(t)
	if _, err := c.Switch(ctx, "speech", "acceptance"); err != nil {
		t.Fatal(err)
	}
	r.failStart = "media"
	if _, err := c.Switch(ctx, "media", "acceptance"); err == nil {
		t.Fatal("failed target reported success")
	}
	r.failStart = ""
	state, err := c.Recover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if state.ActiveWorkload != control.WorkloadIdle || r.active != control.WorkloadIdle {
		t.Fatal("recovery restarted workload")
	}
	if id, err := s.InProgressTransition(ctx); err != nil || id != "" {
		t.Fatalf("unfinished transition %s %v", id, err)
	}
}
func TestControllerPinnedCatalogAndStaleRevisionAcceptance(t *testing.T) {
	ctx := context.Background()
	c, s, r, snap := acceptanceController(t)
	snap.Catalog.Profiles[2].ID = "mutated"
	if _, err := c.Switch(ctx, "speech", "acceptance"); err != nil {
		t.Fatal("controller aliases input catalog", err)
	}
	before, _ := s.State(ctx)
	calls := len(r.calls)
	if _, err := c.Switch(ctx, "unconfigured", "acceptance"); err == nil {
		t.Fatal("unconfigured admitted")
	}
	after, _ := s.State(ctx)
	if before != after || calls != len(r.calls) {
		t.Fatal("invalid target changed state or runtime")
	}
	next := c.config.Catalog.Catalog.Clone()
	next.Profiles[0].Label = "Updated"
	if _, err := s.ReplaceCatalog(ctx, c.config.Catalog.Revision, next); err != nil {
		t.Fatal(err)
	}
	calls = len(r.calls)
	if _, err := c.Switch(ctx, control.WorkloadIdle, "acceptance"); !errors.Is(err, store.ErrVersionConflict) {
		t.Fatalf("stale catalog: %v", err)
	}
	if calls != len(r.calls) {
		t.Fatal("stale catalog caused effects")
	}
}
func TestRetainedCatalogDoesNotBypassOpposingUnfinishedWorkAcceptance(t *testing.T) {
	ctx := context.Background()
	c, s, r, _ := acceptanceController(t)
	cat := c.config.Catalog.Catalog.Clone()
	cat.Profiles[2].BootPolicy = "retain"
	snap, err := s.ReplaceCatalog(ctx, c.config.Catalog.Revision, cat)
	if err != nil {
		t.Fatal(err)
	}
	c.config.Catalog = &snap
	// Registrations from an older fence remain authoritative until completed.
	state, _ := s.State(ctx)
	state = stableTarget(state, control.OwnerSupervisor, "media")
	state, err = s.UpdateState(ctx, state.Version, state)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AdmitWorkToken(ctx, "opposing-media", "", "media", state.LeaseFence); err != nil {
		t.Fatal(err)
	}
	state = stableTarget(state, control.OwnerSupervisor, "speech")
	state, err = s.UpdateState(ctx, state.Version, state)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AdmitWorkToken(ctx, "retained-speech", "", "speech", state.LeaseFence); err != nil {
		t.Fatal(err)
	}
	r.active = "speech"
	r.calls = nil
	c.config.DrainTimeout = 10 * time.Millisecond
	if _, err = c.Reconcile(ctx); err == nil {
		t.Fatal("retained registration masked unfinished opposing work")
	}
	for _, call := range r.calls {
		if call == "stop media" {
			t.Fatal("stopped media before its work completed")
		}
	}
	after, _ := s.State(ctx)
	if after.Admission != control.AdmissionClosed {
		t.Fatal("drain failure left admission open")
	}
}

type acceptanceObservationRuntime struct {
	*acceptanceRuntime
	observationErr error
	bothActive     bool
}

func (r *acceptanceObservationRuntime) Observe(ctx context.Context) (gpuruntime.Snapshot, error) {
	if r.observationErr != nil {
		return gpuruntime.Snapshot{}, r.observationErr
	}
	snap, err := r.acceptanceRuntime.Observe(ctx)
	if r.bothActive {
		snap.Workloads["text"] = gpuruntime.WorkloadObservation{Active: true, Exclusive: true}
		snap.Workloads["speech"] = gpuruntime.WorkloadObservation{Active: true, Exclusive: true}
	}
	return snap, err
}
func TestCatalogReconcileFailureAcceptance(t *testing.T) {
	for _, kind := range []string{"owner", "latched", "observation", "multiple retained", "stop", "release", "health"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			c, s, r, _ := acceptanceController(t)
			c.config.VerifyTimeout = 5 * time.Millisecond
			c.config.CleanupTimeout = 5 * time.Millisecond
			switch kind {
			case "owner":
				st, _ := s.State(ctx)
				st.Owner = control.OwnerUser
				if _, err := s.UpdateState(ctx, st.Version, st); err != nil {
					t.Fatal(err)
				}
			case "latched":
				st, _ := s.State(ctx)
				st.Health = control.HealthError
				st.Admission = control.AdmissionClosed
				if _, err := s.UpdateState(ctx, st.Version, st); err != nil {
					t.Fatal(err)
				}
			case "observation":
				c.runtime = &acceptanceObservationRuntime{acceptanceRuntime: r, observationErr: errors.New("runtime unavailable")}
			case "multiple retained":
				cat := c.config.Catalog.Catalog.Clone()
				cat.Profiles[0].BootPolicy = "retain"
				cat.Profiles[2].BootPolicy = "retain"
				snap, err := s.ReplaceCatalog(ctx, c.config.Catalog.Revision, cat)
				if err != nil {
					t.Fatal(err)
				}
				c.config.Catalog = &snap
				c.runtime = &acceptanceObservationRuntime{acceptanceRuntime: r, bothActive: true}
			case "stop":
				r.stopErr = errors.New("cannot stop")
			case "release":
				r.blockRelease = true
			case "health":
				r.healthFailures = 10000
			}
			if _, err := c.Reconcile(ctx); err == nil {
				t.Fatal("failure ignored")
			}
			st, _ := s.State(ctx)
			if kind != "owner" && st.Admission != control.AdmissionClosed {
				t.Fatalf("failure left admission open: %+v", st)
			}
		})
	}
}
func TestCatalogFailedRollbackStaysClosedAcceptance(t *testing.T) {
	for _, kind := range []string{"stop", "restart"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			c, s, r, _ := acceptanceController(t)
			if _, err := c.Switch(ctx, "speech", "acceptance"); err != nil {
				t.Fatal(err)
			}
			if kind == "stop" {
				r.stopErr = errors.New("cannot stop")
			} else {
				r.failStart = "speech"
				r.healthFailures = 10000
				c.config.VerifyTimeout = 3 * time.Millisecond
				c.config.CleanupTimeout = 3 * time.Millisecond
			}
			if _, err := c.Switch(ctx, "media", "acceptance"); err == nil {
				t.Fatal("failed rollback reported success")
			}
			state, _ := s.State(ctx)
			if state.Admission != control.AdmissionClosed || state.Health != control.HealthError {
				t.Fatalf("failed rollback admitted work %+v", state)
			}
		})
	}
}

type catalogReadFailureStore struct {
	*store.Store
	fault string
}

func (s *catalogReadFailureStore) State(ctx context.Context) (control.State, error) {
	if s.fault == "state" {
		return control.State{}, errors.New("state unavailable")
	}
	return s.Store.State(ctx)
}
func (s *catalogReadFailureStore) Catalog(ctx context.Context) (control.CatalogSnapshot, error) {
	if s.fault == "catalog" {
		return control.CatalogSnapshot{}, errors.New("catalog unavailable")
	}
	return s.Store.Catalog(ctx)
}
func (s *catalogReadFailureStore) InProgressTransition(ctx context.Context) (string, error) {
	if s.fault == "journal" {
		return "", errors.New("journal unavailable")
	}
	return s.Store.InProgressTransition(ctx)
}
func (s *catalogReadFailureStore) PendingWork(ctx context.Context) (int, error) {
	if s.fault == "work" {
		return 0, errors.New("work unavailable")
	}
	return s.Store.PendingWork(ctx)
}
func (s *catalogReadFailureStore) Recover(ctx context.Context, v uint64, st control.State, reason string) (control.State, error) {
	if s.fault == "write" || s.fault == "fence" {
		return st, errors.New("state unwritable")
	}
	return s.Store.Recover(ctx, v, st, reason)
}
func TestCatalogUnavailableDurableStateAcceptance(t *testing.T) {
	for _, fault := range []string{"state", "catalog", "journal", "work", "write", "fence"} {
		t.Run(fault, func(t *testing.T) {
			ctx := context.Background()
			c, s, r, _ := acceptanceController(t)
			c.store = &catalogReadFailureStore{Store: s, fault: fault}
			r.calls = nil
			var err error
			if fault == "fence" {
				_, err = c.Recover(ctx)
			} else {
				_, err = c.Reconcile(ctx)
			}
			if err == nil {
				t.Fatal("durable failure ignored")
			}
			if len(r.calls) != 0 {
				t.Fatalf("durable failure caused runtime effects: %v", r.calls)
			}
		})
	}
}
func TestThirdWorkloadOperatorResolutionAcceptance(t *testing.T) {
	ctx := context.Background()
	c, s, _, _ := acceptanceController(t)
	state, err := c.Switch(ctx, "speech", "acceptance")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AdmitWorkToken(ctx, "abandoned-speech", "", "speech", state.LeaseFence); err != nil {
		t.Fatal(err)
	} // Generic resolution must stop every configured workload.
	resolved, count, err := c.ResolveUnfinishedWork(ctx, "operator confirmed job abandoned")
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 || resolved.Admission != control.AdmissionClosed {
		t.Fatalf("resolution %+v count %d", resolved, count)
	}
	if pending, err := s.PendingWork(ctx); err != nil || pending != 0 {
		t.Fatalf("pending %d %v", pending, err)
	}
}
func TestCatalogConstructorRejectsInvalidAcceptance(t *testing.T) {
	c, s, r, snap := acceptanceController(t)
	cfg := c.config
	snap.Catalog.Version = 99
	cfg.Catalog = snap
	if _, err := New(s, r, cfg); err == nil {
		t.Fatal("invalid catalog controller created")
	}
	cfg = c.config
	fresh, err := New(s, r, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = fresh.Switch(context.Background(), "speech", "acceptance"); err != nil {
		t.Fatal(err)
	}
}

func (r *acceptanceRuntime) StopForRecovery(ctx context.Context) error {
	for _, id := range []control.Workload{"text", "media", "speech"} {
		if err := r.Stop(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

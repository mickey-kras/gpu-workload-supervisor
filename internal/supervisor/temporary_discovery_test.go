package supervisor

import (
	"context"
	"errors"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
	"testing"
)

type discoveryRuntimeFixture struct {
	fakeRuntime
	stopped       bool
	running       bool
	cleanupErr    error
	preflightErr  error
	starts, stops int
	cancel        context.CancelFunc
}

func (r *discoveryRuntimeFixture) Preflight(context.Context) error { return r.preflightErr }
func (r *discoveryRuntimeFixture) PrepareTemporaryDiscovery(context.Context, control.TemporaryDiscoveryCandidate) error {
	if !r.stopped {
		return errors.New("already running")
	}
	return nil
}
func (r *discoveryRuntimeFixture) StartTemporaryDiscovery(context.Context, control.TemporaryDiscoveryCandidate) (control.TemporaryDiscoveryLaunchEvidence, error) {
	r.starts++
	r.running = true
	if r.cancel != nil {
		r.cancel()
	}
	return control.TemporaryDiscoveryLaunchEvidence{InvocationID: "12345678901234567890123456789012", JobID: "1", ActivationTimestamp: "10"}, nil
}
func (r *discoveryRuntimeFixture) StopTemporaryDiscovery(ctx context.Context, _ control.TemporaryDiscoveryCandidate, id string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	r.stops++
	if id == "" && r.running {
		return errors.New("unknown invocation")
	}
	if r.cleanupErr != nil {
		return r.cleanupErr
	}
	r.running = false
	return nil
}
func TestTemporaryDiscoveryCleanupBeforeResultsAndCancellation(t *testing.T) {
	for _, mode := range []string{"complete", "cancel", "cleanup-failure", "already-running", "no-consent"} {
		t.Run(mode, func(t *testing.T) {
			s := openStore(t)
			state := ownershipState(t, s, control.OwnerSupervisor, control.WorkloadIdle)
			r := &discoveryRuntimeFixture{fakeRuntime: fakeRuntime{active: control.WorkloadIdle}, stopped: mode != "already-running"}
			c := testController(t, s, r)
			snap, _ := s.Catalog(context.Background())
			e := control.OperatorPrecondition{Incarnation: state.LeaseFence.Incarnation, Version: state.Version, Owner: state.Owner, ConfigurationRevision: snap.Revision}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "cancel" {
				r.cancel = cancel
			}
			if mode == "cleanup-failure" {
				r.cleanupErr = errors.New("GPU subtree remains populated")
			}
			raw, err := c.DiscoverNativeTemporary(ctx, control.TemporaryDiscoveryCandidate{Unit: "ollama.service"}, e, mode != "no-consent", func(ctx context.Context, _ string) ([]byte, error) {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				if !r.running {
					t.Fatal("inventory before start")
				}
				state, _ := s.State(ctx)
				if state.Admission != control.AdmissionClosed {
					t.Fatal("admission opened")
				}
				return []byte("inventory"), nil
			})
			if mode == "complete" {
				if err != nil || string(raw) != "inventory" || r.running || r.stops != 1 {
					t.Fatalf("%s %v %+v", raw, err, r)
				}
				return
			}
			if err == nil || raw != nil {
				t.Fatalf("failed session returned inventory %s %v", raw, err)
			}
			if mode == "already-running" || mode == "no-consent" {
				if r.starts != 0 || r.stops != 0 {
					t.Fatal("runtime effect before authorization")
				}
				return
			}
			if mode == "cancel" {
				if !errors.Is(err, context.Canceled) || r.running || r.stops != 1 {
					t.Fatalf("canceled cleanup %v %+v", err, r)
				}
				return
			}
			if !errors.Is(err, store.ErrTemporaryCleanupRequired) {
				t.Fatal(err)
			}
			record, _ := s.TemporaryDiscoveryStatus(context.Background())
			if record == nil || record.Status != "cleanup_required" || !r.running {
				t.Fatalf("%+v %+v", record, r)
			}
			if _, err := c.Recover(context.Background()); !errors.Is(err, store.ErrTemporaryCleanupRequired) {
				t.Fatalf("generic recovery %v", err)
			}
			r.cleanupErr = nil
			if err := c.CleanupTemporaryDiscovery(context.Background(), record.ID, record.Token); err != nil || r.running {
				t.Fatalf("explicit retry %v %+v", err, r)
			}
		})
	}
}

type noTemporaryStore struct{ storeGateway }
type discoveryPersistenceFailure struct{ *store.Store }

func (s discoveryPersistenceFailure) RecordTemporaryLaunch(context.Context, string, string, control.TemporaryDiscoveryLaunchEvidence) error {
	return errors.New("evidence writer unavailable")
}

func TestTemporaryDiscoveryRequiresSupportedStoreAndRuntime(t *testing.T) {
	s := openStore(t)
	ownershipState(t, s, control.OwnerSupervisor, control.WorkloadIdle)
	unsupportedStore := testController(t, noTemporaryStore{s}, &discoveryRuntimeFixture{stopped: true})
	unsupportedRuntime := testController(t, s, &fakeRuntime{active: control.WorkloadIdle})
	for _, c := range []*Controller{unsupportedStore, unsupportedRuntime} {
		if _, err := c.DiscoverNativeTemporary(context.Background(), control.TemporaryDiscoveryCandidate{}, control.OperatorPrecondition{}, true, func(context.Context, string) ([]byte, error) {
			t.Fatal("inspected unsupported runtime")
			return nil, nil
		}); err == nil {
			t.Fatal("unsupported host accepted")
		}
		if err := c.CleanupTemporaryDiscovery(context.Background(), "id", "token"); err == nil {
			t.Fatal("unsupported cleanup accepted")
		}
	}
	if _, err := unsupportedStore.TemporaryDiscoveryStatus(context.Background()); err == nil {
		t.Fatal("unsupported status accepted")
	}
}
func TestTemporaryDiscoveryFailuresStayClosedAndObservable(t *testing.T) {
	for _, mode := range []string{"preflight", "wrong-owner", "stale-catalog", "first-id", "token-id", "persistence"} {
		t.Run(mode, func(t *testing.T) {
			s := openStore(t)
			state := ownershipState(t, s, control.OwnerSupervisor, control.WorkloadIdle)
			r := &discoveryRuntimeFixture{fakeRuntime: fakeRuntime{active: control.WorkloadIdle}, stopped: true}
			var gateway storeGateway = s
			if mode == "persistence" {
				gateway = discoveryPersistenceFailure{s}
			}
			c := testController(t, gateway, r)
			e, err := c.TemporaryDiscoveryEligibility(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if record, err := c.TemporaryDiscoveryStatus(context.Background()); err != nil || record != nil {
				t.Fatalf("%+v %v", record, err)
			}
			switch mode {
			case "preflight":
				r.preflightErr = errors.New("preflight failed")
			case "wrong-owner":
				e.Owner = control.OwnerUser
			case "stale-catalog":
				e.ConfigurationRevision = "obsolete"
			case "first-id", "token-id":
				count := 0
				c.id = func() (string, error) {
					count++
					if count == 1 && mode == "token-id" {
						return "id", nil
					}
					return "", errors.New("secure identity unavailable")
				}
			}
			_, err = c.DiscoverNativeTemporary(context.Background(), control.TemporaryDiscoveryCandidate{Unit: "ollama.service"}, e, true, func(context.Context, string) ([]byte, error) { t.Fatal("inventory should be blocked"); return nil, nil })
			if err == nil {
				t.Fatal("unsafe prerequisite accepted")
			}
			if mode == "persistence" {
				if !errors.Is(err, store.ErrTemporaryCleanupRequired) || r.stops != 1 {
					t.Fatalf("persist failure cleanup %v %+v", err, r)
				}
				record, err := c.TemporaryDiscoveryStatus(context.Background())
				if err != nil || record == nil || record.Status != "cleanup_required" {
					t.Fatalf("%+v %v", record, err)
				}
				return
			}
			if r.starts != 0 || r.stops != 0 {
				t.Fatalf("effects before entry %+v", r)
			}
			after, _ := s.State(context.Background())
			if after.Version != state.Version {
				t.Fatal("prerequisite failure changed state")
			}
		})
	}
}
func TestTemporarySessionStatusAndCleanupRejectClosedStoreAndWrongToken(t *testing.T) {
	s := openStore(t)
	ownershipState(t, s, control.OwnerSupervisor, control.WorkloadIdle)
	r := &discoveryRuntimeFixture{fakeRuntime: fakeRuntime{active: control.WorkloadIdle}, stopped: true, cleanupErr: errors.New("cleanup failed")}
	c := testController(t, s, r)
	e, _ := c.TemporaryDiscoveryEligibility(context.Background())
	_, err := c.DiscoverNativeTemporary(context.Background(), control.TemporaryDiscoveryCandidate{}, e, true, func(context.Context, string) ([]byte, error) { return nil, nil })
	if err == nil {
		t.Fatal("cleanup failure hidden")
	}
	record, _ := c.TemporaryDiscoveryStatus(context.Background())
	if err := c.CleanupTemporaryDiscovery(context.Background(), record.ID, "wrong"); !errors.Is(err, store.ErrStaleFence) {
		t.Fatal(err)
	}
	if _, err := c.TemporaryDiscoveryEligibility(context.Background()); err == nil {
		t.Fatal("unfinished session eligible")
	}
	s.Close()
	if _, err := c.TemporaryDiscoveryEligibility(context.Background()); err == nil {
		t.Fatal("closed store eligible")
	}
	if _, err := c.TemporaryDiscoveryStatus(context.Background()); err == nil {
		t.Fatal("closed store status succeeded")
	}
}

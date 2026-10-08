package supervisor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
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

type flakyDiscoveryUpdate struct {
	*store.Store
	failures int
}

func (s *flakyDiscoveryUpdate) UpdateTemporaryDiscovery(ctx context.Context, id, token, invocation, status, message string) error {
	if s.failures > 0 {
		s.failures--
		return errors.New("session store busy")
	}
	return s.Store.UpdateTemporaryDiscovery(ctx, id, token, invocation, status, message)
}

func TestTemporaryDiscoveryRecoversWhenSessionUpdateFailsAfterLaunchRecord(t *testing.T) {
	s := openStore(t)
	ownershipState(t, s, control.OwnerSupervisor, control.WorkloadIdle)
	r := &discoveryRuntimeFixture{fakeRuntime: fakeRuntime{active: control.WorkloadIdle}, stopped: true}
	c := testController(t, &flakyDiscoveryUpdate{Store: s, failures: 1}, r)
	e, err := c.TemporaryDiscoveryEligibility(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.DiscoverNativeTemporary(context.Background(), control.TemporaryDiscoveryCandidate{Unit: "ollama.service"}, e, true, func(context.Context, string) ([]byte, error) {
		t.Fatal("inventory reached despite failed session update")
		return nil, nil
	})
	if err == nil || !strings.Contains(err.Error(), "session store busy") {
		t.Fatalf("update failure hidden: %v", err)
	}
	if r.stops != 1 || r.running {
		t.Fatalf("recorded launch survived cleanup: %+v", r)
	}
	record, err := c.TemporaryDiscoveryStatus(context.Background())
	if err != nil || record == nil || record.Status != "completed" {
		t.Fatalf("session stuck after cleanup: %+v %v", record, err)
	}
	if _, err := c.Recover(context.Background()); err != nil {
		t.Fatalf("admission remained closed: %v", err)
	}
}

func TestTemporaryDiscoveryCleanupFallsBackToLaunchEvidenceAfterCrash(t *testing.T) {
	s := openStore(t)
	state := ownershipState(t, s, control.OwnerSupervisor, control.WorkloadIdle)
	r := &discoveryRuntimeFixture{fakeRuntime: fakeRuntime{active: control.WorkloadIdle}, stopped: true, running: true}
	c := testController(t, s, r)
	snap, err := s.Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	e := control.OperatorPrecondition{Incarnation: state.LeaseFence.Incarnation, Version: state.Version, Owner: state.Owner, ConfigurationRevision: snap.Revision}
	tr := store.Transition{ID: "crash-session", Source: state, Target: state, Previous: state, Initiator: "temporary-native-discovery", Phase: control.PhaseDraining, Deadline: time.Now().Add(time.Minute), ConfigurationRevision: snap.Revision}
	record := control.TemporaryDiscoverySession{ID: "crash-session", Token: "crash-token", Status: "starting", PriorStopped: true, Candidate: control.TemporaryDiscoveryCandidate{Unit: "ollama.service"}}
	if _, err := s.StartTemporaryDiscovery(context.Background(), e, tr, record); err != nil {
		t.Fatal(err)
	}
	launch := control.TemporaryDiscoveryLaunchEvidence{InvocationID: "12345678901234567890123456789012", JobID: "1", ActivationTimestamp: "10"}
	if err := s.RecordTemporaryLaunch(context.Background(), record.ID, record.Token, launch); err != nil {
		t.Fatal(err)
	}
	pending, err := s.TemporaryDiscoveryStatus(context.Background())
	if err != nil || pending.InvocationID != "" || pending.LaunchEvidence.InvocationID != launch.InvocationID {
		t.Fatalf("crash window record: %+v %v", pending, err)
	}
	if err := c.CleanupTemporaryDiscovery(context.Background(), record.ID, record.Token); err != nil {
		t.Fatal(err)
	}
	if r.stops != 1 || r.running {
		t.Fatalf("crash-window launch survived cleanup: %+v", r)
	}
	done, err := s.TemporaryDiscoveryStatus(context.Background())
	if err != nil || done.Status != "completed" {
		t.Fatalf("%+v %v", done, err)
	}
	if _, err := c.Recover(context.Background()); err != nil {
		t.Fatalf("admission remained closed: %v", err)
	}
}

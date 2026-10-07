package supervisor

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

type evidenceFixture struct {
	attestation     Attestation
	err             error
	acquireErr      error
	calls           int
	acquireCalls    int
	releaseCalls    int
}

func (e *evidenceFixture) Attest(context.Context) (Attestation, error) {
	e.calls++
	return e.attestation, e.err
}

func (e *evidenceFixture) AcquireFence(_ context.Context, token string) (func(), error) {
	e.acquireCalls++
	if e.acquireErr != nil {
		return nil, e.acquireErr
	}
	if token == "" || token != e.attestation.Token {
		return nil, errors.New("stale evidence generation")
	}
	return func() { e.releaseCalls++ }, nil
}

func freshEvidence() *evidenceFixture {
	return &evidenceFixture{attestation: Attestation{AttestedAt: time.Now(), Token: "gen-1"}}
}

func enableIdlePolicy(t *testing.T, s *store.Store, timeout int) control.PolicyState {
	t.Helper()
	ctx := context.Background()
	state, err := s.State(ctx)
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
	committed, err := s.SetIdlePolicy(ctx, e, control.IdlePolicy{TimeoutMinutes: timeout}, true)
	if err != nil {
		t.Fatal(err)
	}
	return committed
}

func TestPolicyTickOffDisarmsAndNeverConsultsEvidence(t *testing.T) {
	s := openStore(t)
	ownershipState(t, s, control.OwnerSupervisor, control.WorkloadText)
	evidence := freshEvidence()
	c := testController(t, s, &fakeRuntime{active: control.WorkloadText})
	if err := c.PolicyTick(context.Background(), evidence); err != nil {
		t.Fatal(err)
	}
	if evidence.calls != 0 {
		t.Fatal("off policy consulted evidence")
	}
	settings, _ := s.Settings(context.Background())
	if settings.ArmedDeadline != nil {
		t.Fatal("off policy armed a deadline")
	}
}

func TestPolicyTickEnabledWithoutEvidenceFailsClosed(t *testing.T) {
	s := openStore(t)
	ownershipState(t, s, control.OwnerSupervisor, control.WorkloadText)
	enableIdlePolicy(t, s, 5)
	before, err := s.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	c := testController(t, s, &fakeRuntime{active: control.WorkloadText})
	if err := c.PolicyTick(context.Background(), nil); !errors.Is(err, store.ErrEvidenceUnavailable) {
		t.Fatalf("tick without evidence: %v", err)
	}
	after, _ := s.State(context.Background())
	if after != before {
		t.Fatal("failed tick changed state")
	}
	settings, _ := s.Settings(context.Background())
	if settings.ArmedDeadline != nil {
		t.Fatal("failed tick armed a deadline")
	}
}

func TestPolicyTickNoOpsForUserOwnershipAndNonStableStates(t *testing.T) {
	s := openStore(t)
	ownershipState(t, s, control.OwnerUser, control.WorkloadText)
	enableIdlePolicy(t, s, 5)
	c := testController(t, s, &fakeRuntime{active: control.WorkloadText})
	if err := c.PolicyTick(context.Background(), freshEvidence()); err != nil {
		t.Fatal(err)
	}
	settings, _ := s.Settings(context.Background())
	if settings.ArmedDeadline != nil {
		t.Fatal("user-owned tick armed a deadline")
	}
}

func TestPolicyTickArmsVerifiedDeadlineFromFreshEmptyEvidence(t *testing.T) {
	s := openStore(t)
	ownershipState(t, s, control.OwnerSupervisor, control.WorkloadText)
	enabled := enableIdlePolicy(t, s, 5)
	c := testController(t, s, &fakeRuntime{active: control.WorkloadText})
	if err := c.PolicyTick(context.Background(), freshEvidence()); err != nil {
		t.Fatal(err)
	}
	settings, _ := s.Settings(context.Background())
	want := settings.LastActivityAt.Add(5 * time.Minute)
	if settings.ArmedDeadline == nil || !settings.ArmedDeadline.Equal(want) {
		t.Fatalf("armed = %v, want %v", settings.ArmedDeadline, want)
	}
	if settings.AttestationAt == nil {
		t.Fatal("attestation timestamp not recorded")
	}
	_ = enabled
	state, _ := s.State(context.Background())
	if state.ActiveWorkload != control.WorkloadText || state.PendingIdleDeadline == nil {
		t.Fatalf("tick must not move the workload: %+v", state)
	}
	// A second tick before the deadline keeps the arm without firing.
	if err := c.PolicyTick(context.Background(), freshEvidence()); err != nil {
		t.Fatal(err)
	}
	state, _ = s.State(context.Background())
	if state.ActiveWorkload != control.WorkloadText || state.Phase != control.PhaseStable {
		t.Fatalf("premature idle: %+v", state)
	}
}

func TestPolicyTickFailsClosedOnStaleAttestation(t *testing.T) {
	s := openStore(t)
	ownershipState(t, s, control.OwnerSupervisor, control.WorkloadText)
	enableIdlePolicy(t, s, 5)
	c := testController(t, s, &fakeRuntime{active: control.WorkloadText})
	if err := c.PolicyTick(context.Background(), freshEvidence()); err != nil {
		t.Fatal(err)
	}
	settings, _ := s.Settings(context.Background())
	if settings.ArmedDeadline == nil {
		t.Fatal("expected armed deadline")
	}
	stale := &evidenceFixture{attestation: Attestation{AttestedAt: time.Now().Add(-MaxAttestationAge - time.Minute)}}
	if err := c.PolicyTick(context.Background(), stale); !errors.Is(err, store.ErrEvidenceUnavailable) {
		t.Fatalf("stale attestation: %v", err)
	}
	settings, _ = s.Settings(context.Background())
	if settings.ArmedDeadline != nil {
		t.Fatal("stale attestation kept the armed deadline")
	}
}

func TestPolicyTickDisarmsOnEvidenceOrPendingWork(t *testing.T) {
	for _, mode := range []string{"running evidence", "pending work"} {
		t.Run(mode, func(t *testing.T) {
			s := openStore(t)
			before := ownershipState(t, s, control.OwnerSupervisor, control.WorkloadText)
			enableIdlePolicy(t, s, 5)
			c := testController(t, s, &fakeRuntime{active: control.WorkloadText})
			if err := c.PolicyTick(context.Background(), freshEvidence()); err != nil {
				t.Fatal(err)
			}
			evidence := freshEvidence()
			if mode == "running evidence" {
				evidence.attestation.Running = []string{"job-1"}
			} else {
				if _, err := s.AdmitWorkToken(context.Background(), "req", "job", control.WorkloadText, before.LeaseFence); err != nil {
					t.Fatal(err)
				}
			}
			if err := c.PolicyTick(context.Background(), evidence); err != nil {
				t.Fatal(err)
			}
			settings, _ := s.Settings(context.Background())
			if settings.ArmedDeadline != nil {
				t.Fatalf("%s kept the armed deadline", mode)
			}
		})
	}
}

func TestPolicyTickFiresOnceArmedDeadlineElapses(t *testing.T) {
	// The store is the durable time authority, so both clocks travel together.
	now := time.Now().UTC().Truncate(time.Second)
	s := openStoreWithClock(t, func() time.Time { return now })
	ownershipState(t, s, control.OwnerSupervisor, control.WorkloadText)
	enableIdlePolicy(t, s, 5)
	r := &fakeRuntime{active: control.WorkloadText}
	c := tickController(t, s, r, func() time.Time { return now })
	attest := func() *evidenceFixture {
		return &evidenceFixture{attestation: Attestation{AttestedAt: now, Token: "gen-1"}}
	}
	if err := c.PolicyTick(context.Background(), attest()); err != nil {
		t.Fatal(err)
	}
	if settings, _ := s.Settings(context.Background()); settings.ArmedDeadline == nil {
		t.Fatal("first tick did not arm")
	}
	if state, _ := s.State(context.Background()); state.ActiveWorkload != control.WorkloadText {
		t.Fatal("first tick fired instead of arming")
	}
	now = now.Add(6 * time.Minute)
	firing := attest()
	if err := c.PolicyTick(context.Background(), firing); err != nil {
		t.Fatal(err)
	}
	if firing.acquireCalls != 1 || firing.releaseCalls != 1 {
		t.Fatalf("drain did not acquire and release evidence once: %d acquisitions, %d releases", firing.acquireCalls, firing.releaseCalls)
	}
	state, _ := s.State(context.Background())
	if state.ActiveWorkload != control.WorkloadIdle || state.Owner != control.OwnerSupervisor || state.Phase != control.PhaseStable {
		t.Fatalf("elapsed tick did not drain to idle: %+v", state)
	}
	if settings, _ := s.Settings(context.Background()); settings.ArmedDeadline != nil {
		t.Fatal("fired deadline still armed")
	}
	stopped := false
	for _, call := range r.calls {
		if call == "stop "+string(control.WorkloadText) {
			stopped = true
		}
	}
	if !stopped {
		t.Fatalf("workload never stopped: %v", r.calls)
	}
}

func tickController(t *testing.T, s *store.Store, r *fakeRuntime, now func() time.Time) *Controller {
	t.Helper()
	snapshot, err := s.Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	controller, err := newController(s, r, Config{
		Catalog:      &snapshot,
		DrainTimeout: time.Second, VerifyTimeout: time.Second,
		ActionTimeout:  time.Second,
		CleanupTimeout: time.Second, FinalizeTimeout: time.Second,
		PollInterval: time.Millisecond,
	}, now, func() (string, error) {
		return "11111111-1111-4111-8111-111111111111", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return controller
}

func openStoreWithClock(t *testing.T, clock func() time.Time) *store.Store {
	t.Helper()
	s, err := store.OpenWithClock(context.Background(), filepath.Join(t.TempDir(), "state.db"), clock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestPolicyTickFailsClosedOnFutureDatedAttestation(t *testing.T) {
	s := openStore(t)
	ownershipState(t, s, control.OwnerSupervisor, control.WorkloadText)
	enableIdlePolicy(t, s, 5)
	c := testController(t, s, &fakeRuntime{active: control.WorkloadText})
	future := &evidenceFixture{attestation: Attestation{AttestedAt: time.Now().Add(time.Minute)}}
	if err := c.PolicyTick(context.Background(), future); !errors.Is(err, store.ErrEvidenceUnavailable) {
		t.Fatalf("future attestation: %v", err)
	}
	settings, _ := s.Settings(context.Background())
	if settings.ArmedDeadline != nil {
		t.Fatal("future attestation armed a deadline")
	}
}

func TestPolicyTickDisarmsAndNoOpsWhileDegradedThenArmsWhenHealthy(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	ownershipState(t, s, control.OwnerSupervisor, control.WorkloadText)
	enableIdlePolicy(t, s, 5)
	c := testController(t, s, &fakeRuntime{active: control.WorkloadText})
	// Arm once on the healthy workload, then degrade health.
	if err := c.PolicyTick(ctx, freshEvidence()); err != nil {
		t.Fatal(err)
	}
	if settings, _ := s.Settings(ctx); settings.ArmedDeadline == nil {
		t.Fatal("healthy tick did not arm")
	}
	state, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	state.Health = control.HealthDegraded
	if _, err := s.UpdateState(ctx, state.Version, state); err != nil {
		t.Fatal(err)
	}
	if err := c.PolicyTick(ctx, freshEvidence()); err != nil {
		t.Fatal(err)
	}
	settings, _ := s.Settings(ctx)
	if settings.ArmedDeadline != nil {
		t.Fatal("degraded tick kept the armed deadline")
	}
	state, _ = s.State(ctx)
	if state.Phase != control.PhaseStable || state.ActiveWorkload != control.WorkloadText {
		t.Fatalf("degraded tick moved the workload: %+v", state)
	}
	// Recovery to healthy arms normally again.
	state.Health = control.HealthHealthy
	if _, err := s.UpdateState(ctx, state.Version, state); err != nil {
		t.Fatal(err)
	}
	if err := c.PolicyTick(ctx, freshEvidence()); err != nil {
		t.Fatal(err)
	}
	if settings, _ := s.Settings(ctx); settings.ArmedDeadline == nil {
		t.Fatal("recovered workload did not arm")
	}
}

// Work queued after the attestation but before the drain commit must abort
// the idle: generation acquisition is the final fence.
func TestPolicyTickAbortsDrainWhenEvidenceInvalidatedBeforeCommit(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	s := openStoreWithClock(t, func() time.Time { return now })
	ownershipState(t, s, control.OwnerSupervisor, control.WorkloadText)
	enableIdlePolicy(t, s, 5)
	r := &fakeRuntime{active: control.WorkloadText}
	c := tickController(t, s, r, func() time.Time { return now })
	if err := c.PolicyTick(context.Background(), &evidenceFixture{attestation: Attestation{AttestedAt: now, Token: "gen-1"}}); err != nil {
		t.Fatal(err)
	}
	if settings, _ := s.Settings(context.Background()); settings.ArmedDeadline == nil {
		t.Fatal("first tick did not arm")
	}
	now = now.Add(6 * time.Minute)
	// The provider's evidence moved on: the attested generation cannot be
	// acquired (new external work queued since the attestation).
	revoked := &evidenceFixture{
		attestation:   Attestation{AttestedAt: now, Token: "gen-2"},
		acquireErr: errors.New("evidence generation moved"),
	}
	if err := c.PolicyTick(context.Background(), revoked); !errors.Is(err, store.ErrEvidenceUnavailable) {
		t.Fatalf("invalidated evidence: %v", err)
	}
	state, _ := s.State(context.Background())
	if state.ActiveWorkload != control.WorkloadText || state.Phase != control.PhaseStable {
		t.Fatalf("drain proceeded on invalidated evidence: %+v", state)
	}
	if settings, _ := s.Settings(context.Background()); settings.ArmedDeadline != nil {
		t.Fatal("invalidated evidence kept the armed deadline")
	}
	if len(r.calls) != 0 {
		t.Fatal("invalidated drain drove runtime effects", r.calls)
	}
	// Stable evidence re-arms and drains normally on a later tick.
	if err := c.PolicyTick(context.Background(), &evidenceFixture{attestation: Attestation{AttestedAt: now, Token: "gen-2"}}); err != nil {
		t.Fatal(err)
	}
	if settings, _ := s.Settings(context.Background()); settings.ArmedDeadline == nil {
		t.Fatal("stable evidence did not re-arm")
	}
}


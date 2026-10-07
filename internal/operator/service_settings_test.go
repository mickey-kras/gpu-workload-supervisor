package operator

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

type policyStoreFixture struct {
	state    control.State
	settings control.PolicyState
	err      error
	setCalls int
}

func (p *policyStoreFixture) Settings(context.Context) (control.PolicyState, error) {
	return p.settings, p.err
}
func (p *policyStoreFixture) SetIdlePolicy(_ context.Context, e control.SettingsPrecondition, policy control.IdlePolicy, evidenceAvailable bool) (control.PolicyState, error) {
	if p.err != nil {
		return p.settings, p.err
	}
	if policy.TimeoutMinutes != control.IdlePolicyOff && !evidenceAvailable {
		return p.settings, store.ErrEvidenceUnavailable
	}
	p.setCalls++
	p.settings.Policy = policy
	p.settings.SettingsRevision = "rotated"
	return p.settings, nil
}

func stableFixtureState() control.State {
	state := control.InitialState("inc", time.Now())
	state.Phase = control.PhaseStable
	state.ActiveWorkload = control.WorkloadIdle
	return state
}

func settingsService(t *testing.T, b Backend, p PolicyStore, configurable bool) Service {
	t.Helper()
	return Service{StatePath: filepath.Join(t.TempDir(), "state.db"), Open: func(context.Context) (Session, error) {
		return Session{Backend: b, Revision: "rev", Workloads: []Workload{{"idle", "Idle"}}, Close: func() error { return nil }, PolicyStore: p, IdlePolicyConfigurable: configurable}, nil
	}}
}

func TestGetSettingsReturnsCommittedPolicyAndRevision(t *testing.T) {
	b := &backendFixture{state: stableFixtureState()}
	p := &policyStoreFixture{state: stableFixtureState(), settings: control.PolicyState{Policy: control.IdlePolicy{TimeoutMinutes: 30}, SettingsRevision: "s-rev"}}
	s := settingsService(t, b, p, false)
	r := s.Handle(Request{ProtocolVersion: 1, RequestID: "r", Action: "get-settings"})
	if r.Code != OK || r.Settings == nil {
		t.Fatalf("response %+v", r)
	}
	if r.Status != nil {
		t.Fatalf("get-settings minted a status from durable state: %+v", r.Status)
	}
	if r.Settings.Policy.TimeoutMinutes != 30 || r.Settings.SettingsRevision != "s-rev" {
		t.Fatalf("settings %+v", r.Settings)
	}
	if b.called {
		t.Fatal("get-settings touched the controller backend")
	}
}

// The status response keeps the exact protocol-v1 shape: no idle policy
// fields, no settings object — the readout lives on get-settings only.
func TestStatusKeepsV1ShapeAndOmitsSettings(t *testing.T) {
	state := stableFixtureState()
	b := &backendFixture{state: state}
	p := &policyStoreFixture{state: state, settings: control.PolicyState{SettingsRevision: "s-rev"}}
	s := settingsService(t, b, p, true)
	r := s.Handle(Request{ProtocolVersion: 1, RequestID: "r", Action: "status"})
	if r.Code != OK || r.Settings != nil || r.Status == nil {
		t.Fatalf("response %+v", r)
	}
	encoded, err := json.Marshal(r.Status)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"idlePolicy", "idlePolicyConfigurable"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("status leaks %s: %s", forbidden, encoded)
		}
	}
}

func setIdleRequest(state control.State, timeout int) Request {
	return Request{ProtocolVersion: 1, RequestID: "r", Action: "set-idle-policy",
		Expected: &Expected{state.LeaseFence.Incarnation, strconv.FormatUint(state.Version, 10), state.Owner, "rev"},
		Settings: &SettingsRequest{TimeoutMinutes: timeout, SettingsRevision: "s-rev"}}
}

func TestSetIdlePolicyEnableRejectedWithoutEvidenceProvider(t *testing.T) {
	b := &backendFixture{state: stableFixtureState()}
	p := &policyStoreFixture{state: stableFixtureState(), settings: control.PolicyState{SettingsRevision: "s-rev"}}
	s := settingsService(t, b, p, false)
	r := s.Handle(setIdleRequest(p.state, 60))
	if r.Code != Unavailable || r.Status != nil || r.Settings != nil {
		t.Fatalf("response %+v", r)
	}
	if p.setCalls != 0 {
		t.Fatal("enable reached the store without an evidence provider")
	}
}

func TestSetIdlePolicyOffAlwaysAllowedAndRotatesRevision(t *testing.T) {
	b := &backendFixture{state: stableFixtureState()}
	p := &policyStoreFixture{state: stableFixtureState(), settings: control.PolicyState{SettingsRevision: "s-rev"}}
	s := settingsService(t, b, p, false)
	r := s.Handle(setIdleRequest(p.state, 0))
	if r.Code != OK || r.Settings == nil || r.Status != nil {
		t.Fatalf("response %+v", r)
	}
	if r.Settings.SettingsRevision != "rotated" || r.Settings.Policy.TimeoutMinutes != 0 {
		t.Fatalf("settings %+v", r.Settings)
	}
	if p.setCalls != 1 || b.called {
		t.Fatal("set-idle-policy touched the controller backend")
	}
}

func TestSetIdlePolicyEnableCommitsWhenConfigurable(t *testing.T) {
	b := &backendFixture{state: stableFixtureState()}
	p := &policyStoreFixture{state: stableFixtureState(), settings: control.PolicyState{SettingsRevision: "s-rev"}}
	s := settingsService(t, b, p, true)
	r := s.Handle(setIdleRequest(p.state, 120))
	if r.Code != OK || r.Settings == nil || r.Settings.Policy.TimeoutMinutes != 120 || r.Status != nil {
		t.Fatalf("response %+v", r)
	}
}

func TestSetIdlePolicyMapsTypedStoreErrors(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code Code
	}{
		{store.ErrSettingsConflict, StaleState},
		{store.ErrInvalidIdleTimeout, InvalidRequest},
		{store.ErrWrongOwner, WrongOwner},
		{store.ErrUnstableState, RecoveryRequired},
		{store.ErrTransitionRunning, Busy},
		{store.ErrConfigurationConflict, StaleState},
		{errors.New("disk gone"), Unavailable},
	} {
		b := &backendFixture{state: stableFixtureState()}
		p := &policyStoreFixture{state: stableFixtureState(), err: tc.err, settings: control.PolicyState{SettingsRevision: "s-rev"}}
		s := settingsService(t, b, p, true)
		if r := s.Handle(setIdleRequest(p.state, 60)); r.Code != tc.code || r.Status != nil {
			t.Fatalf("%v -> %+v, want %s", tc.err, r, tc.code)
		}
	}
}

func TestSetIdlePolicyRejectsStaleCatalogRevisionBeforeStore(t *testing.T) {
	b := &backendFixture{state: stableFixtureState()}
	p := &policyStoreFixture{state: stableFixtureState(), settings: control.PolicyState{SettingsRevision: "s-rev"}}
	s := settingsService(t, b, p, true)
	req := setIdleRequest(p.state, 60)
	req.Expected.ConfigurationRevision = "old"
	if r := s.Handle(req); r.Code != StaleState || p.setCalls != 0 {
		t.Fatalf("response %+v calls=%d", r, p.setCalls)
	}
}

func TestSettingsActionsUseStatusBudget(t *testing.T) {
	s := Service{StatusTimeout: 31 * time.Second}
	for _, action := range []string{"status", "get-settings", "set-idle-policy"} {
		budget, ok := s.requestBudget(action)
		if !ok || budget != 31*time.Second {
			t.Fatalf("%s budget=%v ok=%v", action, budget, ok)
		}
	}
}

func TestSettingsActionsFailClosedWithoutPolicyStore(t *testing.T) {
	b := &backendFixture{state: stableFixtureState()}
	s := settingsService(t, b, nil, false)
	if r := s.Handle(Request{ProtocolVersion: 1, RequestID: "r", Action: "get-settings"}); r.Code != Unavailable || r.Status != nil {
		t.Fatalf("get-settings %+v", r)
	}
	if r := s.Handle(setIdleRequest(stableFixtureState(), 0)); r.Code != Unavailable || r.Status != nil {
		t.Fatalf("set-idle-policy %+v", r)
	}
}

// TestSettingsSurfaceAgainstRealStore drives the full service path over a real
// SQLite store: seeded Off, enable gated without an evidence provider, and a
// committed write rotating the opaque settings revision.
func TestSettingsSurfaceAgainstRealStore(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	snap, err := db.ReplaceCatalog(ctx, "", control.Catalog{Version: 1, Profiles: []control.WorkloadProfile{{ID: "third", Label: "Third", Adapter: "systemd", Unit: "third.service", Cgroup: "/user/third", HealthURL: "http://localhost:9999"}}})
	if err != nil {
		t.Fatal(err)
	}
	state, err := db.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	state.Phase = control.PhaseStable
	state.ActiveWorkload = control.WorkloadIdle
	state, err = db.UpdateState(ctx, state.Version, state)
	if err != nil {
		t.Fatal(err)
	}
	b := &backendFixture{state: state}
	svc := Service{StatePath: path, Open: func(context.Context) (Session, error) {
		return Session{Backend: b, Revision: snap.Revision, Workloads: []Workload{{"idle", "Idle"}, {"third", "Third"}}, PolicyStore: db, IdlePolicyConfigurable: false}, nil
	}}

	r := svc.Handle(Request{ProtocolVersion: 1, RequestID: "g1", Action: "get-settings"})
	if r.Code != OK || r.Settings == nil || r.Status != nil {
		t.Fatalf("get-settings %+v", r)
	}
	if r.Settings.Policy.TimeoutMinutes != 0 || r.Settings.SettingsRevision == "" {
		t.Fatalf("seeded settings %+v", r.Settings)
	}

	expected := &Expected{state.LeaseFence.Incarnation, strconv.FormatUint(state.Version, 10), state.Owner, snap.Revision}
	r = svc.Handle(Request{ProtocolVersion: 1, RequestID: "s1", Action: "set-idle-policy", Expected: expected,
		Settings: &SettingsRequest{TimeoutMinutes: 60, SettingsRevision: r.Settings.SettingsRevision}})
	if r.Code != Unavailable {
		t.Fatalf("enable without evidence provider = %s", r.Code)
	}

	r = svc.Handle(Request{ProtocolVersion: 1, RequestID: "s2", Action: "set-idle-policy", Expected: expected,
		Settings: &SettingsRequest{TimeoutMinutes: 0, SettingsRevision: "stale-revision"}})
	if r.Code != StaleState {
		t.Fatalf("stale settings revision = %s", r.Code)
	}

	fresh := svc.Handle(Request{ProtocolVersion: 1, RequestID: "g2", Action: "get-settings"})
	if fresh.Code != OK {
		t.Fatalf("get-settings %+v", fresh)
	}
	r = svc.Handle(Request{ProtocolVersion: 1, RequestID: "s3", Action: "set-idle-policy", Expected: expected,
		Settings: &SettingsRequest{TimeoutMinutes: 0, SettingsRevision: fresh.Settings.SettingsRevision}})
	if r.Code != OK || r.Settings == nil || r.Settings.Policy.TimeoutMinutes != 0 {
		t.Fatalf("committed off write %+v", r)
	}
	if r.Settings.SettingsRevision == fresh.Settings.SettingsRevision {
		t.Fatal("off write did not rotate the settings revision")
	}
	after, err := db.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.Version != state.Version {
		t.Fatalf("settings write moved control state: %+v", after)
	}
}

// TestSetIdlePolicyDuringLiveTransitionReturnsBusy drives the full protocol
// path while a real operator transition is persisted (phase=draining): the
// response code must be busy, never recovery_required.
func TestSetIdlePolicyDuringLiveTransitionReturnsBusy(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	snap, err := db.ReplaceCatalog(ctx, "", control.Catalog{Version: 1, Profiles: []control.WorkloadProfile{{ID: "third", Label: "Third", Adapter: "systemd", Unit: "third.service", Cgroup: "/user/third", HealthURL: "http://localhost:9999"}}})
	if err != nil {
		t.Fatal(err)
	}
	state, err := db.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	state.Phase = control.PhaseStable
	state.ActiveWorkload = control.WorkloadIdle
	state, err = db.UpdateState(ctx, state.Version, state)
	if err != nil {
		t.Fatal(err)
	}
	settings, err := db.Settings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	precondition := control.OperatorPrecondition{
		Incarnation: state.LeaseFence.Incarnation, Version: state.Version,
		Owner: state.Owner, ConfigurationRevision: snap.Revision,
	}
	started, err := db.StartOperatorTransition(ctx, precondition, store.Transition{
		ID: "operator-live", Target: state, Previous: state,
		ConfigurationRevision: snap.Revision, Deadline: time.Now().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if started.Phase != control.PhaseDraining {
		t.Fatalf("live transition phase = %q", started.Phase)
	}
	b := &backendFixture{state: started}
	svc := Service{StatePath: path, Open: func(context.Context) (Session, error) {
		return Session{Backend: b, Revision: snap.Revision, Workloads: []Workload{{"idle", "Idle"}, {"third", "Third"}}, PolicyStore: db, IdlePolicyConfigurable: true}, nil
	}}
	r := svc.Handle(Request{ProtocolVersion: 1, RequestID: "s-busy", Action: "set-idle-policy",
		Expected: &Expected{started.LeaseFence.Incarnation, strconv.FormatUint(started.Version, 10), started.Owner, snap.Revision},
		Settings: &SettingsRequest{TimeoutMinutes: 30, SettingsRevision: settings.SettingsRevision}})
	if r.Code != Busy || r.Status != nil {
		t.Fatalf("set-idle-policy during live transition = %+v, want busy", r)
	}
}

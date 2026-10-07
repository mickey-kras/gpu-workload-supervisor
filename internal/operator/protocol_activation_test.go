package operator

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

const activateRequest = `{"protocolVersion":1,"requestId":"r","action":"activate-workload","expected":{"incarnation":"i","version":"1","owner":"supervisor","configurationRevision":"c"},"target":"third"}`

func TestDecodeActivateWorkloadEnvelope(t *testing.T) {
	r, c := Decode([]byte(activateRequest))
	if c != OK || r.Target != "third" || r.Settings != nil {
		t.Fatal(r, c)
	}
	for name, s := range map[string]string{
		"missing target":   strings.Replace(activateRequest, `,"target":"third"`, ``, 1),
		"idle target":      strings.Replace(activateRequest, `"third"`, `"idle"`, 1),
		"unknown target":   strings.Replace(activateRequest, `"third"`, `"bogus!"`, 1),
		"null target":      strings.Replace(activateRequest, `"third"`, `null`, 1),
		"numeric target":   strings.Replace(activateRequest, `"third"`, `7`, 1),
		"settings present": strings.Replace(activateRequest, `"expected"`, `"settings":{"timeoutMinutes":0,"settingsRevision":"s"},"expected"`, 1),
		"missing expected": strings.Replace(activateRequest, `"expected":{"incarnation":"i","version":"1","owner":"supervisor","configurationRevision":"c"},`, ``, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, c := Decode([]byte(s)); c != InvalidRequest {
				t.Fatalf("%s: %s", s, c)
			}
		})
	}
}

func statePathT(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "state.db")
}

func TestActivateWorkloadCommitsAndHandsOutRotatedFence(t *testing.T) {
	state := stableFixtureState()
	b := &backendFixture{state: state}
	s := Service{StatePath: statePathT(t), Open: func(ctx context.Context) (Session, error) {
		return Session{Backend: b, Revision: "rev", Workloads: []Workload{{"idle", "Idle"}, {"third", "Third"}}}, nil
	}}
	r := s.Handle(Request{ProtocolVersion: 1, RequestID: "r", Action: actionActivateWorkload,
		Expected: &Expected{state.LeaseFence.Incarnation, strconv.FormatUint(state.Version, 10), state.Owner, "rev"},
		Target:   "third"})
	if r.Code != OK || r.Status == nil || r.Settings != nil {
		t.Fatalf("response %+v", r)
	}
	if r.LeaseFence == nil || r.LeaseFence.Incarnation != state.LeaseFence.Incarnation {
		t.Fatalf("lease fence %+v", r.LeaseFence)
	}
	if r.LeaseFence.Epoch != strconv.FormatUint(state.LeaseFence.Epoch+1, 10) {
		t.Fatalf("epoch not rotated: %+v", r.LeaseFence)
	}
	if r.Status.ActiveWorkload != "third" {
		t.Fatalf("status %+v", r.Status)
	}
}

func TestActivateWorkloadDeferredCarriesStatus(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code Code
	}{
		{store.ErrNotIdle, Deferred},
		{store.ErrUnresolvedWork, Deferred},
		{store.ErrTransitionRunning, Busy},
		{store.ErrWrongOwner, WrongOwner},
		{store.ErrUnstableState, RecoveryRequired},
		{store.ErrVersionConflict, StaleState},
	} {
		b := &backendFixture{state: stableFixtureState(), err: tc.err}
		s := Service{StatePath: statePathT(t), Open: func(ctx context.Context) (Session, error) {
			return Session{Backend: b, Revision: "rev", Workloads: []Workload{{"idle", "Idle"}, {"third", "Third"}}}, nil
		}}
		state := stableFixtureState()
		r := s.Handle(Request{ProtocolVersion: 1, RequestID: "r", Action: actionActivateWorkload,
			Expected: &Expected{state.LeaseFence.Incarnation, strconv.FormatUint(state.Version, 10), state.Owner, "rev"},
			Target:   "third"})
		if r.Code != tc.code {
			t.Fatalf("%v -> %+v, want %s", tc.err, r, tc.code)
		}
		if tc.code == Deferred {
			// Deferred carries the observed status AND the current committed
			// fence, so a caller that lost a committed activation response can
			// recover the committed generation on retry.
			if r.Status == nil || r.LeaseFence == nil {
				t.Fatalf("deferred response must carry status and fence: %+v", r)
			}
			want := strconv.FormatUint(state.LeaseFence.Epoch, 10)
			if r.LeaseFence.Incarnation != state.LeaseFence.Incarnation || r.LeaseFence.Epoch != want {
				t.Fatalf("deferred fence %+v, want %s/%s", r.LeaseFence, state.LeaseFence.Incarnation, want)
			}
			if r.Status.Expected.Incarnation != r.LeaseFence.Incarnation {
				t.Fatalf("fence not bound to status: %+v", r)
			}
		} else if r.Status != nil {
			t.Fatalf("non-deferred error carries status: %+v", r)
		}
	}
}

func TestActivateWorkloadRejectsUnknownSessionTarget(t *testing.T) {
	b := &backendFixture{state: stableFixtureState()}
	s := Service{StatePath: statePathT(t), Open: func(ctx context.Context) (Session, error) {
		return Session{Backend: b, Revision: "rev", Workloads: []Workload{{"idle", "Idle"}}}, nil
	}}
	state := stableFixtureState()
	r := s.Handle(Request{ProtocolVersion: 1, RequestID: "r", Action: actionActivateWorkload,
		Expected: &Expected{state.LeaseFence.Incarnation, strconv.FormatUint(state.Version, 10), state.Owner, "rev"},
		Target:   "third"})
	if r.Code != InvalidRequest || b.called {
		t.Fatalf("response %+v", r)
	}
}

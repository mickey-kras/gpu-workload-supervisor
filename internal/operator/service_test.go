package operator

import (
	"context"
	"errors"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/lock"
	"path/filepath"
	"testing"
	"time"
)

type backendFixture struct {
	state  control.State
	called bool
	err    error
}

func (b *backendFixture) Status(context.Context) (control.State, error) {
	b.called = true
	return b.state, b.err
}
func (b *backendFixture) OperatorTransition(ctx context.Context, a string, w control.Workload, e control.OperatorPrecondition) (control.State, error) {
	b.called = true
	if ctx.Err() != nil {
		return b.state, ctx.Err()
	}
	return b.state, b.err
}
func (b *backendFixture) ActivateWorkload(ctx context.Context, w control.Workload, e control.OperatorPrecondition) (control.State, error) {
	b.called = true
	if ctx.Err() != nil {
		return b.state, ctx.Err()
	}
	if b.err != nil {
		return b.state, b.err
	}
	state := b.state
	state.DesiredWorkload = w
	state.ActiveWorkload = w
	state.LeaseFence.Epoch++
	return state, nil
}
func TestServiceGateBeforeOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	gate, e := lock.TryAcquire(path + ".lock")
	if e != nil {
		t.Fatal(e)
	}
	defer gate.Close()
	opened := false
	s := Service{StatePath: path, Open: func(context.Context) (Session, error) { opened = true; return Session{}, errors.New("unexpected") }}
	r := s.Handle(Request{ProtocolVersion: 1, RequestID: "r", Action: "status"})
	if r.Code != Busy || opened || r.Status != nil {
		t.Fatal(r, opened)
	}
}
func TestServiceDetachedAndErrorHasNoStatus(t *testing.T) {
	b := &backendFixture{state: control.InitialState("inc", time.Now())}
	b.state.ActiveWorkload = control.WorkloadIdle
	b.state.Phase = control.PhaseStable
	s := Service{StatePath: filepath.Join(t.TempDir(), "state.db"), Open: func(context.Context) (Session, error) {
		return Session{Backend: b, Revision: "rev", Workloads: []Workload{{"idle", "Idle"}}, Close: func() error { return nil }}, nil
	}}
	r := s.Handle(Request{ProtocolVersion: 1, RequestID: "r", Action: "status"})
	if r.Code != OK || r.Status == nil || r.Status.Expected.Version != "1" {
		t.Fatal(r)
	}
	b.err = context.DeadlineExceeded
	r = s.Handle(Request{ProtocolVersion: 1, RequestID: "r", Action: "status"})
	if r.Code != Timeout || r.Status != nil {
		t.Fatal(r)
	}
}
func TestServiceMutationsAndFailureBoundary(t *testing.T) {
	b := &backendFixture{state: control.InitialState("inc", time.Now())}
	b.state.Phase = control.PhaseStable
	b.state.ActiveWorkload = control.WorkloadIdle
	s := Service{StatePath: filepath.Join(t.TempDir(), "state"), Open: func(context.Context) (Session, error) {
		return Session{Backend: b, Revision: "rev", Workloads: []Workload{{"idle", "Idle"}, {"third", "Third"}}}, nil
	}}
	req := Request{ProtocolVersion: 1, RequestID: "r", Action: "user-switch", Target: "third", Expected: &Expected{"inc", "1", control.OwnerUser, "rev"}}
	if r := s.Handle(req); r.Code != OK || !b.called {
		t.Fatal(r)
	}
	b.called = false
	req.Expected.ConfigurationRevision = "old"
	if r := s.Handle(req); r.Code != StaleState || b.called {
		t.Fatal(r)
	}
	req.Expected.ConfigurationRevision = "rev"
	req.Target = "missing"
	if r := s.Handle(req); r.Code != InvalidRequest || b.called {
		t.Fatal(r)
	}
	req.Expected = nil
	if r := s.Handle(req); r.Code != InvalidRequest {
		t.Fatal(r)
	}
	s.StatusTimeout = time.Hour
	if r := s.Handle(Request{Action: "status"}); r.Code != Unavailable {
		t.Fatal(r)
	}
	s.OperationTimeout = time.Hour
	if r := s.Handle(req); r.Code != Unavailable {
		t.Fatal(r)
	}
	s.OperationTimeout = -1
	if r := s.Handle(req); r.Code != Unavailable {
		t.Fatal(r)
	}
	s.OperationTimeout = 0
	s.Open = func(context.Context) (Session, error) { return Session{}, context.DeadlineExceeded }
	if r := s.Handle(req); r.Code != Timeout {
		t.Fatal(r)
	}
	s.Open = func(context.Context) (Session, error) { return Session{}, nil }
	if r := s.Handle(req); r.Code != IncompatibleConfiguration {
		t.Fatal(r)
	}
	s.StatePath = "relative"
	if r := s.Handle(req); r.Code != Unavailable {
		t.Fatal(r)
	}
}
func TestCatalogOutputSafety(t *testing.T) {
	for _, ws := range [][]Workload{nil, {{"idle", ""}}, {{"idle", "a\nb"}}, {{"idle", "Idle"}, {"idle", "Duplicate"}}, {{"idle", "Idle"}, {"UPPER", "Upper"}}, {{"idle", string([]byte{0xff})}}} {
		if validCatalog(Session{Revision: "rev", Workloads: ws}) {
			t.Fatal(ws)
		}
	}
}

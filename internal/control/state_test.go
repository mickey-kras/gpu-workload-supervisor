package control

import (
	"strings"
	"testing"
	"time"
)

func TestInitialStateRequiresReconciliationBeforeAdmission(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.FixedZone("offset", 2*60*60))
	state := InitialState("incarnation", now)
	if err := state.Validate(); err != nil {
		t.Fatal(err)
	}
	if state.Owner != OwnerSupervisor || state.DesiredWorkload != WorkloadIdle ||
		state.ActiveWorkload != WorkloadUnknown || state.Phase != PhaseReconciling ||
		state.Admission != AdmissionClosed || state.LeaseFence.Epoch != 1 ||
		state.UpdatedAt.Location() != time.UTC {
		t.Fatalf("unsafe initial state: %#v", state)
	}
}

func TestFenceRejectsMissingIdentityOrEpoch(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fence Fence
	}{
		{"missing incarnation", Fence{Epoch: 1}},
		{"missing epoch", Fence{Incarnation: "incarnation"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.fence.Validate(); err == nil {
				t.Fatal("invalid fence accepted")
			}
		})
	}
	if err := (Fence{Incarnation: "incarnation", Epoch: 1}).Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestStateRejectsInvalidFieldsAndUnsafeAdmission(t *testing.T) {
	valid := InitialState("incarnation", time.Now())
	valid.Phase = PhaseStable
	valid.ActiveWorkload = WorkloadIdle
	valid.Admission = AdmissionOpen
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func(*State)
		want   string
	}{
		{"owner", func(s *State) { s.Owner = "other" }, "owner"},
		{"desired", func(s *State) { s.DesiredWorkload = "INVALID" }, "desired"},
		{"active", func(s *State) { s.ActiveWorkload = "INVALID" }, "active"},
		{"phase", func(s *State) { s.Phase = "other" }, "phase"},
		{"health", func(s *State) { s.Health = "other" }, "health"},
		{"admission", func(s *State) { s.Admission = "other" }, "admission"},
		{"incarnation", func(s *State) { s.LeaseFence.Incarnation = "" }, "incarnation"},
		{"epoch", func(s *State) { s.LeaseFence.Epoch = 0 }, "epoch"},
		{"version", func(s *State) { s.Version = 0 }, "version"},
		{"timestamp", func(s *State) { s.UpdatedAt = time.Time{} }, "timestamp"},
		{"transition admission", func(s *State) { s.Phase = PhaseDraining }, "admission"},
		{"error health admission", func(s *State) { s.Health = HealthError }, "admission"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := valid
			tc.change(&state)
			if err := state.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("validation error = %v, want %q", err, tc.want)
			}
		})
	}
}

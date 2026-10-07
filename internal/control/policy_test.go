package control

import (
	"testing"
	"time"
)

func TestIdlePolicyValidateAcceptsOffAndBoundedRange(t *testing.T) {
	for _, timeout := range []int{0, 5, 60, 1440} {
		if err := (IdlePolicy{TimeoutMinutes: timeout}).Validate(); err != nil {
			t.Fatalf("timeout %d rejected: %v", timeout, err)
		}
	}
}

func TestIdlePolicyValidateRejectsOutOfBounds(t *testing.T) {
	for _, timeout := range []int{-1, 1, 4, 1441, 1 << 40} {
		if err := (IdlePolicy{TimeoutMinutes: timeout}).Validate(); err == nil {
			t.Fatalf("timeout %d accepted", timeout)
		}
	}
}

func TestSettingsPreconditionProjectsOperatorPrecondition(t *testing.T) {
	e := SettingsPrecondition{
		Incarnation: "inc", Version: 7, Owner: OwnerSupervisor,
		ConfigurationRevision: "catalog-rev", SettingsRevision: "settings-rev",
	}
	projected := e.OperatorPrecondition()
	want := OperatorPrecondition{Incarnation: "inc", Version: 7, Owner: OwnerSupervisor, ConfigurationRevision: "catalog-rev"}
	if projected != want {
		t.Fatalf("projected = %#v, want %#v", projected, want)
	}
}

func TestStateValidateRejectsInvalidIdlePolicyReadModel(t *testing.T) {
	state := InitialState("inc", time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC))
	state.IdlePolicy = IdlePolicy{TimeoutMinutes: 3}
	if err := state.Validate(); err == nil {
		t.Fatal("state with out-of-bounds idle policy validated")
	}
	state.IdlePolicy = IdlePolicy{TimeoutMinutes: 30}
	if err := state.Validate(); err != nil {
		t.Fatalf("state with bounded idle policy rejected: %v", err)
	}
}

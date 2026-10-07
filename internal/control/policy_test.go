package control

import "testing"

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

package runtime

import (
	"context"
	"errors"
	"testing"
)

func TestObservationOnlyRuntimeRefusesExecution(t *testing.T) {
	m := &SystemdManager{config: SystemdConfig{ObservationOnly: true}}
	if err := m.Start(context.Background(), "text"); !errors.Is(err, errObservationOnly) {
		t.Fatalf("start = %v", err)
	}
	if err := m.Wake(context.Background(), "text"); !errors.Is(err, errObservationOnly) {
		t.Fatalf("wake = %v", err)
	}
}

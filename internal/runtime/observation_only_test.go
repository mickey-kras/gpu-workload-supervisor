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

type statusRunner struct{ status string }

func (r statusRunner) Run(context.Context, string, ...string) ([]byte, error) {
	return []byte("ExecMainStatus=" + r.status + "\n"), nil
}

func TestSandboxStartErrorNamesNamespaceFix(t *testing.T) {
	failed := errors.New("start failed")
	for status, want := range map[string]bool{"226": true, "1": false} {
		m := &SystemdManager{runner: statusRunner{status: status}}
		err := m.sandboxStartError(context.Background(), "gws-owned-x.service", failed)
		if !errors.Is(err, failed) || errors.Is(err, ErrSandboxNamespace) != want {
			t.Fatalf("status %s: %v", status, err)
		}
	}
}

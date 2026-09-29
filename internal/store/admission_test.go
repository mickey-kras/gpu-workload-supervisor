package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

func TestRejectedAdmissionReleasesTransaction(t *testing.T) {
	stateStore := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	state, err := stateStore.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, admit := range []struct {
		name string
		fn   func(context.Context, string, string, control.Workload, control.Fence) error
	}{
		{"register", stateStore.RegisterWork},
		{"admit", stateStore.AdmitWork},
	} {
		t.Run(admit.name, func(t *testing.T) {
			if err := admit.fn(ctx, "request-1", "", control.WorkloadText, state.LeaseFence); !errors.Is(err, ErrAdmissionClosed) {
				t.Fatalf("closed admission: %v", err)
			}
			if _, err := stateStore.State(ctx); err != nil {
				t.Fatalf("admission rejection held the database connection: %v", err)
			}
		})
	}
}

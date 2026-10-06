package store

import (
	"context"
	"errors"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

func TestAdmissionRequestIDBounds(t *testing.T) {
	for _, api := range []string{"register", "admit", "token"} {
		t.Run(api, func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()
			state, err := s.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			state.DesiredWorkload, state.ActiveWorkload = control.WorkloadMedia, control.WorkloadMedia
			state.Phase, state.Health, state.Admission = control.PhaseStable, control.HealthHealthy, control.AdmissionOpen
			state, err = s.UpdateState(ctx, state.Version, state)
			if err != nil {
				t.Fatal(err)
			}
			admit := func(id string) error {
				switch api {
				case "register":
					return s.RegisterWork(ctx, id, "", control.WorkloadMedia, state.LeaseFence)
				case "admit":
					return s.AdmitWork(ctx, id, "", control.WorkloadMedia, state.LeaseFence)
				default:
					_, err := s.AdmitWorkToken(ctx, id, "", control.WorkloadMedia, state.LeaseFence)
					return err
				}
			}
			// Boundary inputs are covered by the control package policy tests;
			// here one invalid ID proves rejection leaves no rows behind.
			if err := admit("invalid\xff"); err == nil {
				t.Error("accepted invalid request ID")
			}
			var count int
			if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM registered_work").Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 0 {
				t.Fatalf("rejected registrations left %d rows", count)
			}
			id := "request-" + api
			if err := admit(id); err != nil {
				t.Fatal(err)
			}
			if err := admit(id); err == nil || (api != "register" && !errors.Is(err, ErrRequestConflict)) {
				t.Fatalf("duplicate boundary ID = %v", err)
			}
		})
	}
}

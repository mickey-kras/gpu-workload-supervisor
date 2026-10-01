package store

import (
	"context"
	"errors"
	"strings"
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
			for _, id := range []string{strings.Repeat("x", 8193), strings.Repeat("é", 4097), "invalid\xff"} {
				if err := admit(id); err == nil {
					t.Errorf("accepted invalid request ID with %d bytes", len(id))
				}
			}
			var count int
			if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM registered_work").Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 0 {
				t.Fatalf("rejected registrations left %d rows", count)
			}
			id := strings.Repeat("é", 4096)
			if err := admit(id); err != nil {
				t.Fatal(err)
			}
			if err := admit(id); err == nil || (api != "register" && !errors.Is(err, ErrRequestConflict)) {
				t.Fatalf("duplicate boundary ID = %v", err)
			}
		})
	}
}

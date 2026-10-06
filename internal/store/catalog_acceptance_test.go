package store

import (
	"context"
	"errors"
	"fmt"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"reflect"
	"testing"
	"time"
)

func acceptanceCatalog() control.Catalog {
	return control.Catalog{Version: 1, Profiles: []control.WorkloadProfile{{ID: "speech", Label: "Speech", Adapter: "systemd", Unit: "speech.service", Cgroup: "/workloads/speech", HealthURL: "http://localhost:9000"}}}
}
func TestReferencedCatalogAcceptance(t *testing.T) {
	for _, ref := range []string{"active", "desired", "work", "source", "target", "previous"} {
		t.Run(ref, func(t *testing.T) {
			ctx := context.Background()
			s := testStore(t)
			c := acceptanceCatalog()
			snap, err := s.ReplaceCatalog(ctx, "", c)
			if err != nil {
				t.Fatal(err)
			}
			state, _ := s.State(ctx)
			switch ref {
			case "active":
				state.ActiveWorkload = "speech"
			case "desired":
				state.DesiredWorkload = "speech"
			case "work":
				state.ActiveWorkload = "speech"
				state.DesiredWorkload = "speech"
				state.Phase = control.PhaseStable
				state.Health = control.HealthHealthy
				state.Admission = control.AdmissionOpen
			}
			state, err = s.UpdateState(ctx, state.Version, state)
			if err != nil {
				t.Fatal(err)
			}
			if ref == "work" {
				if _, err = s.AdmitWorkToken(ctx, "speech-work", "", "speech", state.LeaseFence); err != nil {
					t.Fatal(err)
				}
				state.ActiveWorkload = control.WorkloadIdle
				state.DesiredWorkload = control.WorkloadIdle
				state.Admission = control.AdmissionClosed
				state, err = s.UpdateState(ctx, state.Version, state)
				if err != nil {
					t.Fatal(err)
				}
			}
			if ref == "source" || ref == "target" || ref == "previous" {
				tr := Transition{ID: "pending", Fence: state.LeaseFence, Source: state, Target: state, Previous: state, Phase: state.Phase, Deadline: time.Now().Add(time.Minute)}
				switch ref {
				case "source":
					tr.Source.ActiveWorkload = "speech"
				case "target":
					tr.Target.DesiredWorkload = "speech"
				case "previous":
					tr.Previous.ActiveWorkload = "speech"
				}
				if err = s.BeginTransition(ctx, tr); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := s.State(ctx)
			c.Profiles[0].Unit = "replacement.service"
			if _, err = s.ReplaceCatalog(ctx, snap.Revision, c); !errors.Is(err, ErrCatalogReferenced) {
				t.Fatalf("edit of %s: %v", ref, err)
			}
			after, _ := s.State(ctx)
			got, _ := s.Catalog(ctx)
			if !reflect.DeepEqual(got, snap) || before != after {
				t.Fatal("rejected update changed durable state")
			}
			c = acceptanceCatalog()
			c.Profiles[0].ID = "vision"
			if _, err = s.ReplaceCatalog(ctx, snap.Revision, c); !errors.Is(err, ErrCatalogReferenced) {
				t.Fatalf("removal of %s: %v", ref, err)
			}
		})
	}
}
func TestCatalogSnapshotIsolationAcceptance(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	n := 0
	s.uuid = func() (string, error) { n++; return fmt.Sprintf("revision-%d", n), nil }
	c := acceptanceCatalog()
	first, err := s.ReplaceCatalog(ctx, "", c)
	if err != nil {
		t.Fatal(err)
	}
	c.Profiles[0].Unit = "mutated.service"
	got, _ := s.Catalog(ctx)
	if got.Catalog.Profiles[0].Unit != "speech.service" || first.Catalog.Profiles[0].Unit != "speech.service" {
		t.Fatal("input alias")
	}
	next := got.Catalog.Clone()
	next.Profiles[0].Label = "Speech changed"
	if _, err = s.ReplaceCatalog(ctx, first.Revision, next); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Catalog(ctx)
	if got.Catalog.Profiles[0].Label != "Speech changed" || first.Catalog.Profiles[0].Label != "Speech" {
		t.Fatal("snapshot alias")
	}
}
func TestCatalogCommitStorageFailureAtomicityAcceptance(t *testing.T) {
	for _, table := range []string{"workload_catalog", "workload_catalog_history", "control_state"} {
		t.Run(table, func(t *testing.T) {
			ctx := context.Background()
			s := testStore(t)
			before, _ := s.State(ctx)
			if _, err := s.db.Exec("CREATE TRIGGER reject_catalog BEFORE " + map[string]string{"workload_catalog": "INSERT", "workload_catalog_history": "INSERT", "control_state": "UPDATE"}[table] + " ON " + table + " BEGIN SELECT RAISE(ABORT, 'injected durable write failure'); END"); err != nil {
				t.Fatal(err)
			}
			if _, err := s.ReplaceCatalog(ctx, "", acceptanceCatalog()); err == nil {
				t.Fatal("failed storage accepted catalog")
			}
			after, _ := s.State(ctx)
			snap, err := s.Catalog(ctx)
			if err != nil || snap.Revision != "" || before != after {
				t.Fatalf("partial commit: %+v %v", snap, err)
			}
			var count int
			if err = s.db.QueryRow("SELECT COUNT(*) FROM workload_catalog_history").Scan(&count); err != nil || count != 0 {
				t.Fatalf("partial history %d %v", count, err)
			}
		})
	}
}
func TestCatalogCorruptReferencesFailClosedAcceptance(t *testing.T) {
	for _, table := range []string{"registered_work", "transitions", "control_state", "workload_catalog"} {
		t.Run(table, func(t *testing.T) {
			ctx := context.Background()
			s := testStore(t)
			if _, err := s.db.Exec("DROP TABLE " + table); err != nil {
				t.Fatal(err)
			}
			if _, err := s.ReplaceCatalog(ctx, "", acceptanceCatalog()); err == nil {
				t.Fatal("unreadable references accepted")
			}
		})
	}
	t.Run("invalid transition JSON", func(t *testing.T) {
		ctx := context.Background()
		s := testStore(t)
		st, _ := s.State(ctx)
		if err := s.BeginTransition(ctx, Transition{ID: "corrupt", Source: st, Target: st, Previous: st, Fence: st.LeaseFence, Phase: st.Phase, Deadline: time.Now().Add(time.Minute)}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec("UPDATE transitions SET previous_state='invalid'"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ReplaceCatalog(ctx, "", acceptanceCatalog()); err == nil {
			t.Fatal("corrupt referenced profile accepted")
		}
	})
}
func TestCatalogValidationAndEntropyFailureAcceptance(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	bad := acceptanceCatalog()
	bad.Version = 99
	if _, err := s.ReplaceCatalog(ctx, "", bad); err == nil {
		t.Fatal("invalid schema accepted")
	}
	s.uuid = func() (string, error) { return "", errors.New("entropy failed") }
	if _, err := s.ReplaceCatalog(ctx, "", acceptanceCatalog()); err == nil {
		t.Fatal("missing revision accepted")
	}
	if snap, err := s.Catalog(ctx); err != nil || snap.Revision != "" {
		t.Fatal(snap, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReplaceCatalog(ctx, "", acceptanceCatalog()); err == nil {
		t.Fatal("closed store accepted catalog")
	}
}

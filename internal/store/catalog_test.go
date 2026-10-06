package store

import (
	"context"
	"errors"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"path/filepath"
	"testing"
	"time"
)

func TestCatalogCommitAtomicRevision(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	before, _ := s.State(ctx)
	c := control.Catalog{Version: 1, Profiles: []control.WorkloadProfile{{ID: "speech", Label: "Speech", Adapter: "systemd", Unit: "speech.service", Cgroup: "/user/speech", HealthURL: "http://localhost:9000"}}}
	snap, err := s.ReplaceCatalog(ctx, "", c)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := s.State(ctx)
	if snap.Revision == "" || after.Version != before.Version+1 || after.LeaseFence != before.LeaseFence {
		t.Fatal("catalog commit changed fence or failed version increment")
	}
	next := snap.Catalog.Clone()
	next.Profiles[0].Label = "Speech changed"
	second, err := s.ReplaceCatalog(ctx, snap.Revision, next)
	if err != nil {
		t.Fatal(err)
	}
	after, _ = s.State(ctx)
	if _, err = s.ReplaceCatalog(ctx, snap.Revision, next); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale revision accepted: %v", err)
	}
	got, _ := s.Catalog(ctx)
	if got.Revision != second.Revision {
		t.Fatal("stale writer won")
	}
	state, _ := s.State(ctx)
	if state != after {
		t.Fatal("stale writer advanced state")
	}
	var count int
	if err = s.db.QueryRow("SELECT COUNT(*) FROM workload_catalog_history").Scan(&count); err != nil || count != 2 {
		t.Fatalf("history %d %v", count, err)
	}
}

func driftCatalog(workloads ...control.Workload) control.Catalog {
	c := control.Catalog{Version: 1}
	for _, id := range workloads {
		c.Profiles = append(c.Profiles, control.WorkloadProfile{ID: id, Label: string(id), Adapter: "systemd", Unit: string(id) + ".service", Cgroup: "/user/" + string(id), HealthURL: "http://localhost:9000"})
	}
	return c
}

func TestCatalogDriftRejectsStaleRevisionAndDroppedProfile(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	first, err := s.ReplaceCatalog(ctx, "", driftCatalog(control.WorkloadText, control.WorkloadMedia))
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.ReplaceCatalog(ctx, first.Revision, driftCatalog(control.WorkloadText))
	if err != nil {
		t.Fatal(err)
	}
	state, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	transition := func(revision string, target control.Workload) error {
		next := state
		next.DesiredWorkload = target
		_, err := s.StartTransition(ctx, state.Version, Transition{
			ID: "drift-" + string(target), Target: next, Previous: state,
			ConfigurationRevision: revision, Deadline: time.Now().Add(time.Minute),
		})
		return err
	}
	if err := transition(first.Revision, control.WorkloadText); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale catalog revision transition = %v", err)
	}
	if err := transition(second.Revision, control.WorkloadMedia); !errors.Is(err, ErrWorkloadMismatch) {
		t.Fatalf("transition to dropped profile = %v", err)
	}
	opened := state
	opened.DesiredWorkload, opened.ActiveWorkload = control.WorkloadText, control.WorkloadText
	opened.Phase, opened.Health, opened.Admission = control.PhaseStable, control.HealthHealthy, control.AdmissionOpen
	opened, err = s.UpdateState(ctx, state.Version, opened)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdmitWorkTokenAtCatalog(ctx, "current-revision", "", control.WorkloadText, opened.LeaseFence, second.Revision); err != nil {
		t.Fatalf("current revision admission rejected: %v", err)
	}
	if _, err := s.AdmitWorkTokenAtCatalog(ctx, "stale-revision", "", control.WorkloadText, opened.LeaseFence, first.Revision); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale revision admission = %v", err)
	}
	moved := opened
	moved.DesiredWorkload, moved.ActiveWorkload = control.WorkloadMedia, control.WorkloadMedia
	moved, err = s.UpdateState(ctx, opened.Version, moved)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdmitWorkToken(ctx, "dropped-profile", "", control.WorkloadMedia, moved.LeaseFence); !errors.Is(err, ErrWorkloadMismatch) {
		t.Fatalf("admission for dropped profile = %v", err)
	}
}

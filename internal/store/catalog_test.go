package store

import (
	"context"
	"errors"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"path/filepath"
	"testing"
)

func TestCatalogCommitAtomicRevision(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	before, _ := s.State(ctx)
	c := control.Catalog{Version: 1, Profiles: []control.Profile{{ID: "speech", Label: "Speech", Adapter: "systemd", Unit: "speech.service", Cgroup: "/user/speech", HealthURL: "http://localhost:9000"}}}
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

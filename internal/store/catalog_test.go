package store

import (
	"context"
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
	if _, err = s.ReplaceCatalog(ctx, "", c); err == nil {
		t.Fatal("stale revision accepted")
	}
	got, _ := s.Catalog(ctx)
	if got.Revision != snap.Revision {
		t.Fatal("failed update changed catalog")
	}
}

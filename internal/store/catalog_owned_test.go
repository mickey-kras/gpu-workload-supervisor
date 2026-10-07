package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

func ownedTestCatalog() control.Catalog {
	return control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{{
		ID:        "vision",
		Label:     "Vision",
		Adapter:   "systemd",
		Unit:      "gws-owned-vision.service",
		Cgroup:    "/user.slice/user-1000.slice/user@1000.service/app.slice/gws-owned-vision.service",
		HealthURL: "http://127.0.0.1:9100/health",
		NativeModel: &control.NativeModel{
			Runtime:      "llama.cpp",
			Instance:     "local",
			Model:        "vision-q8",
			Endpoint:     "http://127.0.0.1:9100",
			LaunchFile:   "/home/u/.config/systemd/user/gws-owned-vision.service",
			LaunchSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			Owned:        &control.OwnedLaunch{ModelPath: "/models/vision-q8.gguf", Port: 9100},
		},
	}}}
}

func TestReadCatalogRejectsSmuggledFields(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for name, blob := range map[string]string{
		"unknown top-level": `{"version":1,"profiles":[{"id":"text","label":"Text","adapter":"systemd","unit":"text.service","cgroup":"/user/text","healthURL":"http://localhost:9000"}],"extra":true}`,
		"unknown owned key": `{"version":2,"profiles":[{"id":"vision","label":"Vision","adapter":"systemd","unit":"gws-owned-vision.service","cgroup":"/user.slice/app.slice/gws-owned-vision.service","healthURL":"http://127.0.0.1:9100/health","nativeModel":{"runtime":"llama.cpp","instance":"local","model":"m","endpoint":"http://127.0.0.1:9100","launchFile":"/home/u/.config/systemd/user/gws-owned-vision.service","launchSHA256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","owned":{"modelPath":"/models/m.gguf","port":9100,"threads":4}}}]}`,
		"duplicate key":     `{"version":1,"version":1,"profiles":[{"id":"text","label":"Text","adapter":"systemd","unit":"text.service","cgroup":"/user/text","healthURL":"http://localhost:9000"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := s.db.Exec("INSERT INTO workload_catalog(singleton,revision,catalog) VALUES(1,'rev',?) ON CONFLICT(singleton) DO UPDATE SET catalog=excluded.catalog", blob); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Catalog(ctx); err == nil {
				t.Fatal("smuggled catalog blob accepted")
			}
		})
	}
}

func TestCatalogVersionTransitions(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	v1 := driftCatalog(control.WorkloadText)
	first, err := s.ReplaceCatalog(ctx, "", v1)
	if err != nil {
		t.Fatal(err)
	}
	owned := ownedTestCatalog()
	second, err := s.ReplaceCatalog(ctx, first.Revision, owned)
	if err != nil {
		t.Fatalf("1→2 rejected: %v", err)
	}
	changed := owned.Clone()
	changed.Profiles[0].Label = "Vision changed"
	third, err := s.ReplaceCatalog(ctx, second.Revision, changed)
	if err != nil {
		t.Fatalf("2→2 rejected: %v", err)
	}
	if _, err := s.ReplaceCatalog(ctx, third.Revision, driftCatalog(control.WorkloadText)); err != nil {
		t.Fatalf("2→1 clean downgrade rejected: %v", err)
	}
	downgradeWithOwned := owned.Clone()
	downgradeWithOwned.Version = 1
	if _, err := s.ReplaceCatalog(ctx, third.Revision, downgradeWithOwned); !errors.Is(err, control.ErrOwnedRequiresV2) {
		t.Fatalf("2→1 with owned = %v", err)
	}
	if err := validateCatalogVersionTransition(owned, downgradeWithOwned); !errors.Is(err, control.ErrOwnedCatalogManagedBySetup) {
		t.Fatalf("transition guard = %v", err)
	}
}

func TestCatalogAtRevision(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	first, err := s.ReplaceCatalog(ctx, "", driftCatalog(control.WorkloadText))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReplaceCatalog(ctx, first.Revision, ownedTestCatalog()); err != nil {
		t.Fatal(err)
	}
	historical, err := s.CatalogAtRevision(ctx, first.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if historical.Catalog.Version != 1 || len(historical.Catalog.Profiles) != 1 || historical.Catalog.Profiles[0].ID != control.WorkloadText {
		t.Fatalf("historical catalog mismatch: %+v", historical.Catalog)
	}
	if _, err := s.CatalogAtRevision(ctx, "missing"); !errors.Is(err, ErrCatalogRevisionNotFound) {
		t.Fatalf("missing revision = %v", err)
	}
	if _, err := s.db.Exec("INSERT INTO workload_catalog_history(revision,catalog) VALUES('corrupt','not json')"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CatalogAtRevision(ctx, "corrupt"); err == nil {
		t.Fatal("corrupt historical blob accepted")
	}
}

func TestOwnedProfileReferencedByLiveState(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	owned := ownedTestCatalog()
	snap, err := s.ReplaceCatalog(ctx, "", owned)
	if err != nil {
		t.Fatal(err)
	}
	state, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	state.DesiredWorkload, state.ActiveWorkload = "vision", "vision"
	if _, err := s.UpdateState(ctx, state.Version, state); err != nil {
		t.Fatal(err)
	}
	mutated := owned.Clone()
	mutated.Profiles[0].NativeModel.Owned.CtxSize = 4096
	if _, err := s.ReplaceCatalog(ctx, snap.Revision, mutated); !errors.Is(err, ErrCatalogReferenced) {
		t.Fatalf("referenced owned mutation = %v", err)
	}
	if _, err := s.ReplaceCatalog(ctx, snap.Revision, owned.Clone()); err != nil {
		t.Fatalf("verbatim referenced owned profile rejected: %v", err)
	}
}

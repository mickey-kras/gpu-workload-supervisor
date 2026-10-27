package setup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
	"os"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

func TestRemovedRuntimeCanBeDiscoveredAndRemoved(t *testing.T) {
	for _, name := range []string{"ollama", "llama.cpp"} {
		t.Run(name, func(t *testing.T) {
			b, home, r := fixture(t)
			b.runCommand = fakeOwnedCommandFor(home)
			profile, _ := ownedFixtureProfile(t, home, "legacy", 9100)
			r.Catalog = control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{profile}}
			ctx := context.Background()
			if err := b.Apply(ctx, home, r); err != nil {
				t.Fatal(err)
			}
			before, err := ReadCatalog(ctx, r.Profile.StatePath)
			if err != nil {
				t.Fatal(err)
			}
			legacy := before.Catalog.Clone()
			legacy.Profiles[0].NativeModel.Runtime = name
			raw, err := json.Marshal(legacy)
			if err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite", r.Profile.StatePath)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = db.Exec("UPDATE workload_catalog SET catalog=?", raw); err != nil {
				t.Fatal(err)
			}
			db.Close()
			d, err := b.Discover(ctx, home)
			if err != nil {
				t.Fatal(err)
			}
			if d.Request.ExpectedRevision != before.Revision || !d.Request.Catalog.Profiles[0].RemovedRuntime() {
				t.Fatal("legacy profile unavailable")
			}
			if err := d.Request.Catalog.Validate(); err == nil {
				t.Fatal("legacy execution admitted")
			}
			b.makeRuntime = func(request Request) (gpuruntime.Manager, error) {
				if err := request.Catalog.Validate(); err != nil {
					return nil, err
				}
				return idleRuntime{}, nil
			}
			r.ExpectedRevision = before.Revision
			if err := b.Apply(ctx, home, r); err == nil {
				t.Fatal("same-unit replacement accepted before legacy removal")
			}
			r.Catalog = control.Catalog{Version: 2, Disabled: true}
			unit := legacy.Profiles[0].NativeModel.LaunchFile
			content, err := os.ReadFile(unit)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(unit, []byte("modified"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := b.Apply(ctx, home, r); !errors.Is(err, ErrOwnedUnitModified) {
				t.Fatal("modified unit accepted", err)
			}
			if err := os.WriteFile(unit, content, 0600); err != nil {
				t.Fatal(err)
			}
			if err := b.Apply(ctx, home, r); err != nil {
				t.Fatal(err)
			}
			after, err := ReadCatalog(ctx, r.Profile.StatePath)
			if err != nil || !after.Catalog.Disabled {
				t.Fatal(after, err)
			}
			if _, err := os.Stat(legacy.Profiles[0].NativeModel.LaunchFile); !os.IsNotExist(err) {
				t.Fatal("legacy owned unit survived", err)
			}
		})
	}
}

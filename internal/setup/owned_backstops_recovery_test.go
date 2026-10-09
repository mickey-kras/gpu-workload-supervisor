package setup

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/deployment"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
)

func TestLegacyOwnedBackstopsResumeOriginalActivation(t *testing.T) {
	for _, phase := range []string{"precommit", "postcommit-before-pin", "postcommit-after-pin", "postcommit-fence-cleared"} {
		t.Run(phase, func(t *testing.T) {
			backend, home, request := fixture(t)
			backend.runCommand = fakeOwnedCommandFor(home)
			a, rawA := ownedFixtureProfile(t, home, "a", 9100)
			z, rawZ := ownedFixtureProfile(t, home, "z", 9200)
			z.NativeModel.Instance = "z"
			request.Catalog = control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{a, z}}
			if err := Validate(request); err != nil {
				t.Fatal(err)
			}
			root := filepath.Join(home, ".config/gpu-workload-supervisor")
			for _, dir := range []string{root, filepath.Dir(request.Profile.StatePath), ownedUnitDirectory(home)} {
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			maintenance := phase != "postcommit-fence-cleared"
			if err := deployment.Write(request.Profile.StatePath, deployment.Marker{Version: 1, Release: deployment.Release, Maintenance: maintenance}); err != nil {
				t.Fatal(err)
			}
			journalPhase := ownedJournalPending
			if phase == "postcommit-after-pin" || !maintenance {
				journalPhase = ownedJournalCommitted
			}
			journal := unitJournal{Version: 1, StatePath: request.Profile.StatePath, Writes: map[string]string{a.Unit: digest(rawA), z.Unit: digest(rawZ)}, Deletes: map[string]string{}, Phase: journalPhase}
			if err := writeOwnedUnitJournal(root, journal); err != nil {
				t.Fatal(err)
			}
			// Materialize the unit writes that existed when the legacy process crashed.
			for unit, raw := range map[string][]byte{a.Unit: rawA, z.Unit: rawZ} {
				if err := os.WriteFile(filepath.Join(ownedUnitDirectory(home), unit), raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if phase != "precommit" {
				s := openStoreAt(t, request.Profile.StatePath)
				snapshot, err := s.ReplaceCatalog(context.Background(), "", request.Catalog)
				if err != nil {
					t.Fatal(err)
				}
				if !maintenance {
					if err := initializeIdleState(context.Background(), s); err != nil {
						t.Fatal(err)
					}
				}
				s.Close()
				// The fence-cleared case models an idempotent activation of
				// an already accepted catalog, so its original revision remains
				// current without bypassing the normal revision guard.
				if !maintenance {
					request.ExpectedRevision = snapshot.Revision
				}
			}
			if err := writeJSON(filepath.Join(root, "activation.json"), activation{Request: request, Fresh: true}); err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := Decode(strings.NewReader(string(encoded)))
			if err != nil || !reflect.DeepEqual(decoded, request) {
				t.Fatalf("decode changed recovery request: %v", err)
			}
			preview, err := backend.Plan(home, decoded)
			if err != nil || !reflect.DeepEqual(preview.Catalog, request.Catalog) {
				t.Fatalf("preview rewrote pending activation: %#v %v", preview.Catalog, err)
			}
			backend.makeRuntime = func(got Request) (gpuruntime.Manager, error) {
				if !reflect.DeepEqual(got.Catalog, request.Catalog) {
					t.Fatal("recovery runtime saw rewritten catalog")
				}
				return launchPreflightRuntime{verify: func() error { return nil }, preview: func(map[string]string) error { return nil }}, nil
			}
			if err := backend.verifyBindings(context.Background(), home, decoded); err != nil {
				t.Fatal(err)
			}
			if err := backend.Apply(context.Background(), home, decoded); err != nil {
				t.Fatalf("exact original retry cannot resume: %v", err)
			}
			accepted, err := ReadCatalog(context.Background(), request.Profile.StatePath)
			if err != nil || !reflect.DeepEqual(accepted.Catalog, request.Catalog) {
				t.Fatalf("recovery committed changed unit hashes: %#v %v", accepted, err)
			}
			for unit, want := range map[string][]byte{a.Unit: rawA, z.Unit: rawZ} {
				raw, err := os.ReadFile(filepath.Join(ownedUnitDirectory(home), unit))
				if err != nil || string(raw) != string(want) {
					t.Fatalf("recovery rewrote %s: %v", unit, err)
				}
			}
			marker, err := deployment.Read(request.Profile.StatePath)
			if err != nil || marker.Maintenance {
				t.Fatalf("recovery fence: %#v %v", marker, err)
			}
			if _, present, err := readOwnedUnitJournal(root); err != nil || present {
				t.Fatalf("journal not retired: %v", err)
			}
			if _, err := os.Stat(filepath.Join(root, "activation.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("activation not retired: %v", err)
			}
			// Reconciliation reads the persisted legacy catalog unchanged. The upgrade
			// is a separately reviewed setup operation after recovery has completed.
			if err := backend.Reconcile(context.Background(), home); err != nil {
				t.Fatal(err)
			}
			backend.makeRuntime = func(Request) (gpuruntime.Manager, error) { return idleRuntime{}, nil }
			request.ExpectedRevision = accepted.Revision
			upgraded, err := backend.Plan(home, request)
			if err != nil || upgraded.Catalog.Profiles[0].NativeModel.Owned.Conflicts != z.Unit {
				t.Fatalf("completed activation did not offer upgrade: %#v %v", upgraded, err)
			}
		})
	}
}

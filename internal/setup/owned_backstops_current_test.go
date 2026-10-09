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

func TestDerivedOwnedBackstopsResumeOriginalRequest(t *testing.T) {
	for _, phase := range []string{"prefence-missing-record", "prefence-stale-record", "precommit", "postcommit-before-pin", "postcommit-after-pin", "postcommit-fence-cleared"} {
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
			candidate, err := prepareOwnedBackstops(request)
			if err != nil {
				t.Fatal(err)
			}
			rawA, err = gpuruntime.RenderOwnedUnit(candidate.Catalog.Profiles[0])
			if err != nil {
				t.Fatal(err)
			}
			rawZ, err = gpuruntime.RenderOwnedUnit(candidate.Catalog.Profiles[1])
			if err != nil {
				t.Fatal(err)
			}
			root := filepath.Join(home, ".config/gpu-workload-supervisor")
			for _, dir := range []string{root, filepath.Dir(request.Profile.StatePath), ownedUnitDirectory(home)} {
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			prefence := strings.HasPrefix(phase, "prefence-")
			maintenance := !prefence && phase != "postcommit-fence-cleared"
			if !prefence {
				if err := deployment.Write(request.Profile.StatePath, deployment.Marker{Version: 1, Release: deployment.Release, Maintenance: maintenance}); err != nil {
					t.Fatal(err)
				}
			}
			journalPhase := ownedJournalPending
			if phase == "postcommit-after-pin" || phase == "postcommit-fence-cleared" {
				journalPhase = ownedJournalCommitted
			}
			journal := unitJournal{Version: 1, StatePath: request.Profile.StatePath, Writes: map[string]string{a.Unit: digest(rawA), z.Unit: digest(rawZ)}, Deletes: map[string]string{}, Phase: journalPhase}
			if err := writeOwnedUnitJournal(root, journal); err != nil {
				t.Fatal(err)
			}
			// Materialize the derived writes from the interrupted activation.
			for unit, raw := range map[string][]byte{a.Unit: rawA, z.Unit: rawZ} {
				if err := os.WriteFile(filepath.Join(ownedUnitDirectory(home), unit), raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if strings.HasPrefix(phase, "postcommit-") {
				s := openStoreAt(t, request.Profile.StatePath)
				snapshot, err := s.ReplaceCatalog(context.Background(), "", candidate.Catalog)
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
					candidate.ExpectedRevision = snapshot.Revision
				}
			}
			if phase != "prefence-missing-record" {
				recorded := candidate
				if phase == "prefence-stale-record" {
					recorded.Catalog = candidate.Catalog.Clone()
					recorded.Catalog.Profiles[0].Label = "Prior completed activation"
				}
				if err := writeJSON(filepath.Join(root, "activation.json"), activation{Request: recorded, Fresh: true}); err != nil {
					t.Fatal(err)
				}
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
			if err != nil || !reflect.DeepEqual(preview.Catalog, candidate.Catalog) {
				t.Fatalf("preview rewrote pending activation: %#v %v", preview.Catalog, err)
			}
			backend.makeRuntime = func(got Request) (gpuruntime.Manager, error) {
				if !reflect.DeepEqual(got.Catalog, candidate.Catalog) {
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
			if err != nil || !reflect.DeepEqual(accepted.Catalog, candidate.Catalog) {
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
			if err := backend.Reconcile(context.Background(), home); err != nil {
				t.Fatal(err)
			}
		})
	}
}

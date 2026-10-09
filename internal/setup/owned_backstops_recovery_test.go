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
			// Materialize the unit writes that existed when the legacy process crashed.
			for unit, raw := range map[string][]byte{a.Unit: rawA, z.Unit: rawZ} {
				if err := os.WriteFile(filepath.Join(ownedUnitDirectory(home), unit), raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if strings.HasPrefix(phase, "postcommit-") {
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
			if phase != "prefence-missing-record" {
				recorded := request
				if phase == "prefence-stale-record" {
					recorded.Catalog = request.Catalog.Clone()
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

func TestLegacyOwnedPrefenceIncrementalEditReplaysProvenUnits(t *testing.T) {
	backend, home, request := fixture(t)
	a, rawA := ownedFixtureProfile(t, home, "a", 9100)
	oldZ, oldRawZ := ownedFixtureProfile(t, home, "z", 9200)
	oldZ.NativeModel.Instance = "z"
	s := openStoreAt(t, request.Profile.StatePath)
	prior, err := s.ReplaceCatalog(context.Background(), "", control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{a, oldZ}})
	if err != nil {
		t.Fatal(err)
	}
	if err := initializeIdleState(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	s.Close()
	newZ, newRawZ := ownedFixtureProfile(t, home, "z", 9300)
	newZ.NativeModel.Instance = "z"
	request.Catalog = control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{a, newZ}}
	request.ExpectedRevision = prior.Revision
	plan, err := backend.planOwnedUnits(request, prior, home)
	if err != nil {
		t.Fatal(err)
	}
	journal := newOwnedUnitJournal(plan, request.Profile.StatePath)
	if len(journal.Writes) != 2 || journal.Writes[a.Unit] != digest(rawA) || journal.Writes[newZ.Unit] != digest(newRawZ) {
		t.Fatal("incremental plan did not journal unchanged and edited units", journal)
	}
	root := filepath.Join(home, ".config/gpu-workload-supervisor")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeOwnedUnitJournal(root, journal); err != nil {
		t.Fatal(err)
	}
	if err := backend.applyOwnedUnitWrites(context.Background(), home, plan, journal); err != nil {
		t.Fatal(err)
	}
	preview, err := backend.Plan(home, request)
	if err != nil || !reflect.DeepEqual(preview.Catalog, request.Catalog) {
		t.Fatalf("incremental retry preview: %v", err)
	}
	// The existing recovery guard must restore the accepted prior bytes before
	// the unchanged legacy request reapplies its edited unit.
	restored := false
	delegate := fakeOwnedCommandFor(home)
	backend.runCommand = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if len(args) > 1 && args[1] == "daemon-reload" && !restored {
			raw, err := os.ReadFile(filepath.Join(ownedUnitDirectory(home), oldZ.Unit))
			if err != nil || string(raw) != string(oldRawZ) {
				t.Fatalf("accepted prior not restored before replay: %v", err)
			}
			restored = true
		}
		return delegate(ctx, name, args...)
	}
	if err := backend.Apply(context.Background(), home, request); err != nil {
		t.Fatalf("incremental retry apply: %v", err)
	}
	if !restored {
		t.Fatal("retry bypassed pending write recovery")
	}
	accepted, err := ReadCatalog(context.Background(), request.Profile.StatePath)
	if err != nil || !reflect.DeepEqual(accepted.Catalog, request.Catalog) {
		t.Fatalf("incremental retry changed legacy spec: %v", err)
	}
	for unit, want := range map[string][]byte{a.Unit: rawA, newZ.Unit: newRawZ} {
		raw, err := os.ReadFile(filepath.Join(ownedUnitDirectory(home), unit))
		if err != nil || string(raw) != string(want) {
			t.Fatalf("incremental retry unit %s: %v", unit, err)
		}
	}
}

func TestLegacyOwnedPrefenceJournalProofGuards(t *testing.T) {
	for _, scenario := range []string{"matching-writes", "missing-unit", "extra-unit", "changed-digest", "foreign-state", "foreign-file", "fenced-different-request"} {
		t.Run(scenario, func(t *testing.T) {
			backend, home, request := fixture(t)
			backend.runCommand = fakeOwnedCommandFor(home)
			a, rawA := ownedFixtureProfile(t, home, "a", 9100)
			z, rawZ := ownedFixtureProfile(t, home, "z", 9200)
			z.NativeModel.Instance = "z"
			request.Catalog = control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{a, z}}
			root := filepath.Join(home, ".config/gpu-workload-supervisor")
			for _, dir := range []string{root, filepath.Dir(request.Profile.StatePath), ownedUnitDirectory(home)} {
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			journal := unitJournal{Version: 1, StatePath: request.Profile.StatePath, Writes: map[string]string{a.Unit: digest(rawA), z.Unit: digest(rawZ)}, Deletes: map[string]string{}, Phase: ownedJournalPending}
			switch scenario {
			case "missing-unit":
				delete(journal.Writes, z.Unit)
			case "extra-unit":
				journal.Writes["gws-owned-extra.service"] = strings.Repeat("a", 64)
			case "changed-digest":
				journal.Writes[a.Unit] = strings.Repeat("a", 64)
			case "foreign-state":
				journal.StatePath = filepath.Join(home, "other.db")
			case "fenced-different-request":
				if err := writeJSON(filepath.Join(root, "activation.json"), activation{Request: request, Fresh: true}); err != nil {
					t.Fatal(err)
				}
				if err := deployment.Write(request.Profile.StatePath, deployment.Marker{Version: 1, Release: deployment.Release, Maintenance: true}); err != nil {
					t.Fatal(err)
				}
				request.Catalog = request.Catalog.Clone()
				request.Catalog.Profiles[0].Label = "Different request, same unit writes"
			}
			if err := writeOwnedUnitJournal(root, journal); err != nil {
				t.Fatal(err)
			}
			if scenario == "fenced-different-request" {
				if _, err := backend.Plan(home, request); err == nil || !strings.Contains(err.Error(), "original request") {
					t.Fatalf("unit proofs bypassed activation equality: %v", err)
				}
				if err := backend.Apply(context.Background(), home, request); err == nil || !strings.Contains(err.Error(), "original request") {
					t.Fatalf("unit proofs bypassed apply equality: %v", err)
				}
				return
			}
			if scenario == "foreign-file" {
				foreign := []byte("foreign content")
				path := filepath.Join(ownedUnitDirectory(home), a.Unit)
				if err := os.WriteFile(path, foreign, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(ownedUnitDirectory(home), z.Unit), rawZ, 0600); err != nil {
					t.Fatal(err)
				}
				if _, err := backend.Plan(home, request); !errors.Is(err, ErrOwnedUnitCollision) {
					t.Fatalf("foreign file preview: %v", err)
				}
				if err := backend.Apply(context.Background(), home, request); !errors.Is(err, ErrOwnedUnitModified) {
					t.Fatalf("foreign file recovery: %v", err)
				}
				after, err := os.ReadFile(path)
				if err != nil || string(after) != string(foreign) {
					t.Fatalf("foreign file changed: %v", err)
				}
				if _, present, err := readOwnedUnitJournal(root); err != nil || !present {
					t.Fatalf("foreign proof failure consumed journal: %v", err)
				}
				return
			}
			prepared, err := prepareOwnedBackstopsForActivation(home, request)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "matching-writes" || scenario == "missing-unit" {
				if !reflect.DeepEqual(prepared, request) {
					t.Fatal("matching legacy writes derived dependencies")
				}
			} else {
				if prepared.Catalog.Profiles[0].NativeModel.Owned.Conflicts != z.Unit {
					t.Fatal("unproven retry suppressed normal derivation")
				}
			}
			if scenario == "foreign-state" {
				if err := backend.Apply(context.Background(), home, request); !errors.Is(err, ErrOwnedJournalStateMismatch) {
					t.Fatalf("foreign journal consumed: %v", err)
				}
			}
		})
	}
}

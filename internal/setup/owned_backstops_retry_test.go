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

func TestLegacyOwnedPrunedJournalAfterActualAbort(t *testing.T) {
	for _, scenario := range []string{"fresh", "incremental-unchanged", "incremental-add", "incremental-edits-both"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			backend, home, request := fixture(t)
			backend.runCommand = fakeOwnedCommandFor(home)
			a, rawA := ownedFixtureProfile(t, home, "a", 9100)
			z, rawZ := ownedFixtureProfile(t, home, "z", 9200)
			z.NativeModel.Instance = "z"
			prior := control.CatalogSnapshot{}
			oldRawA := rawA
			if scenario != "fresh" {
				s := openStoreAt(t, request.Profile.StatePath)
				var err error
				priorProfiles := []control.WorkloadProfile{a, z}
				priorFiles := map[string][]byte{a.Unit: rawA, z.Unit: rawZ}
				if scenario == "incremental-add" {
					priorProfiles = []control.WorkloadProfile{a}
					delete(priorFiles, z.Unit)
				}
				prior, err = s.ReplaceCatalog(ctx, "", control.Catalog{Version: 2, Profiles: priorProfiles})
				if err != nil {
					t.Fatal(err)
				}
				if err := initializeIdleState(ctx, s); err != nil {
					t.Fatal(err)
				}
				s.Close()
				if err := os.MkdirAll(ownedUnitDirectory(home), 0700); err != nil {
					t.Fatal(err)
				}
				for unit, raw := range priorFiles {
					if err := os.WriteFile(filepath.Join(ownedUnitDirectory(home), unit), raw, 0600); err != nil {
						t.Fatal(err)
					}
				}
				a, rawA = ownedFixtureProfile(t, home, "a", 9300)
				if scenario == "incremental-edits-both" {
					z, rawZ = ownedFixtureProfile(t, home, "z", 9400)
					z.NativeModel.Instance = "z"
				}
				request.ExpectedRevision = prior.Revision
			}
			request.Catalog = control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{a, z}}
			plan, err := backend.planOwnedUnits(request, prior, home)
			if err != nil {
				t.Fatal(err)
			}
			root := filepath.Join(home, ".config/gpu-workload-supervisor")
			if err := os.MkdirAll(root, 0700); err != nil {
				t.Fatal(err)
			}
			journal := newOwnedUnitJournal(plan, request.Profile.StatePath)
			if err := writeOwnedUnitJournal(root, journal); err != nil {
				t.Fatal(err)
			}
			originalWrite, originalStat := ownedAtomicWrite, ownedStat
			t.Cleanup(func() { ownedAtomicWrite, ownedStat = originalWrite, originalStat })
			injected := errors.New("injected unit write failure")
			if scenario != "incremental-unchanged" {
				ownedAtomicWrite = func(path string, raw []byte) error {
					if path == filepath.Join(ownedUnitDirectory(home), z.Unit) {
						return injected
					}
					return originalWrite(path, raw)
				}
			}
			writeErr := backend.applyOwnedUnitWrites(ctx, home, plan, journal)
			ownedAtomicWrite = originalWrite
			if scenario != "incremental-unchanged" && !errors.Is(writeErr, injected) {
				t.Fatalf("write failure: %v", writeErr)
			}
			if scenario == "incremental-unchanged" && writeErr != nil {
				t.Fatal(writeErr)
			}
			if !plan.written[a.Unit] || plan.written[z.Unit] {
				t.Fatalf("unexpected landed writes: %#v", plan.written)
			}
			rollbackErr := errors.New("injected rollback failure")
			if scenario == "fresh" {
				ownedStat = func(path string) (os.FileInfo, error) {
					if path == filepath.Join(ownedUnitDirectory(home), a.Unit) {
						return nil, rollbackErr
					}
					return originalStat(path)
				}
			} else {
				ownedAtomicWrite = func(path string, raw []byte) error {
					if path == filepath.Join(ownedUnitDirectory(home), a.Unit) {
						return rollbackErr
					}
					return originalWrite(path, raw)
				}
			}
			abortErr := backend.abortOwnedUnitWrites(ctx, home, root, request, plan, injected)
			ownedAtomicWrite, ownedStat = originalWrite, originalStat
			if !errors.Is(abortErr, injected) || !errors.Is(abortErr, ErrOwnedUnitCollision) {
				t.Fatalf("abort did not retain failed rollback: %v", abortErr)
			}
			pruned, present, err := readOwnedUnitJournal(root)
			if err != nil || !present || len(pruned.Writes) != 1 || pruned.Writes[a.Unit] != digest(rawA) {
				t.Fatalf("pruned journal: %#v %v", pruned, err)
			}
			preview, err := backend.Plan(home, request)
			if err != nil || !reflect.DeepEqual(preview.Catalog, request.Catalog) {
				t.Fatalf("pruned legacy preview: %v", err)
			}
			recovered := false
			delegate := fakeOwnedCommandFor(home)
			backend.runCommand = func(ctx context.Context, name string, args ...string) ([]byte, error) {
				if len(args) > 1 && args[1] == "daemon-reload" && !recovered {
					raw, err := os.ReadFile(filepath.Join(ownedUnitDirectory(home), a.Unit))
					if scenario == "fresh" {
						if !errors.Is(err, os.ErrNotExist) {
							t.Fatalf("fresh pending write not removed: %v", err)
						}
					} else if err != nil || string(raw) != string(oldRawA) {
						t.Fatalf("accepted bytes not restored: %v", err)
					}
					recovered = true
				}
				return delegate(ctx, name, args...)
			}
			if err := backend.Apply(ctx, home, request); err != nil {
				t.Fatalf("pruned legacy apply: %v", err)
			}
			if !recovered {
				t.Fatal("pending write recovery skipped")
			}
			accepted, err := ReadCatalog(ctx, request.Profile.StatePath)
			if err != nil || !reflect.DeepEqual(accepted.Catalog, request.Catalog) {
				t.Fatalf("legacy catalog changed: %v", err)
			}
			for unit, want := range map[string][]byte{a.Unit: rawA, z.Unit: rawZ} {
				raw, err := os.ReadFile(filepath.Join(ownedUnitDirectory(home), unit))
				if err != nil || string(raw) != string(want) {
					t.Fatalf("replayed unit %s: %v", unit, err)
				}
			}
		})
	}
}

func TestDerivedOwnedOriginalRequestAfterActualPrecommitFailure(t *testing.T) {
	for _, scenario := range []string{"fresh", "incremental-peer-add", "shared-ollama"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			backend, home, request := fixture(t)
			backend.runCommand = fakeOwnedCommandFor(home)
			a, _ := ownedFixtureProfile(t, home, "a", 9100)
			z, _ := ownedFixtureProfile(t, home, "z", 9200)
			z.NativeModel.Instance = "z"
			request.Catalog = control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{a, z}}
			if scenario == "incremental-peer-add" {
				initial := request
				initial.Catalog = control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{a}}
				if err := backend.Apply(ctx, home, initial); err != nil {
					t.Fatal(err)
				}
				accepted, err := ReadCatalog(ctx, request.Profile.StatePath)
				if err != nil {
					t.Fatal(err)
				}
				request.ExpectedRevision = accepted.Revision
			}
			if scenario == "shared-ollama" {
				profiles := []control.WorkloadProfile{z}
				for _, id := range []string{"chat", "code"} {
					p, _, err := OwnedProfile(Draft{ID: id, Label: id, App: "ollama", Model: id + ":latest", Binding: &DraftBinding{Instance: "local", Owned: &DraftOwnedLaunch{Port: 11434}}}, "/user.slice/user-1000.slice/user@1000.service", home)
					if err != nil {
						t.Fatal(err)
					}
					profiles = append(profiles, p)
				}
				request.Catalog = control.Catalog{Version: 2, Profiles: profiles}
			}
			candidate, err := prepareOwnedBackstops(request)
			if err != nil {
				t.Fatal(err)
			}
			if reflect.DeepEqual(candidate, request) {
				t.Fatal("fixture did not derive dependencies")
			}
			injected := errors.New("injected precommit failure")
			checks := 0
			backend.makeRuntime = func(Request) (gpuruntime.Manager, error) {
				return launchPreflightRuntime{verify: func() error {
					checks++
					if checks == 2 {
						return injected
					}
					return nil
				}, preview: func(map[string]string) error { return nil }}, nil
			}
			if err := backend.Apply(ctx, home, request); !errors.Is(err, injected) {
				t.Fatalf("precommit interruption: %v", err)
			}
			root := filepath.Join(home, ".config/gpu-workload-supervisor")
			saved, err := privateRead(filepath.Join(root, "activation.json"))
			if err != nil {
				t.Fatal(err)
			}
			var progress activation
			if err := json.Unmarshal(saved, &progress); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(progress.Request, candidate) {
				t.Fatal("activation did not record normalized request")
			}
			marker, err := deployment.Read(request.Profile.StatePath)
			if err != nil || !marker.Maintenance {
				t.Fatalf("maintenance not entered: %#v %v", marker, err)
			}
			changed := request
			changed.Catalog = request.Catalog.Clone()
			changed.Catalog.Profiles[0].Label = "Changed reviewed request"
			if _, err := backend.Plan(home, changed); err == nil || !strings.Contains(err.Error(), "original request") {
				t.Fatalf("changed preview bypassed equality: %v", err)
			}
			if err := backend.Apply(ctx, home, changed); err == nil || !strings.Contains(err.Error(), "original request") {
				t.Fatalf("changed apply bypassed equality: %v", err)
			}
			after, err := privateRead(filepath.Join(root, "activation.json"))
			if err != nil || string(after) != string(saved) {
				t.Fatalf("rejected request changed activation: %v", err)
			}
			encoded, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := Decode(strings.NewReader(string(encoded)))
			if err != nil || !reflect.DeepEqual(decoded, request) {
				t.Fatalf("decode changed reviewed request: %v", err)
			}
			backend.makeRuntime = func(got Request) (gpuruntime.Manager, error) {
				if !reflect.DeepEqual(got.Catalog, candidate.Catalog) {
					t.Fatal("retry selected a different recorded catalog")
				}
				return launchPreflightRuntime{verify: func() error { return nil }, preview: func(map[string]string) error { return nil }}, nil
			}
			preview, err := backend.Plan(home, decoded)
			if err != nil || !reflect.DeepEqual(preview.Catalog, candidate.Catalog) {
				t.Fatalf("reviewed request retry preview: %v", err)
			}
			if err := backend.verifyBindings(ctx, home, decoded); err != nil {
				t.Fatal(err)
			}
			if err := backend.Apply(ctx, home, decoded); err != nil {
				t.Fatalf("reviewed request retry apply: %v", err)
			}
			accepted, err := ReadCatalog(ctx, request.Profile.StatePath)
			if err != nil || !reflect.DeepEqual(accepted.Catalog, candidate.Catalog) {
				t.Fatalf("normalized recovery catalog: %v", err)
			}
			for _, p := range candidate.Catalog.Profiles {
				want, err := ownedRenderChecked(p)
				if err != nil {
					t.Fatal(err)
				}
				raw, err := os.ReadFile(filepath.Join(ownedUnitDirectory(home), p.Unit))
				if err != nil || string(raw) != string(want) {
					t.Fatalf("recovered unit %s: %v", p.Unit, err)
				}
			}
			if _, present, err := readOwnedUnitJournal(root); err != nil || present {
				t.Fatalf("retry journal remains: %v", err)
			}
			marker, err = deployment.Read(request.Profile.StatePath)
			if err != nil || marker.Maintenance {
				t.Fatalf("retry maintenance remains: %#v %v", marker, err)
			}
		})
	}
}

func TestLegacyOwnedPrunedJournalOmittedPeerProofGuards(t *testing.T) {
	for _, scenario := range []string{"changed-spec", "new-unit", "foreign-file", "foreign-new-file"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			backend, home, request := fixture(t)
			backend.runCommand = fakeOwnedCommandFor(home)
			a, rawA := ownedFixtureProfile(t, home, "a", 9100)
			z, rawZ := ownedFixtureProfile(t, home, "z", 9200)
			z.NativeModel.Instance = "z"
			s := openStoreAt(t, request.Profile.StatePath)
			accepted, err := s.ReplaceCatalog(ctx, "", control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{a, z}})
			if err != nil {
				t.Fatal(err)
			}
			if err := initializeIdleState(ctx, s); err != nil {
				t.Fatal(err)
			}
			s.Close()
			request.ExpectedRevision = accepted.Revision
			request.Catalog = accepted.Catalog.Clone()
			root := filepath.Join(home, ".config/gpu-workload-supervisor")
			for _, dir := range []string{root, ownedUnitDirectory(home)} {
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			for unit, raw := range map[string][]byte{a.Unit: rawA, z.Unit: rawZ} {
				if err := os.WriteFile(filepath.Join(ownedUnitDirectory(home), unit), raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			journal := unitJournal{Version: 1, StatePath: request.Profile.StatePath, Writes: map[string]string{a.Unit: digest(rawA)}, Deletes: map[string]string{}, Phase: ownedJournalPending}
			if err := writeOwnedUnitJournal(root, journal); err != nil {
				t.Fatal(err)
			}
			foreign := []byte("foreign unjournaled content")
			foreignPath := filepath.Join(ownedUnitDirectory(home), z.Unit)
			switch scenario {
			case "changed-spec":
				changed, _ := ownedFixtureProfile(t, home, "z", 9400)
				changed.NativeModel.Instance = "z"
				request.Catalog.Profiles[1] = changed
			case "new-unit", "foreign-new-file":
				added, _ := ownedFixtureProfile(t, home, "extra", 9400)
				added.NativeModel.Instance = "extra"
				request.Catalog.Profiles = append(request.Catalog.Profiles, added)
				if scenario == "foreign-new-file" {
					foreignPath = filepath.Join(ownedUnitDirectory(home), added.Unit)
					if err := os.WriteFile(foreignPath, foreign, 0600); err != nil {
						t.Fatal(err)
					}
				}
			case "foreign-file":
				if err := os.WriteFile(foreignPath, foreign, 0600); err != nil {
					t.Fatal(err)
				}
			}
			prepared, err := prepareOwnedBackstopsForActivation(home, request)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(scenario, "foreign-") {
				if !reflect.DeepEqual(prepared, request) {
					t.Fatal("proven unwritten peer changed retry renders")
				}
				return
			}
			if prepared.Catalog.Profiles[0].NativeModel.Owned.Conflicts == "" {
				t.Fatal("foreign omitted unit suppressed derivation")
			}
			if strings.HasPrefix(scenario, "foreign-") {
				if _, err := backend.Plan(home, request); !errors.Is(err, ErrOwnedUnitCollision) {
					t.Fatalf("foreign omitted file preview: %v", err)
				}
				if err := backend.Apply(ctx, home, request); !errors.Is(err, ErrOwnedUnitCollision) {
					t.Fatalf("foreign omitted file apply: %v", err)
				}
				raw, err := os.ReadFile(foreignPath)
				if err != nil || string(raw) != string(foreign) {
					t.Fatalf("foreign omitted file changed: %v", err)
				}
			}
		})
	}
}

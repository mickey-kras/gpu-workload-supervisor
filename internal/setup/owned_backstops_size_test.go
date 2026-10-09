package setup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

func TestMaximumOwnedBackstopCatalogRoundTrips(t *testing.T) {
	_, home, request := fixture(t)
	request.Catalog = control.Catalog{Version: 2}
	for i := 0; i < 32; i++ {
		id := fmt.Sprintf("p%02d", i) + strings.Repeat("x", 61)
		draft := ownedDraft(id, uint16(9000+i))
		draft.App = "ollama"
		draft.Model = "model"
		draft.Binding.Instance = id
		draft.Binding.Owned = &DraftOwnedLaunch{Port: uint16(9000 + i)}
		p, _, err := OwnedProfile(draft, "/user.slice/user-1000.slice/user@1000.service", home)
		if err != nil {
			t.Fatal(err)
		}
		request.Catalog.Profiles = append(request.Catalog.Profiles, p)
	}
	// The longest owned unit names are per-instance Ollama names. Fill the
	// input budget through a clean placement prefix, retaining the owned suffix.
	p := &request.Catalog.Profiles[0]
	base, err := json.Marshal(request.Catalog)
	if err != nil {
		t.Fatal(err)
	}
	p.Cgroup = "/" + strings.Repeat("x", control.MaxCatalogInputBytes-len(base)) + strings.TrimPrefix(p.Cgroup, "/")
	base, err = json.Marshal(request.Catalog)
	if err != nil || len(base) != control.MaxCatalogInputBytes {
		t.Fatalf("maximum input fixture size=%d: %v", len(base), err)
	}
	normalized, err := prepareOwnedBackstops(request)
	if err != nil || Validate(normalized) != nil {
		t.Fatalf("maximum peer graph invalid: %v", err)
	}
	raw, err := json.Marshal(normalized.Catalog)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) <= control.MaxCatalogInputBytes || len(raw)-len(base) > control.MaxOwnedConflictBytes {
		t.Fatal("fixture does not exercise the derived allowance", len(raw))
	}
	for _, p := range normalized.Catalog.Profiles {
		if len(p.Unit) != 89 || len(strings.Fields(p.NativeModel.Owned.Conflicts)) != 31 {
			t.Fatal("fixture does not have the maximum peer count and unit name length")
		}
	}
	fromFile, err := control.DecodeCatalog(strings.NewReader(string(raw)))
	if err != nil || !reflect.DeepEqual(fromFile, normalized.Catalog) {
		t.Fatalf("file readback: %v", err)
	}
	fromBytes, err := control.DecodeCatalogBytes(raw)
	if err != nil || !reflect.DeepEqual(fromBytes, normalized.Catalog) {
		t.Fatalf("database decode: %v", err)
	}
	requestRaw, _ := json.Marshal(normalized)
	fromRequest, err := Decode(strings.NewReader(string(requestRaw)))
	if err != nil || !reflect.DeepEqual(fromRequest, normalized) {
		t.Fatalf("setup decode: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(request.Profile.StatePath), 0700); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(context.Background(), request.Profile.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := s.ReplaceCatalog(context.Background(), "", normalized.Catalog)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	accepted, err := ReadCatalog(context.Background(), request.Profile.StatePath)
	if err != nil || !reflect.DeepEqual(accepted, snapshot) {
		t.Fatalf("accepted setup readback: %v", err)
	}
	// An oversized struct must fail both storage and setup before any effects.
	oversized := normalized.Catalog.Clone()
	oversized.Profiles[0].Cgroup = "/x" + strings.TrimPrefix(oversized.Profiles[0].Cgroup, "/")
	s, err = store.Open(context.Background(), request.Profile.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReplaceCatalog(context.Background(), snapshot.Revision, oversized); !errors.Is(err, control.ErrCatalogTooLarge) {
		t.Fatalf("oversized catalog stored: %v", err)
	}
	unchanged, err := s.Catalog(context.Background())
	s.Close()
	if err != nil || !reflect.DeepEqual(unchanged, snapshot) {
		t.Fatalf("size rejection changed accepted state: %v", err)
	}
	// Only a valid catalog-derived graph may use the reserved allowance.
	for _, peers := range []string{"gws-owned-foreign.service", "not a unit"} {
		forged := normalized.Catalog.Clone()
		forged.Profiles[0].NativeModel.Owned.Conflicts = peers
		if err := forged.Validate(); err == nil {
			t.Fatal("forged conflicts validated", peers)
		}
		encoded, err := json.Marshal(forged)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := control.DecodeCatalogBytes(encoded); err == nil {
			t.Fatal("forged conflicts decoded", peers)
		}
	}
}

func TestDerivedOwnedSetupRequestSizeFailsBeforeEffects(t *testing.T) {
	backend, home, request := fixture(t)
	a, _ := ownedFixtureProfile(t, home, "a", 9100)
	z, _ := ownedFixtureProfile(t, home, "z", 9200)
	z.NativeModel.Instance = "z"
	request.Catalog = control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{a, z}}
	// A valid-size incoming request can cross the same setup wire boundary after
	// deriving its peer graph. This path only inspects serialized sizes.
	raw, _ := json.Marshal(request)
	request.Profile.SystemctlPath = "/" + strings.Repeat("x", maxSetupRequestBytes-len(raw)-50)
	if err := Validate(request); err != nil {
		t.Fatalf("incoming request should fit: %v", err)
	}
	calls := 0
	backend.makeRuntime = func(Request) (gpuruntime.Manager, error) { calls++; return idleRuntime{}, nil }
	backend.runCommand = func(context.Context, string, ...string) ([]byte, error) { calls++; return nil, nil }
	if _, err := backend.Plan(home, request); err == nil || !strings.Contains(err.Error(), "setup request exceeds") {
		t.Fatalf("oversized derived preview: %v", err)
	}
	if err := backend.Apply(context.Background(), home, request); err == nil || !strings.Contains(err.Error(), "setup request exceeds") {
		t.Fatalf("oversized derived apply: %v", err)
	}
	if calls != 0 {
		t.Fatal("size rejection reached host effects", calls)
	}
	if _, err := os.Stat(filepath.Join(home, ".config/gpu-workload-supervisor")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("size rejection wrote activation files", err)
	}
}

func TestNearLimitLegacyOwnedCatalogUpgradesOnlyOnApply(t *testing.T) {
	ctx := context.Background()
	backend, home, request := fixture(t)
	backend.runCommand = fakeOwnedCommandFor(home)
	external := request.Catalog.Profiles[0]
	request.Catalog = control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{external}}
	legacyUnits := map[string][]byte{}
	for i := 0; i < 31; i++ {
		id := fmt.Sprintf("p%02d", i) + strings.Repeat("x", 61)
		p, raw := ownedFixtureProfile(t, home, id, uint16(9000+i))
		p.NativeModel.Instance = fmt.Sprintf("p%d", i)
		request.Catalog.Profiles = append(request.Catalog.Profiles, p)
		legacyUnits[p.Unit] = raw
	}
	original, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	// Leave room for the accepted revision while staying within the old setup
	// wire cap. The external placement path is valid and was never length-capped.
	request.Catalog.Profiles[0].Cgroup += strings.Repeat("x", control.MaxCatalogInputBytes-len(original)-512)
	s := openStoreAt(t, request.Profile.StatePath)
	prior, err := s.ReplaceCatalog(ctx, "", request.Catalog)
	if err != nil {
		t.Fatal(err)
	}
	if err := initializeIdleState(ctx, s); err != nil {
		t.Fatal(err)
	}
	s.Close()
	request.ExpectedRevision = prior.Revision
	original, err = json.Marshal(request)
	if err != nil || len(original) >= control.MaxCatalogInputBytes {
		t.Fatalf("legacy request no longer fits old wire cap: size=%d %v", len(original), err)
	}
	decoded, err := Decode(strings.NewReader(string(original)))
	if err != nil || !reflect.DeepEqual(decoded, request) {
		t.Fatalf("legacy decode changed the original request: %v", err)
	}
	root := filepath.Join(home, ".config/gpu-workload-supervisor")
	for _, dir := range []string{root, ownedUnitDirectory(home)} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeJSON(filepath.Join(root, "catalog.json"), prior.Catalog); err != nil {
		t.Fatal(err)
	}
	legacyFile, err := os.ReadFile(filepath.Join(root, "catalog.json"))
	if err != nil {
		t.Fatal(err)
	}
	for unit, raw := range legacyUnits {
		if err := os.WriteFile(filepath.Join(ownedUnitDirectory(home), unit), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	preview, err := backend.Plan(home, decoded)
	if err != nil {
		t.Fatalf("near-limit legacy preview: %v", err)
	}
	derived, err := json.Marshal(preview.Catalog)
	if err != nil || len(derived) <= control.MaxCatalogInputBytes || len(derived) > control.MaxCatalogBytes {
		t.Fatalf("legacy fixture does not cross the old cap after derivation: size=%d %v", len(derived), err)
	}
	unchanged, err := ReadCatalog(ctx, request.Profile.StatePath)
	if err != nil || !reflect.DeepEqual(unchanged, prior) {
		t.Fatalf("preview changed accepted database catalog: %v", err)
	}
	afterPreview, err := os.ReadFile(filepath.Join(root, "catalog.json"))
	if err != nil || string(afterPreview) != string(legacyFile) {
		t.Fatalf("preview changed accepted catalog file: %v", err)
	}
	for unit, want := range legacyUnits {
		raw, err := os.ReadFile(filepath.Join(ownedUnitDirectory(home), unit))
		if err != nil || string(raw) != string(want) {
			t.Fatalf("preview rewrote legacy unit %s: %v", unit, err)
		}
	}
	if err := backend.Apply(ctx, home, decoded); err != nil {
		t.Fatalf("near-limit legacy apply: %v", err)
	}
	accepted, err := ReadCatalog(ctx, request.Profile.StatePath)
	if err != nil || !reflect.DeepEqual(accepted.Catalog, preview.Catalog) {
		t.Fatalf("derived database readback: %v", err)
	}
	file, err := os.ReadFile(filepath.Join(root, "catalog.json"))
	if err != nil {
		t.Fatal(err)
	}
	fromFile, err := control.DecodeCatalog(strings.NewReader(string(file)))
	if err != nil || !reflect.DeepEqual(fromFile, accepted.Catalog) {
		t.Fatalf("derived catalog file readback: %v", err)
	}
	s = openStoreAt(t, request.Profile.StatePath)
	for revision, want := range map[string]control.CatalogSnapshot{prior.Revision: prior, accepted.Revision: accepted} {
		historical, err := s.CatalogAtRevision(ctx, revision)
		if err != nil || !reflect.DeepEqual(historical, want) {
			t.Fatalf("catalog history readback at %s: %v", revision, err)
		}
	}
	s.Close()
	if err := backend.Reconcile(ctx, home); err != nil {
		t.Fatal(err)
	}
	unchanged, err = ReadCatalog(ctx, request.Profile.StatePath)
	if err != nil || !reflect.DeepEqual(unchanged, accepted) {
		t.Fatalf("reconciliation changed accepted database catalog: %v", err)
	}
	afterReconcile, err := os.ReadFile(filepath.Join(root, "catalog.json"))
	if err != nil || string(afterReconcile) != string(file) {
		t.Fatalf("reconciliation rewrote catalog file: %v", err)
	}
	for _, p := range accepted.Catalog.Profiles[1:] {
		want, err := gpuruntime.RenderOwnedUnit(p)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(filepath.Join(ownedUnitDirectory(home), p.Unit))
		if err != nil || string(raw) != string(want) {
			t.Fatalf("derived unit %s changed after reconciliation: %v", p.Unit, err)
		}
	}
}

func TestCatalogInputBudgetAndMalformedConflictsFailBeforeEffects(t *testing.T) {
	for _, scenario := range []string{"input-budget", "malformed-conflicts"} {
		t.Run(scenario, func(t *testing.T) {
			backend, home, request := fixture(t)
			wantErr := control.ErrCatalogTooLarge
			if scenario == "input-budget" {
				raw, err := json.Marshal(request.Catalog)
				if err != nil {
					t.Fatal(err)
				}
				request.Catalog.Profiles[0].Cgroup += strings.Repeat("x", control.MaxCatalogInputBytes-len(raw)+1)
			} else {
				wantErr = gpuruntime.ErrOwnedRender
				p, _ := ownedFixtureProfile(t, home, "a", 9100)
				p.NativeModel.Owned.Conflicts = "not a unit"
				request.Catalog = control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{p}}
			}
			encoded, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Decode(strings.NewReader(string(encoded))); !errors.Is(err, wantErr) {
				t.Fatalf("invalid input decode: %v", err)
			}
			calls := 0
			backend.makeRuntime = func(Request) (gpuruntime.Manager, error) { calls++; return idleRuntime{}, nil }
			backend.runCommand = func(context.Context, string, ...string) ([]byte, error) { calls++; return nil, nil }
			if _, err := backend.Plan(home, request); !errors.Is(err, wantErr) {
				t.Fatalf("invalid preview: %v", err)
			}
			if err := backend.Apply(context.Background(), home, request); !errors.Is(err, wantErr) {
				t.Fatalf("invalid apply: %v", err)
			}
			if calls != 0 {
				t.Fatal("invalid input reached host effects", calls)
			}
			if _, err := os.Stat(filepath.Join(home, ".config/gpu-workload-supervisor")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("invalid input wrote activation files", err)
			}
		})
	}
}

func TestMaximumCatalogFitsSetupEnvelope(t *testing.T) {
	backend, home, request := fixture(t)
	// Placement paths have no smaller serialized-size bound. Pad a valid catalog
	// exactly to its cap while keeping the setup profile/envelope ordinary.
	raw, err := json.Marshal(request.Catalog)
	if err != nil {
		t.Fatal(err)
	}
	request.Catalog.Profiles[0].Cgroup += strings.Repeat("x", control.MaxCatalogInputBytes-len(raw))
	raw, err = json.Marshal(request.Catalog)
	if err != nil || len(raw) != control.MaxCatalogInputBytes {
		t.Fatalf("catalog fixture size=%d: %v", len(raw), err)
	}
	if err := request.Catalog.Validate(); err != nil {
		t.Fatal(err)
	}
	s := openStoreAt(t, request.Profile.StatePath)
	accepted, err := s.ReplaceCatalog(context.Background(), "", request.Catalog)
	if err != nil {
		t.Fatal(err)
	}
	if err := initializeIdleState(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	s.Close()
	request.ExpectedRevision = accepted.Revision
	requestRaw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if len(requestRaw) <= control.MaxCatalogInputBytes || len(requestRaw) > maxSetupRequestBytes {
		t.Fatal("fixture does not exercise the envelope allowance", len(requestRaw))
	}
	if err := Validate(request); err != nil {
		t.Fatalf("maximum catalog plus envelope rejected: %v", err)
	}
	decoded, err := Decode(strings.NewReader(string(requestRaw)))
	if err != nil || !reflect.DeepEqual(decoded, request) {
		t.Fatalf("maximum catalog plus envelope decode: %v", err)
	}
	preview, err := backend.Plan(home, decoded)
	if err != nil || !reflect.DeepEqual(preview.Catalog, request.Catalog) {
		t.Fatalf("maximum stored catalog preview: %v", err)
	}
	if err := backend.Apply(context.Background(), home, decoded); err != nil {
		t.Fatalf("maximum stored catalog apply: %v", err)
	}
	after, err := ReadCatalog(context.Background(), request.Profile.StatePath)
	if err != nil || !reflect.DeepEqual(after, accepted) {
		t.Fatalf("maximum catalog changed on reapply: %v", err)
	}
}

func TestSetupWholeRequestSizeLimit(t *testing.T) {
	backend, home, request := fixture(t)
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	request.Profile.SystemctlPath += strings.Repeat("x", maxSetupRequestBytes-len(raw)+1)
	raw, err = json.Marshal(request)
	if err != nil || len(raw) != maxSetupRequestBytes+1 {
		t.Fatalf("whole-request fixture size=%d: %v", len(raw), err)
	}
	if err := Validate(request); err == nil {
		t.Fatal("oversized whole request validated")
	}
	if _, err := Decode(strings.NewReader(string(raw))); err == nil {
		t.Fatal("oversized whole request decoded")
	}
	calls := 0
	backend.makeRuntime = func(Request) (gpuruntime.Manager, error) { calls++; return idleRuntime{}, nil }
	backend.runCommand = func(context.Context, string, ...string) ([]byte, error) { calls++; return nil, nil }
	if _, err := backend.Plan(home, request); err == nil {
		t.Fatal("oversized whole request previewed")
	}
	if err := backend.Apply(context.Background(), home, request); err == nil {
		t.Fatal("oversized whole request applied")
	}
	if calls != 0 {
		t.Fatal("whole-request size rejection reached host effects", calls)
	}
	if _, err := os.Stat(filepath.Join(home, ".config/gpu-workload-supervisor")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("whole-request size rejection wrote activation files", err)
	}
}

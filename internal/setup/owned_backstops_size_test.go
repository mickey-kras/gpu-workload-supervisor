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
		p, _ := ownedFixtureProfile(t, home, id, uint16(9000+i))
		p.NativeModel.Instance = fmt.Sprintf("p%d", i)
		request.Catalog.Profiles = append(request.Catalog.Profiles, p)
	}
	normalized, err := prepareOwnedBackstops(request)
	if err != nil || Validate(normalized) != nil {
		t.Fatalf("maximum peer graph invalid: %v", err)
	}
	raw, err := json.Marshal(normalized.Catalog)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) <= 64*1024 {
		t.Fatal("fixture no longer exercises legacy decoder cap", len(raw))
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
	oversized.Profiles[0].NativeModel.LaunchFile = "/" + strings.Repeat("x", control.MaxCatalogBytes)
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

func TestMaximumCatalogFitsSetupEnvelope(t *testing.T) {
	backend, home, request := fixture(t)
	// Placement paths have no smaller serialized-size bound. Pad a valid catalog
	// exactly to its cap while keeping the setup profile/envelope ordinary.
	raw, err := json.Marshal(request.Catalog)
	if err != nil {
		t.Fatal(err)
	}
	request.Catalog.Profiles[0].Cgroup += strings.Repeat("x", control.MaxCatalogBytes-len(raw))
	raw, err = json.Marshal(request.Catalog)
	if err != nil || len(raw) != control.MaxCatalogBytes {
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
	if len(requestRaw) <= control.MaxCatalogBytes || len(requestRaw) > maxSetupRequestBytes {
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

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

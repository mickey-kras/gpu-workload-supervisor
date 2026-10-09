package setup

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

func TestSetupOwnedBackstopsUpgradeRollbackAndPeerRemoval(t *testing.T) {
	backend, home, request := fixture(t)
	backend.runCommand = func(context.Context, string, ...string) ([]byte, error) { return []byte("NeedDaemonReload=no"), nil }
	a, oldA := ownedFixtureProfile(t, home, "a", 9100)
	z, oldZ := ownedFixtureProfile(t, home, "z", 9200)
	z.NativeModel.Instance = "z"
	request.Catalog = control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{a, z}}
	// Install an already accepted pre-backstop catalog without rewriting its proof.
	dir := ownedUnitDirectory(home)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for unit, raw := range map[string][]byte{a.Unit: oldA, z.Unit: oldZ} {
		if err := os.WriteFile(filepath.Join(dir, unit), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(request.Profile.StatePath), 0700); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(context.Background(), request.Profile.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := s.ReplaceCatalog(context.Background(), "", request.Catalog)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	request.ExpectedRevision = accepted.Revision
	preview, err := backend.Plan(home, request)
	if err != nil {
		t.Fatal(err)
	}
	if a.NativeModel.Owned.Conflicts != "" || a.NativeModel.LaunchSHA256 != digest(oldA) {
		t.Fatal("preview mutated accepted profile")
	}
	if len(preview.Changes) == 0 || !reflect.DeepEqual(preview.Catalog.Profiles[0].NativeModel.Owned.Conflicts, z.Unit) {
		t.Fatal("upgrade not reviewed", preview)
	}
	for _, change := range preview.Changes {
		if strings.HasPrefix(change, "Remove supervisor-owned unit") {
			t.Fatal("upgrade reported removal", change)
		}
	}
	candidate := request
	candidate.Catalog = preview.Catalog
	plan, err := backend.planOwnedUnits(candidate, accepted, home)
	if err != nil {
		t.Fatal(err)
	}
	journal := newOwnedUnitJournal(plan, request.Profile.StatePath)
	if err := backend.applyOwnedUnitWrites(context.Background(), home, plan, journal); err != nil {
		t.Fatal(err)
	}
	if err := backend.rollbackOwnedUnitWrites(context.Background(), home, request.Profile.SystemctlPath, plan); err != nil {
		t.Fatal(err)
	}
	for unit, want := range map[string][]byte{a.Unit: oldA, z.Unit: oldZ} {
		raw, err := os.ReadFile(filepath.Join(dir, unit))
		if err != nil || string(raw) != string(want) {
			t.Fatal("legacy rollback failed", unit, err)
		}
	}
	// A carried profile can remove or adopt its peer: dependencies are recomputed.
	candidate.Catalog.Profiles = candidate.Catalog.Profiles[:1]
	normalized, err := prepareOwnedBackstops(candidate)
	if err != nil || normalized.Catalog.Validate() != nil || normalized.Catalog.Profiles[0].NativeModel.Owned.Conflicts != "" || normalized.Catalog.Profiles[0].NativeModel.LaunchSHA256 != digest(oldA) {
		t.Fatal("removed peer retained", err)
	}
	candidate.Catalog = control.Catalog{Version: 2, Disabled: true}
	normalized, err = prepareOwnedBackstops(candidate)
	if err != nil || normalized.Catalog.Validate() != nil {
		t.Fatal("disabled catalog", err)
	}
	request.Catalog.Profiles[0].NativeModel.LaunchSHA256 = strings.Repeat("a", 64)
	if _, err := backend.Plan(home, request); err == nil {
		t.Fatal("normalization laundered invalid incoming proof")
	}
}

func TestSetupOwnedBackstopsSharedAndExternalUnits(t *testing.T) {
	_, home, request := fixture(t)
	draft := ownedDraft("chat", 11434)
	draft.App = "ollama"
	draft.Model = "model-a"
	draft.Binding.Owned = &DraftOwnedLaunch{Port: 11434}
	chat, _, err := OwnedProfile(draft, "/user.slice/user-1000.slice/user@1000.service", home)
	if err != nil {
		t.Fatal(err)
	}
	draft.ID = "code"
	draft.Model = "model-b"
	code, _, err := OwnedProfile(draft, "/user.slice/user-1000.slice/user@1000.service", home)
	if err != nil {
		t.Fatal(err)
	}
	media, _ := ownedFixtureProfile(t, home, "media", 9100)
	media.NativeModel.Instance = "media"
	external := request.Catalog.Profiles[0]
	request.Catalog = control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{chat, external, media, code}}
	before, _ := json.Marshal(external)
	raw, _ := json.Marshal(request)
	decoded, err := Decode(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, request) {
		t.Fatal("decode changed original request")
	}
	decoded, err = prepareOwnedBackstops(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Catalog.Profiles[0].NativeModel.Owned.Conflicts != media.Unit || decoded.Catalog.Profiles[3].NativeModel.Owned.Conflicts != media.Unit || decoded.Catalog.Profiles[2].NativeModel.Owned.Conflicts != chat.Unit {
		t.Fatal("shared/foreign dependencies", decoded.Catalog)
	}
	after, _ := json.Marshal(decoded.Catalog.Profiles[1])
	if string(before) != string(after) {
		t.Fatal("external profile changed")
	}
	mixed := decoded.Catalog.Clone()
	mixed.Profiles[3].NativeModel.Owned.Conflicts = ""
	if mixed.Validate() == nil {
		t.Fatal("partial backstop generation accepted")
	}
	// A native adopted peer is excluded even when its filename has the owned prefix.
	adoptedRequest := decoded
	adoptedRequest.Catalog = decoded.Catalog.Clone()
	adoptedRequest.Catalog.Profiles[2].NativeModel.Owned = nil
	adoptedBefore, _ := json.Marshal(adoptedRequest.Catalog.Profiles[2])
	adoptedRequest, err = prepareOwnedBackstops(adoptedRequest)
	if err != nil || adoptedRequest.Catalog.Validate() != nil {
		t.Fatal("adopted peer", err)
	}
	adoptedAfter, _ := json.Marshal(adoptedRequest.Catalog.Profiles[2])
	if string(adoptedBefore) != string(adoptedAfter) || adoptedRequest.Catalog.Profiles[0].NativeModel.Owned.Conflicts != "" {
		t.Fatal("adopted peer changed or kept dependency")
	}
	wrong := decoded.Catalog.Clone()
	wrong.Profiles[2].NativeModel.Owned.Conflicts = external.Unit
	if wrong.Validate() == nil {
		t.Fatal("external dependency accepted")
	}
}

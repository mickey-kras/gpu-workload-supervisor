package setup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
)

// bindingReleaseRuntime models the binding-sensitive release path: like the
// systemd manager, ReleasedFor rechecks launch fingerprints for UUID-bound
// profiles. It reads the actual files written by Apply rather than accepting
// every request as idleRuntime does.
type bindingReleaseRuntime struct {
	idleRuntime
	catalog    control.Catalog
	busy       control.Workload
	bindingErr error
	releases   *[]control.Catalog
}

func (r bindingReleaseRuntime) verifyBindings(uuidOnly bool) error {
	if r.bindingErr != nil {
		return r.bindingErr
	}
	for _, p := range r.catalog.Profiles {
		n := p.NativeModel
		if n == nil || (uuidOnly && n.GPUUUID == "") {
			continue
		}
		raw, err := os.ReadFile(n.LaunchFile)
		if err != nil || digest(raw) != n.LaunchSHA256 {
			return gpuruntime.ErrLaunchChanged
		}
	}
	return nil
}

func (r bindingReleaseRuntime) Preflight(context.Context) error {
	return r.verifyBindings(false)
}

func (r bindingReleaseRuntime) PreflightWithOwnedRemovals(context.Context, map[string]string) error {
	return r.verifyBindings(false)
}

func (r bindingReleaseRuntime) ReleasedFor(context.Context, control.Workload) error {
	if err := r.verifyBindings(true); err != nil {
		return err
	}
	if r.releases != nil {
		*r.releases = append(*r.releases, r.catalog)
	}
	if _, ok := r.catalog.Profile(r.busy); ok {
		return errors.New("accepted workload is busy")
	}
	return nil
}

func uuidOwnedFixture(t *testing.T, home, id string, port uint16) control.WorkloadProfile {
	t.Helper()
	p, _ := ownedFixtureProfile(t, home, id, port)
	p.NativeModel.Instance = id
	p.NativeModel.GPUUUID = "GPU-01234567-89ab-cdef-0123-456789abcdef"
	return p
}

func TestApplyVerifiesAcceptedUUIDBindingsBeforeOwnedReplacement(t *testing.T) {
	for _, change := range []string{"same-name edit", "peer addition", "peer removal"} {
		t.Run(change, func(t *testing.T) {
			backend, home, request := fixture(t)
			backend.runCommand = fakeOwnedCommandFor(home)
			a := uuidOwnedFixture(t, home, "a", 9100)
			z := uuidOwnedFixture(t, home, "z", 9200)
			request.Catalog = control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{a}}
			if change == "peer removal" {
				request.Catalog.Profiles = append(request.Catalog.Profiles, z)
			}
			var releases []control.Catalog
			backend.makeRuntime = func(got Request) (gpuruntime.Manager, error) {
				return bindingReleaseRuntime{catalog: got.Catalog, releases: &releases}, nil
			}
			if err := backend.Apply(t.Context(), home, request); err != nil {
				t.Fatal(err)
			}
			accepted, err := ReadCatalog(t.Context(), request.Profile.StatePath)
			if err != nil {
				t.Fatal(err)
			}
			request.ExpectedRevision = accepted.Revision
			switch change {
			case "same-name edit":
				request.Catalog.Profiles = []control.WorkloadProfile{uuidOwnedFixture(t, home, "a", 9101)}
			case "peer addition":
				request.Catalog.Profiles = []control.WorkloadProfile{a, z}
			case "peer removal":
				request.Catalog.Profiles = []control.WorkloadProfile{a}
			}
			releases = nil
			if err := backend.Apply(t.Context(), home, request); err != nil {
				t.Fatalf("UUID-bound %s failed: %v", change, err)
			}
			updated, err := ReadCatalog(t.Context(), request.Profile.StatePath)
			if err != nil {
				t.Fatal(err)
			}
			if len(releases) < 3 || !reflect.DeepEqual(releases[0], accepted.Catalog) {
				t.Fatalf("accepted release not proved before candidate checks: %+v", releases)
			}
			for _, checked := range releases[1:] {
				if !reflect.DeepEqual(checked, updated.Catalog) {
					t.Fatal("post-write release used stale accepted fingerprints")
				}
			}
			if accepted.Catalog.Profiles[0].NativeModel.LaunchSHA256 == updated.Catalog.Profiles[0].NativeModel.LaunchSHA256 {
				t.Fatal("test did not replace same-name unit content")
			}
			if change == "peer removal" {
				if _, err := os.Stat(z.NativeModel.LaunchFile); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("removed peer unit survived: %v", err)
				}
			}
		})
	}
}

func TestApplyRejectsUnsafeAcceptedRuntimeBeforeOwnedMutation(t *testing.T) {
	for _, failure := range []string{"busy retained workload", "busy removed workload", "loaded binding changed", "tampered unit"} {
		t.Run(failure, func(t *testing.T) {
			backend, home, request := fixture(t)
			backend.runCommand = fakeOwnedCommandFor(home)
			a := uuidOwnedFixture(t, home, "a", 9100)
			z := uuidOwnedFixture(t, home, "z", 9200)
			request.Catalog = control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{a, z}}
			if err := backend.Apply(t.Context(), home, request); err != nil {
				t.Fatal(err)
			}
			request.ExpectedRevision = currentRevision(t, request)
			request.Catalog.Profiles = []control.WorkloadProfile{uuidOwnedFixture(t, home, "a", 9101)}
			busy := control.Workload("")
			if failure == "busy retained workload" {
				busy = a.ID
			} else if failure == "busy removed workload" {
				busy = z.ID
			}
			var bindingErr error
			if failure == "loaded binding changed" {
				bindingErr = gpuruntime.ErrLaunchChanged
			}
			if failure == "tampered unit" {
				if err := os.WriteFile(a.NativeModel.LaunchFile, []byte("foreign unit"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(a.NativeModel.LaunchFile)
			if err != nil {
				t.Fatal(err)
			}
			backend.makeRuntime = func(got Request) (gpuruntime.Manager, error) {
				return bindingReleaseRuntime{catalog: got.Catalog, busy: busy, bindingErr: bindingErr}, nil
			}
			writes := 0
			restore := ownedAtomicWrite
			ownedAtomicWrite = func(path string, raw []byte) error {
				writes++
				return restore(path, raw)
			}
			defer func() { ownedAtomicWrite = restore }()
			commands := 0
			backend.runCommand = func(context.Context, string, ...string) ([]byte, error) {
				commands++
				return nil, errors.New("unexpected mutation command")
			}
			if err := backend.Apply(t.Context(), home, request); err == nil {
				t.Fatal("unsafe accepted runtime applied")
			}
			if writes != 0 || commands != 0 {
				t.Fatalf("accepted failure followed mutation: writes=%d commands=%d", writes, commands)
			}
			after, err := os.ReadFile(a.NativeModel.LaunchFile)
			if err != nil || string(after) != string(before) {
				t.Fatalf("accepted unit changed: %v", err)
			}
			if currentRevision(t, request) != request.ExpectedRevision {
				t.Fatal("rejected activation changed committed catalog")
			}
			if _, present, err := readOwnedUnitJournal(filepath.Join(home, ".config/gpu-workload-supervisor")); err != nil || present {
				t.Fatalf("rejected activation created journal: %v", err)
			}
		})
	}
}

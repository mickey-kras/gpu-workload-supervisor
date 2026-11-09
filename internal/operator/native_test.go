package operator

import (
	"context"
	"errors"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/deployment"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
	"os"
	"path/filepath"
	"testing"
)

// useProfile makes the reread of the profile under the state lock give p
// and err.
func useProfile(t *testing.T, p Profile, err error) {
	t.Helper()
	old := reloadProfile
	t.Cleanup(func() { reloadProfile = old })
	reloadProfile = func() (Profile, error) { return p, err }
}

func TestNativeActivationBeforeStateOpen(t *testing.T) {
	p := Profile{Version: 1, StatePath: filepath.Join(t.TempDir(), "state.db"), ActivatedRelease: "other", SystemctlPath: "/usr/bin/true", NvidiaSMIPath: "/usr/bin/true"}
	useProfile(t, p, nil)
	s := NativeService(p)
	if r := s.Handle(Request{Action: "status"}); r.Code != IncompatibleConfiguration {
		t.Fatal(r)
	}
	if _, e := os.Stat(p.StatePath); !os.IsNotExist(e) {
		t.Fatal("opened incompatible state", e)
	}
	p.ActivatedRelease = deployment.Release
	if e := deployment.Write(p.StatePath, deployment.Marker{Version: 1, Release: deployment.Release}); e != nil {
		t.Fatal(e)
	}
	s = NativeService(p)
	if r := s.Handle(Request{Action: "status"}); r.Code != IncompatibleConfiguration {
		t.Fatal(r)
	}
	if _, err := os.Stat(p.StatePath); !os.IsNotExist(err) {
		t.Fatal("managed status created missing database", err)
	}
	db, e := store.Open(context.Background(), p.StatePath)
	if e != nil {
		t.Fatal(e)
	}
	_, e = db.ReplaceCatalog(context.Background(), "", control.Catalog{Version: 1, Profiles: []control.WorkloadProfile{{ID: "third", Label: "Third", Adapter: "systemd", Unit: "third.service", Cgroup: "/user/third", HealthURL: "http://localhost:9999"}}})
	if e != nil {
		t.Fatal(e)
	}
	db.Close()
	session, e := s.Open(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	defer session.Close()
	if session.Revision == "" || len(session.Workloads) != 2 {
		t.Fatal(session)
	}
}

// A session reads the profile again under the state lock and uses its
// limit reserve, so a reserve that setup changed after the first read
// reaches the runtime with the new catalog. A profile that cannot be read
// again, or one for another state path, refuses the session.
func TestNativeSessionRereadsTheReserve(t *testing.T) {
	p := Profile{Version: 1, StatePath: filepath.Join(t.TempDir(), "state.db"), ActivatedRelease: deployment.Release, SystemctlPath: "/usr/bin/true", NvidiaSMIPath: "/usr/bin/true"}
	if e := deployment.Write(p.StatePath, deployment.Marker{Version: 1, Release: deployment.Release}); e != nil {
		t.Fatal(e)
	}
	db, e := store.Open(context.Background(), p.StatePath)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = db.ReplaceCatalog(context.Background(), "", control.Catalog{Version: 1, Profiles: []control.WorkloadProfile{{ID: "third", Label: "Third", Adapter: "systemd", Unit: "third.service", Cgroup: "/user/third", HealthURL: "http://localhost:9999"}}}); e != nil {
		t.Fatal(e)
	}
	db.Close()
	var got *gpuruntime.LimitReserve
	old := newNativeManager
	t.Cleanup(func() { newNativeManager = old })
	newNativeManager = func(c gpuruntime.SystemdConfig) (*gpuruntime.SystemdManager, error) {
		got = c.LimitReserve
		return old(c)
	}
	updated := p
	updated.LimitReserve = &gpuruntime.LimitReserve{MemoryMiB: 4096, Tasks: 1024, CPUPercent: 200, IOPercent: 20}
	useProfile(t, updated, nil)
	session, e := NativeService(p).Open(context.Background())
	if e != nil || got != updated.LimitReserve {
		t.Fatalf("session reserve = %+v, %v", got, e)
	}
	session.Close()
	other := updated
	other.StatePath = "/other/state.db"
	for name, reload := range map[string]func(){
		"unreadable":       func() { useProfile(t, Profile{}, errors.New("untrusted")) },
		"other state path": func() { useProfile(t, other, nil) },
	} {
		reload()
		if _, e := NativeService(p).Open(context.Background()); !errors.Is(e, ErrIncompatibleConfiguration) {
			t.Errorf("%s: %v", name, e)
		}
	}
}

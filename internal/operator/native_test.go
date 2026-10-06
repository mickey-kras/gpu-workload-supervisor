package operator

import (
	"context"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/deployment"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
	"os"
	"path/filepath"
	"testing"
)

func TestNativeActivationBeforeStateOpen(t *testing.T) {
	p := Profile{Version: 1, StatePath: filepath.Join(t.TempDir(), "state.db"), ActivatedRelease: "other", SystemctlPath: "/usr/bin/true", NvidiaSMIPath: "/usr/bin/true"}
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

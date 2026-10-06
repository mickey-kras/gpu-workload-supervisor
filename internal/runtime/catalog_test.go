package runtime

import (
	"context"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"net/http"
	"testing"
)

func TestCatalogObservesThirdRuntime(t *testing.T) {
	cfg := testConfig()
	c := control.Catalog{Version: 1, Profiles: []control.WorkloadProfile{{ID: "speech", Label: "Speech", Adapter: "systemd", Unit: "speech.service", Cgroup: "/user/speech", HealthURL: "http://localhost:9000"}}}
	cfg.Catalog = &c
	r := &fakeRunner{outputs: map[string][]byte{"/usr/bin/true --user show --property=LoadState --property=ActiveState --property=SubState --property=ControlGroup -- speech.service": []byte("LoadState=loaded\nActiveState=active\nSubState=running\nControlGroup=/user/speech\n")}}
	m, err := newSystemdManager(cfg, r, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := m.Observe(context.Background())
	if err != nil || !snap.Workloads["speech"].Active {
		t.Fatalf("%+v %v", snap, err)
	}
}

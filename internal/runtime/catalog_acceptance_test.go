package runtime

import (
	"context"
	"errors"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func acceptanceCatalog() control.Catalog {
	c := control.Catalog{Version: 1}
	for _, id := range []control.Workload{"text", "media", "speech"} {
		c.Profiles = append(c.Profiles, control.WorkloadProfile{ID: id, Label: string(id), Adapter: "systemd", Unit: string(id) + ".service", Cgroup: "/workloads/" + string(id) + ".service", HealthURL: "http://localhost:9000"})
	}
	return c
}
func catalogShow(id string) string {
	return "/usr/bin/true --user show --property=LoadState --property=ActiveState --property=SubState --property=ControlGroup -- " + id + ".service"
}
func acceptanceManager(t *testing.T, c control.Catalog) (*SystemdManager, *fakeRunner) {
	t.Helper()
	r := &fakeRunner{outputs: map[string][]byte{}}
	for _, p := range c.Profiles {
		r.outputs[catalogShow(string(p.ID))] = stoppedOutput()
	}
	cfg := testConfig()
	cfg.Catalog = &c
	m, err := newSystemdManager(cfg, r, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	fixtureCgroups(t, m)
	return m, r
}
func TestCatalogOpposingReleaseAcceptance(t *testing.T) {
	ctx := context.Background()
	for _, opponent := range []string{"text", "media"} {
		t.Run(opponent, func(t *testing.T) {
			m, r := acceptanceManager(t, acceptanceCatalog())
			r.outputs[catalogShow(opponent)] = []byte("LoadState=loaded\nActiveState=active\nSubState=running\nControlGroup=/workloads/" + opponent + ".service\n")
			if err := m.ReleasedFor(ctx, "speech"); err == nil {
				t.Fatal("opposing workload accepted")
			}
			if err := m.Start(ctx, "speech"); err == nil {
				t.Fatal("start skipped opposing release")
			}
			if err := m.Healthy(ctx, "speech"); err == nil {
				t.Fatal("health skipped opposing release")
			}
		})
	}
	m, _ := acceptanceManager(t, acceptanceCatalog())
	if err := m.ReleasedFor(ctx, "speech"); err != nil {
		t.Fatal(err)
	}
	for _, call := range []func() error{func() error { return m.Start(ctx, "unconfigured") }, func() error { return m.Stop(ctx, "unconfigured") }, func() error { return m.Healthy(ctx, "unconfigured") }, func() error { return m.ReleasedFor(ctx, "unconfigured") }} {
		if call() == nil {
			t.Fatal("unconfigured ID accepted")
		}
	}
	if err := m.StopForRecovery(ctx); err != nil {
		t.Fatal(err)
	}
}
func TestCatalogUnloadAndHealthAcceptance(t *testing.T) {
	ctx := context.Background()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method == http.MethodPost && !strings.Contains(r.Header.Get("Content-Type"), "json") {
			t.Error("missing JSON content type")
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()
	c := acceptanceCatalog()
	c.Profiles[1].Adapter = "media-unload"
	c.Profiles[1].ReleaseURL = srv.URL
	c.Profiles[2].HealthURL = srv.URL
	m, r := acceptanceManager(t, c)
	if err := m.Healthy(ctx, "speech"); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal(calls)
	}
	if err := m.Healthy(ctx, control.WorkloadIdle); err != nil {
		t.Fatal(err)
	}
	if err := m.Stop(ctx, "media"); err != nil {
		t.Fatal(err)
	}
	r.outputs[catalogShow("media")] = []byte("LoadState=loaded\nActiveState=active\nSubState=running\nControlGroup=/workloads/media.service\n")
	if err := m.Stop(ctx, "media"); err != nil {
		t.Fatal(err)
	}
	if err := m.ReleasedFor(ctx, "speech"); !errors.Is(err, ErrUnloadUnverified) {
		t.Fatalf("successful unload proved release: %v", err)
	}
	r.outputs[catalogShow("media")] = []byte("LoadState=loaded\nActiveState=activating\nSubState=start\nControlGroup=/workloads/media.service\n")
	if _, err := m.Observe(ctx); err == nil {
		t.Fatal("ambiguous observed state accepted")
	}
	if err := m.Stop(ctx, "media"); err == nil {
		t.Fatal("ambiguous unload accepted")
	}
}
func TestCatalogImmutableRuntimeAcceptance(t *testing.T) {
	c := acceptanceCatalog()
	cfg := testConfig()
	cfg.Catalog = &c
	r := &fakeRunner{outputs: map[string][]byte{catalogShow("text"): stoppedOutput(), catalogShow("media"): stoppedOutput()}}
	m, err := newSystemdManager(cfg, r, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	fixtureCgroups(t, m)
	c.Profiles[2].Unit = "changed.service"
	if err = m.Start(context.Background(), "speech"); err != nil {
		t.Fatal(err)
	}
	if len(r.calls) == 0 || !strings.HasSuffix(r.calls[len(r.calls)-1], "speech.service") {
		t.Fatal(r.calls)
	}
	c.Profiles[0].RequiredMiB = ^uint64(0)
	cfg.CapacityHeadroomMiB = 1
	if _, err = newSystemdManager(cfg, r, http.DefaultClient); err == nil {
		t.Fatal("overflow accepted")
	}
}
func TestCatalogCapacityAndObservationErrorsAcceptance(t *testing.T) {
	ctx := context.Background()
	c := acceptanceCatalog()
	c.Profiles[2].RequiredMiB = 100
	m, r := acceptanceManager(t, c)
	r.outputs[gpuFreeCommand] = []byte("99\n")
	if err := m.Start(ctx, "speech"); !errors.Is(err, ErrCapacity) {
		t.Fatal(err)
	}
	r.outputs[gpuFreeCommand] = []byte("100\n")
	if err := m.Start(ctx, "speech"); err != nil {
		t.Fatal(err)
	}
	if err := m.capacity(ctx, "unconfigured"); !errors.Is(err, ErrCapacity) {
		t.Fatal(err)
	}
	r.errs = map[string]error{catalogShow("text"): errors.New("manager unavailable")}
	if _, err := m.Observe(ctx); err == nil {
		t.Fatal("observation ignored command failure")
	}
	c.Profiles[0].Adapter = "media-unload"
	c.Profiles[0].ReleaseURL = "http://localhost:9000"
	m.config.Catalog = &c
	if err := m.Stop(ctx, "text"); err == nil {
		t.Fatal("unload ignored observation failure")
	}
}
func TestCatalogAnyActiveAcceptance(t *testing.T) {
	for _, s := range []Snapshot{{Workloads: map[control.Workload]WorkloadObservation{"text": {Active: true}}}, {Workloads: map[control.Workload]WorkloadObservation{"media": {Active: true}}}, {Workloads: map[control.Workload]WorkloadObservation{"speech": {Active: true}}}} {
		if !s.AnyActive() {
			t.Fatal("active workload missed")
		}
	}
	if (Snapshot{Workloads: map[control.Workload]WorkloadObservation{"speech": {}}}).AnyActive() {
		t.Fatal("inactive marked active")
	}
}

package runtime

import (
	"context"
	"errors"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"strings"
	"testing"
)

func placementEvidence() (map[string]string, map[string]string, map[string]string) {
	return map[string]string{"Id": "comfy.service", "LoadState": "loaded", "ActiveState": "inactive", "SubState": "dead", "Slice": "app.slice", "ControlGroup": ""}, map[string]string{"Id": "app.slice", "LoadState": "loaded", "ActiveState": "active", "SubState": "active", "ControlGroup": "/manager/app.slice"}, map[string]string{"LoadState": "loaded", "ActiveState": "active", "SubState": "active", "ControlGroup": "/manager"}
}
func TestStoppedPlacementUsesVersionAndAuthoritativeAnchors(t *testing.T) {
	for _, value := range []string{"systemd 252", "systemd 255 (255.1)", "systemd 259"} {
		if _, err := SupportedSystemdPlacementVersion([]byte(value)); err != nil {
			t.Fatal(err)
		}
	}
	for _, value := range []string{"", "wrong 255", "systemd bad", "systemd 999"} {
		if _, err := SupportedSystemdPlacementVersion([]byte(value)); err == nil {
			t.Fatal(value)
		}
	}
	service, slice, manager := placementEvidence()
	group, err := ResolveAutomaticCgroup("comfy.service", service, slice, manager)
	if err != nil || group != "/manager/app.slice/comfy.service" {
		t.Fatalf("%s %v", group, err)
	}
	service["ControlGroup"] = "/manager/custom.service"
	group, err = ResolveAutomaticCgroup("comfy.service", service, slice, manager)
	if err != nil || group != service["ControlGroup"] {
		t.Fatal(group, err)
	}
	for _, tc := range []struct{ which, key, value string }{{"service", "Id", "other.service"}, {"service", "LoadState", "not-found"}, {"service", "ActiveState", "active"}, {"service", "SubState", "failed"}, {"service", "Slice", "custom.slice"}, {"slice", "Id", "custom.slice"}, {"slice", "LoadState", "not-found"}, {"slice", "ActiveState", "inactive"}, {"slice", "SubState", "dead"}, {"slice", "ControlGroup", "/foreign"}, {"manager", "LoadState", "not-found"}, {"manager", "ActiveState", "inactive"}, {"manager", "SubState", "dead"}, {"manager", "ControlGroup", "/"}, {"manager", "ControlGroup", "relative"}, {"manager", "ControlGroup", "/a/../b"}} {
		service, slice, manager := placementEvidence()
		values := service
		if tc.which == "slice" {
			values = slice
		}
		if tc.which == "manager" {
			values = manager
		}
		values[tc.key] = tc.value
		if _, err := ResolveAutomaticCgroup("comfy.service", service, slice, manager); err == nil {
			t.Fatal(tc)
		}
	}
	for _, name := range []string{"template@one.service", "cgroup.foo.service", "cpu.foo.service", "memory.foo.service", "../bad.service", "bpf-firewall.foo.service", "bpf-devices.foo.service", "bpf-foreign.foo.service", "bpf-socket-bind.foo.service", "bpf-restrict-network-interfaces.foo.service", "perf_event.foo.service", "debug.foo.service", "dmem.foo.service"} {
		s, sl, m := placementEvidence()
		s["Id"] = name
		if _, err := ResolveAutomaticCgroup(name, s, sl, m); err == nil {
			t.Fatal(name)
		}
	}
	for _, group := range []string{"/foreign/comfy.service", "/manager/a/../b"} {
		s, sl, m := placementEvidence()
		s["ControlGroup"] = group
		if _, err := ResolveAutomaticCgroup("comfy.service", s, sl, m); err == nil {
			t.Fatal(group)
		}
	}
}
func TestAutomaticPlacementRuntimeRevalidation(t *testing.T) {
	service, slice, manager := placementEvidence()
	r := &fakeRunner{outputs: map[string][]byte{}}
	cmd := func(u string) string {
		return "/usr/bin/true --user show --property=Id,LoadState,ControlGroup,ActiveState,SubState,Slice --no-pager -- " + u
	}
	props := func(v map[string]string) []byte {
		var b strings.Builder
		for k, x := range v {
			b.WriteString(k + "=" + x + "\n")
		}
		return []byte(b.String())
	}
	r.outputs["/usr/bin/true --version"] = []byte("systemd 255")
	r.outputs[cmd("comfy.service")] = props(service)
	r.outputs[cmd("app.slice")] = props(slice)
	r.outputs[cmd("-.slice")] = props(manager)
	m := &SystemdManager{config: SystemdConfig{SystemctlPath: "/usr/bin/true"}, runner: r}
	p := control.WorkloadProfile{Unit: "comfy.service", Cgroup: "/manager/app.slice/comfy.service", SystemdSlice: "app.slice", SystemdVersion: 255}
	if err := m.verifyAutomaticPlacement(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	p.Cgroup = "/manager/wrong"
	if err := m.verifyAutomaticPlacement(context.Background(), p); err == nil {
		t.Fatal("placement drift accepted")
	}
	p.Cgroup = "/manager/app.slice/comfy.service"
	p.SystemdVersion = 252
	if err := m.verifyAutomaticPlacement(context.Background(), p); err == nil {
		t.Fatal("version drift accepted")
	}
	p.SystemdVersion = 255
	for _, key := range []string{"/usr/bin/true --version", cmd("comfy.service"), cmd("-.slice"), cmd("app.slice")} {
		old := r.outputs[key]
		delete(r.outputs, key)
		if err := m.verifyAutomaticPlacement(context.Background(), p); err == nil {
			t.Fatal(key)
		}
		r.outputs[key] = old
	}
	r.errs = map[string]error{}
	for _, key := range []string{"/usr/bin/true --version", cmd("comfy.service"), cmd("-.slice"), cmd("app.slice")} {
		r.errs[key] = errors.New("bus down")
		if err := m.verifyAutomaticPlacement(context.Background(), p); err == nil {
			t.Fatal(key)
		}
		delete(r.errs, key)
	}
	r.outputs["/usr/bin/true --version"] = []byte("systemd 999")
	if err := m.verifyAutomaticPlacement(context.Background(), p); err == nil {
		t.Fatal("unsupported version")
	}
	r.outputs["/usr/bin/true --version"] = []byte("systemd 255")
	r.outputs[cmd("app.slice")] = []byte("Id=other\n")
	if err := m.verifyAutomaticPlacement(context.Background(), p); err == nil {
		t.Fatal("slice drift accepted")
	}
	p.SystemdSlice = ""
	if err := m.verifyAutomaticPlacement(context.Background(), p); err != nil {
		t.Fatal(err)
	}
}

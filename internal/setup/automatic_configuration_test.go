package setup

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
)

func automaticFixture(t *testing.T, app string) (Backend, Draft, map[string]map[string]string) {
	t.Helper()
	command := "/usr/bin/" + app + " serve"
	if app == "comfyui" {
		command = "/usr/bin/python3 /opt/ComfyUI/main.py"
	}
	if app == appLlamaCPP {
		command = "/usr/bin/llama-server -m /models/a.gguf --host 127.0.0.1 --port 8080"
	}
	metadata := map[string]map[string]string{
		app + ".service": {"Id": app + ".service", "LoadState": "loaded", "ExecStart": "{ path=" + strings.Fields(command)[0] + " ; argv[]=" + command + " ; }", "ControlGroup": "", "ActiveState": "inactive", "SubState": "dead", "Slice": "app.slice", "FragmentPath": "/opt/launch/" + app + ".service", "NeedDaemonReload": "no", "DropInPaths": ""},
		"-.slice":        {"LoadState": "loaded", "ActiveState": "active", "SubState": "active", "ControlGroup": "/manager"},
		"app.slice":      {"Id": "app.slice", "LoadState": "loaded", "ActiveState": "active", "SubState": "active", "ControlGroup": "/manager/app.slice"},
	}
	fragment := metadata[app+".service"]["FragmentPath"]
	b := Backend{runCommand: func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name != "/usr/bin/systemctl" {
			t.Fatal(name)
		}
		for _, arg := range args {
			if arg == "start" || arg == "stop" {
				t.Fatal("discovery mutated service")
			}
		}
		if len(args) == 1 && args[0] == "--version" {
			return []byte("systemd 255"), nil
		}
		if strings.Contains(strings.Join(args, " "), "list-unit-files") {
			return []byte(app + ".service disabled\n"), nil
		}
		unit := args[len(args)-1]
		v, ok := metadata[unit]
		if !ok {
			return nil, errors.New("missing metadata")
		}
		var out strings.Builder
		for k, x := range v {
			out.WriteString(k + "=" + x + "\n")
		}
		return []byte(out.String()), nil
	}, inspectAutomatic: func(path, kind string) (gpuruntime.AutomaticLaunch, error) {
		if path != fragment || kind != app {
			return gpuruntime.AutomaticLaunch{}, errors.New("wrong fragment")
		}
		return gpuruntime.AutomaticLaunch{Endpoint: "http://127.0.0.1:8080", Model: "served-model", SHA256: strings.Repeat("a", 64), Command: command}, nil
	}, probeApplication: func(context.Context, ProbeRequest) (ApplicationCandidate, error) {
		return ApplicationCandidate{InstanceStatus: "present", InventoryStatus: "available", Models: []ModelCandidate{{ID: "local", Locality: "local"}}}, nil
	}}
	model := "served-model"
	if app == "ollama" {
		model = "local-model"
	}
	if app == "comfyui" {
		model = ""
	}
	return b, Draft{ID: "chosen", Label: "Chosen installation", App: app, Binding: &DraftBinding{Unit: app + ".service"}, Model: model}, metadata
}
func TestPrepareFourStoppedApplicationsFromMetadata(t *testing.T) {
	for _, app := range []string{"comfyui", "ollama", appLlamaCPP, "vllm"} {
		t.Run(app, func(t *testing.T) {
			b, d, _ := automaticFixture(t, app)
			if app == "comfyui" {
				d.Model = ""
			}
			got, err := b.Prepare(context.Background(), PrepareRequest{Draft: d})
			if err != nil {
				t.Fatal(err)
			}
			p := got.Profile
			if p.ID != "chosen" || p.SystemdSlice != "app.slice" || p.SystemdVersion != 255 || p.Cgroup != "/manager/app.slice/"+app+".service" {
				t.Fatalf("%+v", p)
			}
			if app == "comfyui" {
				if p.NativeModel != nil || p.LaunchBinding == nil {
					t.Fatal(p)
				}
			} else {
				if p.NativeModel == nil || p.NativeModel.Owned != nil {
					t.Fatal(p)
				}
				want := "served-model"
				if app == "ollama" {
					want = "local-model"
				}
				if p.NativeModel.Model != want {
					t.Fatal(p.NativeModel)
				}
			}
			d.Binding.Cgroup = "/attacker"
			d.Binding.LaunchFile = "/attacker"
			d.Endpoint = "http://127.0.0.1:6666"
			second, err := b.Prepare(context.Background(), PrepareRequest{Draft: d})
			if err == nil {
				t.Fatal("mismatched override accepted", second, err)
			}
		})
	}
}
func TestPrepareRefusesMissingChangedAndUnsupportedEvidence(t *testing.T) {
	b, d, v := automaticFixture(t, "comfyui")
	d.Model = ""
	for _, tc := range []struct{ key, value string }{{"NeedDaemonReload", "yes"}, {"DropInPaths", "/override"}, {"ExecStart", "{ path=/bin/sh ; argv[]=/bin/sh -c x ; }"}, {"ExecStart", "{ path=/usr/bin/python3 ; argv[]=/usr/bin/python3 /opt/ComfyUI/main.py --port 9000 ; }"}, {"Id", "foreign.service"}, {"LoadState", "not-found"}, {"Slice", "custom.slice"}, {"FragmentPath", "/other"}} {
		old := v["comfyui.service"][tc.key]
		v["comfyui.service"][tc.key] = tc.value
		if _, err := b.Prepare(context.Background(), PrepareRequest{Draft: d}); err == nil {
			t.Fatal(tc)
		}
		v["comfyui.service"][tc.key] = old
	}
	oldRun := b.runCommand
	b.runCommand = func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("bus down") }
	if _, err := b.Prepare(context.Background(), PrepareRequest{Draft: d}); err == nil {
		t.Fatal("bus failure accepted")
	}
	b.runCommand = oldRun
	for _, unit := range []string{"bad@name.service", "missing.service", ""} {
		d.Binding.Unit = unit
		if _, err := b.Prepare(context.Background(), PrepareRequest{Draft: d}); err == nil {
			t.Fatal(unit)
		}
	}
	d.ID = ""
	if _, err := b.Prepare(context.Background(), PrepareRequest{Draft: d}); err == nil {
		t.Fatal("invalid draft accepted")
	}
	b, d, _ = automaticFixture(t, "ollama")
	d.Model = ""
	if _, err := b.Prepare(context.Background(), PrepareRequest{Draft: d}); err == nil {
		t.Fatal("missing Ollama choice accepted")
	}
}
func TestPrepareExplicitLocationWithoutScanning(t *testing.T) {
	for _, kind := range []string{"configuration", "application-directory"} {
		b, d, _ := automaticFixture(t, "comfyui")
		d.Model = ""
		d.Binding = nil
		d.Reference = "/opt/launch/comfyui.service"
		if kind == "application-directory" {
			d.Reference = "/opt/ComfyUI"
		}
		d.ReferenceKind = kind
		if _, err := b.Prepare(context.Background(), PrepareRequest{Draft: d}); err != nil {
			t.Fatal(err)
		}
		d.Reference = "/missing"
		if _, err := b.Prepare(context.Background(), PrepareRequest{Draft: d}); err == nil {
			t.Fatal("missing location accepted")
		}
	}
	b, d, _ := automaticFixture(t, "comfyui")
	d.Model = ""
	d.Binding = nil
	if _, err := b.Prepare(context.Background(), PrepareRequest{Draft: d}); err == nil {
		t.Fatal("missing installation accepted")
	}
}
func TestDiscoveryReportsReadFailureAndRecognizedStoppedInstallation(t *testing.T) {
	b, _, v := automaticFixture(t, "comfyui")
	result := Discovery{}
	b.discoverUnit(context.Background(), &result, "comfyui.service")
	if len(result.Applications) != 1 || !result.Applications[0].Recognized || result.Applications[0].Binding == nil || result.Applications[0].ConfigurationStatus != "ready" || result.Applications[0].NextStep != "Installed and stopped. Ready to configure." {
		t.Fatal(result)
	}
	v["comfyui.service"]["ActiveState"] = "active"
	v["comfyui.service"]["SubState"] = "running"
	v["comfyui.service"]["ControlGroup"] = "/manager/live"
	b.discoverUnit(context.Background(), &result, "comfyui.service")
	if result.Applications[1].InstanceStatus != "present" {
		t.Fatal(result)
	}
	b.discoverUnit(context.Background(), &result, "missing.service")
	if len(result.Errors) != 1 {
		t.Fatal(result)
	}
	b, d, _ := automaticFixture(t, "ollama")
	b.discoverUnit(context.Background(), &result, d.Binding.Unit)
	if result.Applications[2].ConfigurationStatus != "model-required" || result.Applications[2].Binding.Model != "" {
		t.Fatal(result)
	}
	result = Discovery{}
	b.discoverApplications(context.Background(), &result, make([]string, 257))
	if len(result.Errors) != 1 {
		t.Fatal(result)
	}
}
func TestLaunchBindingCatalogCloneAndValidation(t *testing.T) {
	b, d, _ := automaticFixture(t, "comfyui")
	d.Model = ""
	got, err := b.Prepare(context.Background(), PrepareRequest{Draft: d})
	if err != nil {
		t.Fatal(err)
	}
	c := control.Catalog{Version: 1, Profiles: []control.WorkloadProfile{got.Profile}}
	clone := c.Clone()
	clone.Profiles[0].LaunchBinding.LaunchFile = "/changed"
	if c.Profiles[0].LaunchBinding.LaunchFile == "/changed" {
		t.Fatal("clone shares launch evidence")
	}
	for _, mutate := range []func(*control.WorkloadProfile){func(p *control.WorkloadProfile) { p.SystemdVersion = 999 }, func(p *control.WorkloadProfile) { p.LaunchBinding.Runtime = "other" }, func(p *control.WorkloadProfile) { p.HealthURL = "http://127.0.0.1:8080/other" }, func(p *control.WorkloadProfile) { p.LaunchBinding.LaunchSHA256 = "bad" }} {
		copy := c.Clone()
		mutate(&copy.Profiles[0])
		if err := copy.Validate(); err == nil {
			t.Fatal("invalid launch binding accepted")
		}
	}
}

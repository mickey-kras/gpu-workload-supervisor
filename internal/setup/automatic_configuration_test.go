package setup

import (
	"context"
	"errors"
	"fmt"
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

func TestDiscoveryRecognizesKeywordlessDirectApplications(t *testing.T) {
	for _, app := range []string{"comfyui", "ollama", appLlamaCPP, "vllm"} {
		t.Run(app, func(t *testing.T) {
			b, d, metadata := automaticFixture(t, app)
			unit := "inference-worker.service"
			metadata[unit] = metadata[d.Binding.Unit]
			metadata[unit]["Id"] = unit
			oldRun := b.runCommand
			b.runCommand = func(ctx context.Context, exe string, args ...string) ([]byte, error) {
				if strings.Contains(strings.Join(args, " "), "list-unit-files") {
					return []byte(unit + " disabled\n"), nil
				}
				return oldRun(ctx, exe, args...)
			}
			result := Discovery{}
			b.discoverApplications(context.Background(), &result, []string{unit})
			if len(result.Units) != 1 || result.Units[0] != unit || len(result.Applications) != 5 || !result.Applications[4].Recognized {
				t.Fatalf("keywordless direct service excluded: %+v", result)
			}
			d.Binding.Unit = unit
			if _, err := b.Prepare(context.Background(), PrepareRequest{Draft: d}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPrepareRequiresLoadedStartupPreparationMatch(t *testing.T) {
	b, d, metadata := automaticFixture(t, appLlamaCPP)
	inspect := b.inspectAutomatic
	b.inspectAutomatic = func(path, app string) (gpuruntime.AutomaticLaunch, error) {
		launch, err := inspect(path, app)
		launch.PreCommands = []string{"/usr/bin/true"}
		return launch, err
	}
	if _, err := b.Prepare(context.Background(), PrepareRequest{Draft: d}); err == nil {
		t.Fatal("missing loaded preparation accepted")
	}
	metadata[d.Binding.Unit]["ExecStartPre"] = "{ path=/usr/bin/true ; argv[]=/usr/bin/true ; ignore_errors=no ; }"
	if _, err := b.Prepare(context.Background(), PrepareRequest{Draft: d}); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareRetainsLoadedDropInEvidence(t *testing.T) {
	b, d, metadata := automaticFixture(t, appLlamaCPP)
	inspect := b.inspectAutomatic
	source := control.LaunchSource{Path: "/opt/launch/10-tuning.conf", SHA256: strings.Repeat("b", 64)}
	b.inspectAutomatic = func(path, app string) (gpuruntime.AutomaticLaunch, error) {
		launch, err := inspect(path, app)
		launch.DropIns = []control.LaunchSource{source}
		return launch, err
	}
	metadata[d.Binding.Unit]["DropInPaths"] = source.Path
	got, err := b.Prepare(context.Background(), PrepareRequest{Draft: d})
	if err != nil {
		t.Fatal(err)
	}
	if !control.EqualLaunchSources(got.Profile.NativeModel.DropIns, []control.LaunchSource{source}) {
		t.Fatal("drop-in evidence discarded")
	}
	metadata[d.Binding.Unit]["DropInPaths"] += " /opt/launch/20-unbound.conf"
	if _, err := b.Prepare(context.Background(), PrepareRequest{Draft: d}); err == nil {
		t.Fatal("unbound source accepted")
	}
}

func TestKeywordlessApplicationWithRepeatedSystemctlPreCommandsIsDiscovered(t *testing.T) {
	b, d, metadata := automaticFixture(t, appLlamaCPP)
	unit := "inference-worker.service"
	metadata[unit] = metadata[d.Binding.Unit]
	metadata[unit]["Id"] = unit
	metadata[unit]["DropInPaths"] = "/opt/launch/10-precondition.conf"
	metadata[unit]["ExecStartPre"] = "{ path=/usr/bin/true ; argv[]=/usr/bin/true ; ignore_errors=no ; }\nExecStartPre={ path=/usr/bin/test ; argv[]=/usr/bin/test -f /models/a.gguf ; ignore_errors=no ; }"
	inspect := b.inspectAutomatic
	b.inspectAutomatic = func(path, app string) (gpuruntime.AutomaticLaunch, error) {
		launch, err := inspect(path, app)
		launch.PreCommands = []string{"/usr/bin/true", "/usr/bin/test -f /models/a.gguf"}
		launch.DropIns = []control.LaunchSource{{Path: metadata[unit]["DropInPaths"], SHA256: strings.Repeat("b", 64)}}
		return launch, err
	}
	result := Discovery{}
	b.discoverUnit(context.Background(), &result, unit)
	if len(result.Applications) != 1 || !result.Applications[0].Recognized || result.Applications[0].Binding.Unit != unit {
		t.Fatalf("supported keywordless main/drop-in preconditions omitted: %+v", result)
	}
	d.Binding.Unit = unit
	if _, err := b.Prepare(context.Background(), PrepareRequest{Draft: d}); err != nil {
		t.Fatal(err)
	}
	metadata[unit]["ExecStartPre"] = "{ path=/usr/bin/test ; argv[]=/usr/bin/test -f /models/a.gguf ; ignore_errors=no ; }\nExecStartPre={ path=/usr/bin/true ; argv[]=/usr/bin/true ; ignore_errors=no ; }"
	if _, err := b.Prepare(context.Background(), PrepareRequest{Draft: d}); err == nil {
		t.Fatal("reversed startup preconditions accepted")
	}
}

func TestReferenceResolutionSkipsUnrelatedInspectionFailures(t *testing.T) {
	for _, kind := range []string{"endpoint", "application-directory"} {
		t.Run(kind, func(t *testing.T) {
			b, d, _ := automaticFixture(t, "comfyui")
			d.Binding = nil
			if kind == "endpoint" {
				d.Endpoint = "http://127.0.0.1:8080"
			} else {
				d.ReferenceKind = kind
				d.Reference = "/opt/ComfyUI"
			}
			run := b.runCommand
			var failed []string
			b.runCommand = func(ctx context.Context, exe string, args ...string) ([]byte, error) {
				if strings.Contains(strings.Join(args, " "), "list-unit-files") {
					return []byte("disappeared.service disabled\ncomfyui.service enabled\nuninspectable.service disabled\n"), nil
				}
				unit := args[len(args)-1]
				if unit == "disappeared.service" || unit == "uninspectable.service" {
					failed = append(failed, unit)
					return nil, errors.New("unrelated service unavailable")
				}
				return run(ctx, exe, args...)
			}
			got, err := b.Prepare(context.Background(), PrepareRequest{Draft: d})
			if err != nil {
				t.Fatal(err)
			}
			if got.Profile.Unit != "comfyui.service" || len(failed) != 2 {
				t.Fatalf("matching installation lost or enumeration cut short: %+v %v", got, failed)
			}
		})
	}
}

func TestReferenceResolutionRetainsMissingAmbiguousAndInspectionFailureErrors(t *testing.T) {
	for _, kind := range []string{"endpoint", "application-directory"} {
		for _, scenario := range []string{"no-match", "ambiguous", "all-uninspectable", "unsafe-launch"} {
			t.Run(kind+"/"+scenario, func(t *testing.T) {
				b, d, metadata := automaticFixture(t, "comfyui")
				d.Binding = nil
				if kind == "endpoint" {
					d.Endpoint = "http://127.0.0.1:8080"
				} else {
					d.ReferenceKind = kind
					d.Reference = "/opt/ComfyUI"
				}
				if scenario == "no-match" {
					d.Endpoint = "http://127.0.0.1:9999"
					if kind == "application-directory" {
						d.Endpoint = ""
						d.Reference = "/missing"
					}
				}
				metadata["second.service"] = metadata["comfyui.service"]
				listing := "uninspectable.service disabled\ncomfyui.service enabled\n"
				if scenario == "ambiguous" {
					listing += "second.service enabled\n"
				}
				if scenario == "all-uninspectable" {
					listing = "uninspectable.service disabled\nmissing.service disabled\n"
				}
				cause := errors.New("service inspection unavailable")
				run := b.runCommand
				b.runCommand = func(ctx context.Context, exe string, args ...string) ([]byte, error) {
					if strings.Contains(strings.Join(args, " "), "list-unit-files") {
						return []byte(listing), nil
					}
					unit := args[len(args)-1]
					if unit == "uninspectable.service" || unit == "missing.service" {
						return nil, cause
					}
					return run(ctx, exe, args...)
				}
				if scenario == "unsafe-launch" {
					b.inspectAutomatic = func(string, string) (gpuruntime.AutomaticLaunch, error) {
						return gpuruntime.AutomaticLaunch{}, errors.New("untrusted executable")
					}
				}
				_, err := b.Prepare(context.Background(), PrepareRequest{Draft: d})
				if err == nil {
					t.Fatal("unsafe reference resolved", scenario)
				}
				if scenario == "all-uninspectable" && (!errors.Is(err, cause) || !strings.Contains(err.Error(), "could not be inspected")) {
					t.Fatal("systemic inspection failure hidden", err)
				}
				if scenario == "ambiguous" && !strings.Contains(err.Error(), "multiple installations") {
					t.Fatal("ambiguity was not retained", err)
				}
				if scenario == "no-match" && !strings.Contains(err.Error(), "no supported loaded installation") {
					t.Fatal("no-match gate changed", err)
				}
			})
		}
	}
}

func TestReferenceResolutionCancellationDoesNotBecomeFallback(t *testing.T) {
	for _, stage := range []string{"before-listing", "listing", "before-match", "after-match", "successful-observation", "launch-inspection", "wrapped-listing", "wrapped-observation", "wrapped-launch"} {
		t.Run(stage, func(t *testing.T) {
			b, d, _ := automaticFixture(t, "comfyui")
			d.Binding = nil
			d.Endpoint = "http://127.0.0.1:8080"
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			run := b.runCommand
			listing := "before.service disabled\ncomfyui.service enabled\nafter.service disabled\n"
			b.runCommand = func(ctx context.Context, exe string, args ...string) ([]byte, error) {
				if strings.Contains(strings.Join(args, " "), "list-unit-files") {
					if stage == "listing" {
						cancel()
					}
					if stage == "wrapped-listing" {
						return nil, fmt.Errorf("command: %w", context.DeadlineExceeded)
					}
					return []byte(listing), nil
				}
				unit := args[len(args)-1]
				if unit == "before.service" || unit == "after.service" {
					if (stage == "before-match" && unit == "before.service") || (stage == "after-match" && unit == "after.service") {
						cancel()
					}
					if stage == "wrapped-observation" {
						return nil, fmt.Errorf("command: %w", context.Canceled)
					}
					return nil, errors.New("unrelated service unavailable")
				}
				result, err := run(ctx, exe, args...)
				if stage == "successful-observation" {
					cancel()
				}
				return result, err
			}
			inspect := b.inspectAutomatic
			b.inspectAutomatic = func(path, app string) (gpuruntime.AutomaticLaunch, error) {
				if stage == "launch-inspection" {
					cancel()
				}
				if stage == "wrapped-launch" {
					return gpuruntime.AutomaticLaunch{}, fmt.Errorf("inspect: %w", context.DeadlineExceeded)
				}
				return inspect(path, app)
			}
			if stage == "before-listing" {
				cancel()
			}
			_, err := b.Prepare(ctx, PrepareRequest{Draft: d})
			want := context.Canceled
			if stage == "wrapped-listing" || stage == "wrapped-launch" {
				want = context.DeadlineExceeded
			}
			if !errors.Is(err, want) {
				t.Fatalf("cancellation changed into fallback: %v", err)
			}
		})
	}
}

func TestReferenceResolutionPreservesGlobalBoundsAndExplicitUnitFailures(t *testing.T) {
	for _, kind := range []string{"list-error", "oversized", "too-many"} {
		t.Run(kind, func(t *testing.T) {
			b, d, _ := automaticFixture(t, "comfyui")
			d.Binding = nil
			d.Reference = "/opt/ComfyUI"
			d.ReferenceKind = "application-directory"
			shows := 0
			b.runCommand = func(_ context.Context, _ string, args ...string) ([]byte, error) {
				if !strings.Contains(strings.Join(args, " "), "list-unit-files") {
					shows++
					return nil, errors.New("unexpected inspection")
				}
				switch kind {
				case "list-error":
					return nil, errors.New("desktop bus unavailable")
				case "oversized":
					return []byte(strings.Repeat("x", commandOutputLimit+1)), nil
				default:
					return []byte(strings.Repeat("unit.service disabled\n", 257)), nil
				}
			}
			if _, err := b.Prepare(context.Background(), PrepareRequest{Draft: d}); err == nil || shows != 0 {
				t.Fatal("global discovery boundary weakened", err, shows)
			}
		})
	}
	b, d, _ := automaticFixture(t, "comfyui")
	cause := errors.New("explicit selected unit unavailable")
	b.runCommand = func(context.Context, string, ...string) ([]byte, error) { return nil, cause }
	if _, err := b.Prepare(context.Background(), PrepareRequest{Draft: d}); !errors.Is(err, cause) {
		t.Fatal("explicit unit failure hidden", err)
	}
}

func TestPrepareResolvesUnitAndRetainsMatchingOverrides(t *testing.T) {
	b, d, _ := automaticFixture(t, "comfyui")
	d.Model = ""
	d.Reference = "/opt/launch/comfyui.service"
	d.ReferenceKind = "configuration"
	d.Binding = &DraftBinding{HealthURL: "http://127.0.0.1:8080/system_stats", LaunchFile: "/opt/launch/comfyui.service", Cgroup: "/manager/app.slice/comfyui.service"}
	got, err := b.Prepare(context.Background(), PrepareRequest{Draft: d})
	if err != nil {
		t.Fatal(err)
	}
	if got.Profile.Unit != "comfyui.service" || got.Profile.HealthURL != d.Binding.HealthURL || got.Profile.LaunchBinding == nil {
		t.Fatalf("override discarded: %+v", got.Profile)
	}
	d.Binding.Cgroup = "/attacker"
	if _, err := b.Prepare(context.Background(), PrepareRequest{Draft: d}); err == nil {
		t.Fatal("mismatched override accepted once the unit was resolved")
	}
}

func TestPrepareRejectsOwnedDraft(t *testing.T) {
	b, d, _ := automaticFixture(t, "vllm")
	d.Binding = &DraftBinding{Unit: "vllm.service", Instance: "local", Owned: &DraftOwnedLaunch{ModelPath: "/models/served", Port: 9100}}
	if _, err := b.Prepare(context.Background(), PrepareRequest{Draft: d}); !errors.Is(err, ErrOwnedDraftPrepare) {
		t.Fatalf("owned draft prepared: %v", err)
	}
}

func TestPrepareRejectsModelConflictingWithExistingLaunch(t *testing.T) {
	b, d, _ := automaticFixture(t, appLlamaCPP)
	d.Model = "other-model"
	if _, err := b.Prepare(context.Background(), PrepareRequest{Draft: d}); err == nil {
		t.Fatal("model conflicting with the existing launch accepted")
	}
}

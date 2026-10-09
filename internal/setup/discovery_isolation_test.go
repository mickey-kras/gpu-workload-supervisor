package setup

import (
	"context"
	"fmt"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
	"strings"
	"testing"
)

func TestDiscoveryClassifiesBeforeStrictBindingMetadata(t *testing.T) {
	for _, app := range []string{appLlamaCPP, "comfyui"} {
		t.Run(app, func(t *testing.T) {
			b, d, metadata := automaticFixture(t, app)
			arbitrary := "chosen-installation.service"
			metadata[arbitrary] = metadata[d.Binding.Unit]
			metadata[arbitrary]["Id"] = arbitrary
			originalRun := b.runCommand
			b.runCommand = func(ctx context.Context, name string, args ...string) ([]byte, error) {
				unit := args[len(args)-1]
				switch unit {
				case "unrelated@.service":
					return []byte("ExecStart=\nExecStartPre={ path=/bin/true ; argv[]=/bin/true ; ignore_errors=no ; start_time=[n/a] ; }\nExecStartPre=\n"), nil
				case "gpu-workload-supervisor-reconcile.service":
					return []byte("ExecStart={ path=/usr/bin/gpu-setup ; argv[]=/usr/bin/gpu-setup reconcile ; }\nExecStartPre=garbled unsupported data\n"), nil
				}
				return originalRun(ctx, name, args...)
			}
			result := Discovery{}
			for _, unit := range []string{"unrelated@.service", "gpu-workload-supervisor-reconcile.service", arbitrary} {
				b.discoverUnit(context.Background(), &result, unit)
			}
			if len(result.Errors) != 0 || len(result.Applications) != 1 || !result.Applications[0].Recognized || result.Applications[0].Unit != arbitrary || result.Applications[0].InstanceStatus != "not-running" {
				t.Fatalf("unrelated units poisoned discovery: %+v", result)
			}
			metadata[arbitrary]["ExecStartPre"] = "unparseable preparation metadata"
			failed := Discovery{}
			b.discoverUnit(context.Background(), &failed, arbitrary)
			if len(failed.Errors) != 0 || len(failed.Applications) != 1 || failed.Applications[0].ConfigurationStatus != "inspection-failed" || failed.Applications[0].Binding != nil || !strings.Contains(failed.Applications[0].NextStep, "ExecStartPre") {
				t.Fatalf("supported failure was lost: %+v", failed)
			}
		})
	}
}

func TestExecutableReferenceCreatesOnlyOwnedDraftEvidence(t *testing.T) {
	r := ProbeRequest{App: "ollama", Reference: "/opt/trusted/ollama", ReferenceKind: "application"}
	selected, err := probeSelectedExecutable(r, candidate(r), func(app, path string) error {
		if app != "ollama" || path != r.Reference {
			t.Fatal("wrong executable inspected")
		}
		return nil
	})
	if err != nil || !selected.Recognized || selected.SourceKind != "owned" || selected.InstanceStatus != "installed" || selected.Binding.Owned.Executable != r.Reference || selected.Binding.Owned.Port != 11434 || selected.LifecycleControl != "unverified" {
		t.Fatalf("owned candidate contract: %+v %v", selected, err)
	}
	// /usr/bin/true is trusted but its name does not establish a supported app.
	got, err := Probe(context.Background(), ProbeRequest{App: "ollama", Reference: "/usr/bin/true", ReferenceKind: "application"})
	if err != nil || got.Recognized || got.Binding != nil || got.InstanceStatus != "inspection-failed" {
		t.Fatalf("unsupported executable was accepted: %+v %v", got, err)
	}
	d := ownedDraft("selected", 9100)
	d.Reference = "/opt/trusted/llama-server"
	d.ReferenceKind = "application"
	d.Binding.Owned.Executable = d.Reference
	p, raw, err := OwnedProfile(d, "/manager", t.TempDir())
	if err != nil || p.NativeModel.Owned.Executable != d.Reference || !strings.Contains(string(raw), "ExecStart="+d.Reference+" -m ") {
		t.Fatalf("selected executable lost: %+v %s %v", p, string(raw), err)
	}
	d.Binding.Owned.Executable = "/usr/bin/llama-server"
	if _, _, err := OwnedProfile(d, "/manager", t.TempDir()); err == nil {
		t.Fatal("selection mismatch accepted")
	}
}

func TestStoppedApplicationMissingModelDiffersFromInspectionFailure(t *testing.T) {
	b, d, _ := automaticFixture(t, appLlamaCPP)
	b.inspectAutomatic = func(string, string) (gpuruntime.AutomaticLaunch, error) {
		return gpuruntime.AutomaticLaunch{}, fmt.Errorf("%w: choose a local model", gpuruntime.ErrModelUnavailable)
	}
	result := Discovery{}
	b.discoverUnit(context.Background(), &result, d.Binding.Unit)
	if len(result.Applications) != 1 || result.Applications[0].InstanceStatus != "not-running" || result.Applications[0].ConfigurationStatus != "model-missing" || result.Applications[0].InventoryStatus != "missing" || result.Applications[0].Binding != nil {
		t.Fatalf("missing model indistinguishable: %+v", result)
	}
}

func TestReferenceSelectionIsolatesUnselectedSameApplicationMetadata(t *testing.T) {
	b, d, metadata := automaticFixture(t, "ollama")
	d.Reference = metadata[d.Binding.Unit]["FragmentPath"]
	d.ReferenceKind = "configuration"
	d.Binding = nil
	original := b.runCommand
	unrelatedPath := "/different/location.service"
	b.runCommand = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), "list-unit-files") {
			return []byte("unrelated-ollama.service disabled\nollama.service disabled\n"), nil
		}
		if args[len(args)-1] == "unrelated-ollama.service" {
			return []byte("Id=unrelated-ollama.service\nFragmentPath=" + unrelatedPath + "\nExecStart={ path=/usr/bin/ollama ; argv[]=/usr/bin/ollama serve ; }\nExecStartPre=unparseable preparation metadata\n"), nil
		}
		return original(ctx, name, args...)
	}
	got, err := b.unitAtReference(context.Background(), d)
	if err != nil || got != "ollama.service" {
		t.Fatalf("selected configuration poisoned by unselected same-app metadata: %q %v", got, err)
	}
	unrelatedPath = d.Reference
	if _, err := b.unitAtReference(context.Background(), d); err == nil || !strings.Contains(err.Error(), "ExecStartPre") {
		t.Fatalf("selected ambiguous metadata was silently ignored: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := b.unitAtReference(ctx, d); err != context.Canceled {
		t.Fatalf("cancellation became a candidate failure: %v", err)
	}
}

package setup

import (
	"context"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/deployment"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
)

func TestSetupAndRuntimeLoadedCommandParity(t *testing.T) {
	const command = "/usr/bin/vllm serve /models/a --host 127.0.0.1 --port 8080"
	for _, tc := range []struct {
		name, loaded string
		valid        bool
	}{
		{"direct", "{ path=/usr/bin/vllm ; argv[]=/usr/bin/vllm serve /models/a --host 127.0.0.1 --port 8080 ; }", true},
		{"systemd status metadata", "{ path=/usr/bin/vllm ; argv[]=/usr/bin/vllm serve /models/a --host 127.0.0.1 --port 8080 ; ignore_errors=no ; start_time=[n/a] ; }", true},
		{"different executable", "{ path=/usr/bin/other ; argv[]=/usr/bin/vllm serve /models/a --host 127.0.0.1 --port 8080 ; }", false},
		{"different argv", "{ path=/usr/bin/vllm ; argv[]=/usr/bin/vllm serve /models/a --host 127.0.0.1 --port 8080 extra ; }", false},
		{"ignored failure", "{ path=/usr/bin/vllm ; argv[]=/usr/bin/vllm serve /models/a --host 127.0.0.1 --port 8080 ; ignore_errors=yes ; }", false},
		{"unparsed metadata", "{ path=/usr/bin/vllm ; argv[]=/usr/bin/vllm serve /models/a --host 127.0.0.1 --port 8080 ; } unsupported", false},
		{"multiple commands", "{ path=/usr/bin/vllm ; argv[]=/usr/bin/vllm serve /models/a --host 127.0.0.1 --port 8080 ; } { path=/usr/bin/vllm ; argv[]=/usr/bin/vllm serve /models/a --host 127.0.0.1 --port 8080 ; }", false},
		{"missing path", "{ argv[]=/usr/bin/vllm serve /models/a --host 127.0.0.1 --port 8080 ; }", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, d, metadata := automaticFixture(t, "vllm")
			metadata[d.Binding.Unit]["ExecStart"] = tc.loaded
			_, setupErr := b.Prepare(context.Background(), PrepareRequest{Draft: d})
			runtimeErr := gpuruntime.CheckLoadedLaunchCommand(tc.loaded, command)
			if (setupErr == nil) != tc.valid || (runtimeErr == nil) != tc.valid {
				t.Fatalf("setup=%v runtime=%v; valid=%v", setupErr, runtimeErr, tc.valid)
			}
		})
	}
}

func TestSetupAndActivatedProfileValidationParity(t *testing.T) {
	base := Profile{Version: 1, ActivatedRelease: "v1.0.0", StatePath: "/home/operator/state.db", SystemctlPath: "/usr/bin/systemctl", NvidiaSMIPath: "/usr/bin/nvidia-smi"}
	_, _, request := fixture(t)
	catalog := request.Catalog
	for _, tc := range []struct {
		name   string
		change func(*Profile)
		want   string
	}{
		{"defaults", func(p *Profile) {}, ""},
		{"maximum timeouts", func(p *Profile) { p.StatusTimeoutSeconds = 60; p.OperationTimeoutSeconds = 1800 }, ""},
		{"profile version", func(p *Profile) { p.Version = 2 }, "unsupported profile version"},
		{"GPU index", func(p *Profile) { p.GPUIndex = -1 }, "invalid GPU index"},
		{"negative status", func(p *Profile) { p.StatusTimeoutSeconds = -1 }, "status timeout"},
		{"large status", func(p *Profile) { p.StatusTimeoutSeconds = 61 }, "status timeout"},
		{"negative operation", func(p *Profile) { p.OperationTimeoutSeconds = -1 }, "operation timeout"},
		{"large operation", func(p *Profile) { p.OperationTimeoutSeconds = 1801 }, "operation timeout"},
		{"root state", func(p *Profile) { p.StatePath = "/" }, "statePath"},
		{"root systemctl", func(p *Profile) { p.SystemctlPath = "/" }, "systemctlPath"},
		{"root inventory", func(p *Profile) { p.NvidiaSMIPath = "/" }, "nvidiaSMIPath"},
		{"relative state", func(p *Profile) { p.StatePath = "state.db" }, "statePath"},
		{"unclean systemctl", func(p *Profile) { p.SystemctlPath = "/usr//bin/systemctl" }, "systemctlPath"},
		{"empty inventory", func(p *Profile) { p.NvidiaSMIPath = "" }, "nvidiaSMIPath"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := base
			tc.change(&p)
			setupErr := Validate(Request{Version: 1, Profile: p, Catalog: catalog})
			runtimeErr := p.Validate()
			if tc.want == "" {
				if setupErr != nil || runtimeErr != nil {
					t.Fatalf("setup=%v activated=%v", setupErr, runtimeErr)
				}
				return
			}
			if setupErr == nil || runtimeErr == nil || setupErr.Error() != runtimeErr.Error() || !strings.Contains(setupErr.Error(), tc.want) {
				t.Fatalf("setup=%v activated=%v; want %q", setupErr, runtimeErr, tc.want)
			}
		})
	}
	for _, release := range []string{"", "old/release", strings.Repeat("a", 129)} {
		p := base
		p.ActivatedRelease = release
		if err := Validate(Request{Version: 1, Profile: p, Catalog: catalog}); err != nil {
			t.Fatalf("preview release %q: %v", release, err)
		}
		if err := p.Validate(); err == nil {
			t.Fatalf("activated release %q accepted", release)
		}
	}
}

func TestSetupPreviewSelectsInstalledRelease(t *testing.T) {
	for _, release := range []string{"", "v0.1.0", "old/release"} {
		b, home, request := fixture(t)
		request.Profile.ActivatedRelease = release
		preview, err := b.Plan(home, request)
		if err != nil {
			t.Fatal(err)
		}
		if preview.Profile.ActivatedRelease != deployment.Release || request.Profile.ActivatedRelease != release {
			t.Fatalf("preview=%q original=%q", preview.Profile.ActivatedRelease, request.Profile.ActivatedRelease)
		}
		if err := preview.Profile.Validate(); err != nil {
			t.Fatal(err)
		}
	}
}

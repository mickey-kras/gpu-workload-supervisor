package deployment

import (
	"strings"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

func validProfile() Profile {
	return Profile{
		Version:             1,
		StatePath:           "/var/lib/gpu-workload-supervisor/state.db",
		ActivatedRelease:    "v1.2.3-rc_1",
		SystemctlPath:       "/usr/bin/systemctl",
		NvidiaSMIPath:       "/usr/bin/nvidia-smi",
		CapacityHeadroomMiB: 512,
	}
}

func TestProfileValidateAcceptsValidProfile(t *testing.T) {
	for _, bound := range [][2]int{{0, 0}, {60, 1800}} {
		p := validProfile()
		p.StatusTimeoutSeconds = bound[0]
		p.OperationTimeoutSeconds = bound[1]
		if err := p.Validate(); err != nil {
			t.Fatalf("timeouts %v: %v", bound, err)
		}
	}
	p := validProfile()
	p.ActivatedRelease = strings.Repeat("a", 128)
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestProfileValidateRejectsInvalidFields(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Profile)
	}{
		{"zero version", func(p *Profile) { p.Version = 0 }},
		{"future version", func(p *Profile) { p.Version = 2 }},
		{"empty release", func(p *Profile) { p.ActivatedRelease = "" }},
		{"oversized release", func(p *Profile) { p.ActivatedRelease = strings.Repeat("a", 129) }},
		{"release with slash", func(p *Profile) { p.ActivatedRelease = "a/b" }},
		{"release with space", func(p *Profile) { p.ActivatedRelease = "a b" }},
		{"release with dollar", func(p *Profile) { p.ActivatedRelease = "a$b" }},
		{"negative GPU index", func(p *Profile) { p.GPUIndex = -1 }},
		{"negative status timeout", func(p *Profile) { p.StatusTimeoutSeconds = -1 }},
		{"oversized status timeout", func(p *Profile) { p.StatusTimeoutSeconds = 61 }},
		{"negative operation timeout", func(p *Profile) { p.OperationTimeoutSeconds = -1 }},
		{"oversized operation timeout", func(p *Profile) { p.OperationTimeoutSeconds = 1801 }},
		{"relative state path", func(p *Profile) { p.StatePath = "var/state.db" }},
		{"unclean state path", func(p *Profile) { p.StatePath = "/var//state.db" }},
		{"root state path", func(p *Profile) { p.StatePath = "/" }},
		{"empty systemctl path", func(p *Profile) { p.SystemctlPath = "" }},
		{"unclean nvidia-smi path", func(p *Profile) { p.NvidiaSMIPath = "/usr/bin/../bin/nvidia-smi" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := validProfile()
			tc.change(&p)
			if err := p.Validate(); err == nil {
				t.Fatalf("accepted %+v", p)
			}
		})
	}
}

func TestProfileSystemdConfigMapsFields(t *testing.T) {
	p := validProfile()
	p.GPUIndex = 2
	catalog := &control.Catalog{}
	config := p.SystemdConfig(catalog)
	if config.Catalog != catalog || config.SystemctlPath != p.SystemctlPath || config.NvidiaSMIPath != p.NvidiaSMIPath ||
		config.GPUIndex != p.GPUIndex || config.CapacityHeadroomMiB != p.CapacityHeadroomMiB || config.HealthTimeout != 10*time.Second {
		t.Fatalf("mapped config = %+v", config)
	}
}

func TestDefaultStatePathFallsBackWhenHomeUnknown(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", "")
	if got := DefaultStatePath(); got != stateFile {
		t.Fatalf("fallback path = %q, want %q", got, stateFile)
	}
}

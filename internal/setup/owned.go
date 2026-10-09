package setup

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
)

var ErrManagerCgroupMismatch = errors.New("manager cgroup does not match the running user manager")

// ManagerCgroup asks the running user manager for its root control group so
// owned profiles derive cgroups from the host, not from request input.
func ManagerCgroup(ctx context.Context, systemctl string) (string, error) {
	return managerCgroup(ctx, SystemBackend().runCommand, systemctl)
}

func managerCgroup(ctx context.Context, runCommand func(context.Context, string, ...string) ([]byte, error), systemctl string) (string, error) {
	out, err := runCommand(ctx, systemctl, "--user", "show", "-.slice", "--property=ControlGroup")
	if err != nil {
		return "", fmt.Errorf("query manager cgroup: %w", err)
	}
	var cgroup string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		value, ok := strings.CutPrefix(line, "ControlGroup=")
		if !ok || value == "" {
			continue
		}
		if cgroup != "" {
			return "", ErrManagerCgroupMismatch
		}
		cgroup = value
	}
	if cgroup == "" {
		return "", ErrManagerCgroupMismatch
	}
	return cgroup, nil
}

// OwnedProfile synthesizes a catalog profile and its deterministic unit
// rendering from an owned launch draft. The manager cgroup and home directory
// are host facts only setup knows; control validation enforces the derived
// suffixes.
func OwnedProfile(d Draft, managerCgroup, home string) (control.WorkloadProfile, []byte, error) {
	if d.Binding == nil || d.Binding.Owned == nil {
		return control.WorkloadProfile{}, nil, errors.New("draft has no owned launch spec")
	}
	if d.ReferenceKind == "application" && d.Reference != d.Binding.Owned.Executable {
		return control.WorkloadProfile{}, nil, errors.New("selected application does not match the owned executable; select it again")
	}
	if err := validateDraftOwned(d.App, d.Binding.Owned); err != nil {
		return control.WorkloadProfile{}, nil, err
	}
	if d.App == "ollama" {
		if d.Model == "" || d.Binding.Instance == "" {
			return control.WorkloadProfile{}, nil, errors.New("owned ollama drafts require a model and instance")
		}
	} else if d.Binding.Instance == "" {
		return control.WorkloadProfile{}, nil, errors.New("owned drafts require an instance")
	}
	owned := control.OwnedLaunch{
		Executable:  d.Binding.Owned.Executable,
		ModelPath:   d.Binding.Owned.ModelPath,
		Port:        d.Binding.Owned.Port,
		CtxSize:     d.Binding.Owned.CtxSize,
		GPULayers:   d.Binding.Owned.GPULayers,
		MaxModelLen: d.Binding.Owned.MaxModelLen,
		Alias:       d.Binding.Owned.Alias,
	}
	model := d.Model
	if d.App != "ollama" {
		model = owned.Alias
		if model == "" {
			model = owned.ModelPath
		}
	}
	id := control.Workload(d.ID)
	unit := control.OwnedUnitName(d.App, d.Binding.Instance, id)
	endpoint := "http://127.0.0.1:" + strconv.Itoa(int(owned.Port))
	healthURL := endpoint + "/health"
	if d.App == "ollama" {
		healthURL = endpoint + "/api/tags"
	}
	profile := control.WorkloadProfile{
		ID:        id,
		Label:     d.Label,
		Adapter:   "systemd",
		Unit:      unit,
		HealthURL: healthURL,
		NativeModel: &control.NativeModel{
			Runtime:    d.App,
			Instance:   d.Binding.Instance,
			Model:      model,
			Endpoint:   endpoint,
			LaunchFile: filepath.Join(home, ".config/systemd/user", unit),
			Owned:      &owned,
		},
	}
	profile.Cgroup = gpuruntime.OwnedCgroup(managerCgroup, profile)
	raw, err := gpuruntime.RenderOwnedUnit(profile)
	if err != nil {
		return control.WorkloadProfile{}, nil, err
	}
	profile.NativeModel.LaunchSHA256 = digest(raw)
	if err := (control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{profile}}).Validate(); err != nil {
		return control.WorkloadProfile{}, nil, fmt.Errorf("owned profile synthesis: %w", err)
	}
	return profile, raw, nil
}

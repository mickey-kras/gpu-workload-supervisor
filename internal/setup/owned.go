package setup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/deployment"
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
	model := ownedProfileModel(d, owned)
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

func ownedProfileModel(d Draft, owned control.OwnedLaunch) string {
	if d.App == "ollama" {
		return d.Model
	}
	if owned.Alias != "" {
		return owned.Alias
	}
	return owned.ModelPath
}

// prepareOwnedBackstops upgrades only the candidate catalog, after proving the
// incoming spec/fingerprint. Accepted catalogs/files remain untouched until the
// existing owned-unit transaction applies the reviewed writes.
func prepareOwnedBackstops(request Request) (Request, error) {
	request.Catalog = request.Catalog.Clone()
	for _, p := range request.Catalog.Profiles {
		if p.NativeModel != nil && p.NativeModel.Owned != nil {
			if _, err := ownedRenderChecked(p); err != nil {
				return request, err
			}
		}
	}
	for i := range request.Catalog.Profiles {
		p := &request.Catalog.Profiles[i]
		if p.NativeModel == nil || p.NativeModel.Owned == nil {
			continue
		}
		p.NativeModel.Owned.Conflicts = request.Catalog.OwnedConflictUnits(*p)
		raw, err := gpuruntime.RenderOwnedUnit(*p)
		if err != nil {
			return request, err
		}
		p.NativeModel.LaunchSHA256 = digest(raw)
	}
	return request, nil
}

// An interrupted activation is an immutable transaction, including legacy unit
// hashes. Defer new dependencies until that exact activation has completed;
// otherwise recovery's equality and file-proof guards reject the original retry.
func prepareOwnedBackstopsForActivation(home string, request Request) (Request, error) {
	candidate, err := prepareOwnedBackstops(request)
	if err != nil {
		return request, err
	}
	root := filepath.Join(home, ".config/gpu-workload-supervisor")
	marker, err := deployment.Read(request.Profile.StatePath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return request, err
	}
	if marker.Maintenance {
		if err := requireMatchingActivation(root, request); err != nil {
			return request, err
		}
		return request, nil
	}
	journal, present, err := readOwnedUnitJournal(root)
	if err != nil {
		return request, err
	}
	if present && journal.StatePath == request.Profile.StatePath {
		// Unit writes precede activation.json and the maintenance fence. The
		// pending journal can therefore be the only proof of a legacy retry,
		// or coexist with a stale record from a previous completed activation.
		// Matching its complete write set only postpones dependency derivation;
		// it does not establish full-request identity or bypass recovery guards.
		if pendingOwnedWritesMatch(journal, request) {
			return request, nil
		}
		// A crash after the maintenance fence clears can still leave the
		// committed journal unfinished. Preserve an exact original retry then
		// too. A stale activation record must not block a different new plan.
		saved, err := privateRead(filepath.Join(root, "activation.json"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return request, err
		}
		if err == nil {
			var progress activation
			if err := json.Unmarshal(saved, &progress); err != nil {
				return request, err
			}
			original, _ := json.Marshal(progress.Request)
			requested, _ := json.Marshal(request)
			if digest(original) == digest(requested) {
				return request, nil
			}
		}
	}
	return candidate, nil
}

// planOwnedUnitWrites journals every unique requested owned unit, including
// unchanged units, before any filesystem effects. The caller has already
// verified each incoming spec/fingerprint through prepareOwnedBackstops.
func pendingOwnedWritesMatch(journal unitJournal, request Request) bool {
	if journal.Phase != ownedJournalPending || len(journal.Writes) == 0 {
		return false
	}
	writes := map[string]string{}
	for _, p := range request.Catalog.Profiles {
		if p.NativeModel != nil && p.NativeModel.Owned != nil {
			writes[p.Unit] = p.NativeModel.LaunchSHA256
		}
	}
	if len(writes) != len(journal.Writes) {
		return false
	}
	for unit, hash := range writes {
		if journal.Writes[unit] != hash {
			return false
		}
	}
	return true
}

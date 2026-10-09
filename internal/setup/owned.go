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

// Preserve recorded activation renders during recovery.
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
		saved, err := privateRead(filepath.Join(root, "activation.json"))
		if err != nil {
			return request, err
		}
		return selectRecordedActivation(saved, request, candidate)
	}
	journal, present, err := readOwnedUnitJournal(root)
	if err != nil {
		return request, err
	}
	if present && journal.StatePath == request.Profile.StatePath {
		for _, retry := range []Request{request, candidate} {
			matches, err := pendingOwnedRetryUnits(home, journal, retry)
			if err != nil {
				return request, err
			}
			if matches {
				return retry, nil
			}
		}
		saved, err := privateRead(filepath.Join(root, "activation.json"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return request, err
		}
		if err == nil {
			matched, err := selectRecordedActivation(saved, request, candidate)
			if err == nil {
				return matched, nil
			}
			var progress activation
			if decodeErr := json.Unmarshal(saved, &progress); decodeErr != nil {
				return request, decodeErr
			}
		}
	}
	return candidate, nil
}

// Both forms must pass the existing serialized-request equality contract.
func selectRecordedActivation(saved []byte, request, candidate Request) (Request, error) {
	var progress activation
	if err := json.Unmarshal(saved, &progress); err != nil {
		return request, err
	}
	recorded, _ := json.Marshal(progress.Request)
	for _, retry := range []Request{request, candidate} {
		encoded, _ := json.Marshal(retry)
		if digest(recorded) == digest(encoded) {
			return retry, nil
		}
	}
	return request, errors.New("interrupted activation must resume its original request")
}

// Pruned journals omit units whose writes never landed.
func pendingOwnedRetryUnits(home string, journal unitJournal, request Request) (bool, error) {
	if journal.Phase != ownedJournalPending || len(journal.Writes) == 0 {
		return false, nil
	}
	requested := map[string]control.WorkloadProfile{}
	for _, p := range request.Catalog.Profiles {
		if p.NativeModel != nil && p.NativeModel.Owned != nil {
			requested[p.Unit] = p
		}
	}
	for unit, hash := range journal.Writes {
		p, found := requested[unit]
		if !found || p.NativeModel.LaunchSHA256 != hash {
			return false, nil
		}
	}
	if len(requested) == len(journal.Writes) {
		return true, nil
	}
	accepted, err := ReadCatalog(context.Background(), request.Profile.StatePath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	for unit, p := range requested {
		if _, journaled := journal.Writes[unit]; journaled {
			continue
		}
		raw, fileErr := privateRead(filepath.Join(ownedUnitDirectory(home), unit))
		if fileErr != nil && !errors.Is(fileErr, os.ErrNotExist) {
			return false, fileErr
		}
		prior, found := ownedProfileForUnit(accepted.Catalog, unit)
		if !found {
			if accepted.Revision == "" && errors.Is(fileErr, os.ErrNotExist) {
				continue
			}
			return false, nil
		}
		proof, err := ownedRenderChecked(prior)
		if err != nil {
			return false, err
		}
		if digest(proof) != p.NativeModel.LaunchSHA256 || (fileErr == nil && digest(raw) != digest(proof)) {
			return false, nil
		}
	}
	return true, nil
}

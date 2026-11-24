package setup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mickey-kras/llm-workload-supervisor/internal/control"
	"github.com/mickey-kras/llm-workload-supervisor/internal/deployment"
	gpuruntime "github.com/mickey-kras/llm-workload-supervisor/internal/runtime"
)

var ErrManagerCgroupMismatch = errors.New("manager cgroup does not match the running user manager")

// ManagerCgroup asks the running user manager for its root control group so
// owned profiles derive cgroups from the host, not from request input. The
// selected systemctl executable is resolved and trusted before it is invoked.
func (b Backend) ManagerCgroup(ctx context.Context, systemctl string) (string, error) {
	return managerCgroup(ctx, b.runCommand, systemctl)
}

func managerCgroup(ctx context.Context, runCommand func(context.Context, string, ...string) ([]byte, error), systemctl string) (string, error) {
	path, err := exec.LookPath(systemctl)
	if err != nil {
		return "", fmt.Errorf("resolve systemctl executable: %w", err)
	}
	systemctl, err = gpuruntime.ValidateSystemctlExecutable(path)
	if err != nil {
		return "", fmt.Errorf("systemctl executable trust: %w", err)
	}
	out, err := runCommand(ctx, systemctl, "--user", "show", "--property=ControlGroup", "--", "-.slice")
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

// ownedTemplateError explains what to do when the managed launch cannot use
// the chat template file of the model folder. A missing model folder is
// not a missing template, so it gets its own message. A model without the
// file has its template only in a JSON file, so it needs an existing launch
// with --chat-template.
func ownedTemplateError(template string, err error) error {
	folder := filepath.Dir(template)
	if info, statErr := os.Stat(folder); statErr != nil || !info.IsDir() {
		return fmt.Errorf("the model folder %s does not exist or is not a folder: %w", folder, err)
	}
	if _, statErr := os.Lstat(template); errors.Is(statErr, fs.ErrNotExist) {
		return fmt.Errorf("the managed launch needs %s in the model folder: %w. For a model without it, use an existing launch with --chat-template, see docs/MODEL-CHECKS.md#result", ownedChatTemplateName, err)
	}
	return fmt.Errorf("the managed launch cannot use %s in the model folder: %w", ownedChatTemplateName, err)
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
	if d.Binding.Instance == "" {
		return control.WorkloadProfile{}, nil, errors.New("owned drafts require an instance")
	}
	owned := control.OwnedLaunch{
		Executable:      d.Binding.Owned.Executable,
		ModelPath:       d.Binding.Owned.ModelPath,
		Port:            d.Binding.Owned.Port,
		MaxModelLen:     d.Binding.Owned.MaxModelLen,
		Alias:           d.Binding.Owned.Alias,
		Limits:          d.Binding.Owned.Limits,
		Serving:         d.Binding.Owned.Serving,
		ArtifactSandbox: true,
	}
	model := ownedProfileModel(owned)
	id := control.Workload(d.ID)
	template := filepath.Join(owned.ModelPath, ownedChatTemplateName)
	templateSHA256, err := gpuruntime.ChatTemplateSHA256(template)
	if err != nil {
		return control.WorkloadProfile{}, nil, ownedTemplateError(template, err)
	}
	unit := control.OwnedUnitName(id)
	endpoint := "http://127.0.0.1:" + strconv.Itoa(int(owned.Port))
	profile := control.WorkloadProfile{
		ID:        id,
		Label:     d.Label,
		Adapter:   "systemd",
		Unit:      unit,
		HealthURL: endpoint + "/health",
		NativeModel: &control.NativeModel{
			Runtime:    d.App,
			Instance:   d.Binding.Instance,
			Model:      model,
			Endpoint:   endpoint,
			LaunchFile: filepath.Join(home, ".config/systemd/user", unit),
			Owned:      &owned,
			// The hardened launch pins the model's own chat template and reads
			// the API key from a private file that setup creates on apply.
			ChatTemplate:       template,
			ChatTemplateSHA256: templateSHA256,
			EnvironmentFile:    OwnedEnvironmentFile(home, id),
		},
	}
	profile.Cgroup = gpuruntime.OwnedCgroup(managerCgroup, profile)
	raw, err := gpuruntime.RenderOwnedUnit(profile)
	if err != nil {
		return control.WorkloadProfile{}, nil, err
	}
	profile.NativeModel.LaunchSHA256 = digest(raw)
	single := control.Catalog{Profiles: []control.WorkloadProfile{profile}}
	single.Version = control.LowestCatalogVersion(single)
	if err := single.Validate(); err != nil {
		return control.WorkloadProfile{}, nil, fmt.Errorf("owned profile synthesis: %w", err)
	}
	return profile, raw, nil
}

// ownedChatTemplateName is the chat template file in the model folder that an
// owned vLLM launch pins.
const ownedChatTemplateName = "chat_template.jinja"

// OwnedEnvironmentFile is the private file that holds the vLLM API key of an owned
// workload.
func OwnedEnvironmentFile(home string, id control.Workload) string {
	return filepath.Join(home, ".config/gpu-workload-supervisor/vllm", string(id)+".env")
}

func ownedProfileModel(owned control.OwnedLaunch) string {
	if owned.Alias != "" {
		return owned.Alias
	}
	return owned.ModelPath
}

// prepareOwnedBackstops upgrades only the candidate catalog, after proving the
// incoming spec/fingerprint. Accepted catalogs/files remain untouched until the
// existing owned-unit transaction applies the reviewed writes.
func prepareOwnedBackstops(request Request) (Request, error) {
	if err := verifyOwnedBackstopRequest(request); err != nil {
		return request, err
	}
	return deriveOwnedBackstops(request)
}

func verifyOwnedBackstopRequest(request Request) error {
	// Normalization must not turn an unknown version into a known one.
	if !control.SupportedCatalogVersion(request.Catalog.Version) {
		return control.ErrUnsupportedCatalogVersion
	}
	for _, p := range request.Catalog.Profiles {
		if p.NativeModel == nil || p.NativeModel.Owned == nil {
			continue
		}
		if _, err := ownedRenderChecked(p); err != nil {
			return err
		}
	}
	return nil
}

func deriveOwnedBackstops(request Request) (Request, error) {
	request.Catalog = request.Catalog.Clone()
	for i := range request.Catalog.Profiles {
		p := &request.Catalog.Profiles[i]
		if p.NativeModel == nil || p.NativeModel.Owned == nil {
			continue
		}
		p.NativeModel.Owned.Conflicts = request.Catalog.OwnedConflictUnits(*p)
		p.NativeModel.Owned.ArtifactSandbox = p.NativeModel.Owned.ArtifactSandbox || p.NativeModel.Hardened()
		raw, err := gpuruntime.RenderOwnedUnit(*p)
		if err != nil {
			return request, err
		}
		p.NativeModel.LaunchSHA256 = digest(raw)
	}
	// A resumed activation can keep its recorded request, which
	// selectRecordedActivation chooses after the candidate is rendered.
	request.Catalog.Version = control.LowestCatalogVersion(request.Catalog)
	return request, nil
}

// Preserve recorded activation renders during recovery.
func prepareOwnedBackstopsForActivation(home string, request Request) (Request, error) {
	if err := verifyOwnedBackstopRequest(request); err != nil {
		return request, err
	}
	// A historical activation can prove its original rendering even when
	// a fresh sandboxed candidate cannot be rendered.
	candidate, candidateErr := deriveOwnedBackstops(request)
	retries := []Request{request}
	if candidateErr == nil {
		retries = append(retries, candidate)
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
		return selectRecordedActivation(saved, retries)
	}
	journal, present, err := readOwnedUnitJournal(root)
	if err != nil {
		return request, err
	}
	if present && journal.StatePath == request.Profile.StatePath {
		retry, matched, err := prepareOwnedBackstopsForJournal(home, root, journal, retries)
		if err != nil || matched {
			return retry, err
		}
	}
	return candidate, candidateErr
}

func prepareValidatedOwnedBackstopsForActivation(home string, request Request) (Request, error) {
	request, err := prepareOwnedBackstopsForActivation(home, request)
	if err != nil {
		return request, err
	}
	return request, Validate(request)
}

// Without a maintenance fence, unit proofs precede the saved request check.
func prepareOwnedBackstopsForJournal(home, root string, journal unitJournal, retries []Request) (Request, bool, error) {
	for _, retry := range retries {
		matches, err := pendingOwnedRetryUnits(home, journal, retry)
		if err != nil || matches {
			return retry, matches, err
		}
	}
	saved, err := privateRead(filepath.Join(root, "activation.json"))
	if errors.Is(err, os.ErrNotExist) {
		return Request{}, false, nil
	}
	if err != nil {
		return Request{}, false, err
	}
	return matchingRecordedActivation(saved, retries)
}

func selectRecordedActivation(saved []byte, retries []Request) (Request, error) {
	request, matched, err := matchingRecordedActivation(saved, retries)
	if err == nil && !matched {
		return Request{}, errors.New("interrupted activation must resume its original request")
	}
	return request, err
}

// Both forms must pass the existing serialized-request equality contract.
func matchingRecordedActivation(saved []byte, retries []Request) (Request, bool, error) {
	var progress activation
	if err := json.Unmarshal(saved, &progress); err != nil {
		return Request{}, false, err
	}
	recorded, _ := json.Marshal(progress.Request)
	for _, retry := range retries {
		encoded, _ := json.Marshal(retry)
		if digest(recorded) == digest(encoded) {
			return retry, true, nil
		}
	}
	return Request{}, false, nil
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
	return pendingOwnedRetryPrunedUnits(home, journal, requested, accepted.Catalog)
}

func pendingOwnedRetryPrunedUnits(home string, journal unitJournal, requested map[string]control.WorkloadProfile, accepted control.Catalog) (bool, error) {
	for unit := range requested {
		if _, journaled := journal.Writes[unit]; journaled {
			continue
		}
		matches, err := pendingOwnedRetryPrunedUnit(home, unit, accepted)
		if err != nil || !matches {
			return false, err
		}
	}
	return true, nil
}

// A pruned write may be absent or still have the accepted catalog's bytes.
func pendingOwnedRetryPrunedUnit(home, unit string, accepted control.Catalog) (bool, error) {
	raw, fileErr := privateRead(filepath.Join(ownedUnitDirectory(home), unit))
	if fileErr != nil && !errors.Is(fileErr, os.ErrNotExist) {
		return false, fileErr
	}
	if errors.Is(fileErr, os.ErrNotExist) {
		return true, nil
	}
	prior, found := ownedProfileForUnit(accepted, unit)
	if !found {
		return false, nil
	}
	proof, err := ownedRenderChecked(prior)
	if err != nil {
		return false, err
	}
	return digest(raw) == digest(proof), nil
}

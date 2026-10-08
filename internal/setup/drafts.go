package setup

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/lock"
)

// Drafts record user choices only. They never enter the executable catalog.
type DraftBinding struct {
	Unit       string            `json:"unit,omitempty"`
	Cgroup     string            `json:"cgroup,omitempty"`
	HealthURL  string            `json:"healthURL,omitempty"`
	Instance   string            `json:"instance,omitempty"`
	Model      string            `json:"model,omitempty"`
	LaunchFile string            `json:"launchFile,omitempty"`
	Owned      *DraftOwnedLaunch `json:"owned,omitempty"`
}

// DraftOwnedLaunch mirrors control.OwnedLaunch for the draft surface.
type DraftOwnedLaunch struct {
	ModelPath   string `json:"modelPath,omitempty"`
	Port        uint16 `json:"port"`
	CtxSize     uint32 `json:"ctxSize,omitempty"`
	GPULayers   uint32 `json:"gpuLayers,omitempty"`
	MaxModelLen uint32 `json:"maxModelLen,omitempty"`
	Alias       string `json:"alias,omitempty"`
}

type Draft struct {
	ID            string        `json:"id"`
	Label         string        `json:"label"`
	App           string        `json:"app"`
	Endpoint      string        `json:"endpoint,omitempty"`
	Reference     string        `json:"reference,omitempty"`
	ReferenceKind string        `json:"referenceKind,omitempty"`
	Model         string        `json:"model,omitempty"`
	Binding       *DraftBinding `json:"binding,omitempty"`
}
type DraftSnapshot struct {
	Version  int     `json:"version"`
	Revision string  `json:"revision"`
	Drafts   []Draft `json:"drafts"`
}
type DraftRequest struct {
	Version          int     `json:"version"`
	ExpectedRevision string  `json:"expectedRevision"`
	Drafts           []Draft `json:"drafts"`
}

func ReadDrafts(home string) (DraftSnapshot, error) {
	result := DraftSnapshot{Version: 1, Drafts: []Draft{}}
	data, err := privateRead(filepath.Join(home, ".config/gpu-workload-supervisor/drafts.json"))
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	if err = json.Unmarshal(data, &result); err != nil {
		return result, err
	}
	if err = validateDrafts(result.Version, result.Drafts); err != nil {
		return result, err
	}
	result.Revision = digest(data)
	return result, nil
}
func SaveDrafts(home string, request DraftRequest) (DraftSnapshot, error) {
	if err := validateDrafts(request.Version, request.Drafts); err != nil {
		return DraftSnapshot{}, err
	}
	root := filepath.Join(home, ".config/gpu-workload-supervisor")
	if err := mkdirTrusted(root); err != nil {
		return DraftSnapshot{}, err
	}
	gate, err := lock.TryAcquire(filepath.Join(root, "drafts.lock"))
	if err != nil {
		return DraftSnapshot{}, err
	}
	defer gate.Close()
	current, err := ReadDrafts(home)
	if err != nil {
		return current, err
	}
	if current.Revision != request.ExpectedRevision {
		return current, errors.New("drafts changed; refresh before saving")
	}
	if request.Drafts == nil {
		request.Drafts = []Draft{}
	}
	result := DraftSnapshot{Version: 1, Drafts: request.Drafts}
	if err = writeJSON(filepath.Join(root, "drafts.json"), result); err != nil {
		return result, err
	}
	return ReadDrafts(home)
}
func validateDrafts(version int, drafts []Draft) error {
	if version != 1 || len(drafts) > 32 {
		return errors.New("unsupported draft version or too many drafts")
	}
	ids := map[string]bool{}
	for _, d := range drafts {
		if err := validateDraft(d, ids); err != nil {
			return err
		}
	}
	return nil
}
func validateDraft(d Draft, ids map[string]bool) error {
	if !control.ValidWorkloadID(control.Workload(d.ID)) || ids[d.ID] || !control.ValidWorkloadLabel(d.Label) {
		return errors.New("invalid or duplicate draft identity")
	}
	ids[d.ID] = true
	switch d.App {
	case "comfyui", "ollama", appLlamaCPP, "vllm":
	default:
		return errors.New("unsupported draft application")
	}
	if err := validateDraftBinding(d.Binding); err != nil {
		return err
	}
	if err := validateDraftSynthesis(d); err != nil {
		return err
	}
	if d.Endpoint != "" && d.Reference != "" {
		return errors.New("choose an endpoint or file location")
	}
	if len(d.Endpoint) > 2048 || len(d.Reference) > 4096 || len(d.Model) > 1024 {
		return errors.New("draft value too long")
	}
	if err := validateDraftReference(d); err != nil {
		return err
	}
	if d.App == "comfyui" && d.Model != "" {
		return errors.New("ComfyUI workflows select models")
	}
	return nil
}
func validateDraftBinding(binding *DraftBinding) error {
	if binding == nil {
		return nil
	}
	for _, value := range []string{binding.Unit, binding.Cgroup, binding.HealthURL, binding.Instance, binding.Model, binding.LaunchFile} {
		if len(value) > 4096 {
			return errors.New("draft binding value too long")
		}
	}
	return nil
}

// validateDraftOwned enforces complete model choices before profile synthesis.
func validateDraftOwned(app string, o *DraftOwnedLaunch) error {
	return validateOwnedOptions(app, o, false)
}

// Drafts can defer model selection while retaining otherwise valid launch choices.
func validateOwnedOptions(app string, o *DraftOwnedLaunch, allowMissingPath bool) error {
	if o == nil {
		return nil
	}
	if app != "ollama" && app != appLlamaCPP && app != "vllm" {
		return errors.New("owned launches are only supported for native model runtimes")
	}
	if o.Port < 1024 {
		return errors.New("owned launch port must be an unprivileged TCP port")
	}
	validModelPath := validDraftModelPath(o.ModelPath, allowMissingPath)
	switch app {
	case "ollama":
		if o.ModelPath != "" || o.CtxSize != 0 || o.GPULayers != 0 || o.MaxModelLen != 0 || o.Alias != "" {
			return errors.New("ollama owned launches accept only a port")
		}
	case appLlamaCPP:
		if o.MaxModelLen != 0 || !validModelPath {
			return errors.New("llama.cpp owned launches require an absolute model file")
		}
	case "vllm":
		if o.CtxSize != 0 || o.GPULayers != 0 || !validModelPath {
			return errors.New("vllm owned launches require an absolute model directory")
		}
	}
	return validateDraftLaunchGrammar(o)
}

func validDraftModelPath(path string, allowMissing bool) bool {
	if path == "" {
		return allowMissing
	}
	return filepath.IsAbs(path) && filepath.Clean(path) == path
}

func validateDraftLaunchGrammar(o *DraftOwnedLaunch) error {
	// Same grammar gate as catalog validation: values that cannot survive the
	// unit command grammar must fail at save time, not at apply time.
	if o.ModelPath != "" && !control.LaunchGrammarExpressible(o.ModelPath) {
		return errors.New("owned launch values must be expressible in the unit command grammar")
	}
	if o.Alias != "" && !control.LaunchGrammarExpressible(o.Alias) {
		return errors.New("owned launch values must be expressible in the unit command grammar")
	}
	return nil
}

func validateDraftReference(d Draft) error {
	if d.Reference == "" {
		if d.ReferenceKind != "" {
			return errors.New("draft reference kind requires path")
		}
		return nil
	}
	if !filepath.IsAbs(d.Reference) || filepath.Clean(d.Reference) != d.Reference {
		return errors.New("draft path must be absolute and clean")
	}
	switch d.ReferenceKind {
	case "application", "application-directory", "configuration", "model-file", "model-directory":
		return nil
	}
	return errors.New("invalid draft reference kind")
}

func validateDraftSynthesis(d Draft) error {
	if d.Binding == nil {
		return nil
	}
	if err := validateOwnedOptions(d.App, d.Binding.Owned, true); err != nil {
		return err
	}
	// Validate supplied identity; readiness still requires a model in OwnedProfile.
	if d.Binding.Owned == nil {
		return nil
	}
	if d.Binding.Instance == "" {
		return errors.New("owned drafts require an instance")
	}
	if !control.ValidInstanceID(d.Binding.Instance) {
		return errors.New("owned draft instance must be a valid workload identifier")
	}
	if d.App == "ollama" && d.Model != "" && !control.ValidNativeModelIdentity(d.App, d.Model) {
		return errors.New("invalid native model identity")
	}
	return nil
}

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
	Unit       string `json:"unit,omitempty"`
	Cgroup     string `json:"cgroup,omitempty"`
	HealthURL  string `json:"healthURL,omitempty"`
	Instance   string `json:"instance,omitempty"`
	Model      string `json:"model,omitempty"`
	LaunchFile string `json:"launchFile,omitempty"`
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
	case "application", "configuration", "model-file", "model-directory":
		return nil
	}
	return errors.New("invalid draft reference kind")
}

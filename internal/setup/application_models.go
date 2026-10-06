package setup

import (
	"context"
	"errors"
	"strings"
)

const maxDiscoveryModels = 4096

func inventoryBound(ctx context.Context, count int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if count > maxDiscoveryModels {
		return errors.New("model inventory exceeds 4096 entries")
	}
	return nil
}

func (p applicationHTTP) comfy(ctx context.Context, result *ApplicationCandidate) error {
	var body struct {
		System *struct {
			Version string `json:"comfyui_version"`
		} `json:"system"`
	}
	if err := p.get(ctx, "/system_stats", &body); err != nil {
		return err
	}
	if body.System == nil || body.System.Version == "" {
		return errors.New("missing ComfyUI version")
	}
	result.Version = body.System.Version
	result.InstanceStatus = "available"
	result.InventoryStatus = "not-applicable"
	result.NextStep = "ComfyUI workflows select models. Verify lifecycle control for this instance."
	return nil
}
func (p applicationHTTP) version(ctx context.Context, path string, result *ApplicationCandidate) error {
	var body struct {
		Version string `json:"version"`
	}
	if err := p.get(ctx, path, &body); err != nil {
		return err
	}
	if body.Version == "" {
		return errors.New("missing application version")
	}
	result.Version = body.Version
	result.InstanceStatus = "available"
	return nil
}

type ollamaModel struct {
	Name        string `json:"name"`
	Model       string `json:"model"`
	RemoteHost  string `json:"remote_host"`
	RemoteModel string `json:"remote_model"`
}

func (m ollamaModel) id() string {
	if m.Model != "" {
		return m.Model
	}
	return m.Name
}
func (p applicationHTTP) ollama(ctx context.Context, result *ApplicationCandidate) error {
	if err := p.version(ctx, "/api/version", result); err != nil {
		return err
	}
	models, err := p.ollamaInventory(ctx)
	if err != nil {
		return err
	}
	loadedIDs, loadedKnown, err := p.ollamaLoadedIDs(ctx)
	if err != nil {
		return err
	}
	return ollamaCandidates(ctx, models, loadedIDs, loadedKnown, result)
}
func (p applicationHTTP) ollamaInventory(ctx context.Context) ([]ollamaModel, error) {
	var tags struct {
		Models []ollamaModel `json:"models"`
	}
	if err := p.get(ctx, "/api/tags", &tags); err != nil {
		return nil, err
	}
	if err := inventoryBound(ctx, len(tags.Models)); err != nil {
		return nil, err
	}
	if tags.Models == nil {
		return nil, errors.New("missing available model inventory")
	}
	for _, model := range tags.Models {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !validModelID(model.id()) {
			return nil, errors.New("invalid model identity")
		}
	}
	return tags.Models, nil
}
func (p applicationHTTP) ollamaLoadedIDs(ctx context.Context) (map[string]bool, bool, error) {
	var ps struct {
		Models []ollamaModel `json:"models"`
	}
	loadedKnown := p.get(ctx, "/api/ps", &ps) == nil && ps.Models != nil
	if err := inventoryBound(ctx, len(ps.Models)); err != nil {
		return nil, false, err
	}
	loadedIDs := make(map[string]bool, len(ps.Models))
	for _, model := range ps.Models {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		id := model.id()
		if !validModelID(id) || loadedIDs[id] {
			loadedKnown = false
		}
		loadedIDs[id] = true
	}
	return loadedIDs, loadedKnown, nil
}
func ollamaCandidates(ctx context.Context, models []ollamaModel, loadedIDs map[string]bool, loadedKnown bool, result *ApplicationCandidate) error {
	seen := map[string]bool{}
	for _, model := range models {
		if err := ctx.Err(); err != nil {
			return err
		}
		id := model.id()
		if seen[id] {
			return errors.New("duplicate model identity")
		}
		seen[id] = true
		result.Models = append(result.Models, ModelCandidate{ID: id, Label: id, Source: "inventory", Loaded: ollamaLoaded(id, loadedIDs, loadedKnown), Locality: ollamaLocality(model)})
	}
	result.InventoryStatus = "available"
	if !loadedKnown {
		result.NextStep = "Available inventory found; loaded-model observation is unavailable. Verify lifecycle control."
	}
	return nil
}
func ollamaLoaded(id string, loadedIDs map[string]bool, loadedKnown bool) string {
	if !loadedKnown {
		return "unknown"
	}
	if loadedIDs[id] {
		return "yes"
	}
	return "no"
}
func ollamaLocality(model ollamaModel) string {
	if model.RemoteHost != "" || model.RemoteModel != "" {
		return "non-local"
	}
	return "local"
}

type servedModel struct {
	ID      string `json:"id"`
	Root    string `json:"root"`
	OwnedBy string `json:"owned_by"`
	Path    string `json:"path"`
	Status  *struct {
		Value string `json:"value"`
	} `json:"status"`
}

func validModelID(id string) bool {
	return strings.TrimSpace(id) != "" && len(id) <= 4096 && !strings.ContainsAny(id, "\x00\r\n")
}
func (p applicationHTTP) llama(ctx context.Context, result *ApplicationCandidate) error {
	var body struct {
		Data []servedModel `json:"data"`
	}
	err := p.get(ctx, "/models", &body)
	if err != nil {
		var status *probeHTTPError
		if !errors.As(err, &status) || status.code != 404 {
			return err
		}
		if err := p.get(ctx, "/v1/models", &body); err != nil {
			return err
		}
		return llamaServedCandidates(ctx, body.Data, result)
	}
	if len(body.Data) == 0 {
		return errors.New("no llama.cpp identity evidence")
	}
	if err := inventoryBound(ctx, len(body.Data)); err != nil {
		return err
	}
	if body.Data[0].Status == nil {
		return llamaServedCandidates(ctx, body.Data, result)
	}
	return llamaNativeCandidates(ctx, body.Data, result)
}
func llamaLoadedState(status string) (string, error) {
	switch status {
	case "loaded":
		return "yes", nil
	case "unloaded":
		return "no", nil
	case "loading", "sleeping", "downloading":
		return "unknown", nil
	}
	return "", errors.New("unsupported model status")
}
func llamaNativeCandidates(ctx context.Context, models []servedModel, result *ApplicationCandidate) error {
	seen := map[string]bool{}
	for _, model := range models {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !validModelID(model.ID) || model.Status == nil || seen[model.ID] {
			return errors.New("invalid native model candidate")
		}
		seen[model.ID] = true
		loaded, err := llamaLoadedState(model.Status.Value)
		if err != nil {
			return err
		}
		locality := "unknown"
		if model.Path != "" {
			locality = "local"
		}
		result.Models = append(result.Models, ModelCandidate{ID: model.ID, Label: model.ID, Source: "inventory", Loaded: loaded, Locality: locality})
	}
	result.InstanceStatus = "available"
	result.InventoryStatus = "available"
	return nil
}
func llamaServedCandidates(ctx context.Context, models []servedModel, result *ApplicationCandidate) error {
	if len(models) == 0 {
		return errors.New("no llama.cpp identity evidence")
	}
	if err := inventoryBound(ctx, len(models)); err != nil {
		return err
	}
	for _, model := range models {
		if err := ctx.Err(); err != nil {
			return err
		}
		if model.OwnedBy != "llamacpp" || model.Status != nil {
			return errors.New("unrecognized or mixed llama.cpp serving response")
		}
	}
	return servedCandidates(ctx, models, result)
}

func (p applicationHTTP) vllm(ctx context.Context, result *ApplicationCandidate) error {
	if err := p.version(ctx, "/version", result); err != nil {
		return err
	}
	var body struct {
		Data []servedModel `json:"data"`
	}
	if err := p.get(ctx, "/v1/models", &body); err != nil {
		return err
	}
	if body.Data == nil {
		return errors.New("missing serving identities")
	}
	return servedCandidates(ctx, body.Data, result)
}
func servedCandidates(ctx context.Context, models []servedModel, result *ApplicationCandidate) error {
	if err := inventoryBound(ctx, len(models)); err != nil {
		return err
	}
	// IDs are serving aliases, not evidence of different model files. Collapse
	// aliases only when the API explicitly identifies the same root.
	indices := map[string]int{}
	seen := map[string]bool{}
	for _, model := range models {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !validModelID(model.ID) || seen[model.ID] {
			return errors.New("invalid or duplicate serving identity")
		}
		seen[model.ID] = true
		key := "root:" + model.Root
		if model.Root == "" {
			key = "alias:" + model.ID
		}
		if index, ok := indices[key]; ok {
			result.Models[index].Aliases = append(result.Models[index].Aliases, model.ID)
			continue
		}
		indices[key] = len(result.Models)
		result.Models = append(result.Models, ModelCandidate{ID: model.ID, Label: model.ID, Source: "served", Loaded: "unknown", Locality: "unknown", Aliases: []string{model.ID}})
	}
	result.InstanceStatus = "available"
	result.InventoryStatus = "available"
	result.NextStep = "Serving identities observed; aliases may share one model. Select existing launch settings and verify lifecycle control."
	return nil
}

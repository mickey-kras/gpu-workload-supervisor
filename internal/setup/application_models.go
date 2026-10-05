package setup

import (
	"context"
	"errors"
	"strings"
)

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
	var tags struct {
		Models []ollamaModel `json:"models"`
	}
	if err := p.get(ctx, "/api/tags", &tags); err != nil {
		return err
	}
	if tags.Models == nil {
		return errors.New("missing available model inventory")
	}
	for _, model := range tags.Models {
		if !validModelID(model.id()) {
			return errors.New("invalid model identity")
		}
	}
	var ps struct {
		Models []ollamaModel `json:"models"`
	}
	loadedKnown := p.get(ctx, "/api/ps", &ps) == nil && ps.Models != nil
	for _, model := range ps.Models {
		if !validModelID(model.id()) {
			loadedKnown = false
		}
	}
	seen := map[string]bool{}
	for _, model := range tags.Models {
		id := model.id()
		if seen[id] {
			return errors.New("duplicate model identity")
		}
		seen[id] = true
		loaded := "unknown"
		if loadedKnown {
			loaded = "no"
			for _, active := range ps.Models {
				if active.id() == id {
					loaded = "yes"
				}
			}
		}
		locality := "local"
		if model.RemoteHost != "" || model.RemoteModel != "" {
			locality = "non-local"
		}
		result.Models = append(result.Models, ModelCandidate{ID: id, Label: id, Source: "inventory", Loaded: loaded, Locality: locality})
	}
	result.InventoryStatus = "available"
	if !loadedKnown {
		result.NextStep = "Available inventory found; loaded-model observation is unavailable. Verify lifecycle control."
	}
	return nil
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
		if body.Data == nil {
			return errors.New("missing served model identities")
		}
		for _, model := range body.Data {
			if model.OwnedBy != "llamacpp" {
				return errors.New("unrecognized llama.cpp model response")
			}
		}
		return servedCandidates(body.Data, result)
	}
	if body.Data == nil {
		return errors.New("missing native model inventory")
	}
	seen := map[string]bool{}
	for _, model := range body.Data {
		if !validModelID(model.ID) || model.Status == nil || seen[model.ID] {
			return errors.New("invalid native model candidate")
		}
		seen[model.ID] = true
		loaded := "unknown"
		switch model.Status.Value {
		case "loaded":
			loaded = "yes"
		case "unloaded":
			loaded = "no"
		case "loading", "sleeping", "downloading":
		default:
			return errors.New("unsupported model status")
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
	return servedCandidates(body.Data, result)
}
func servedCandidates(models []servedModel, result *ApplicationCandidate) error {
	// IDs are serving aliases, not evidence of different model files. Collapse
	// aliases only when the API explicitly identifies the same root.
	indices := map[string]int{}
	seen := map[string]bool{}
	for _, model := range models {
		if !validModelID(model.ID) || seen[model.ID] {
			return errors.New("invalid or duplicate serving identity")
		}
		seen[model.ID] = true
		key := model.Root
		if key == "" {
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

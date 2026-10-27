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

type servedModel struct {
	ID   string `json:"id"`
	Root string `json:"root"`
}

func validModelID(id string) bool {
	return strings.TrimSpace(id) != "" && len(id) <= 4096 && !strings.ContainsAny(id, "\x00\r\n")
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

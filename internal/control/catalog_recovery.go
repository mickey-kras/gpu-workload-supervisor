package control

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/strictjson"
)

func (p WorkloadProfile) RemovedRuntime() bool {
	return p.NativeModel != nil && (p.NativeModel.Runtime == "ollama" || p.NativeModel.Runtime == "llama.cpp")
}

// DecodeStoredCatalog permits removal of legacy profiles, never new execution.
func DecodeStoredCatalog(raw []byte) (Catalog, error) {
	if len(raw) > MaxCatalogBytes {
		return Catalog{}, ErrCatalogTooLarge
	}
	decoded, originalErr := DecodeCatalogBytes(raw)
	if originalErr == nil {
		return decoded, nil
	}
	if err := strictjson.Check(json.NewDecoder(bytes.NewReader(raw))); err != nil {
		return Catalog{}, err
	}
	var wire struct {
		Version  int               `json:"version"`
		Disabled bool              `json:"disabled,omitempty"`
		Profiles []json.RawMessage `json:"profiles"`
	}
	if err := decodeRecoveryJSON(raw, &wire); err != nil {
		return Catalog{}, err
	}
	original := Catalog{Version: wire.Version, Disabled: wire.Disabled}
	removed := false
	for _, data := range wire.Profiles {
		p, err := decodeStoredProfile(data)
		if err != nil {
			return Catalog{}, err
		}
		removed = removed || p.RemovedRuntime()
		original.Profiles = append(original.Profiles, p)
	}
	if !removed {
		return Catalog{}, originalErr
	}
	if err := validateRecoveryCatalog(original); err != nil {
		return Catalog{}, err
	}
	return original, nil
}

func decodeRecoveryJSON[T any](raw []byte, target *T) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("catalog must contain one JSON value")
	}
	return nil
}

func decodeStoredProfile(raw []byte) (WorkloadProfile, error) {
	var wire struct {
		WorkloadProfile
		NativeModel json.RawMessage `json:"nativeModel,omitempty"`
	}
	if err := decodeRecoveryJSON(raw, &wire); err != nil {
		return WorkloadProfile{}, err
	}
	p := wire.WorkloadProfile
	if len(wire.NativeModel) == 0 || bytes.Equal(wire.NativeModel, []byte("null")) {
		return p, p.validate()
	}
	// Probe only to select the retired schema; both paths then decode strictly.
	var identity struct {
		Runtime string `json:"runtime"`
	}
	if err := json.Unmarshal(wire.NativeModel, &identity); err != nil {
		return p, err
	}
	p.NativeModel = &NativeModel{Runtime: identity.Runtime}
	if !p.RemovedRuntime() {
		if err := decodeRecoveryJSON(wire.NativeModel, p.NativeModel); err != nil {
			return p, err
		}
		return p, p.validate()
	}
	n, err := decodeRemovedNative(wire.NativeModel)
	if err != nil {
		return p, err
	}
	p.NativeModel = &n
	return p, validateRemovedProfile(p)
}

func decodeRemovedNative(raw []byte) (NativeModel, error) {
	var wire struct {
		NativeModel
		Owned *struct {
			OwnedLaunch
			CtxSize   uint32 `json:"ctxSize,omitempty"`
			GPULayers uint32 `json:"gpuLayers,omitempty"`
		} `json:"owned,omitempty"`
	}
	if err := decodeRecoveryJSON(raw, &wire); err != nil {
		return NativeModel{}, err
	}
	n := wire.NativeModel
	if wire.Owned != nil {
		n.Owned = &wire.Owned.OwnedLaunch
	}
	return n, nil
}

func validateRemovedProfile(p WorkloadProfile) error {
	n := p.NativeModel
	if !ValidInstanceID(n.Instance) || !ValidNativeModelIdentity(n.Model) || n.GPUUUID != "" && !ValidGPUUUID(n.GPUUUID) {
		return errors.New("invalid removed runtime identity")
	}
	if err := validateLaunchEvidence(n.Endpoint, n.LaunchFile, n.LaunchSHA256); err != nil {
		return err
	}
	if err := ValidateLaunchSources(n.LaunchFile, n.DropIns); err != nil {
		return err
	}
	if p.Adapter != "systemd" || p.LaunchBinding != nil || p.Trainer != nil {
		return errors.New("invalid removed runtime binding")
	}
	if err := p.validateOwnedPlacement(); err != nil {
		return err
	}
	p.NativeModel = nil
	return p.validate()
}

func validateRecoveryCatalog(c Catalog) error {
	if len(c.Profiles) > 32 {
		return errors.New("catalog requires 1 to 32 profiles")
	}
	if err := c.validateSize(); err != nil {
		return err
	}
	if err := c.validateOwnedConflicts(); err != nil {
		return err
	}
	observation := Catalog{Version: c.Version, Disabled: c.Disabled}
	seen := map[string]WorkloadProfile{}
	ids := map[Workload]bool{}
	for _, p := range c.Profiles {
		if c.Version == 1 && p.NativeModel != nil && p.NativeModel.Owned != nil {
			return ErrOwnedRequiresV2
		}
		if ids[p.ID] {
			return errors.New("duplicate workload ID")
		}
		ids[p.ID] = true
		if prior, ok := seen[p.Unit]; ok && legacySharedUnit(prior, p) {
			continue
		}
		seen[p.Unit] = p
		p.NativeModel = nil
		observation.Profiles = append(observation.Profiles, p)
	}
	return observation.Validate()
}

func legacySharedUnit(a, b WorkloadProfile) bool {
	if !a.RemovedRuntime() || !b.RemovedRuntime() || a.NativeModel.Runtime != "ollama" || b.NativeModel.Runtime != "ollama" || a.Unit != b.Unit || a.Cgroup != b.Cgroup || a.NativeModel.Model == b.NativeModel.Model {
		return false
	}
	x, y := *a.NativeModel, *b.NativeModel
	x.Model = ""
	y.Model = ""
	return reflect.DeepEqual(x, y)
}

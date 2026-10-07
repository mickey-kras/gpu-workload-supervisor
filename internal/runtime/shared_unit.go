package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

// Profiles sharing one Ollama unit switch at model level: the unit keeps
// running, the outgoing model is unloaded through the API and the target
// model is preloaded. GPU release evidence is the Ollama loaded-model list
// while the unit runs, and cgroup emptiness once it is dead.
func (m *SystemdManager) sharedUnitProfile(p control.WorkloadProfile) bool {
	if p.NativeModel == nil || p.NativeModel.Runtime != "ollama" {
		return false
	}
	for _, q := range m.config.Catalog.Profiles {
		if q.ID != p.ID && control.SharedOllamaUnit(p, q) {
			return true
		}
	}
	return false
}

func ollamaLoadedModels(b []byte) (map[string]bool, error) {
	var payload modelListPayload
	if err := json.Unmarshal(b, &payload); err != nil {
		return nil, ErrModelIdentity
	}
	loaded := map[string]bool{}
	for _, entry := range payload.Models {
		if entry.Name == "" || entry.Model == "" {
			return nil, ErrModelIdentity
		}
		loaded[entry.Name] = true
		loaded[entry.Model] = true
	}
	return loaded, nil
}

func (m *SystemdManager) loadedOllamaModels(ctx context.Context, n control.NativeModel) (map[string]bool, error) {
	b, err := m.fetchModelList(ctx, n)
	if err != nil {
		return nil, err
	}
	return ollamaLoadedModels(b)
}

func (m *SystemdManager) ollamaModelLoaded(ctx context.Context, n control.NativeModel) (bool, error) {
	loaded, err := m.loadedOllamaModels(ctx, n)
	if err != nil {
		return false, err
	}
	return loaded[n.ComparisonModel()], nil
}

func (m *SystemdManager) setOllamaKeepAlive(ctx context.Context, endpoint, model string, keepAlive int) error {
	b, _ := json.Marshal(struct {
		Model     string `json:"model"`
		Stream    bool   `json:"stream"`
		KeepAlive int    `json:"keep_alive"`
	}{Model: model, KeepAlive: keepAlive})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/api/generate", bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return m.checkHTTPResponse(req, "ollama model lifetime")
}

// keep_alive=0 unloads asynchronously server-side, so callers poll /api/ps
// until pending reports the goal state. Identity failures are permanent;
// only transient transport errors are retried.
func (m *SystemdManager) pollOllamaModels(ctx context.Context, n control.NativeModel, pending func(map[string]bool) bool) error {
	var lastErr error
	for {
		loaded, err := m.loadedOllamaModels(ctx, n)
		if errors.Is(err, ErrModelIdentity) {
			return err
		}
		if err != nil {
			lastErr = err
		} else if !pending(loaded) {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.Join(ErrUnloadUnverified, ctx.Err(), lastErr)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (m *SystemdManager) stopSharedOllama(ctx context.Context, p control.WorkloadProfile) error {
	st, err := m.unitState(ctx, p.Unit)
	if err != nil {
		return err
	}
	if st.active == "inactive" && st.sub == "dead" {
		return nil
	}
	if st.active != "active" || st.sub != "running" {
		return errors.New("workload cannot unload in current state")
	}
	n := *p.NativeModel
	loaded, err := m.ollamaModelLoaded(ctx, n)
	if err != nil {
		return err
	}
	if !loaded {
		return nil
	}
	if err := m.setOllamaKeepAlive(ctx, n.Endpoint, n.Model, 0); err != nil {
		return err
	}
	target := n.ComparisonModel()
	if err := m.pollOllamaModels(ctx, n, func(loaded map[string]bool) bool { return loaded[target] }); err != nil {
		return fmt.Errorf("%s model unload unverified: %w", p.ID, err)
	}
	return nil
}

// Release of a shared unit is whole-list and target-aware: any loaded model
// other than the sibling target's own model is unreleased VRAM. A dead unit
// cannot serve /api/ps, so cgroup emptiness is the only release evidence.
func (m *SystemdManager) releasedSharedOllama(ctx context.Context, p control.WorkloadProfile, target control.Workload, loadedByUnit map[string]map[string]bool) error {
	st, err := m.unitState(ctx, p.Unit)
	if err != nil {
		return err
	}
	if st.active == "inactive" && st.sub == "dead" {
		return m.releasedDeadSharedOllama(ctx, p, st)
	}
	if st.active != "active" || st.sub != "running" {
		return fmt.Errorf("%s is neither running nor stopped", p.Unit)
	}
	loaded, err := m.sharedOllamaLoaded(ctx, p, loadedByUnit)
	if err != nil {
		return err
	}
	allowed := m.allowedSharedModel(p, target)
	for model := range loaded {
		if model != allowed {
			return fmt.Errorf("%s model %s is still loaded", p.Unit, model)
		}
	}
	return nil
}

func (m *SystemdManager) releasedDeadSharedOllama(ctx context.Context, p control.WorkloadProfile, st systemdUnitState) error {
	if !st.hasCgroup {
		return fmt.Errorf("%s ControlGroup metadata unavailable", p.Unit)
	}
	if err := m.verifyManagerCgroup(ctx); err != nil {
		return err
	}
	if err := m.cgroups.empty(p.Cgroup); err != nil {
		return fmt.Errorf("%s release: %w", p.Unit, err)
	}
	return nil
}

func (m *SystemdManager) sharedOllamaLoaded(ctx context.Context, p control.WorkloadProfile, loadedByUnit map[string]map[string]bool) (map[string]bool, error) {
	if loaded, ok := loadedByUnit[p.Unit]; ok {
		return loaded, nil
	}
	loaded, err := m.loadedOllamaModels(ctx, *p.NativeModel)
	if err != nil {
		return nil, err
	}
	loadedByUnit[p.Unit] = loaded
	return loaded, nil
}

func (m *SystemdManager) allowedSharedModel(p control.WorkloadProfile, target control.Workload) string {
	if target == control.WorkloadIdle {
		return ""
	}
	if t, found := m.config.Catalog.Profile(target); found && control.SharedOllamaUnit(p, t) {
		return t.NativeModel.ComparisonModel()
	}
	return ""
}

func (m *SystemdManager) observeSharedOllama(ctx context.Context, p control.WorkloadProfile, unitActive bool) (bool, error) {
	if !unitActive {
		return false, nil
	}
	return m.ollamaModelLoaded(ctx, *p.NativeModel)
}

// A shared unit may hold auto-loaded leftovers; evict every model but the
// target before the release and capacity checks, then confirm the eviction
// before preloading so readiness verifies exactly one loaded model.
func (m *SystemdManager) evictOtherOllamaModels(ctx context.Context, n control.NativeModel) error {
	loaded, err := m.loadedOllamaModels(ctx, n)
	if err != nil {
		return err
	}
	target := n.ComparisonModel()
	for model := range loaded {
		if model != target {
			if err := m.setOllamaKeepAlive(ctx, n.Endpoint, model, 0); err != nil {
				return err
			}
		}
	}
	return m.pollOllamaModels(ctx, n, func(loaded map[string]bool) bool {
		for model := range loaded {
			if model != target {
				return true
			}
		}
		return false
	})
}

// Eviction needs the running unit's API; a stopped unit holds no models.
func (m *SystemdManager) evictSharedOllamaUnit(ctx context.Context, p control.WorkloadProfile) error {
	st, err := m.unitState(ctx, p.Unit)
	if err != nil {
		return err
	}
	if st.active != "active" || st.sub != "running" {
		return nil
	}
	return m.evictOtherOllamaModels(ctx, *p.NativeModel)
}

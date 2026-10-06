package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

func (m *SystemdManager) unitGroups() []struct{ unit, group string } {
	var out []struct{ unit, group string }
	for _, p := range m.config.Catalog.Profiles {
		out = append(out, struct{ unit, group string }{p.Unit, p.Cgroup})
	}
	return out
}
func (m *SystemdManager) Observe(ctx context.Context) (Snapshot, error) {
	s := Snapshot{Workloads: map[control.Workload]WorkloadObservation{}}
	for _, p := range m.config.Catalog.Profiles {
		st, err := m.unitState(ctx, p.Unit)
		if err != nil {
			return Snapshot{}, err
		}
		if !(st.active == "active" && st.sub == "running") && !(st.active == "inactive" && st.sub == "dead") {
			return Snapshot{}, fmt.Errorf("%s is neither running nor stopped", p.Unit)
		}
		s.Workloads[p.ID] = WorkloadObservation{Active: st.active == "active", Exclusive: p.Adapter != control.AdapterMediaUnload}
	}
	return s, nil
}
func (m *SystemdManager) ReleasedFor(ctx context.Context, target control.Workload) error {
	if target != control.WorkloadIdle {
		if _, ok := m.config.Catalog.Profile(target); !ok {
			return errors.New("unconfigured release target")
		}
	}
	for _, p := range m.config.Catalog.Profiles {
		if p.ID == target {
			continue
		}
		if err := m.releasedUnit(ctx, p.Unit, p.Cgroup, p.Adapter == control.AdapterMediaUnload); err != nil {
			return err
		}
	}
	return nil
}
func (m *SystemdManager) Healthy(ctx context.Context, id control.Workload) error {
	ctx, cancel := context.WithTimeout(ctx, m.config.HealthTimeout)
	defer cancel()
	if err := m.ReleasedFor(ctx, id); err != nil {
		return err
	}
	if id == control.WorkloadIdle {
		return nil
	}
	p, _ := m.config.Catalog.Profile(id)
	if err := m.verifyNativeBinding(ctx, p); err != nil {
		return err
	}
	if err := m.getHealthy(ctx, p.HealthURL); err != nil {
		return err
	}
	if p.NativeModel != nil {
		return m.nativeReady(ctx, *p.NativeModel)
	}
	return nil
}
func (m *SystemdManager) Stop(ctx context.Context, id control.Workload) error {
	p, ok := m.config.Catalog.Profile(id)
	if !ok {
		return errors.New("unconfigured workload")
	}
	if p.Adapter != control.AdapterMediaUnload {
		return m.stopUnit(ctx, p.Unit)
	}
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.ReleaseURL, bytes.NewBufferString(`{"unload_models":true,"free_memory":true}`))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return m.checkHTTPResponse(req, "media release")
}

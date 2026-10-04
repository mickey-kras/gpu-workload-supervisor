package runtime

import (
	"bytes"
	"context"
	"errors"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"net/http"
)

func (m *SystemdManager) units() []string {
	var out []string
	for _, p := range m.unitGroups() {
		out = append(out, p.unit)
	}
	return out
}
func (m *SystemdManager) groups() []string {
	var out []string
	for _, p := range m.unitGroups() {
		out = append(out, p.group)
	}
	return out
}
func (m *SystemdManager) unitGroups() []struct{ unit, group string } {
	out := []struct{ unit, group string }{{m.config.TextUnit, m.config.TextCgroup}, {m.config.MediaUnit, m.config.MediaCgroup}}
	if m.config.Catalog != nil {
		out = nil
		for _, p := range m.config.Catalog.Profiles {
			out = append(out, struct{ unit, group string }{p.Unit, p.Cgroup})
		}
	}
	return out
}
func (m *SystemdManager) observeCatalog(ctx context.Context) (Snapshot, error) {
	s := Snapshot{Workloads: map[control.Workload]WorkloadObservation{}}
	for _, p := range m.config.Catalog.Profiles {
		st, err := m.unitState(ctx, p.Unit)
		if err != nil {
			return Snapshot{}, err
		}
		if !(st.active == "active" && st.sub == "running") && !(st.active == "inactive" && st.sub == "dead") {
			return Snapshot{}, errors.New("workload is neither running nor stopped")
		}
		s.Workloads[p.ID] = WorkloadObservation{Active: st.active == "active", Exclusive: p.Adapter != "media-unload"}
	}
	return s, nil
}
func (m *SystemdManager) releasedCatalog(ctx context.Context, target control.Workload) error {
	if target != control.WorkloadIdle {
		if _, ok := m.config.Catalog.Profile(target); !ok {
			return errors.New("unconfigured release target")
		}
	}
	for _, p := range m.config.Catalog.Profiles {
		if p.ID == target {
			continue
		}
		if err := m.releasedUnit(ctx, p.Unit, p.Cgroup, p.Adapter == "media-unload"); err != nil {
			return err
		}
	}
	return nil
}
func (m *SystemdManager) healthyCatalog(ctx context.Context, id control.Workload) error {
	ctx, cancel := context.WithTimeout(ctx, m.config.HealthTimeout)
	defer cancel()
	if err := m.releasedCatalog(ctx, id); err != nil {
		return err
	}
	if id == control.WorkloadIdle {
		return nil
	}
	p, _ := m.config.Catalog.Profile(id)
	return m.getHealthy(ctx, p.HealthURL)
}
func (m *SystemdManager) stopCatalog(ctx context.Context, id control.Workload) error {
	p, ok := m.config.Catalog.Profile(id)
	if !ok {
		return errors.New("unconfigured workload")
	}
	if p.Adapter != "media-unload" {
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

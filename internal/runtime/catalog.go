package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

func (m *SystemdManager) Observe(ctx context.Context) (Snapshot, error) {
	return m.observe(ctx, false)
}

// ObserveForRecovery selects workloads that can be retained. A failed unit is
// not retainable; this observation is never evidence of resource release.
func (m *SystemdManager) ObserveForRecovery(ctx context.Context) (Snapshot, error) {
	return m.observe(ctx, true)
}

func (m *SystemdManager) observe(ctx context.Context, recovering bool) (Snapshot, error) {
	s := Snapshot{Workloads: map[control.Workload]WorkloadObservation{}}
	for _, p := range m.config.Catalog.Profiles {
		st, err := m.unitState(ctx, p.Unit)
		if err != nil {
			return Snapshot{}, err
		}
		if !validObservedUnit(st, p.Trainer != nil, recovering) {
			return Snapshot{}, fmt.Errorf("%s is neither running nor stopped", p.Unit)
		}
		s.Workloads[p.ID] = WorkloadObservation{Active: st.active == "active", Exclusive: p.Adapter != control.AdapterMediaUnload}
	}
	return s, nil
}
func (m *SystemdManager) ReleasedFor(ctx context.Context, target control.Workload) error {
	return m.releasedFor(ctx, target, nil)
}

// ReleasedForUnchangedLaunches rechecks accepted workloads during setup. Only
// profiles whose exact launch sources were replaced are excluded; setup must
// separately verify and prove release of the replacement catalog before
// calling this method.
func (m *SystemdManager) ReleasedForUnchangedLaunches(ctx context.Context, target control.Workload, replaced map[string]bool) error {
	return m.releasedFor(ctx, target, replaced)
}

func (m *SystemdManager) releasedFor(ctx context.Context, target control.Workload, replaced map[string]bool) error {
	if target != control.WorkloadIdle {
		if _, ok := m.config.Catalog.Profile(target); !ok {
			return errors.New("unconfigured release target")
		}
	}
	for _, p := range m.config.Catalog.Profiles {
		if launchSourceReplaced(p, replaced) {
			continue
		}
		if profileGPUUUID(p) != "" {
			if err := m.verifyNativeBinding(ctx, p); err != nil {
				return err
			}
		}
		if p.ID == target {
			continue
		}
		if err := m.releasedUnit(ctx, p.Unit, p.Cgroup, p.Adapter == control.AdapterMediaUnload); err != nil {
			return err
		}
	}
	return nil
}

func launchSourceReplaced(p control.WorkloadProfile, replaced map[string]bool) bool {
	var file string
	var dropIns []control.LaunchSource
	if p.NativeModel != nil {
		file, dropIns = p.NativeModel.LaunchFile, p.NativeModel.DropIns
	} else if p.LaunchBinding != nil {
		file, dropIns = p.LaunchBinding.LaunchFile, p.LaunchBinding.DropIns
	}
	if file != "" && replaced[file] {
		return true
	}
	for _, source := range dropIns {
		if replaced[source.Path] {
			return true
		}
	}
	return false
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
	if err := RequireEnforceableLimits(p); err != nil {
		return err
	}
	if err := m.verifyNativeBinding(ctx, p); err != nil {
		return err
	}
	if p.Trainer != nil {
		return nil
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
	if st.active == "failed" && st.sub == "failed" {
		return m.stopUnit(ctx, p.Unit)
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

func validObservedUnit(st systemdUnitState, trainer, recovering bool) bool {
	return !(!(st.active == "active" && st.sub == "running") && !(st.active == "inactive" && st.sub == "dead") && !(trainer && st.active == "active" && st.sub == "exited") && !((recovering || trainer) && st.active == "failed" && st.sub == "failed"))
}

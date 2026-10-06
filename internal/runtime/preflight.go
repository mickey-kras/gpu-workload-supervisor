package runtime

import (
	"context"
	"errors"
)

func (m *SystemdManager) Preflight(ctx context.Context) error {
	if m.config.Catalog != nil {
		for _, p := range m.config.Catalog.Profiles {
			if err := m.verifyNativeBinding(ctx, p); err != nil {
				return err
			}
		}
	}
	if err := m.verifyManagerCgroup(ctx); err != nil {
		return err
	}
	for _, workload := range m.unitGroups() {
		if err := m.preflightWorkloadCgroup(ctx, workload.unit, workload.group); err != nil {
			return err
		}
	}
	return nil
}

func (m *SystemdManager) preflightWorkloadCgroup(ctx context.Context, unit, group string) error {
	state, err := m.unitState(ctx, unit)
	if err != nil {
		return err
	}
	if !state.hasCgroup || (state.active == "active" && state.cgroup == "") {
		return errors.New("workload ControlGroup metadata unavailable")
	}
	// systemd can remove an empty cgroup after either a clean stop or a
	// crash. Neither this nor populated groups prove workload release;
	// recovery must still stop units and verify release independently.
	allowRemoved := (state.active == "inactive" && state.sub == "dead") ||
		(state.active == "failed" && state.sub == "failed")
	if err := m.cgroups.check(group, allowRemoved); err != nil && !errors.Is(err, errCgroupPopulated) {
		return err
	}
	return nil
}

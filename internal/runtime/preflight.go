package runtime

import (
	"context"
	"errors"
)

// CapabilityPreflight checks release verification capability without requiring
// workloads to be stopped. Release evidence must still be checked after stopping.
type CapabilityPreflight interface{ Preflight(context.Context) error }

func (m *SystemdManager) Preflight(ctx context.Context) error {
	if err := m.verifyManagerCgroup(ctx); err != nil {
		return err
	}
	for _, workload := range []struct{ unit, group string }{{m.config.TextUnit, m.config.TextCgroup}, {m.config.MediaUnit, m.config.MediaCgroup}} {
		state, err := m.unitState(ctx, workload.unit)
		if err != nil {
			return err
		}
		if !state.hasCgroup || (state.active == "active" && state.cgroup == "") {
			return errors.New("workload ControlGroup metadata unavailable")
		}
		// A removed stopped workload is supported; populated groups are
		// valid capability evidence, never release evidence.
		allowRemoved := state.active == "inactive" && state.sub == "dead"
		if err := m.cgroups.check(workload.group, allowRemoved); err != nil && !errors.Is(err, errCgroupPopulated) {
			return err
		}
	}
	return nil
}

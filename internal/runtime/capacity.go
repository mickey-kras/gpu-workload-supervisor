package runtime

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

var ErrCapacity = errors.New("target GPU capacity unavailable")

// Capacity is optional deployment policy, independent of release evidence.
// Requirements are measured for each target, not inferred from idle usage.
func (m *SystemdManager) capacity(ctx context.Context, workload control.Workload) error {
	required := m.config.TextRequiredMiB
	if workload == control.WorkloadMedia {
		required = m.config.MediaRequiredMiB
	}
	if required == 0 {
		return nil
	}
	output, err := m.runner.Run(ctx, m.config.NvidiaSMIPath, "--query-gpu=memory.free", "--format=csv,noheader,nounits", "-i", strconv.Itoa(m.config.GPUIndex))
	if err != nil {
		return fmt.Errorf("%w: query memory.free: %v: %s", ErrCapacity, err, strings.TrimSpace(string(output)))
	}
	free, err := strconv.ParseUint(strings.TrimSpace(string(output)), 10, 64)
	if err != nil {
		return fmt.Errorf("%w: parse memory.free: %v", ErrCapacity, err)
	}
	needed := required + m.config.CapacityHeadroomMiB
	if free < needed {
		return fmt.Errorf("%w: %s needs %d MiB (measured requirement %d plus headroom %d), available %d MiB; retry after freeing capacity", ErrCapacity, workload, needed, required, m.config.CapacityHeadroomMiB, free)
	}
	return nil
}

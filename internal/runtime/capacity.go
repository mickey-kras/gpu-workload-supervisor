package runtime

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/mickey-kras/llm-workload-supervisor/internal/control"
)

var ErrCapacity = errors.New("target GPU capacity unavailable")

// Capacity is optional deployment policy, independent of release evidence.
// Requirements are measured for each target, not inferred from idle usage.
// A vLLM launch with an explicit GPU memory utilization also reserves that
// share of total VRAM at start, independent of the model size, so the
// requirement is at least that reservation.
func (m *SystemdManager) capacity(ctx context.Context, workload control.Workload) error {
	p, ok := m.config.Catalog.Profile(workload)
	if !ok {
		return ErrCapacity
	}
	percent, err := servingReservationPercent(p)
	if err != nil {
		return SafeError("vLLM GPU memory utilization could not be read from the launch", ErrCapacity, err)
	}
	if percent != 0 {
		return m.reservedCapacity(ctx, p, percent)
	}
	required := p.RequiredMiB
	if required == 0 {
		return nil
	}
	output, err := m.runner.Run(ctx, m.config.NvidiaSMIPath, "--query-gpu=memory.free", "--format=csv,noheader,nounits", "-i", m.gpuDeviceTarget())
	if err != nil {
		return SafeError("query memory.free failed", ErrCapacity, err)
	}
	free, err := strconv.ParseUint(strings.TrimSpace(string(output)), 10, 64)
	if err != nil {
		return SafeError("invalid memory.free response", ErrCapacity, err)
	}
	needed := required + m.config.CapacityHeadroomMiB
	if free < needed {
		return fmt.Errorf("%w: %s needs %d MiB (measured requirement %d plus headroom %d), available %d MiB; retry after freeing capacity", ErrCapacity, workload, needed, required, m.config.CapacityHeadroomMiB, free)
	}
	return nil
}

// reservedCapacity admits a vLLM launch that reserves percent of total VRAM.
// The requirement is the larger of the measured requirement and the
// reservation; headroom is added as for a measured requirement.
func (m *SystemdManager) reservedCapacity(ctx context.Context, p control.WorkloadProfile, percent uint32) error {
	output, err := m.runner.Run(ctx, m.config.NvidiaSMIPath, "--query-gpu=memory.free,memory.total", "--format=csv,noheader,nounits", "-i", m.gpuDeviceTarget())
	if err != nil {
		return SafeError("query memory.free,memory.total failed", ErrCapacity, err)
	}
	free, total, err := parseFreeAndTotal(output)
	if err != nil {
		return SafeError("invalid memory.free,memory.total response", ErrCapacity, err)
	}
	reservation := control.GPUReservationMiB(percent, total)
	required := max(p.RequiredMiB, reservation)
	if required > ^uint64(0)-m.config.CapacityHeadroomMiB {
		return fmt.Errorf("%w: capacity requirement plus headroom overflows", ErrCapacity)
	}
	needed := required + m.config.CapacityHeadroomMiB
	if free < needed {
		return fmt.Errorf("%w: %s needs %d MiB (vLLM reserves %s of %d MiB total VRAM = %d MiB, measured requirement %d, plus headroom %d), available %d MiB; retry after freeing capacity",
			ErrCapacity, p.ID, needed, control.FormatGPUMemoryUtilization(percent), total, reservation, p.RequiredMiB, m.config.CapacityHeadroomMiB, free)
	}
	return nil
}

func (m *SystemdManager) gpuDeviceTarget() string {
	if m.config.GPUUUID != "" {
		return m.config.GPUUUID
	}
	return strconv.Itoa(m.config.GPUIndex)
}

func parseFreeAndTotal(output []byte) (uint64, uint64, error) {
	freeText, totalText, ok := strings.Cut(strings.TrimSpace(string(output)), ",")
	if !ok {
		return 0, 0, errors.New("expected memory.free,memory.total")
	}
	free, err := strconv.ParseUint(strings.TrimSpace(freeText), 10, 64)
	if err != nil {
		return 0, 0, err
	}
	total, err := strconv.ParseUint(strings.TrimSpace(totalText), 10, 64)
	if err != nil {
		return 0, 0, err
	}
	if total == 0 || free > total {
		return 0, 0, errors.New("memory.free must not exceed a nonzero memory.total")
	}
	return free, total, nil
}

// servingReservationPercent is the GPU memory utilization of a vLLM launch
// that sets it: the owned serving options, or the explicit option of an
// adopted launch, read from the fingerprinted launch file. It is 0 for a
// launch that leaves it to vLLM and for other workloads.
func servingReservationPercent(p control.WorkloadProfile) (uint32, error) {
	n := p.NativeModel
	if n == nil {
		return 0, nil
	}
	if n.Owned != nil {
		if n.Owned.Serving.IsZero() {
			return 0, nil
		}
		return n.Owned.Serving.EffectiveGPUMemoryPercent(), nil
	}
	sources, err := readLaunchSources(n.LaunchFile, n.DropIns)
	if err != nil {
		return 0, err
	}
	if fmt.Sprintf("%x", sha256.Sum256(sources[0])) != n.LaunchSHA256 {
		return 0, ErrLaunchChanged
	}
	unit, err := parseExternalLaunchSources(sources)
	if err != nil {
		return 0, err
	}
	args := strings.Fields(unit.execStart)
	if len(args) < 2 {
		return 0, ErrLaunchUnsupported
	}
	flags, err := parseServerCommand(args, n.Runtime)
	if err != nil {
		return 0, err
	}
	return flags.gpuMemoryPercent, nil
}

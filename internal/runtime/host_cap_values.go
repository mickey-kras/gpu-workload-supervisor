package runtime

import (
	"fmt"
	"slices"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

// The upper bounds of the host cap values. The task cap is the kernel
// maximum of pids.max. The other caps are twice the largest unit limit, so
// a cap can hold that limit and a reserve of up to 50 percent.
const (
	maxHostCapTasks     = control.MaxLimitTasks
	maxHostCapCPU       = 2 * control.MaxLimitCPUQuota
	maxHostCapBandwidth = 2 * control.MaxLimitIOBandwidth
	maxHostCapIOPS      = 2 * control.MaxLimitIOPS
)

// HostCapFor fills want for the units with limits. The operator sets
// MemoryHighMiB and MemoryMaxMiB. An unset task or CPU cap gets the largest
// unit limit plus the reserve. An unset I/O cap gets the smallest cap that
// holds the largest unit cap after the I/O reserve. It refuses values that
// leave less than the reserve above a unit limit, as the reserve check of
// the runtime does. It returns the filled values and the I/O device of the
// units.
func (r LimitReserve) HostCapFor(want HostCapValues, limits []control.ResourceLimits) (HostCapValues, string, error) {
	if err := r.checkHostMemory(want, limits); err != nil {
		return want, "", err
	}
	var err error
	if want.TasksMax, err = fillCap("tasksMax", "tasks", want.TasksMax, largest(limits, func(l control.ResourceLimits) uint64 { return l.TasksMax }),
		r.Tasks, maxHostCapTasks); err != nil {
		return want, "", err
	}
	if want.CPUQuotaPercent, err = fillCap("cpuQuotaPercent", "percent", want.CPUQuotaPercent,
		largest(limits, func(l control.ResourceLimits) uint64 { return l.CPUQuotaPercent }), r.CPUPercent, maxHostCapCPU); err != nil {
		return want, "", err
	}
	device, err := ioDevice(limits)
	if err != nil {
		return want, "", err
	}
	return want, device, r.fillHostIO(&want, limits, device)
}

// checkHostMemory requires both memory values, MemoryMax above MemoryHigh,
// and room for the reserve above each unit memory limit. MemoryHigh is the
// lower of the two, so it is the ceiling of the units.
func (r LimitReserve) checkHostMemory(want HostCapValues, limits []control.ResourceLimits) error {
	for _, value := range []uint64{want.MemoryHighMiB, want.MemoryMaxMiB} {
		if value < control.MinLimitMemoryMiB || value > control.MaxLimitMemoryMiB {
			return fmt.Errorf("%w: hostCap needs memoryHighMiB and memoryMaxMiB of %d to %d MiB", ErrLimitReserve, control.MinLimitMemoryMiB, control.MaxLimitMemoryMiB)
		}
	}
	if want.MemoryMaxMiB <= want.MemoryHighMiB {
		return fmt.Errorf("%w: hostCap.memoryMaxMiB must be above hostCap.memoryHighMiB", ErrLimitReserve)
	}
	need := largest(limits, func(l control.ResourceLimits) uint64 { return max(l.MemoryHighMiB, l.MemoryMaxMiB) }) + r.MemoryMiB
	if need >= control.MaxLimitMemoryMiB {
		// memoryMaxMiB must stay above memoryHighMiB.
		return fmt.Errorf("%w: no host cap fits the largest unit memory limit plus the %d MiB reserve; lower the unit memory limit or the reserve",
			ErrLimitReserve, r.MemoryMiB)
	}
	if need > want.MemoryHighMiB {
		return fmt.Errorf("%w: hostCap.memoryHighMiB %d leaves too little room; set it to at least %d MiB: the largest unit memory limit plus the %d MiB reserve",
			ErrLimitReserve, want.MemoryHighMiB, need, r.MemoryMiB)
	}
	return nil
}

// fillCap gives value, or unit plus reserve when value is unset. A set value
// must leave the reserve above the largest unit limit.
func fillCap(field, unit string, value, largestUnit, reserve, upper uint64) (uint64, error) {
	need := largestUnit + reserve
	if value == 0 && need > upper {
		return 0, unfillable(field, unit, upper)
	}
	if value == 0 {
		value = need
	}
	switch {
	case value > upper:
		return 0, fmt.Errorf("%w: hostCap.%s must be %d %s or less", ErrLimitReserve, field, upper, unit)
	case value < need:
		return 0, fmt.Errorf("%w: hostCap.%s %d leaves too little room; set it to at least %d %s: the largest unit limit plus the reserve",
			ErrLimitReserve, field, value, need, unit)
	}
	return value, nil
}

// unfillable refuses a cap that setup fills above its upper bound: the
// operator set no value, so the fix is a lower unit limit or reserve.
func unfillable(field, unit string, upper uint64) error {
	return fmt.Errorf("%w: the largest unit %s plus the reserve is above the host maximum of %d %s; lower the unit %s or the reserve",
		ErrLimitReserve, field, upper, unit, field)
}

// largest gives the largest value of the limits.
func largest(limits []control.ResourceLimits, value func(control.ResourceLimits) uint64) uint64 {
	var top uint64
	for _, l := range limits {
		top = max(top, value(l))
	}
	return top
}

// ioDevice gives the one device of the units that cap I/O. The slice caps
// one device: the runtime refuses a slice cap on a device that a unit does
// not cap.
func ioDevice(limits []control.ResourceLimits) (string, error) {
	var devices []string
	for _, l := range limits {
		if l.ThrottlesIO() && !slices.Contains(devices, l.IODevice) {
			devices = append(devices, l.IODevice)
		}
	}
	if len(devices) > 1 {
		return "", fmt.Errorf("%w: the units cap I/O on %s and %s, but the host cap can cap one device; use one ioDevice", ErrLimitReserve, devices[0], devices[1])
	}
	if len(devices) == 0 {
		return "", nil
	}
	return devices[0], nil
}

// fillHostIO fills each I/O cap that a unit sets. The slice then caps that
// key, so each unit with limits must set it: the runtime refuses an unset
// unit cap under a slice cap. A key that no unit sets stays unset.
func (r LimitReserve) fillHostIO(want *HostCapValues, limits []control.ResourceLimits, device string) error {
	for _, limit := range ioLimits {
		top := largest(limits, limit.value)
		value := limit.value(want.io())
		if top == 0 {
			if value != 0 {
				return fmt.Errorf("%w: hostCap.%s is set, but no unit sets %s; remove it from hostCap", ErrLimitReserve, limit.field, limit.field)
			}
			continue
		}
		if slices.ContainsFunc(limits, func(l control.ResourceLimits) bool { return limit.value(l) == 0 }) {
			return fmt.Errorf("%w: a unit sets %s, so the host cap caps it on %s; set %s on each unit with limits", ErrLimitReserve, limit.field, device, limit.field)
		}
		filled, err := r.fillIOCap(limit, value, top)
		if err != nil {
			return err
		}
		want.setIO(limit.key, filled)
	}
	return nil
}

// fillIOCap gives value, or the smallest cap whose share after the reserve
// holds the largest unit cap.
func (r LimitReserve) fillIOCap(limit ioLimit, value, top uint64) (uint64, error) {
	// A filled cap stays under hostUpper: the I/O reserve is at most 50
	// percent, and hostUpper is twice the largest unit cap.
	if value == 0 {
		value = r.ioCeilingFor(top)
	}
	switch {
	case value > limit.hostUpper:
		return 0, fmt.Errorf("%w: hostCap.%s must be %d %s or less", ErrLimitReserve, limit.field, limit.hostUpper, limit.unit)
	case value-r.ioReserve(value) < top:
		return 0, fmt.Errorf("%w: hostCap.%s %d leaves too little room; set it to at least %d %s: the largest unit cap plus the %d%% reserve",
			ErrLimitReserve, limit.field, value, r.ioCeilingFor(top), limit.unit, r.IOPercent)
	}
	return value, nil
}

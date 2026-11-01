package control

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// ResourceLimitsVersion is the only accepted resource limits contract.
const ResourceLimitsVersion = 1

// ErrLimitsRequireV5 rejects limits in a catalog version that predates them.
var ErrLimitsRequireV5 = errors.New("resource limits require catalog version 5")

// ErrEmptyLimits rejects a present but null, empty or all-zero limits object.
// omitzero would otherwise decode it to the absent form and drop the key, so
// the accepted bytes could not round-trip.
var ErrEmptyLimits = errors.New("resource limits object must not be empty")

// ResourceLimits are opt-in systemd resource controls for one owned unit. The
// type is a comparable value struct: profiles that share a unit compare their
// owned launch specifications with ==, and Catalog.Clone copies by value.
// A zero field is absent. MemorySwapMaxMiB needs MemoryMaxMiB, so an absent
// swap value and 0 render the same MemorySwapMax=0.
type ResourceLimits struct {
	Version             int    `json:"version"`
	MemoryHighMiB       uint64 `json:"memoryHighMiB,omitempty"`
	MemoryMaxMiB        uint64 `json:"memoryMaxMiB,omitempty"`
	MemorySwapMaxMiB    uint64 `json:"memorySwapMaxMiB,omitempty"`
	TasksMax            uint64 `json:"tasksMax,omitempty"`
	CPUWeight           uint64 `json:"cpuWeight,omitempty"`
	CPUQuotaPercent     uint64 `json:"cpuQuotaPercent,omitempty"`
	IODevice            string `json:"ioDevice,omitempty"`
	IODeviceNumber      string `json:"ioDeviceNumber,omitempty"`
	IOReadBandwidthMax  uint64 `json:"ioReadBandwidthMax,omitempty"`
	IOWriteBandwidthMax uint64 `json:"ioWriteBandwidthMax,omitempty"`
	IOReadIOPSMax       uint64 `json:"ioReadIOPSMax,omitempty"`
	IOWriteIOPSMax      uint64 `json:"ioWriteIOPSMax,omitempty"`
	IOWeight            uint64 `json:"ioWeight,omitempty"`
}

// Bounds of the limits schema. Memory values are MiB, so each is a multiple
// of the page size; I/O bandwidth is bytes per second.
const (
	MinLimitMemoryMiB   = 256
	MaxLimitMemoryMiB   = 16 * 1024 * 1024
	MinLimitTasks       = 64
	MaxLimitTasks       = 4194304
	MaxLimitWeight      = 10000
	MaxLimitCPUQuota    = 102400
	MinLimitIOBandwidth = 1 << 20
	MaxLimitIOBandwidth = 1 << 40
	MinLimitIOPS        = 100
	MaxLimitIOPS        = 10000000
)

var (
	limitDevicePath   = regexp.MustCompile(`^/dev/[a-z][a-z0-9]{0,31}$`)
	limitDeviceNumber = regexp.MustCompile(`^[0-9]{1,7}:[0-9]{1,7}$`)
)

// UnmarshalJSON decodes the limits object strictly in every catalog decoder,
// including setup requests, and rejects the empty forms with ErrEmptyLimits.
// An absent key never reaches this method.
func (l *ResourceLimits) UnmarshalJSON(raw []byte) error {
	type plain ResourceLimits
	var decoded plain
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&decoded); err != nil {
		return err
	}
	if ResourceLimits(decoded).IsZero() {
		return ErrEmptyLimits
	}
	*l = ResourceLimits(decoded)
	return nil
}

// IsZero reports an absent limits object.
func (l ResourceLimits) IsZero() bool {
	return l == ResourceLimits{}
}

// UsesIO reports whether any I/O control is set.
func (l ResourceLimits) UsesIO() bool {
	return l.IOReadBandwidthMax != 0 || l.IOWriteBandwidthMax != 0 || l.IOReadIOPSMax != 0 || l.IOWriteIOPSMax != 0 || l.IOWeight != 0
}

func (l ResourceLimits) hasControl() bool {
	return l.MemoryHighMiB != 0 || l.MemoryMaxMiB != 0 || l.TasksMax != 0 || l.CPUWeight != 0 || l.CPUQuotaPercent != 0 || l.UsesIO()
}

// Validate applies the bounded schema. systemd ignores an invalid value with
// only a warning, so every rendered value must pass here first.
func (l ResourceLimits) Validate() error {
	if l.Version != ResourceLimitsVersion {
		return errors.New("unsupported resource limits version")
	}
	if !l.hasControl() {
		return errors.New("resource limits need at least one control")
	}
	if err := l.validateMemory(); err != nil {
		return err
	}
	if !inRange(l.TasksMax, MinLimitTasks, MaxLimitTasks) || !inRange(l.CPUWeight, 1, MaxLimitWeight) || !inRange(l.CPUQuotaPercent, 1, MaxLimitCPUQuota) {
		return errors.New("task or CPU limit is out of range")
	}
	return l.validateIO()
}

func (l ResourceLimits) validateMemory() error {
	if !inRange(l.MemoryHighMiB, MinLimitMemoryMiB, MaxLimitMemoryMiB) || !inRange(l.MemoryMaxMiB, MinLimitMemoryMiB, MaxLimitMemoryMiB) {
		return errors.New("memory limit must be 256 MiB to 16 TiB")
	}
	if l.MemoryHighMiB != 0 && l.MemoryMaxMiB != 0 && l.MemoryHighMiB >= l.MemoryMaxMiB {
		return errors.New("memory high limit must be less than the memory maximum")
	}
	if l.MemorySwapMaxMiB != 0 && (l.MemoryMaxMiB == 0 || l.MemorySwapMaxMiB > MaxLimitMemoryMiB) {
		return errors.New("swap limit needs a memory maximum and must be at most 16 TiB")
	}
	return nil
}

func (l ResourceLimits) validateIO() error {
	if !l.UsesIO() {
		if l.IODevice != "" || l.IODeviceNumber != "" {
			return errors.New("I/O device needs an I/O control")
		}
		return nil
	}
	if !limitDevicePath.MatchString(l.IODevice) || !limitDeviceNumber.MatchString(l.IODeviceNumber) {
		return errors.New("I/O controls need a canonical /dev/<kernel name> device and its MAJ:MIN number")
	}
	if !inRange(l.IOReadBandwidthMax, MinLimitIOBandwidth, MaxLimitIOBandwidth) || !inRange(l.IOWriteBandwidthMax, MinLimitIOBandwidth, MaxLimitIOBandwidth) {
		return errors.New("I/O bandwidth limit must be 1 MiB/s to 1 TiB/s")
	}
	if !inRange(l.IOReadIOPSMax, MinLimitIOPS, MaxLimitIOPS) || !inRange(l.IOWriteIOPSMax, MinLimitIOPS, MaxLimitIOPS) {
		return errors.New("I/O operations limit must be 100 to 10000000 per second")
	}
	if !inRange(l.IOWeight, 1, MaxLimitWeight) {
		return errors.New("I/O weight must be 1 to 10000")
	}
	return nil
}

// inRange treats 0 as an absent control.
func inRange(v, minimum, maximum uint64) bool {
	return v == 0 || (v >= minimum && v <= maximum)
}

// SwapLimitMiB is the rendered swap limit: the set value, or 0 with a memory
// maximum. ok is false when the unit renders no swap limit.
func (l ResourceLimits) SwapLimitMiB() (uint64, bool) {
	return l.MemorySwapMaxMiB, l.MemoryMaxMiB != 0
}

// RenderLines returns the exact directives for a unit with limits, in the
// documented order, each with a trailing newline. The caller places them
// directly after Restart=no.
func (l ResourceLimits) RenderLines() string {
	if l.IsZero() {
		return ""
	}
	var b strings.Builder
	b.WriteString("OOMPolicy=stop\nOOMScoreAdjust=500\n")
	write := func(key string, v uint64, suffix string) {
		b.WriteString(key + "=" + strconv.FormatUint(v, 10) + suffix + "\n")
	}
	writeValue := func(key string, v uint64, suffix string) {
		if v != 0 {
			write(key, v, suffix)
		}
	}
	writeValue("MemoryHigh", l.MemoryHighMiB, "M")
	writeValue("MemoryMax", l.MemoryMaxMiB, "M")
	// A memory maximum always renders a swap limit, which can be 0.
	if swap, ok := l.SwapLimitMiB(); ok {
		write("MemorySwapMax", swap, "M")
	}
	writeValue("TasksMax", l.TasksMax, "")
	writeValue("CPUWeight", l.CPUWeight, "")
	writeValue("CPUQuota", l.CPUQuotaPercent, "%")
	writeValue("IOWeight", l.IOWeight, "")
	for _, line := range []struct {
		key string
		v   uint64
	}{{"IOReadBandwidthMax", l.IOReadBandwidthMax}, {"IOWriteBandwidthMax", l.IOWriteBandwidthMax}, {"IOReadIOPSMax", l.IOReadIOPSMax}, {"IOWriteIOPSMax", l.IOWriteIOPSMax}} {
		if line.v != 0 {
			b.WriteString(line.key + "=" + l.IODevice + " " + strconv.FormatUint(line.v, 10) + "\n")
		}
	}
	return b.String()
}

// HasLimits reports whether the profile's owned launch carries limits. It is
// the one test for every version rule, store guard and launch gate. The setup
// client mirrors it in clients/setup/review.mjs.
func (p WorkloadProfile) HasLimits() bool {
	return p.NativeModel != nil && p.NativeModel.Owned != nil && !p.NativeModel.Owned.Limits.IsZero()
}

// HasLimits reports whether any owned launch carries limits.
func (c Catalog) HasLimits() bool {
	return slices.ContainsFunc(c.Profiles, WorkloadProfile.HasLimits)
}

// LimitedProfile returns the first profile with limits, if any.
func (c Catalog) LimitedProfile() (WorkloadProfile, bool) {
	for _, p := range c.Profiles {
		if p.HasLimits() {
			return p, true
		}
	}
	return WorkloadProfile{}, false
}

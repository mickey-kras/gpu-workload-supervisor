package runtime

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

// The classes of a limit outcome. Each class is a distinct result. Only the
// kill class tells that a process ended because of a limit. No class is
// completion evidence or release evidence.
const (
	LimitOutcomeKill         = "kill"
	LimitOutcomeAllocation   = "allocation-failure"
	LimitOutcomeTaskCreation = "task-creation-failure"
	LimitOutcomeThrottle     = "throttle"
	LimitOutcomeNone         = "none"
)

// LimitOutcome is the diagnostic outcome of the limits of one unit. Class
// is one of the LimitOutcome constants. Detail gives the counters and the
// systemd result that the class comes from. The supervisor writes both as
// text only. A throttle never proves completion, and a kill never proves
// release.
type LimitOutcome struct {
	Class  string
	Detail string
}

// String gives the class and the detail as one line.
func (o LimitOutcome) String() string {
	if o.Detail == "" {
		return o.Class
	}
	return o.Class + " (" + o.Detail + ")"
}

// limitCounters are the kernel counters and the systemd result that the
// outcome uses. A counter that cannot be read stays zero, and unread names
// it.
type limitCounters struct {
	memoryHigh, memoryMax, oomKill, pidsMax, cpuThrottled uint64
	result                                                string
	exited                                                bool
	unread                                                []string
}

// class gives the most severe class of the counters. A kill comes first.
// A memory.max event of a unit that exited with a failure is an allocation
// failure; while the unit runs, it is a throttle. The limit stop reads a
// running unit, so it reports a memory.max event as a throttle. The I/O
// cgroup files have no throttle counter, so I/O throttles have no class.
func (c limitCounters) class() string {
	switch {
	case c.result == "oom-kill" || c.oomKill > 0:
		return LimitOutcomeKill
	case c.memoryMax > 0 && c.exited:
		return LimitOutcomeAllocation
	case c.pidsMax > 0:
		return LimitOutcomeTaskCreation
	case c.memoryHigh > 0 || c.memoryMax > 0 || c.cpuThrottled > 0:
		return LimitOutcomeThrottle
	}
	return LimitOutcomeNone
}

func (c limitCounters) detail() string {
	text := fmt.Sprintf("memory.events high %d max %d oom_kill %d; pids.events max %d; cpu.stat nr_throttled %d; result %s",
		c.memoryHigh, c.memoryMax, c.oomKill, c.pidsMax, c.cpuThrottled, c.result)
	if len(c.unread) != 0 {
		text += "; unread " + strings.Join(c.unread, ", ")
	}
	return text
}

// LimitOutcome reads the limit outcome of a unit. It reads before the stop,
// because the counters disappear with an empty cgroup. A failed read leaves
// the counter out and names it in the detail, so the outcome never fails.
func (m *SystemdManager) LimitOutcome(ctx context.Context, id control.Workload) LimitOutcome {
	p, ok := m.config.Catalog.Profile(id)
	if !ok {
		return LimitOutcome{Class: LimitOutcomeNone, Detail: "unconfigured workload"}
	}
	var c limitCounters
	events := func(name string, keys map[string]*uint64) {
		data, err := m.cgroups.read(p.Cgroup, name)
		if err == nil {
			err = parseFlatKeyed(string(data), keys)
		}
		if err != nil {
			c.unread = append(c.unread, name)
		}
	}
	events("memory.events", map[string]*uint64{"high": &c.memoryHigh, "max": &c.memoryMax, "oom_kill": &c.oomKill})
	events("pids.events", map[string]*uint64{"max": &c.pidsMax})
	events("cpu.stat", map[string]*uint64{"nr_throttled": &c.cpuThrottled})
	out, err := m.runner.Run(ctx, m.config.SystemctlPath, systemctlUserFlag, "show", "--property=ActiveState,Result,ExecMainCode,ExecMainStatus", "--", p.Unit)
	var values map[string]string
	if err == nil {
		values, err = ParseUnitProperties(out)
	}
	if err != nil {
		c.unread = append(c.unread, "systemd result")
	} else {
		c.result = values["Result"]
		// ExecMainCode 1 is a normal exit; a nonzero ExecMainStatus is a
		// failed exit. A signal or a failed state also counts.
		code, status := values["ExecMainCode"], values["ExecMainStatus"]
		c.exited = values["ActiveState"] == "failed" || (code != "" && code != "0" && (code != "1" || status != "0"))
	}
	return LimitOutcome{Class: c.class(), Detail: c.detail()}
}

// parseFlatKeyed reads the wanted keys of a flat keyed cgroup file. A
// wanted key that is absent stays zero.
func parseFlatKeyed(data string, keys map[string]*uint64) error {
	for _, line := range strings.Split(strings.TrimSuffix(data, "\n"), "\n") {
		key, value, ok := strings.Cut(line, " ")
		if !ok {
			return errors.New("malformed cgroup counter line")
		}
		into, wanted := keys[key]
		if !wanted {
			continue
		}
		n, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			return fmt.Errorf("malformed cgroup counter %s", key)
		}
		*into = n
	}
	return nil
}

// ErrLimitStopUnreleased marks a release check after a limit stop that did
// not pass.
var ErrLimitStopUnreleased = errors.New("the release check after the limit stop did not pass")

// LimitStopReleased is the release check after a limit stop. It never
// takes the stop or its outcome as evidence. The launch binding of each
// unit with a GPU UUID must match first. Each configured unit must be
// inactive and dead with an empty recursive cgroup. Then the GPU of the
// stopped unit must have no compute process, and that unit must have its
// capacity. A runtime without a GPU probe fails the check.
func (m *SystemdManager) LimitStopReleased(ctx context.Context, id control.Workload) error {
	p, ok := m.config.Catalog.Profile(id)
	if !ok {
		return errors.New("unconfigured workload")
	}
	if err := m.ReleasedFor(ctx, control.WorkloadIdle); err != nil {
		return err
	}
	if err := m.gpuReleased(ctx, p); err != nil {
		return err
	}
	return m.capacity(ctx, id)
}

// gpuReleased requires no compute process on the GPU of p: its GPU UUID,
// or the configured GPU index when it has none.
func (m *SystemdManager) gpuReleased(ctx context.Context, p control.WorkloadProfile) error {
	if m.config.NvidiaSMIPath == "" {
		return fmt.Errorf("%w: no GPU probe is configured, so GPU release cannot be checked", ErrLimitStopUnreleased)
	}
	device := profileGPUUUID(p)
	if device == "" {
		device = strconv.Itoa(m.config.GPUIndex)
	}
	out, err := m.runner.Run(ctx, m.config.NvidiaSMIPath, "--query-compute-apps=pid", "--format=csv,noheader,nounits", "-i", device)
	if err != nil {
		return SafeError("GPU release observation failed", ErrLimitStopUnreleased, err)
	}
	if strings.TrimSpace(string(out)) != "" {
		return fmt.Errorf("%w: GPU compute processes remain after the stop", ErrLimitStopUnreleased)
	}
	return nil
}

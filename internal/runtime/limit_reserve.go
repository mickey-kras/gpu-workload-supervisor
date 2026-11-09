package runtime

import (
	"errors"
	"fmt"
	"strings"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

// LimitReserve is the room for the supervisor and proxy under the host cap.
// Setup stores it in the operator profile. A runtime without a stored
// reserve uses DefaultLimitReserve. The swap reserve is always 0.
type LimitReserve struct {
	MemoryMiB  uint64 `json:"memoryMiB"`
	Tasks      uint64 `json:"tasks"`
	CPUPercent uint64 `json:"cpuPercent"`
	IOPercent  uint64 `json:"ioPercent"`
}

// DefaultLimitReserve is the reserve when the profile stores none.
var DefaultLimitReserve = LimitReserve{MemoryMiB: 2048, Tasks: 512, CPUPercent: 100, IOPercent: 10}

// The upper bounds of a stored reserve. maxReserveIOPercent keeps room in
// a slice cap twice the largest unit cap.
const (
	maxReserveCPUPercent = 10000
	maxReserveIOPercent  = 50
)

// EffectiveReserve gives the stored reserve, or DefaultLimitReserve when
// none is stored.
func EffectiveReserve(stored *LimitReserve) LimitReserve {
	if stored != nil {
		return *stored
	}
	return DefaultLimitReserve
}

// Validate checks the bounds of a stored reserve: memory and tasks in the
// range of a unit limit, a CPU quota of 1 to 10000 percent and an I/O share
// of 1 to 50 percent. Each value must be set.
func (r LimitReserve) Validate() error {
	switch {
	case r.MemoryMiB < control.MinLimitMemoryMiB || r.MemoryMiB > control.MaxLimitMemoryMiB:
		return fmt.Errorf("the limit reserve memoryMiB must be %d to %d", control.MinLimitMemoryMiB, control.MaxLimitMemoryMiB)
	case r.Tasks < control.MinLimitTasks || r.Tasks > control.MaxLimitTasks:
		return fmt.Errorf("the limit reserve tasks must be %d to %d", control.MinLimitTasks, control.MaxLimitTasks)
	case r.CPUPercent < 1 || r.CPUPercent > maxReserveCPUPercent:
		return fmt.Errorf("the limit reserve cpuPercent must be 1 to %d", maxReserveCPUPercent)
	case r.IOPercent < 1 || r.IOPercent > maxReserveIOPercent:
		return fmt.Errorf("the limit reserve ioPercent must be 1 to %d", maxReserveIOPercent)
	}
	return nil
}

// memoryBytes is the memory reserve in bytes.
func (r LimitReserve) memoryBytes() uint64 {
	return r.MemoryMiB * mebibyte
}

// checkReserve keeps each unit limit at or below its slice cap minus the
// reserve, so the supervisor and proxy keep room under the shared cap. Its
// caller runs requireCappedControls first, so memoryMaxMiB, tasksMax and
// cpuQuotaPercent are set. Of the unset values, only an I/O cap that the
// slice caps needs a check here.
func checkReserve(l control.ResourceLimits, c hostCap, reserve LimitReserve) error {
	memory := func(field string, value uint64) reserveLimit {
		return reserveLimit{field: field, unit: "MiB", value: value * mebibyte, ceiling: c.memoryCeiling(), reserve: reserve.memoryBytes(), scale: mebibyte,
			minimum: control.MinLimitMemoryMiB}
	}
	checks := []error{
		memory("memoryHighMiB", l.MemoryHighMiB).check(),
		memory("memoryMaxMiB", l.MemoryMaxMiB).check(),
		reserveLimit{field: "tasksMax", unit: "tasks", value: l.TasksMax, ceiling: c.tasks, reserve: reserve.Tasks, scale: 1,
			minimum: control.MinLimitTasks}.check(),
		reserveLimit{field: "cpuQuotaPercent", unit: "percent", value: l.CPUQuotaPercent * 10000, ceiling: c.cpu, reserve: reserve.CPUPercent * 10000, scale: 10000,
			minimum: 1}.check(),
	}
	for _, limit := range ioLimits {
		ceiling := c.io[limit.key]
		r := reserveLimit{field: limit.field, unit: limit.unit, value: limit.value(l), ceiling: ceiling, reserve: reserve.ioReserve(ceiling), scale: 1,
			minimum: limit.minimum, need: reserve.ioCeilingFor(limit.minimum)}
		if r.value == 0 && ceiling != 0 {
			// The slice caps this key, so an unset unit cap could use the
			// reserve.
			checks = append(checks, r.unset(limit.key))
			continue
		}
		checks = append(checks, r.check())
	}
	return errors.Join(checks...)
}

// reserveLimit is one unit limit with its slice ceiling and the reserve
// under that ceiling, all in the base unit of the cgroup file. scale turns
// the base unit into the unit of the catalog field. minimum is the schema
// minimum of the field, in the unit of the field. need is set only for an
// I/O row: see neededCap.
type reserveLimit struct {
	field, unit                    string
	value, ceiling, reserve, scale uint64
	minimum, need                  uint64
}

// check refuses a value above the ceiling minus the reserve. It suggests a
// lower value only when the schema accepts that value.
func (r reserveLimit) check() error {
	if r.value == 0 || r.value <= subtractFloor(r.ceiling, r.reserve) {
		return nil
	}
	return r.refuse(func(allowed uint64) error {
		return fmt.Errorf("%w: use %d %s or less for %s, or remove the limits through setup", ErrLimitReserve, allowed, r.unit, r.field)
	})
}

// unset refuses an unset I/O cap whose key the slice caps.
func (r reserveLimit) unset(key string) error {
	return r.refuse(func(allowed uint64) error {
		return fmt.Errorf("%w: the slice caps io.max %s; set %s to %d %s or less, or remove the limits through setup", ErrLimitReserve, key, r.field, allowed, r.unit)
	})
}

// refuse gives suggest the allowed value when the schema accepts it, and
// asks for a larger slice cap otherwise.
func (r reserveLimit) refuse(suggest func(allowed uint64) error) error {
	if allowed := r.allowed(); allowed >= r.minimum {
		return suggest(allowed)
	}
	return r.smallCap()
}

// allowed is the highest value of the field under the ceiling minus the
// reserve, in the unit of the field.
func (r reserveLimit) allowed() uint64 {
	return subtractFloor(r.ceiling, r.reserve) / r.scale
}

// neededCap is the smallest slice cap that leaves the minimum after the
// reserve, in the unit of the field. A fixed reserve gives the minimum plus
// the reserve. An I/O reserve is a share of the cap, so its rows set need.
func (r reserveLimit) neededCap() uint64 {
	if r.need != 0 {
		return r.need
	}
	return r.minimum + r.reserve/r.scale
}

// smallCap reports a slice cap that leaves less than the schema minimum of
// the field after the reserve.
func (r reserveLimit) smallCap() error {
	return fmt.Errorf("%w: after the reserve, the slice cap leaves less than the minimum %d %s for %s; "+
		"raise the slice cap to at least %d %s, or remove the limits through setup", ErrLimitReserve, r.minimum, r.unit, r.field, r.neededCap(), r.unit)
}

// ioReserve is the I/O reserve under a slice cap. It divides first, so a
// very large cap cannot overflow.
func (r LimitReserve) ioReserve(ceiling uint64) uint64 {
	return ceiling/100*r.IOPercent + ceiling%100*r.IOPercent/100
}

// ioCeilingFor is the smallest slice I/O cap that leaves minimum after the
// reserve: c - floor(c*p/100) >= minimum holds when c*(100-p) > 100*(minimum-1).
func (r LimitReserve) ioCeilingFor(minimum uint64) uint64 {
	return 100*(minimum-1)/(100-r.IOPercent) + 1
}

func subtractFloor(value, reserve uint64) uint64 {
	if value < reserve {
		return 0
	}
	return value - reserve
}

// admitMemory checks, just before start, that the slice memory in use plus
// the unit maximum plus the reserve fits under the slice memory ceiling. The
// memory in use is memory.current less the page cache that the kernel can
// reclaim: active_file plus inactive_file of memory.stat (see
// reclaimableCache). A stopped workload leaves the page cache of its model
// in the slice. Failed reads never count as 0. Both files get the trust
// checks of the other slice files. They are usage counters, not caps, so
// any failure to read them is a readback failure.
func (m *SystemdManager) admitMemory(c hostCap, l control.ResourceLimits) error {
	data, err := m.trustedCgroupFile(c.slice, "memory.current")
	if err != nil {
		return fmt.Errorf("%w: the memory in use of %s cannot be read: %v", ErrLimitReadback, c.slice, err)
	}
	current, ok := parseCgroupUint(data)
	if !ok {
		return fmt.Errorf("%w: the memory in use of %s is malformed", ErrLimitReadback, c.slice)
	}
	cache, err := m.reclaimableCache(c.slice)
	if err != nil {
		return err
	}
	// The two files are read at different times, so the cache can be
	// above memory.current.
	reclaimable := min(cache, current)
	inUse := current - reclaimable
	ceiling := c.memoryCeiling()
	need := l.MemoryMaxMiB*mebibyte + m.reserve.memoryBytes()
	if inUse > ceiling || need > ceiling-inUse {
		return fmt.Errorf("%w: %d MiB is in use. The %d MiB of page cache that the kernel can reclaim does not count. "+
			"The start needs %d MiB: memoryMaxMiB plus a %d MiB reserve. The ceiling is %d MiB. Check /dev/shm and tmpfs use", ErrLimitMemory,
			inUse/mebibyte, reclaimable/mebibyte, need/mebibyte, m.reserve.MemoryMiB, ceiling/mebibyte)
	}
	return nil
}

// reclaimableCache reads the page cache on the file LRU lists from the
// slice memory.stat: active_file plus inactive_file. Shmem, such as
// /dev/shm and tmpfs, is on the anon lists and needs swap to leave memory,
// and mlocked pages are unevictable, so neither counts.
func (m *SystemdManager) reclaimableCache(slice string) (uint64, error) {
	data, err := m.trustedCgroupFile(slice, "memory.stat")
	if err != nil {
		return 0, fmt.Errorf("%w: the page cache of %s cannot be read: %v", ErrLimitReadback, slice, err)
	}
	values := map[string]uint64{}
	for _, line := range strings.Split(string(data), "\n") {
		key, value, _ := strings.Cut(line, " ")
		if key != "active_file" && key != "inactive_file" {
			continue
		}
		v, ok := control.CanonicalUint(value)
		if !ok {
			return 0, fmt.Errorf("%w: %s/memory.stat %s is malformed", ErrLimitReadback, slice, key)
		}
		values[key] = v
	}
	active, activeOK := values["active_file"]
	inactive, inactiveOK := values["inactive_file"]
	if !activeOK || !inactiveOK || active > ^uint64(0)-inactive {
		return 0, fmt.Errorf("%w: %s/memory.stat has no valid active_file and inactive_file counts", ErrLimitReadback, slice)
	}
	return active + inactive, nil
}

// requireCappedControls refuses a unit without memoryMaxMiB, tasksMax or
// cpuQuotaPercent. An unset control would let the unit use the reserve of the
// supervisor and proxy under the shared slice cap, which always caps memory,
// tasks and CPU. The schema accepts such limits, so setup can still read and
// remove them. The reserve check also needs each I/O cap that the slice caps.
// checkHostProtection runs it first, so the branches for these unset
// controls in the later checks are defensive code only.
func requireCappedControls(l control.ResourceLimits) error {
	var missing []string
	for _, c := range []struct {
		field string
		value uint64
	}{{"memoryMaxMiB", l.MemoryMaxMiB}, {"tasksMax", l.TasksMax}, {"cpuQuotaPercent", l.CPUQuotaPercent}} {
		if c.value == 0 {
			missing = append(missing, c.field)
		}
	}
	switch len(missing) {
	case 0:
		return nil
	case 1:
		return fmt.Errorf("%w: a unit with limits needs %s to start; set it in the catalog limits, or remove the limits through setup", ErrLimitReserve, missing[0])
	default:
		list := strings.Join(missing[:len(missing)-1], ", ") + " and " + missing[len(missing)-1]
		return fmt.Errorf("%w: a unit with limits needs %s to start; set them in the catalog limits, or remove the limits through setup", ErrLimitReserve, list)
	}
}

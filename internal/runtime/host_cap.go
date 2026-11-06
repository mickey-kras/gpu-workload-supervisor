package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"golang.org/x/sys/unix"
)

// Reserve for the supervisor and proxy under the host cap.
const (
	reserveMemoryMiB  = 2048
	reserveTasks      = 512
	reserveCPUPercent = 100
	reserveIOPercent  = 10
	// reserveMemoryBytes is the memory reserve in bytes.
	reserveMemoryBytes = reserveMemoryMiB * mebibyte
)

// limitHost locates the host files of the limit checks. Tests replace the
// root, the trusted owner, the UID, the file status, the owner lookup and
// the extended attribute lookup.
type limitHost struct {
	root  string
	owner uint32
	uid   int
	// lstat reads the status of a host path without following a symlink.
	lstat func(string) (unix.Stat_t, error)
	// ownerOf returns the owner of a file status. Tests replace it to give
	// one file another owner without root.
	ownerOf func(unix.Stat_t) uint32
	// hasXattr reports whether an open file has an extended attribute.
	// Tests replace it to give one file a file capability without root.
	hasXattr func(fd int, attribute string) (bool, error)
}

func defaultLimitHost() limitHost {
	return limitHost{root: "/", owner: 0, uid: os.Getuid(), ownerOf: statOwner, hasXattr: hasXattr, lstat: func(name string) (unix.Stat_t, error) {
		var stat unix.Stat_t
		err := unix.Lstat(name, &stat)
		return stat, err
	}}
}

// hasXattr reports whether an open file has an extended attribute. A file
// system without extended attributes has none.
func hasXattr(fd int, attribute string) (bool, error) {
	_, err := unix.Fgetxattr(fd, attribute, nil)
	return xattrPresent(err)
}

// xattrPresent classifies the error of an extended attribute lookup. No
// attribute, and a file system without extended attributes, give false.
func xattrPresent(err error) (bool, error) {
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, unix.ENODATA) || errors.Is(err, unix.EOPNOTSUPP):
		return false, nil
	default:
		return false, err
	}
}

func statOwner(stat unix.Stat_t) uint32 {
	return stat.Uid
}

func (h limitHost) path(name string) string {
	return filepath.Join(h.root, name)
}

func (h limitHost) sliceUnit() string {
	return "user-" + strconv.Itoa(h.uid) + ".slice"
}

func (h limitHost) managerUnit() string {
	return "user@" + strconv.Itoa(h.uid) + ".service"
}

func (h limitHost) capDropIn() string {
	return "/etc/systemd/system/" + h.sliceUnit() + ".d/50-gws-host-cap.conf"
}

// setInCap names the fix of a slice file: the setting in the host cap
// drop-in.
func (h limitHost) setInCap(setting string) string {
	return "set " + setting + " in " + h.capDropIn() + ", then run `sudo systemctl daemon-reload`"
}

func (h limitHost) delegateDropIn() string {
	return "/etc/systemd/system/" + h.managerUnit() + ".d/50-gws-delegate-io.conf"
}

// hostCap holds the effective slice ceilings. cpu is microseconds per second.
type hostCap struct {
	slice                 string
	memoryHigh, memoryMax uint64
	tasks, cpu            uint64
	io                    map[string]uint64
}

// sliceCapProperties are the slice cap properties that matchProperties
// compares with the slice cgroup files.
var sliceCapProperties = []string{"MemoryHigh", "MemoryMax", "MemorySwapMax", "TasksMax", "CPUQuotaPerSecUSec"}

var sliceProperties = append([]string{"LoadState", "ControlGroup", "DropInPaths", "NeedDaemonReload"}, sliceCapProperties...)

// verifyHostCap checks the root-owned slice cap of this account: the drop-in
// files, the system manager properties and the slice cgroup files.
func (m *SystemdManager) verifyHostCap(ctx context.Context, managerRoot string, l control.ResourceLimits) (hostCap, error) {
	h := m.host
	values, err := m.showProperties(ctx, false, h.sliceUnit(), sliceProperties)
	if err != nil {
		return hostCap{}, err
	}
	if err := h.checkUnitDropIns(h.sliceUnit(), values, h.capDropIn(), true, m.dropInReader(ctx, h.sliceUnit())); err != nil {
		return hostCap{}, err
	}
	slice, ok := single(values, "ControlGroup")
	if !ok || validateCgroup(slice) != nil || !strings.HasPrefix(managerRoot, slice+"/") {
		return hostCap{}, fmt.Errorf("%w: the user manager cgroup is not inside %s", ErrHostCap, h.sliceUnit())
	}
	limitCap, err := m.readSliceCap(slice, l)
	if err != nil {
		return hostCap{}, err
	}
	if detail := limitCap.matchProperties(h.sliceUnit(), values); detail != "" {
		return hostCap{}, m.sliceCapChanged(ctx, values, detail)
	}
	manager, err := m.showProperties(ctx, false, h.managerUnit(), []string{"LoadState", "DropInPaths", "NeedDaemonReload"})
	if err != nil {
		return hostCap{}, err
	}
	return limitCap, h.checkUnitDropIns(h.managerUnit(), manager, h.delegateDropIn(), l.UsesIO(), m.dropInReader(ctx, h.managerUnit()))
}

// sliceCapChanged classifies a slice property that differs from its cgroup
// file. An administrator can change the cap between the show call and the
// file reads. So the check reads the properties again: changed values, a
// reload need or a failed read are a readback failure. The same values keep
// the definite failure.
// detail is the mismatch without its class: a readback failure keeps it
// with one class prefix.
func (m *SystemdManager) sliceCapChanged(ctx context.Context, first map[string][]string, detail string) error {
	unit := m.host.sliceUnit()
	again, err := m.showProperties(ctx, false, unit, sliceProperties)
	if err != nil {
		return fmt.Errorf("%w: %s, and the properties of %s cannot be read again: %w", ErrLimitReadback, detail, unit, err)
	}
	if err := reloadNeed(unit, again); err != nil {
		return fmt.Errorf("%w; first read: %s", err, detail)
	}
	for _, name := range sliceCapProperties {
		if !slices.Equal(first[name], again[name]) {
			return fmt.Errorf("%w: %s, and the %s of %s changed during the check", ErrLimitReadback, detail, name, unit)
		}
	}
	return fmt.Errorf("%w: %s", ErrHostCap, detail)
}

// dropInReader reads the drop-in list and the reload flag of a system unit
// again.
func (m *SystemdManager) dropInReader(ctx context.Context, unit string) func() (map[string][]string, error) {
	return func() (map[string][]string, error) {
		return m.showProperties(ctx, false, unit, []string{"DropInPaths", "NeedDaemonReload"})
	}
}

// checkUnitDropIns accepts only the host cap drop-in and vendor files. Each
// listed file, including the host cap drop-in named by own, must pass
// trustedFile. Each error names unit. reread reads the list again when a
// listed file is missing.
func (h limitHost) checkUnitDropIns(unit string, values map[string][]string, own string, require bool, reread func() (map[string][]string, error)) error {
	if state, _ := single(values, "LoadState"); state != "loaded" {
		return fmt.Errorf("%w: %s is not loaded", ErrHostCap, unit)
	}
	if err := reloadNeed(unit, values); err != nil {
		return err
	}
	var paths []string
	for _, value := range values["DropInPaths"] {
		paths = append(paths, strings.Fields(value)...)
	}
	if require && !slices.Contains(paths, own) {
		return fmt.Errorf("%w: %s does not load %s", ErrHostCap, unit, own)
	}
	for _, name := range paths {
		if err := h.trustedDropIn(name, own); err != nil {
			return fmt.Errorf("drop-in of %s: %w", unit, dropInGone(unit, name, err, reread))
		}
	}
	return nil
}

// dropInGone classifies a listed drop-in that is missing. A package upgrade
// can remove a vendor drop-in after the show call. If the list changed, or
// the unit now needs daemon-reload, the missing file proves no wrong cap:
// it is a readback failure. If the list still names the file, the missing
// file is definite.
func dropInGone(unit, name string, err error, reread func() (map[string][]string, error)) error {
	if !errors.Is(err, unix.ENOENT) {
		return err
	}
	values, readErr := reread()
	if readErr != nil {
		return fmt.Errorf("%w: %s is missing, and the drop-in list cannot be read again: %w", ErrLimitReadback, name, readErr)
	}
	if reloadErr := reloadNeed(unit, values); reloadErr != nil {
		return fmt.Errorf("%w; %s went away during the check", reloadErr, name)
	}
	if !slices.ContainsFunc(values["DropInPaths"], func(value string) bool { return slices.Contains(strings.Fields(value), name) }) {
		return fmt.Errorf("%w: %s went away during the check; the drop-ins changed", ErrLimitReadback, name)
	}
	return err
}

// reloadNeed checks the NeedDaemonReload flag of a system unit. A package
// upgrade that changes a vendor drop-in sets it until its daemon-reload. A
// missing or repeated flag is a problem of the show output. The loaded
// properties and the cgroup files are checked separately, so neither
// proves a wrong cap: both are readback failures. A launch still fails,
// and Start stops the unit that it just started. An already running unit
// (status, reconcile) does not stop.
func reloadNeed(unit string, values map[string][]string) error {
	reload, ok := single(values, "NeedDaemonReload")
	if !ok {
		return fmt.Errorf("%w: %s: NeedDaemonReload is missing or ambiguous in the systemctl show output", ErrLimitReadback, unit)
	}
	if reload != "no" {
		return fmt.Errorf("%w: %s needs daemon-reload; run `sudo systemctl daemon-reload` for the system manager", ErrLimitReadback, unit)
	}
	return nil
}

// trustedDropIn checks one listed drop-in: own, or a vendor file.
func (h limitHost) trustedDropIn(name, own string) error {
	if name != own {
		vendor, err := h.vendorFile(name, own)
		if err != nil {
			return err
		}
		name = vendor
	}
	return h.trustedFile(name)
}

// vendorFile accepts a drop-in under /usr/lib/systemd/system. On a merged
// /usr host, systemd can name it under /lib, when /lib is a root-owned link
// to usr/lib or /usr/lib. The check reads that link and returns the
// /usr/lib name, which trustedFile then walks without symlinks.
func (h limitHost) vendorFile(name, own string) (string, error) {
	if path.Clean(name) != name {
		return "", unknownDropIn(name, own)
	}
	if rest, ok := strings.CutPrefix(name, "/lib/systemd/system/"); ok {
		if err := h.checkLibLink(); err != nil {
			return "", fmt.Errorf("%s needs a root-owned /lib link to usr/lib or /usr/lib: %w", name, err)
		}
		name = "/usr/lib/systemd/system/" + rest
	}
	if !strings.HasPrefix(name, "/usr/lib/systemd/system/") {
		return "", unknownDropIn(name, own)
	}
	return name, nil
}

// unknownDropIn refuses a drop-in that is neither own nor a vendor file,
// and names the fix.
func unknownDropIn(name, own string) error {
	origin := ""
	switch {
	case strings.HasPrefix(name, "/etc/systemd/system.control/"):
		origin = "; it came from `systemctl set-property`"
	case strings.HasPrefix(name, "/run/systemd/system.control/"):
		origin = "; it came from `systemctl set-property --runtime` and goes away at reboot"
	}
	return fmt.Errorf("%w: unknown drop-in %s%s; remove it, or move its settings into %s, then run `sudo systemctl daemon-reload`",
		ErrHostCap, name, origin, own)
}

// checkLibLink requires /lib to be a root-owned link to usr/lib or /usr/lib.
// A missing /lib, a /lib that is not a link, a link elsewhere and another
// owner are ErrHostCap. Other read failures are readback failures.
func (h limitHost) checkLibLink() error {
	target, err := os.Readlink(h.path("/lib"))
	switch {
	case errors.Is(err, unix.EINVAL):
		return fmt.Errorf("%w: /lib is not a link", ErrHostCap)
	case err != nil:
		return libError(err)
	case target != "usr/lib" && target != "/usr/lib":
		return fmt.Errorf("%w: /lib links to %q", ErrHostCap, target)
	}
	stat, err := h.lstat(h.path("/lib"))
	if err != nil {
		return libError(err)
	}
	if owner := h.ownerOf(stat); owner != h.owner {
		return fmt.Errorf("%w: /lib is owned by uid %d", ErrHostCap, owner)
	}
	return nil
}

// libError classifies a failed read of /lib like a host file. A missing
// /lib does not keep ENOENT, so the caller does not take it for a missing
// drop-in.
func libError(err error) error {
	if errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("%w: /lib is missing", ErrHostCap)
	}
	return hostFileError("/lib", err)
}

// trustedFile walks name from the root without symlinks. Each directory and
// the regular file must have the trusted owner, no group or other write, no
// ACL and no file capability.
func (h limitHost) trustedFile(name string) error {
	fd, err := openNoFollow(h.root, strings.Split(strings.TrimPrefix(name, "/"), "/"), unix.O_NONBLOCK|unix.O_NOCTTY, h.trustedDescriptor)
	if err != nil {
		return hostFileError(name, err)
	}
	defer func() { _ = unix.Close(fd) }()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return hostFileError(name, err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return fmt.Errorf("%w: %s is not a regular file", ErrHostCap, name)
	}
	return nil
}

// errUntrusted marks a proven trust failure of an open file.
var errUntrusted = errors.New("untrusted file")

// hostFileError keeps ErrHostCap for proven mismatches: an untrusted file, a
// missing file, a symlink or a file where a directory must be. Other open
// and read failures are readback failures, which a later probe can pass.
// A walk failure names the component that failed, such as a writable
// parent directory, not only the file.
func hostFileError(name string, err error) error {
	var walk *walkError
	if errors.As(err, &walk) && walk.path != "" {
		name, err = walk.path, walk.err
	}
	if errors.Is(err, errUntrusted) || definiteFileErrno(err) {
		return fmt.Errorf("%w: %s: %w", ErrHostCap, name, err)
	}
	return fmt.Errorf("%w: %s: %w", ErrLimitReadback, name, err)
}

func (h limitHost) trustedDescriptor(fd int) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return err
	}
	if h.ownerOf(stat) != h.owner || stat.Mode&0o022 != 0 {
		return fmt.Errorf("%w: the owner is not root, or group or others can write", errUntrusted)
	}
	return h.noExtendedRights(fd)
}

// noExtendedRights rejects any access ACL or file capability.
func (h limitHost) noExtendedRights(fd int) error {
	for _, attribute := range []string{"system.posix_acl_access", "security.capability"} {
		present, err := h.hasXattr(fd, attribute)
		if err != nil {
			return err
		}
		if present {
			return fmt.Errorf("%w: an ACL or file capability is present", errUntrusted)
		}
	}
	return nil
}

// readSliceCap reads the slice ceilings from root-owned cgroup files. Failed
// reads never count as 0 or as no limit.
func (m *SystemdManager) readSliceCap(slice string, l control.ResourceLimits) (hostCap, error) {
	limitCap := hostCap{slice: slice, io: map[string]uint64{}}
	for _, target := range []struct {
		name, property string
		value          *uint64
	}{{"memory.high", "MemoryHigh", &limitCap.memoryHigh}, {"memory.max", "MemoryMax", &limitCap.memoryMax}, {"pids.max", "TasksMax", &limitCap.tasks}} {
		data, err := m.trustedCgroupFile(slice, target.name)
		if err != nil {
			return hostCap{}, err
		}
		v, ok := parseCgroupUint(data)
		if !ok {
			return hostCap{}, fmt.Errorf("%w: %s/%s is not a finite limit; %s", ErrHostCap, slice, target.name, m.host.setInCap(target.property))
		}
		*target.value = v
	}
	cpu, err := m.readSliceCPU(slice)
	if err != nil {
		return hostCap{}, err
	}
	limitCap.cpu = cpu
	if err := m.checkSliceSwap(slice); err != nil {
		return hostCap{}, err
	}
	return limitCap, m.readSliceIO(&limitCap, l)
}

// trustedCgroupFile reads a host cgroup file, of the slice or of the
// hierarchy root, that must be root-owned without group or other write, ACL
// or file capability. A hierarchy that cannot be opened proves nothing about
// the file.
func (m *SystemdManager) trustedCgroupFile(group, name string) ([]byte, error) {
	data, err := m.cgroups.readChecked(group, name, m.host.trustedDescriptor)
	if errors.Is(err, unix.ENOENT) && !errors.Is(err, errControlFileAbsent) {
		return nil, fmt.Errorf("%w: %s: %w", ErrLimitReadback, path.Join(group, name), err)
	}
	if err != nil {
		return nil, hostFileError(path.Join(group, name), err)
	}
	return data, nil
}

// readSliceCPU returns the slice CPU quota in microseconds per second.
func (m *SystemdManager) readSliceCPU(slice string) (uint64, error) {
	data, err := m.trustedCgroupFile(slice, "cpu.max")
	if err != nil {
		return 0, err
	}
	notFinite := fmt.Errorf("%w: %s/cpu.max is not a finite quota; %s", ErrHostCap, slice, m.host.setInCap("CPUQuota"))
	fields := strings.Fields(string(data))
	if len(fields) != 2 {
		return 0, notFinite
	}
	quota, quotaOK := control.CanonicalUint(fields[0])
	period, periodOK := control.CanonicalUint(fields[1])
	if !quotaOK || !periodOK || period == 0 {
		return 0, notFinite
	}
	if quota > ^uint64(0)/1000000 || quota*1000000%period != 0 {
		// systemd writes the quota for the period of the slice. Only the
		// default period gives a whole per-second quota for each CPUQuota
		// value, and a non-default slice period is unsupported.
		return 0, fmt.Errorf("%w: %s/cpu.max %q does not give a whole number of microseconds per second; "+
			"remove CPUQuotaPeriodSec from the slice drop-in", ErrHostCap, slice, strings.TrimSpace(string(data)))
	}
	return quota * 1000000 / period, nil
}

// checkSliceSwap requires memory.swap.max 0. A unit with limits needs
// MemoryMax, and requireManagerFiles already requires swap accounting, so a
// missing file is a missing host file.
func (m *SystemdManager) checkSliceSwap(slice string) error {
	data, err := m.trustedCgroupFile(slice, "memory.swap.max")
	if err != nil {
		return err
	}
	if strings.TrimSuffix(string(data), "\n") != "0" {
		return fmt.Errorf("%w: %s/memory.swap.max is not 0; %s", ErrHostCap, slice, m.host.setInCap("MemorySwapMax=0"))
	}
	return nil
}

// ioLimit is one io.max key, the catalog field that sets it, the unit of
// that field, the systemd property that sets the key, the schema minimum of
// the field and its unit value.
type ioLimit struct {
	key, field, unit, property string
	minimum                    uint64
	value                      func(control.ResourceLimits) uint64
}

// ioLimits lists the io.max keys in the order of io.max.
var ioLimits = []ioLimit{
	{"rbps", "ioReadBandwidthMax", "bytes per second", "IOReadBandwidthMax", control.MinLimitIOBandwidth, func(l control.ResourceLimits) uint64 { return l.IOReadBandwidthMax }},
	{"wbps", "ioWriteBandwidthMax", "bytes per second", "IOWriteBandwidthMax", control.MinLimitIOBandwidth, func(l control.ResourceLimits) uint64 { return l.IOWriteBandwidthMax }},
	{"riops", "ioReadIOPSMax", "operations per second", "IOReadIOPSMax", control.MinLimitIOPS, func(l control.ResourceLimits) uint64 { return l.IOReadIOPSMax }},
	{"wiops", "ioWriteIOPSMax", "operations per second", "IOWriteIOPSMax", control.MinLimitIOPS, func(l control.ResourceLimits) uint64 { return l.IOWriteIOPSMax }},
}

// knownIOKey reports an io.max key that a unit can cap.
func knownIOKey(key string) bool {
	return slices.ContainsFunc(ioLimits, func(limit ioLimit) bool { return limit.key == key })
}

// readSliceIO requires a finite slice cap for each I/O cap of the unit. It
// refuses a slice cap on a device other than the unit ioDevice.
func (m *SystemdManager) readSliceIO(limitCap *hostCap, l control.ResourceLimits) error {
	data, err := m.trustedCgroupFile(limitCap.slice, "io.max")
	if errors.Is(err, errControlFileAbsent) && !l.ThrottlesIO() {
		// Without the io controller the slice caps no I/O.
		return nil
	}
	if err != nil {
		return err
	}
	// A cap of the slice on any other device needs a matching unit cap on
	// that device, which a unit with one I/O device cannot give: the unit
	// could use the reserve on that device.
	own, other := ioMaxCaps(string(data), l.IODeviceNumber)
	if other != "" {
		device := l.IODevice
		if device == "" {
			device = "none"
		}
		return fmt.Errorf("%w: the slice caps the I/O of device %s, but a unit with limits caps only its one ioDevice (%s); "+
			"remove the cap on %s from the slice drop-in, or set ioDevice to that device with its I/O caps, or remove the limits through setup", ErrLimitReserve, m.host.deviceName(other), device, other)
	}
	limitCap.io = own
	for _, limit := range ioLimits {
		if limit.value(l) != 0 && limitCap.io[limit.key] == 0 {
			return fmt.Errorf("%w: %s/io.max has no %s cap for %s, which %s needs; add an %s line for %s to %s, or remove the limits through setup",
				ErrHostCap, limitCap.slice, limit.key, m.host.deviceName(l.IODeviceNumber), limit.field, limit.property, l.IODevice, m.host.capDropIn())
		}
	}
	return nil
}

// deviceName adds the kernel name of a device number from sysfs, as in
// "8:0 (/dev/sda)". Without that name it returns the number alone.
func (h limitHost) deviceName(number string) string {
	if name, err := h.kernelName(number); err == nil && name != "" {
		return number + " (/dev/" + name + ")"
	}
	return number
}

// kernelName returns the DEVNAME of a device number from sysfs, or empty
// when the uevent file has none.
func (h limitHost) kernelName(number string) (string, error) {
	uevent, err := os.ReadFile(h.path("/sys/dev/block/" + number + "/uevent"))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(uevent), "\n") {
		if name, ok := strings.CutPrefix(line, "DEVNAME="); ok {
			return name, nil
		}
	}
	return "", nil
}

// ioMaxCaps parses io.max once. It returns the finite caps of the device
// number, and the first other device with a finite cap, or empty.
func ioMaxCaps(data, number string) (own map[string]uint64, other string) {
	own = map[string]uint64{}
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		caps := lineCaps(fields[1:])
		if fields[0] == number {
			own = caps
		} else if other == "" && len(caps) != 0 {
			other = fields[0]
		}
	}
	return own, other
}

// lineCaps returns the finite caps of the key=value fields of one io.max
// line. A key that a unit cannot cap is no cap.
func lineCaps(fields []string) map[string]uint64 {
	caps := map[string]uint64{}
	for _, field := range fields {
		key, value, _ := strings.Cut(field, "=")
		if v, ok := control.CanonicalUint(value); ok && knownIOKey(key) {
			caps[key] = v
		}
	}
	return caps
}

// matchProperties compares the system manager properties of the slice unit
// with its cgroup files, so a cap that systemd could not apply fails. The
// kernel keeps memory.high and memory.max in whole pages, so a byte value
// that is not page aligned matches its value rounded down to a page.
//
// hostCap keeps no swap value, and checkSliceSwap has already read the
// slice memory.swap.max as 0. So MemorySwapMax is checked first, against
// 0. Then the first mismatch in the order below fails. It returns that
// mismatch, which names unit, without its class, or empty.
func (c hostCap) matchProperties(unit string, values map[string][]string) string {
	if got, _ := single(values, "MemorySwapMax"); got != "0" {
		return fmt.Sprintf("MemorySwapMax of %s is %q, but must be 0", unit, got)
	}
	for _, p := range []struct {
		name, want string
		pages      bool
	}{
		{"MemoryHigh", strconv.FormatUint(c.memoryHigh, 10), true},
		{"MemoryMax", strconv.FormatUint(c.memoryMax, 10), true},
		{"TasksMax", strconv.FormatUint(c.tasks, 10), false},
		{"CPUQuotaPerSecUSec", formatTimespan(c.cpu), false},
	} {
		got, _ := single(values, p.name)
		if got != p.want && (!p.pages || pageFloor(got) != p.want) {
			return fmt.Sprintf("%s of %s is %q, but its cgroup holds %q", p.name, unit, got, p.want)
		}
	}
	return ""
}

// pageFloor rounds a byte count down to a whole page, as the kernel does
// for memory.high and memory.max. It returns "" for a value that is not a
// byte count.
func pageFloor(value string) string {
	v, ok := control.CanonicalUint(value)
	if !ok {
		return ""
	}
	page := uint64(os.Getpagesize())
	return strconv.FormatUint(v/page*page, 10)
}

// memoryCeiling is the lower of the slice memory.high and memory.max, so
// the reserve stays below the slice hard limit too.
func (c hostCap) memoryCeiling() uint64 {
	return min(c.memoryHigh, c.memoryMax)
}

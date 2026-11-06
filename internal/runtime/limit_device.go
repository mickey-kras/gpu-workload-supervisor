package runtime

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"golang.org/x/sys/unix"
)

// verifyLimitDevice accepts only a root-owned leaf block device with the
// catalog device number, and an I/O weight provider when IOWeight is set. It
// returns that provider, or empty when IOWeight is not set.
func (m *SystemdManager) verifyLimitDevice(l control.ResourceLimits) (string, error) {
	if !l.UsesIO() {
		return "", nil
	}
	stat, err := m.host.lstat(m.host.path(l.IODevice))
	if err != nil {
		return "", deviceError(l.IODevice, err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFBLK {
		return "", fmt.Errorf("%w: %s is not a block device", ErrLimitDevice, l.IODevice)
	}
	if m.host.ownerOf(stat) != m.host.owner {
		return "", fmt.Errorf("%w: %s is not owned by root", ErrLimitDevice, l.IODevice)
	}
	// A disk can get another number after a reboot or a hotplug.
	if number := fmt.Sprintf("%d:%d", unix.Major(stat.Rdev), unix.Minor(stat.Rdev)); number != l.IODeviceNumber {
		return "", fmt.Errorf("%w: %s has device number %s, not the catalog ioDeviceNumber %s; "+
			"update the limits for the new number, or remove the limits through setup",
			ErrLimitDevice, l.IODevice, number, l.IODeviceNumber)
	}
	name, err := m.host.kernelName(l.IODeviceNumber)
	if err != nil {
		return "", deviceError(l.IODeviceNumber, err)
	}
	if name == "" || name != strings.TrimPrefix(l.IODevice, "/dev/") {
		return "", fmt.Errorf("%w: %s does not name %s", ErrLimitDevice, l.IODeviceNumber, l.IODevice)
	}
	if err := checkLeafDevice(m.host.path("/sys/dev/block/"+l.IODeviceNumber), l.IODevice); err != nil {
		return "", err
	}
	if l.IOWeight != 0 {
		return m.ioWeightProvider(l.IODeviceNumber)
	}
	return "", nil
}

// wholeDiskRemedy is the next step for a device that is not a whole leaf
// disk.
const wholeDiskRemedy = "set ioDevice and ioDeviceNumber to the whole leaf disk under it (see `lsblk -s`), or remove the limits through setup"

// checkLeafDevice requires no partition file, a request queue and an empty
// slaves directory. These sysfs reads use the os package directly, not the
// lstat seam of limitHost: the fixture writes a sysfs tree under its root.
func checkLeafDevice(device, name string) error {
	// A partition has a partition file and no request queue, so it is
	// checked first.
	if _, err := os.Lstat(filepath.Join(device, "partition")); err == nil {
		return fmt.Errorf("%w: %s is a partition; %s", ErrLimitDevice, name, wholeDiskRemedy)
	} else if !errors.Is(err, os.ErrNotExist) {
		return deviceError(name+" partition", err)
	}
	info, err := os.Stat(filepath.Join(device, "queue"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return deviceError(name+" queue", err)
	}
	if err != nil || !info.IsDir() {
		return fmt.Errorf("%w: %s has no request queue; %s", ErrLimitDevice, name, wholeDiskRemedy)
	}
	slaves, err := os.ReadDir(filepath.Join(device, "slaves"))
	if err != nil {
		return deviceError(name+" slaves", err)
	}
	if len(slaves) != 0 {
		return fmt.Errorf("%w: %s is a stacked device; %s", ErrLimitDevice, name, wholeDiskRemedy)
	}
	return nil
}

// deviceError keeps ErrLimitDevice for a missing device file. Other stat
// and read failures are readback failures, which a later probe can pass.
func deviceError(name string, err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf(limitPathErrorFormat, ErrLimitDevice, name, err)
	}
	return fmt.Errorf(limitPathErrorFormat, ErrLimitReadback, name, err)
}

// ioWeightProvider returns "bfq" when the device uses BFQ, or "iocost" when
// the root io.cost.qos enables the device. A read failure is a readback
// failure, not a missing provider, and an untrusted io.cost.qos is a host
// cap failure. A kernel without iocost has no io.cost.qos file.
func (m *SystemdManager) ioWeightProvider(number string) (string, error) {
	name := m.host.path("/sys/dev/block/" + number + "/queue/scheduler")
	scheduler, err := os.ReadFile(name)
	if err != nil {
		return "", fmt.Errorf(limitPathErrorFormat, ErrLimitReadback, name, err)
	}
	if slices.Contains(strings.Fields(string(scheduler)), "[bfq]") {
		return "bfq", nil
	}
	// The root io.cost.qos is a host file, so it gets the same owner, mode
	// and extended attribute checks as the slice files.
	qos, err := m.trustedCgroupFile("/", "io.cost.qos")
	if errors.Is(err, errControlFileAbsent) {
		return "", noIOWeight(m.host, number)
	}
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(qos), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 1 && fields[0] == number && slices.Contains(fields[1:], "enable=1") {
			return "iocost", nil
		}
	}
	return "", noIOWeight(m.host, number)
}

// noIOWeight reports a device that neither BFQ nor iocost weights.
func noIOWeight(h limitHost, number string) error {
	return fmt.Errorf("%w: %s does not support I/O weight (no BFQ or iocost)", ErrLimitDevice, h.deviceName(number))
}

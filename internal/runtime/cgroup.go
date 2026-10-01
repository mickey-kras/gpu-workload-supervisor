package runtime

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"golang.org/x/sys/unix"
)

// cgroupFS reads only the kernel's recursive populated bit. No PID is cached or
// attributed to a workload, so PID reuse and unsupported GPU accounting cannot
// turn an unrelated allocation into release evidence.
type cgroupFS struct {
	root   string
	verify func(int) error
}

func verifyCgroup2(fd int) error {
	var stat unix.Statfs_t
	if err := unix.Fstatfs(fd, &stat); err != nil {
		return err
	}
	if stat.Type != unix.CGROUP2_SUPER_MAGIC {
		return errors.New("release verification requires the unified cgroup v2 hierarchy")
	}
	return nil
}

// Only the host unified hierarchy is supported. A subtree mount can have the
// right filesystem type while making a real systemd ControlGroup look absent.
func verifyUnifiedHierarchy(fd int) error {
	if err := verifyCgroup2(fd); err != nil {
		return err
	}
	mounts, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return err
	}
	if err := verifyCgroupMount(string(mounts)); err != nil {
		return err
	}
	return nil
}

func verifyCgroupMount(mounts string) error {
	matches := 0
	for _, line := range strings.Split(mounts, "\n") {
		before, after, ok := strings.Cut(line, " - ")
		fields, filesystem := strings.Fields(before), strings.Fields(after)
		if len(fields) >= 6 && strings.HasPrefix(fields[4], "/sys/fs/cgroup/") {
			return errors.New("nested cgroup mount mappings are unsupported")
		}
		if len(fields) < 6 || fields[4] != "/sys/fs/cgroup" {
			continue
		}
		if !ok || len(filesystem) < 3 || filesystem[0] != "cgroup2" || fields[3] != "/" {
			return errors.New("unsupported cgroup mount mapping; require host cgroup2 root at /sys/fs/cgroup")
		}
		matches++
	}
	if matches != 1 {
		return errors.New("cannot identify a unique unified cgroup root mount")
	}
	return nil
}

func validateCgroup(group string) error {
	if group == "/" || !strings.HasPrefix(group, "/") || path.Clean(group) != group || strings.ContainsAny(group, "\x00\r\n") {
		return errors.New("cgroup must be a canonical non-root absolute path within /sys/fs/cgroup")
	}
	return nil
}

func (fs cgroupFS) empty(group string) error {
	return fs.check(group, true)
}

// A manager anchor must exist, even when an individual workload was removed.
func (fs cgroupFS) check(group string, requireEmpty bool) error {
	fd, err := unix.Open(fs.root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open cgroup hierarchy: %w", err)
	}
	defer func() { _ = unix.Close(fd) }()
	if err := fs.verify(fd); err != nil {
		return fmt.Errorf("verify cgroup hierarchy: %w", err)
	}
	// Walk with directory descriptors and no symlinks, keeping removal distinct
	// from inaccessible state. ENOENT here means the configured group is removed;
	// ENOENT for cgroup.events in an existing group is NOT proof of release.
	for _, component := range strings.Split(strings.TrimPrefix(group, "/"), "/") {
		next, err := unix.Openat(fd, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if errors.Is(err, unix.ENOENT) && requireEmpty {
			return nil
		}
		if err != nil {
			return fmt.Errorf("inspect cgroup %s: %w", group, err)
		}
		_ = unix.Close(fd)
		fd = next
		if err := fs.verify(fd); err != nil {
			return fmt.Errorf("verify cgroup %s: %w", group, err)
		}
	}
	eventsFD, err := unix.Openat(fd, "cgroup.events", unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return fmt.Errorf("read cgroup.events for %s: %w", group, err)
	}
	file := os.NewFile(uintptr(eventsFD), "cgroup.events")
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil {
		return fmt.Errorf("read cgroup.events for %s: %w", group, err)
	}
	if len(data) > 4096 {
		return errors.New("oversized cgroup.events")
	}
	err = parseCgroupEvents(string(data))
	if !requireEmpty && errors.Is(err, errCgroupPopulated) {
		return nil
	}
	return err
}

var errCgroupPopulated = errors.New("workload cgroup still contains processes (including descendants)")

func parseCgroupEvents(data string) error {
	populated := ""
	for _, line := range strings.Split(strings.TrimSpace(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return errors.New("malformed cgroup.events")
		}
		if fields[0] == "populated" {
			if populated != "" || (fields[1] != "0" && fields[1] != "1") {
				return errors.New("ambiguous cgroup.events populated value")
			}
			populated = fields[1]
		}
	}
	if populated == "" {
		return errors.New("cgroup.events is missing populated evidence")
	}
	if populated == "1" {
		return errCgroupPopulated
	}
	return nil
}

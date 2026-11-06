package runtime

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
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
	return requireCgroup2(stat.Type)
}

func requireCgroup2(magic int64) error {
	if magic != unix.CGROUP2_SUPER_MAGIC {
		return errors.New("release verification requires the unified cgroup v2 hierarchy")
	}
	return nil
}

// Only the host unified hierarchy is supported. A subtree mount can have the
// right filesystem type while making a real systemd ControlGroup look absent.
func verifyUnifiedHierarchy(fd int) error {
	return verifyHierarchy(fd, verifyCgroup2, selfMountinfo)
}

func selfMountinfo() (string, error) {
	mounts, err := os.ReadFile("/proc/self/mountinfo")
	return string(mounts), err
}

func verifyHierarchy(fd int, verifyFS func(int) error, mountinfo func() (string, error)) error {
	if err := verifyFS(fd); err != nil {
		return err
	}
	mounts, err := mountinfo()
	if err != nil {
		return err
	}
	return verifyCgroupMount(mounts)
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
	data, err := fs.read(group, "cgroup.events")
	if errors.Is(err, errCgroupRemoved) && requireEmpty {
		return nil
	}
	if err != nil {
		return err
	}
	err = parseCgroupEvents(string(data))
	if !requireEmpty && errors.Is(err, errCgroupPopulated) {
		return nil
	}
	return err
}

// memoryPeak reads the recursive cgroup v2 memory high-water mark. A removed
// group or a kernel without memory.peak gives an error: the peak is unmeasured.
func (fs cgroupFS) memoryPeak(group string) (uint64, error) {
	data, err := fs.read(group, "memory.peak")
	if err != nil {
		return 0, err
	}
	peak, ok := parseCgroupUint(data)
	if !ok {
		return 0, errors.New("malformed cgroup memory.peak")
	}
	return peak, nil
}

// parseCgroupUint parses a cgroup file that holds one canonical unsigned
// integer and a newline.
func parseCgroupUint(data []byte) (uint64, bool) {
	return control.CanonicalUint(strings.TrimSuffix(string(data), "\n"))
}

var errCgroupRemoved = errors.New("cgroup is removed")

// errControlFileAbsent marks a control file that an existing group does not
// have, as opposed to a group or hierarchy that cannot be opened.
var errControlFileAbsent = errors.New("the control file is absent")

// read returns one bounded control file of an existing group.
func (fs cgroupFS) read(group, name string) ([]byte, error) {
	return fs.readChecked(group, name, nil)
}

// readChecked runs check, when set, on the open file before it reads it, so
// a caller can check the owner, mode and extended attributes of the same
// file it reads.
func (fs cgroupFS) readChecked(group, name string, check func(int) error) ([]byte, error) {
	fd, err := fs.openGroup(group)
	if err != nil {
		return nil, err
	}
	defer func() { _ = unix.Close(fd) }()
	fileFD, err := unix.Openat(fd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil, fmt.Errorf("read %s for %s: %w: %w", name, group, errControlFileAbsent, err)
	}
	if err != nil {
		return nil, fmt.Errorf("read %s for %s: %w", name, group, err)
	}
	file := os.NewFile(uintptr(fileFD), name)
	defer file.Close()
	if check != nil {
		if err := check(fileFD); err != nil {
			return nil, fmt.Errorf("check %s for %s: %w", name, group, err)
		}
	}
	data, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil {
		return nil, fmt.Errorf("read %s for %s: %w", name, group, err)
	}
	if len(data) > 4096 {
		return nil, fmt.Errorf("oversized %s", name)
	}
	return data, nil
}

// openGroup walks to an existing group with directory descriptors and no
// symlinks, keeping removal distinct from inaccessible state. ENOENT here
// means the configured group is removed; ENOENT for a control file in an
// existing group is NOT proof of release. The group "/" names the hierarchy
// root, and ENOENT of the root is never removal.
func (fs cgroupFS) openGroup(group string) (int, error) {
	var components []string
	if group != "/" {
		components = strings.Split(strings.TrimPrefix(group, "/"), "/")
	}
	fd, err := openNoFollow(fs.root, components, unix.O_DIRECTORY, fs.verify)
	if err == nil {
		return fd, nil
	}
	// openNoFollow returns only walk errors.
	var walk *walkError
	if !errors.As(err, &walk) {
		return -1, err
	}
	switch {
	case walk.path == "" && walk.verify:
		return -1, fmt.Errorf("verify cgroup hierarchy: %w", walk.err)
	case walk.path == "":
		return -1, fmt.Errorf("open cgroup hierarchy: %w", walk.err)
	case walk.verify:
		return -1, fmt.Errorf("verify cgroup %s: %w", group, walk.err)
	case errors.Is(walk.err, unix.ENOENT):
		return -1, fmt.Errorf("inspect cgroup %s: %w", group, errCgroupRemoved)
	default:
		return -1, fmt.Errorf("inspect cgroup %s: %w", group, walk.err)
	}
}

// errNonCanonicalPath marks an empty, "." or ".." component in a walk.
var errNonCanonicalPath = errors.New("the path is not canonical")

// walkError is a failure of openNoFollow. path is the part of the path that
// the walk reached, with the failed component last, or empty for the root.
// verify tells a verify failure from an open failure.
type walkError struct {
	path   string
	verify bool
	err    error
}

func (e *walkError) Error() string {
	if e.path == "" {
		return e.err.Error()
	}
	return e.path + ": " + e.err.Error()
}

func (e *walkError) Unwrap() error { return e.err }

// openNoFollow opens root as a directory, then each component below it in
// turn, without symlinks, and closes each parent. Each component but the
// last opens as a directory; the last opens with lastFlags. verify runs on
// the root and on each descriptor that it opens. It refuses an empty, "."
// or ".." component, so a walk cannot leave its root. On error, it closes
// every descriptor.
func openNoFollow(root string, components []string, lastFlags int, verify func(int) error) (int, error) {
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, &walkError{err: err}
	}
	if err := verify(fd); err != nil {
		_ = unix.Close(fd)
		return -1, &walkError{verify: true, err: err}
	}
	for i, component := range components {
		path := "/" + strings.Join(components[:i+1], "/")
		if component == "" || component == "." || component == ".." {
			_ = unix.Close(fd)
			return -1, &walkError{path: path, err: fmt.Errorf("%w: path component %q", errNonCanonicalPath, component)}
		}
		flags := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_CLOEXEC | unix.O_DIRECTORY
		if i == len(components)-1 {
			flags = unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_CLOEXEC | lastFlags
		}
		next, err := unix.Openat(fd, component, flags, 0)
		_ = unix.Close(fd)
		if err != nil {
			return -1, &walkError{path: path, err: err}
		}
		fd = next
		if err := verify(fd); err != nil {
			_ = unix.Close(fd)
			return -1, &walkError{path: path, verify: true, err: err}
		}
	}
	return fd, nil
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

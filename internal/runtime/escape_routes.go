package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"golang.org/x/sys/unix"
)

// escapeHost holds the process facts of the escape route checks. Tests
// replace them to check a fixture root without root privileges.
type escapeHost struct {
	// pid is the process that pkcheck asks polkit about.
	pid int
	// verify runs on each descriptor of an escape route walk before any
	// trust check. Tests replace it to make one file unreadable as root.
	verify func(fd int) error
	// processGroups gives the group IDs of this process.
	processGroups func() ([]uint32, error)
	// socketACL reports whether a socket path has an access ACL.
	socketACL func(name string) (bool, error)
}

func defaultEscapeHost() escapeHost {
	return escapeHost{pid: os.Getpid(), verify: func(int) error { return nil }, processGroups: processGroups, socketACL: pathHasACL}
}

// processGroups gives the primary and supplementary group IDs of this
// process.
func processGroups() ([]uint32, error) {
	extra, err := unix.Getgroups()
	if err != nil {
		return nil, err
	}
	gids := []uint32{uint32(unix.Getgid())}
	for _, gid := range extra {
		gids = append(gids, uint32(gid))
	}
	return gids, nil
}

// pathHasACL reports whether name has an access ACL. A file system without
// extended attributes has none.
func pathHasACL(name string) (bool, error) {
	_, err := unix.Getxattr(name, "system.posix_acl_access", nil)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, unix.ENODATA) || errors.Is(err, unix.EOPNOTSUPP):
		return false, nil
	default:
		return false, err
	}
}

// routeScan collects the result of the escape route checks. open lists
// each open route with what matches. failed lists the checks that prove
// nothing, such as a failed command.
type routeScan struct {
	open   []string
	failed []error
}

func (s *routeScan) add(route, detail string) {
	s.open = append(s.open, route+": "+detail)
}

func (s *routeScan) fail(route string, err error) {
	s.failed = append(s.failed, fmt.Errorf("%s: %w", route, err))
}

// result gives ErrEscapeRoute when a route is open, so a failed check next
// to an open route does not hide it. Otherwise a failed check gives
// ErrLimitReadback.
func (s *routeScan) result() error {
	if len(s.open) != 0 {
		return fmt.Errorf("%w through %s; close each route, or remove the limits through setup", ErrEscapeRoute, strings.Join(s.open, ", "))
	}
	if len(s.failed) == 0 {
		return nil
	}
	texts := make([]string, 0, len(s.failed))
	for _, err := range s.failed {
		texts = append(texts, err.Error())
	}
	causes := append([]error{ErrLimitReadback}, s.failed...)
	return SafeError(ErrLimitReadback.Error()+": the escape route check failed: "+strings.Join(texts, "; "), causes...)
}

// checkEscapeRoutes checks each route through which a program of the
// service account can run outside the host cap: cron, at, groups and
// daemon sockets, sudo, polkit and system units that run as the account.
// It fails closed: a policy that it cannot read leaves its route open.
// The profile does not matter, because the routes belong to the account.
func (m *SystemdManager) checkEscapeRoutes(ctx context.Context, _ control.WorkloadProfile) error {
	var scan routeScan
	account, ok := m.lookupAccount(&scan)
	if ok {
		m.checkCron(&scan, account)
		m.checkAt(&scan, account)
		m.checkGroups(&scan, account)
		m.checkSudo(ctx, &scan, account)
	}
	m.checkPolkit(ctx, &scan)
	m.checkSystemUnits(ctx, &scan, account)
	return scan.result()
}

// errNotRegular marks a policy file that is not a regular file.
var errNotRegular = errors.New("not a regular file")

// errPolicyTooLarge marks a policy file above policyLimit.
var errPolicyTooLarge = errors.New("larger than 1 MiB")

const policyLimit = 1 << 20

// openPolicy walks name from the host root without symlinks. With trusted,
// each directory and the last component need the trusted owner, no group
// or other write, no ACL and no file capability.
func (m *SystemdManager) openPolicy(name string, lastFlags int, trusted bool) (int, error) {
	verify := m.escape.verify
	if trusted {
		verify = func(fd int) error {
			if err := m.escape.verify(fd); err != nil {
				return err
			}
			return m.host.trustedDescriptor(fd)
		}
	}
	return openNoFollow(m.host.root, strings.Split(strings.TrimPrefix(name, "/"), "/"), unix.O_NONBLOCK|unix.O_NOCTTY|lastFlags, verify)
}

// readPolicy reads a regular policy file of at most policyLimit bytes.
func (m *SystemdManager) readPolicy(name string, trusted bool) ([]byte, error) {
	fd, err := m.openPolicy(name, 0, trusted)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, &walkError{path: name, err: errNotRegular}
	}
	data, err := io.ReadAll(io.LimitReader(file, policyLimit+1))
	if err != nil {
		return nil, err
	}
	if len(data) > policyLimit {
		return nil, &walkError{path: name, err: errPolicyTooLarge}
	}
	return data, nil
}

// policyState is the meaning of a failed policy read.
type policyState int

const (
	// policyAbsent is a missing file or directory.
	policyAbsent policyState = iota
	// policyBlocked is a file that setup cannot read or trust: a denied
	// read, a symlink, a file where a directory must be, an untrusted
	// file, a file that is not regular or a file that is too large. It
	// leaves its route open.
	policyBlocked
	// policyFailed is another failure, which proves nothing.
	policyFailed
)

// classifyPolicy gives the state of a failed policy read and its text,
// which names the path that failed.
func classifyPolicy(name string, err error) (policyState, string) {
	var walk *walkError
	if errors.As(err, &walk) && walk.path != "" {
		name, err = walk.path, walk.err
	}
	detail := name + " (" + err.Error() + ")"
	switch {
	case errors.Is(err, unix.ENOENT):
		return policyAbsent, detail
	case errors.Is(err, unix.EACCES) || errors.Is(err, unix.EPERM) || errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) ||
		errors.Is(err, errUntrusted) || errors.Is(err, errNotRegular) || errors.Is(err, errPolicyTooLarge):
		return policyBlocked, detail
	default:
		return policyFailed, detail
	}
}

// readRoutePolicy reads name for route. It returns the data and true. A
// missing file gives no data and true. A blocked file opens the route and
// a failed read fails the check: both give false.
func (m *SystemdManager) readRoutePolicy(scan *routeScan, route, name string, trusted bool) ([]byte, bool) {
	data, err := m.readPolicy(name, trusted)
	if err == nil {
		return data, true
	}
	return nil, scan.policyFailure(route, name, err)
}

// policyFailure records a failed read of name. It returns true only for a
// missing file, which adds nothing.
func (s *routeScan) policyFailure(route, name string, err error) bool {
	state, detail := classifyPolicy(name, err)
	switch state {
	case policyAbsent:
		return true
	case policyBlocked:
		s.add(route, "setup cannot read or trust "+detail)
	default:
		s.fail(route, errors.New(detail))
	}
	return false
}

// spoolEntry is one entry of a policy directory and its owner.
type spoolEntry struct {
	name  string
	owner uint32
}

// listPolicyDir lists a policy directory, walked without symlinks. A
// missing directory gives no entries and true. A blocked or failed
// directory gives false.
func (m *SystemdManager) listPolicyDir(scan *routeScan, route, dir string) ([]spoolEntry, bool) {
	fd, err := m.openPolicy(dir, unix.O_DIRECTORY, false)
	if err != nil {
		return nil, scan.policyFailure(route, dir, err)
	}
	file := os.NewFile(uintptr(fd), dir)
	defer file.Close()
	names, err := file.Readdirnames(-1)
	if err != nil {
		return nil, scan.policyFailure(route, dir, err)
	}
	entries := make([]spoolEntry, 0, len(names))
	for _, name := range names {
		var stat unix.Stat_t
		err := unix.Fstatat(fd, name, &stat, unix.AT_SYMLINK_NOFOLLOW)
		if errors.Is(err, unix.ENOENT) {
			// The entry went away during the check.
			continue
		}
		if err != nil {
			return nil, scan.policyFailure(route, dir+"/"+name, err)
		}
		entries = append(entries, spoolEntry{name: name, owner: m.host.ownerOf(stat)})
	}
	return entries, true
}

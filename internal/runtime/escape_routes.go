package runtime

import (
	"context"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// EscapeRoute is one advisory finding: a way through which a program of
// the service account can run outside the host cap, or a source that the
// scan cannot check. Escape routes do not block limits. Limits protect
// against runaway workloads, not against hostile code of the same UID.
type EscapeRoute struct {
	Route string `json:"route"`
	Item  string `json:"item"`
}

// maxEscapeRoutes bounds the report. A longer report ends with one
// "more" entry that gives the number of findings left out.
const maxEscapeRoutes = 32

// CommandFunc runs one command and returns its combined output.
type CommandFunc func(ctx context.Context, name string, args ...string) ([]byte, error)

// ScanEscapeRoutes reports the escape routes of the calling account. It is
// read-only and runs only on request, such as setup discovery. The runtime
// and the supervisor never call it. run runs systemctl at systemctl.
func ScanEscapeRoutes(ctx context.Context, run CommandFunc, systemctl string) []EscapeRoute {
	scanner := escapeScanner{host: defaultLimitHost(), probe: defaultEscapeProbe(), run: run, systemctl: systemctl}
	return scanner.scan(ctx)
}

// escapeScanner holds the host and the process facts of the scan. Tests
// replace them to scan a fixture root without root privileges.
type escapeScanner struct {
	host      limitHost
	probe     escapeProbe
	run       CommandFunc
	systemctl string
}

// escapeProbe holds the process facts of the scan.
type escapeProbe struct {
	// verify runs on each descriptor of a walk before any trust check.
	// Tests replace it to make one file unreadable as root.
	verify func(fd int) error
	// processGroups gives the group IDs of this process.
	processGroups func() ([]uint32, error)
	// socketACL reports whether a socket path has an access ACL.
	socketACL func(name string) (bool, error)
}

func defaultEscapeProbe() escapeProbe {
	return escapeProbe{verify: func(int) error { return nil }, processGroups: processGroups, socketACL: pathHasACL}
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

// scan runs each check and returns the bounded report.
func (s escapeScanner) scan(ctx context.Context) []EscapeRoute {
	var report routeReport
	account, named := s.lookupAccount(&report)
	if named {
		s.checkCron(&report, account)
		s.checkAt(&report, account)
	}
	s.checkGroups(&report, account)
	s.checkPolkit(&report)
	s.checkSystemUnits(ctx, &report, account)
	return report.bounded()
}

// routeReport collects the findings of a scan.
type routeReport struct {
	items []EscapeRoute
}

func (r *routeReport) add(route, item string) {
	r.items = append(r.items, EscapeRoute{Route: route, Item: item})
}

// unchecked reports a source that the scan cannot check.
func (r *routeReport) unchecked(route, source string) {
	r.add(route, "cannot check "+source)
}

// bounded returns at most maxEscapeRoutes findings.
func (r *routeReport) bounded() []EscapeRoute {
	if len(r.items) <= maxEscapeRoutes {
		return r.items
	}
	left := len(r.items) - (maxEscapeRoutes - 1)
	return append(r.items[:maxEscapeRoutes-1:maxEscapeRoutes-1], EscapeRoute{Route: "more", Item: strconv.Itoa(left) + " more findings"})
}

// errNotRegular marks a policy file that is not a regular file.
var errNotRegular = errors.New("not a regular file")

// errPolicyTooLarge marks a policy file above policyLimit.
var errPolicyTooLarge = errors.New("larger than 1 MiB")

const policyLimit = 1 << 20

// openPolicy walks name from the host root without symlinks. With trusted,
// each directory and the last component need the trusted owner, no group
// or other write, no ACL and no file capability.
func (s escapeScanner) openPolicy(name string, lastFlags int, trusted bool) (int, error) {
	verify := s.probe.verify
	if trusted {
		verify = func(fd int) error {
			if err := s.probe.verify(fd); err != nil {
				return err
			}
			return s.host.trustedDescriptor(fd)
		}
	}
	return openNoFollow(s.host.root, strings.Split(strings.TrimPrefix(name, "/"), "/"), unix.O_NONBLOCK|unix.O_NOCTTY|lastFlags, verify)
}

// readPolicy reads a regular policy file of at most policyLimit bytes.
func (s escapeScanner) readPolicy(name string, trusted bool) ([]byte, error) {
	fd, err := s.openPolicy(name, 0, trusted)
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

// policyAbsent reports a missing file or directory.
func policyAbsent(err error) bool {
	return errors.Is(err, unix.ENOENT)
}

// policySource names the path of a failed read and its cause.
func policySource(name string, err error) string {
	var walk *walkError
	if errors.As(err, &walk) && walk.path != "" {
		name, err = walk.path, walk.err
	}
	return name + " (" + err.Error() + ")"
}

// readRoutePolicy reads name for route. A missing file gives no data and
// true. Another failure reports that the scan cannot check the file and
// gives false.
func (s escapeScanner) readRoutePolicy(report *routeReport, route, name string, trusted bool) ([]byte, bool) {
	data, err := s.readPolicy(name, trusted)
	if err == nil {
		return data, true
	}
	return nil, report.policyFailure(route, name, err)
}

// policyFailure records a failed read of name. It returns true only for a
// missing file, which adds nothing.
func (r *routeReport) policyFailure(route, name string, err error) bool {
	if policyAbsent(err) {
		return true
	}
	r.unchecked(route, policySource(name, err))
	return false
}

// dirEntry is one entry of a policy directory and its owner.
type dirEntry struct {
	name  string
	owner uint32
}

// listPolicyDir lists a policy directory, walked without symlinks. A
// missing directory gives no entries and true. A directory that the scan
// cannot read gives false.
func (s escapeScanner) listPolicyDir(report *routeReport, route, dir string) ([]dirEntry, bool) {
	fd, err := s.openPolicy(dir, unix.O_DIRECTORY, false)
	if err != nil {
		return nil, report.policyFailure(route, dir, err)
	}
	file := os.NewFile(uintptr(fd), dir)
	defer file.Close()
	names, err := file.Readdirnames(-1)
	if err != nil {
		return nil, report.policyFailure(route, dir, err)
	}
	entries := make([]dirEntry, 0, len(names))
	for _, name := range names {
		var stat unix.Stat_t
		err := unix.Fstatat(fd, name, &stat, unix.AT_SYMLINK_NOFOLLOW)
		if policyAbsent(err) {
			// The entry went away during the scan.
			continue
		}
		if err != nil {
			return nil, report.policyFailure(route, dir+"/"+name, err)
		}
		entries = append(entries, dirEntry{name: name, owner: s.host.ownerOf(stat)})
	}
	return entries, true
}

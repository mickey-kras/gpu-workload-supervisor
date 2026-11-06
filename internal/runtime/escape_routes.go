package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
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

// The route values of an EscapeRoute.
const (
	routeAccount     = "account"
	routeCron        = "cron"
	routeAt          = "at"
	routeGroups      = "groups"
	routeSockets     = "sockets"
	routePolkit      = "polkit"
	routeSystemUnits = "system-units"
	routeMore        = "more"
)

// ScanEscapeRoutes reports the escape routes of the calling account. It is
// read-only and runs only on request, such as setup discovery. The runtime
// and the supervisor never call it. run runs commands. systemctl is the
// path of the systemctl binary. A scan always returns a non-nil list.
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
	return xattrPresent(err)
}

// scan runs each check and returns the bounded report. A spent context
// stops the scan between checks, and the report says so.
func (s escapeScanner) scan(ctx context.Context) []EscapeRoute {
	var report routeReport
	account, named := s.lookupAccount(&report)
	checks := []func(){
		func() { s.checkGroups(&report, account) },
		func() { s.checkPolkit(&report) },
		func() { s.checkSystemUnits(ctx, &report, account) },
	}
	if named {
		checks = append([]func(){func() { s.checkCron(&report, account) }, func() { s.checkAt(&report, account) }}, checks...)
	}
	for _, check := range checks {
		if err := ctx.Err(); err != nil {
			report.unchecked(routeMore, "the other routes ("+err.Error()+")")
			break
		}
		check()
	}
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

// bounded returns at most maxEscapeRoutes findings, never nil. The
// "cannot check" items come first, so the truncation keeps them. The last
// entry names the routes of the findings that it leaves out.
func (r *routeReport) bounded() []EscapeRoute {
	items := make([]EscapeRoute, 0, len(r.items))
	for _, unchecked := range []bool{true, false} {
		for _, item := range r.items {
			if strings.HasPrefix(item.Item, "cannot check ") == unchecked {
				items = append(items, item)
			}
		}
	}
	if len(items) <= maxEscapeRoutes {
		return items
	}
	kept, left := items[:maxEscapeRoutes-1], items[maxEscapeRoutes-1:]
	var routes []string
	for _, item := range left {
		if !slices.Contains(routes, item.Route) {
			routes = append(routes, item.Route)
		}
	}
	more := strconv.Itoa(len(left)) + " more findings (" + strings.Join(routes, ", ") + ")"
	return append(kept, EscapeRoute{Route: routeMore, Item: more})
}

// errNotRegular marks a policy file that is not a regular file.
var errNotRegular = errors.New("not a regular file")

// policyLimit bounds the size of a policy file. errPolicyTooLarge names
// the same limit.
const policyLimit = 1 << 20

// errPolicyTooLarge marks a policy file above policyLimit.
var errPolicyTooLarge = fmt.Errorf("larger than %d MiB", policyLimit>>20)

// maxDirEntries bounds the entries that the scan reads from one directory.
const maxDirEntries = 1024

// errTooManyEntries marks a directory with more than maxDirEntries entries.
var errTooManyEntries = fmt.Errorf("more than %d entries", maxDirEntries)

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
	report.policyFailure(route, name, err)
	return nil, policyAbsent(err)
}

// policyFailure records a failed read of name. A missing file adds nothing.
func (r *routeReport) policyFailure(route, name string, err error) {
	if !policyAbsent(err) {
		r.unchecked(route, policySource(name, err))
	}
}

// dirEntry is one entry of a policy directory and its owner.
type dirEntry struct {
	name  string
	owner uint32
}

// listPolicyDir lists a policy directory, walked without symlinks. A
// missing directory gives no entries. A directory that the scan cannot
// read, or that has more than maxDirEntries entries, gives none and a
// "cannot check" item.
func (s escapeScanner) listPolicyDir(report *routeReport, route, dir string) []dirEntry {
	fd, err := s.openPolicy(dir, unix.O_DIRECTORY, false)
	if err != nil {
		report.policyFailure(route, dir, err)
		return nil
	}
	file := os.NewFile(uintptr(fd), dir)
	defer file.Close()
	names, err := file.Readdirnames(maxDirEntries + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		report.policyFailure(route, dir, err)
		return nil
	}
	if len(names) > maxDirEntries {
		report.policyFailure(route, dir, errTooManyEntries)
		return nil
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
			report.policyFailure(route, dir+"/"+name, err)
			return nil
		}
		entries = append(entries, dirEntry{name: name, owner: s.host.ownerOf(stat)})
	}
	return entries
}

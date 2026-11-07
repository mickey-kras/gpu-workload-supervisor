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
//
// Unchecked marks a source that the scan cannot check: its item starts
// with "cannot check". Count gives the number of left-out findings on the
// "more" entry.
type EscapeRoute struct {
	Route     string `json:"route"`
	Item      string `json:"item"`
	Unchecked bool   `json:"unchecked,omitempty"`
	Count     int    `json:"count,omitempty"`
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
	// routeMore gives the number and the routes of the findings that the
	// bound leaves out.
	routeMore = "more"
	// routeIncomplete says that the context ended the scan early.
	routeIncomplete = "incomplete"
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
	// ctx is the context of the running scan. scan sets it.
	ctx context.Context
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
// stops the scan between checks, between the entries of a directory and
// between file reads. The report says so.
func (s escapeScanner) scan(ctx context.Context) []EscapeRoute {
	s.ctx = ctx
	var report routeReport
	account := s.lookupAccount(&report)
	var checks []func()
	if account.name != "" {
		checks = append(checks, func() { s.checkCron(&report, account) }, func() { s.checkAt(&report, account) })
	}
	checks = append(checks,
		func() { s.checkGroups(&report, account) },
		func() { s.checkPolkit(&report) },
		func() { s.checkSystemUnits(&report, account) })
	for _, check := range checks {
		if s.stopped(&report) {
			break
		}
		check()
	}
	// The context can end during the last check.
	s.stopped(&report)
	return report.bounded()
}

// stopped reports a spent scan context, and records it once.
func (s escapeScanner) stopped(report *routeReport) bool {
	err := s.ctx.Err()
	if err != nil && report.incomplete == nil {
		report.incomplete = &EscapeRoute{Route: routeIncomplete, Item: "cannot check the rest of the routes (" + err.Error() + ")", Unchecked: true}
	}
	return err != nil
}

// routeReport collects the findings of a scan. missed holds the sources
// that the scan cannot check, found holds the other findings. Each list
// holds an entry only once. incomplete is set when the context ended the
// scan early.
type routeReport struct {
	missed, found []EscapeRoute
	incomplete    *EscapeRoute
}

func (r *routeReport) add(route, item string) {
	r.found = appendOnce(r.found, EscapeRoute{Route: route, Item: item})
}

// unchecked reports a source that the scan cannot check.
func (r *routeReport) unchecked(route, source string) {
	r.missed = appendOnce(r.missed, EscapeRoute{Route: route, Item: "cannot check " + source, Unchecked: true})
}

func appendOnce(list []EscapeRoute, item EscapeRoute) []EscapeRoute {
	if slices.Contains(list, item) {
		return list
	}
	return append(list, item)
}

// bounded returns at most maxEscapeRoutes findings, never nil. The
// incomplete entry always stays first. The "cannot check" items come next:
// a finding that the scan missed can hide behind each of them, so the
// truncation keeps them. The last entry names the routes of the findings
// that it leaves out.
func (r *routeReport) bounded() []EscapeRoute {
	items := make([]EscapeRoute, 0, len(r.missed)+len(r.found)+1)
	if r.incomplete != nil {
		items = append(items, *r.incomplete)
	}
	items = append(append(items, r.missed...), r.found...)
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
	return append(kept, EscapeRoute{Route: routeMore, Item: more, Count: len(left)})
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
	name, err = walkFailure(name, err)
	return name + " (" + err.Error() + ")"
}

// walkFailure gives the component of a failed walk and its cause: a walk
// failure names the component that failed, not only the file.
func walkFailure(name string, err error) (string, error) {
	var walk *walkError
	if errors.As(err, &walk) && walk.path != "" {
		return walk.path, walk.err
	}
	return name, err
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
	mode  uint32
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
		if s.stopped(report) {
			return nil
		}
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
		entries = append(entries, dirEntry{name: name, owner: s.host.ownerOf(stat), mode: stat.Mode})
	}
	return entries
}

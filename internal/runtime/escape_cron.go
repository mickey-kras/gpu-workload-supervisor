package runtime

import (
	"fmt"
	"slices"
	"strings"

	"golang.org/x/sys/unix"
)

// cronSpools hold the crontabs of users. cronie names a crontab after its
// user, and Debian cron also gives it the owner of the user.
var cronSpools = []string{"/var/spool/cron", "/var/spool/cron/crontabs"}

// atSpools hold the queued at jobs. Each job file has the owner of its user.
var atSpools = []string{"/var/spool/at", "/var/spool/atjobs", "/var/spool/cron/atjobs"}

// checkCron reports the cron route unless the allow and deny rule denies
// the account, and each spool or system crontab job of the account. It
// needs no cron binary. Without both rule files, Debian cron lets each
// user use cron, so the scan reports the route.
func (s escapeScanner) checkCron(report *routeReport, account escapeAccount) {
	s.checkAccessRule(report, routeCron, "/etc/cron.allow", "/etc/cron.deny", account.name, false)
	for _, spool := range cronSpools {
		s.checkSpool(report, routeCron, spool, account)
	}
	s.checkSystemCrontab(report, "/etc/crontab", account)
	for _, entry := range s.listPolicyDir(report, routeCron, "/etc/cron.d") {
		// readRoutePolicy also checks the context. This check only ends
		// the loop early.
		if s.markStopped(report) {
			return
		}
		if cronIgnores(entry) {
			continue
		}
		s.checkSystemCrontab(report, "/etc/cron.d/"+entry.name, account)
	}
}

// cronIgnores reports an /etc/cron.d entry that cron does not run: a
// directory, a hidden file or a backup file that ends in a tilde.
func cronIgnores(entry dirEntry) bool {
	return entry.mode&unix.S_IFMT == unix.S_IFDIR || strings.HasPrefix(entry.name, ".") || strings.HasSuffix(entry.name, "~")
}

// checkAt reports the at route unless the allow and deny rule denies the
// account, and each queued job of the account. It needs no at binary.
// Without both rule files, at lets only root use it, so the scan reports
// nothing for the rule.
func (s escapeScanner) checkAt(report *routeReport, account escapeAccount) {
	s.checkAccessRule(report, routeAt, "/etc/at.allow", "/etc/at.deny", account.name, true)
	for _, spool := range atSpools {
		s.checkSpool(report, routeAt, spool, account)
	}
}

// checkAccessRule applies the allow and deny rule of cron and at. The rule
// denies the account only with a readable allow file that omits the name,
// or with no allow file and a deny file that lists the name. rootOnly
// tells that the program lets only root in when both files are missing.
// Both files and their parent directories need the trusted owner, no
// group or other write, no ACL and no file capability. A file that fails
// these checks, or that the account cannot read, gives "cannot check".
func (s escapeScanner) checkAccessRule(report *routeReport, route, allow, deny, name string, rootOnly bool) {
	// A missing file gives no data, and an existing file gives non-nil
	// data, also when it is empty.
	data, ok := s.readRoutePolicy(report, route, allow, true)
	if !ok {
		return
	}
	if data != nil {
		if listsLoosely(string(data), name) {
			report.add(route, allow+" allows "+name)
		}
		return
	}
	data, ok = s.readRoutePolicy(report, route, deny, true)
	if !ok || (data == nil && rootOnly) {
		return
	}
	if data == nil {
		report.add(route, "neither "+allow+" nor "+deny+" denies "+name)
		return
	}
	if !listsExactly(string(data), name) {
		report.add(route, deny+" does not deny "+name)
	}
}

// listsExactly reports a line that is exactly name followed by a newline.
// A trailing space, a carriage return or a last line without a newline
// does not count, so such a deny file does not deny the account.
func listsExactly(data, name string) bool {
	return slices.Contains(strings.SplitAfter(data, "\n"), name+"\n")
}

// listsLoosely reports a line that is name after the removal of spaces,
// tabs and carriage returns. An allow file with such a line counts as one
// that allows the account, whatever the cron implementation reads.
func listsLoosely(data, name string) bool {
	for _, line := range strings.Split(data, "\n") {
		if strings.Trim(line, " \t\r") == name {
			return true
		}
	}
	return false
}

// checkSpool reports each spool entry that has the name or the owner of
// the account. A missing spool is empty.
func (s escapeScanner) checkSpool(report *routeReport, route, spool string, account escapeAccount) {
	for _, entry := range s.listPolicyDir(report, route, spool) {
		if entry.name == account.name || entry.owner == uint32(account.uid) {
			report.add(route, spool+"/"+entry.name)
		}
	}
}

// checkSystemCrontab reports each job of a system crontab that runs as the
// account, by name or by number.
func (s escapeScanner) checkSystemCrontab(report *routeReport, name string, account escapeAccount) {
	data, _ := s.readRoutePolicy(report, routeCron, name, false)
	for i, line := range strings.Split(string(data), "\n") {
		if user, ok := crontabUser(line); ok && account.is(user) {
			report.add(routeCron, fmt.Sprintf("%s line %d runs as %s", name, i+1, user))
		}
	}
}

// crontabUser gives the user field of a system crontab job line. A job
// line has five time fields and then the user, or one @ schedule and then
// the user. Comments, blank lines and variable lines have no user.
func crontabUser(line string) (string, bool) {
	fields := strings.Fields(line)
	if len(fields) == 0 || strings.HasPrefix(fields[0], "#") || isCronVariable(line) {
		return "", false
	}
	index := 5
	if strings.HasPrefix(fields[0], "@") {
		index = 1
	}
	if len(fields) <= index {
		return "", false
	}
	return fields[index], true
}

// isCronVariable reports a line of the form NAME=value.
func isCronVariable(line string) bool {
	name, _, ok := strings.Cut(strings.TrimSpace(line), "=")
	return ok && !strings.ContainsAny(strings.TrimSpace(name), " \t*@")
}

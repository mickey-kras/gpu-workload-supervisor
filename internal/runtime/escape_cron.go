package runtime

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// cronSpools hold the crontabs of users. cronie names a crontab after its
// user, and Debian cron also gives it the owner of the user.
var cronSpools = []string{"/var/spool/cron", "/var/spool/cron/crontabs"}

// atSpools hold the queued at jobs. Each job file has the owner of its user.
var atSpools = []string{"/var/spool/at", "/var/spool/atjobs", "/var/spool/cron/atjobs"}

// checkCron opens the cron route unless the allow and deny rule denies the
// account and no spool or system crontab has a job of the account. It
// needs no cron binary.
func (m *SystemdManager) checkCron(scan *routeScan, account escapeAccount) {
	m.checkAccessRule(scan, "cron", "/etc/cron.allow", "/etc/cron.deny", account.name)
	for _, spool := range cronSpools {
		m.checkSpool(scan, "cron", spool, account)
	}
	m.checkSystemCrontab(scan, "/etc/crontab", account)
	entries, ok := m.listPolicyDir(scan, "cron", "/etc/cron.d")
	if !ok {
		return
	}
	for _, entry := range entries {
		m.checkSystemCrontab(scan, "/etc/cron.d/"+entry.name, account)
	}
}

// checkAt opens the at route unless the allow and deny rule denies the
// account and no queued job belongs to it. It needs no at binary.
func (m *SystemdManager) checkAt(scan *routeScan, account escapeAccount) {
	m.checkAccessRule(scan, "at", "/etc/at.allow", "/etc/at.deny", account.name)
	for _, spool := range atSpools {
		m.checkSpool(scan, "at", spool, account)
	}
}

// checkAccessRule applies the allow and deny rule of cron and at. The rule
// denies the account only with a readable allow file that omits the name,
// or with no allow file and a deny file that lists the name. An allow file
// that setup cannot read counts as absent. Both files and their parent
// directories need the trusted owner, no group or other write, no ACL and
// no file capability.
func (m *SystemdManager) checkAccessRule(scan *routeScan, route, allow, deny, name string) {
	data, err := m.readPolicy(allow, true)
	if err == nil {
		if listsLoosely(string(data), name) {
			scan.add(route, allow+" allows "+name)
		}
		return
	}
	if state, _ := classifyPolicy(allow, err); state != policyAbsent && !deniedRead(err) {
		scan.policyFailure(route, allow, err)
		return
	}
	data, ok := m.readRoutePolicy(scan, route, deny, true)
	if !ok {
		return
	}
	if data == nil {
		scan.add(route, "neither "+allow+" nor "+deny+" denies "+name)
		return
	}
	if !listsExactly(string(data), name) {
		scan.add(route, deny+" does not deny "+name)
	}
}

// deniedRead reports a read that the permissions refused.
func deniedRead(err error) bool {
	return errors.Is(err, unix.EACCES) || errors.Is(err, unix.EPERM)
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

// checkSpool names each spool entry that has the name or the owner of the
// account. A missing spool is empty. A spool that setup cannot read opens
// the route.
func (m *SystemdManager) checkSpool(scan *routeScan, route, spool string, account escapeAccount) {
	entries, _ := m.listPolicyDir(scan, route, spool)
	for _, entry := range entries {
		if entry.name == account.name || entry.owner == uint32(account.uid) {
			scan.add(route, spool+"/"+entry.name)
		}
	}
}

// checkSystemCrontab names each job of a system crontab that runs as the
// account, by name or by number.
func (m *SystemdManager) checkSystemCrontab(scan *routeScan, name string, account escapeAccount) {
	data, _ := m.readRoutePolicy(scan, "cron", name, false)
	for i, line := range strings.Split(string(data), "\n") {
		if user, ok := crontabUser(line); ok && (user == account.name || user == strconv.Itoa(account.uid)) {
			scan.add("cron", fmt.Sprintf("%s line %d runs as %s", name, i+1, user))
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

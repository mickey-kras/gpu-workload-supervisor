package runtime

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// The tools of the sudo and polkit checks, at their usual paths.
var (
	sudoPaths    = []string{"/usr/bin/sudo", "/bin/sudo", "/usr/sbin/sudo", "/usr/local/bin/sudo"}
	pkcheckPaths = []string{"/usr/bin/pkcheck", "/bin/pkcheck"}
	polkitdPaths = []string{"/usr/lib/polkit-1/polkitd", "/usr/libexec/polkitd", "/usr/libexec/polkit-1/polkitd"}
)

// polkitActions let a subject start a system unit, or link and enable a
// unit file that a later start runs.
var polkitActions = []string{"org.freedesktop.systemd1.manage-units", "org.freedesktop.systemd1.manage-unit-files"}

// maxRuleLines bounds the sudo rules that an error names.
const maxRuleLines = 5

// findTool returns the first path of paths that exists under the host
// root, or empty when none does.
func (m *SystemdManager) findTool(paths []string) (string, error) {
	for _, name := range paths {
		_, err := m.host.lstat(m.host.path(name))
		if err == nil {
			return name, nil
		}
		if !errors.Is(err, unix.ENOENT) && !errors.Is(err, unix.ENOTDIR) {
			return "", fmt.Errorf("%s: %w", name, err)
		}
	}
	return "", nil
}

// runTool validates a tool under the host root and runs it. A command that
// cannot start, a context failure and an exit status outside known give
// a failure that proves nothing. Otherwise it returns the output and the
// exit status.
func (m *SystemdManager) runTool(ctx context.Context, name string, known []int, args ...string) ([]byte, int, error) {
	resolved, err := validateExecutable(m.host.path(name))
	if err != nil {
		return nil, 0, SafeError(name+" is not a trusted executable", err)
	}
	output, err := m.runner.Run(ctx, resolved, args...)
	if err == nil {
		return output, 0, nil
	}
	var exit interface{ ExitCode() int }
	if ctx.Err() != nil || !errors.As(err, &exit) || !slices.Contains(known, exit.ExitCode()) {
		return nil, 0, SafeError(name+" failed", err)
	}
	return output, exit.ExitCode(), nil
}

// checkSudo opens the sudo route for any rule that `sudo -l -U` lists for
// the account. Output that is neither a rule list nor the denial counts as
// a policy that setup cannot read. Without sudo, there is no sudo route.
func (m *SystemdManager) checkSudo(ctx context.Context, scan *routeScan, account escapeAccount) {
	sudo, err := m.findTool(sudoPaths)
	if err != nil || sudo == "" {
		if err != nil {
			scan.fail("sudo", err)
		}
		return
	}
	output, _, err := m.runTool(ctx, sudo, []int{1}, "-n", "-l", "-U", account.name)
	if err != nil {
		scan.fail("sudo", err)
		return
	}
	for _, detail := range sudoFindings(string(output), account.name) {
		scan.add("sudo", detail)
	}
}

// sudoFindings gives each rule of a sudo -l listing, the rule list header
// first. The one-line denial gives none. Other output gives one finding.
func sudoFindings(output, name string) []string {
	lines := strings.Split(strings.TrimRight(output, "\n"), "\n")
	header := "User " + name + " may run the following commands on "
	for i, line := range lines {
		if !strings.HasPrefix(line, header) {
			continue
		}
		var rules []string
		for _, rule := range lines[i+1:] {
			if rule = strings.TrimSpace(rule); rule != "" {
				rules = append(rules, "rule "+printable(rule))
			}
		}
		if len(rules) == 0 {
			return []string{"sudo -l lists rules for " + name}
		}
		if len(rules) > maxRuleLines {
			rules = append(rules[:maxRuleLines], strconv.Itoa(len(rules)-maxRuleLines)+" more rules")
		}
		return rules
	}
	if len(lines) == 1 && sudoDenial(lines[0], name) {
		return nil
	}
	return []string{"setup cannot read the sudo policy of " + name}
}

// sudoDenial reports the one line that sudo prints for an account without
// rules, for `sudo -l -U` by root and by the account itself.
func sudoDenial(line, name string) bool {
	for _, form := range []struct{ prefix, suffix string }{
		{"User " + name + " is not allowed to run sudo on ", "."},
		{"Sorry, user " + name + " may not run sudo on ", "."},
	} {
		host, ok := strings.CutPrefix(line, form.prefix)
		host, ok2 := strings.CutSuffix(host, form.suffix)
		if ok && ok2 && host != "" && !strings.ContainsAny(host, " \t") {
			return true
		}
	}
	return false
}

// printable keeps the printable ASCII of a command output line, bounded.
func printable(line string) string {
	line = strings.Map(func(r rune) rune {
		if r < ' ' || r > '~' {
			return -1
		}
		return r
	}, line)
	if len(line) > 120 {
		line = line[:120] + "..."
	}
	return line
}

// checkPolkit opens the polkit route when pkcheck authorizes this process
// for an action that starts a system unit. Exit status 1, 2 and 3 deny it
// or need an authentication that a program cannot give. Without pkcheck,
// an installed polkit daemon is a policy that setup cannot read.
func (m *SystemdManager) checkPolkit(ctx context.Context, scan *routeScan) {
	pkcheck, err := m.findTool(pkcheckPaths)
	if err != nil {
		scan.fail("polkit", err)
		return
	}
	if pkcheck == "" {
		m.checkPolkitDaemon(scan)
		return
	}
	for _, action := range polkitActions {
		_, status, err := m.runTool(ctx, pkcheck, []int{1, 2, 3}, "--action-id", action, "--process", strconv.Itoa(m.escape.pid))
		switch {
		case err != nil:
			scan.fail("polkit", err)
		case status == 0:
			scan.add("polkit", "a rule lets this account use "+action)
		}
	}
}

// checkPolkitDaemon opens the polkit route when a polkit daemon exists but
// pkcheck does not.
func (m *SystemdManager) checkPolkitDaemon(scan *routeScan) {
	polkitd, err := m.findTool(polkitdPaths)
	switch {
	case err != nil:
		scan.fail("polkit", err)
	case polkitd != "":
		scan.add("polkit", polkitd+" exists, but setup cannot read its policy without pkcheck")
	}
}

// checkSystemUnits names each loaded system unit with User= set to the
// account, by name or by number. The user manager of the account runs
// inside the slice, so it does not count.
func (m *SystemdManager) checkSystemUnits(ctx context.Context, scan *routeScan, account escapeAccount) {
	output, err := m.runner.Run(ctx, m.config.SystemctlPath, "show", "--property=Id", "--property=User", "--", "*")
	if err != nil {
		scan.fail("system units", SafeError("systemctl show failed", err))
		return
	}
	uid := strconv.Itoa(account.uid)
	for _, block := range strings.Split(string(output), "\n\n") {
		var id, user string
		for _, line := range strings.Split(block, "\n") {
			if value, ok := strings.CutPrefix(line, "Id="); ok {
				id = value
			} else if value, ok := strings.CutPrefix(line, "User="); ok {
				user = value
			}
		}
		if user != "" && (user == account.name || user == uid) && id != m.host.managerUnit() {
			scan.add("system units", id+" runs as "+user)
		}
	}
}

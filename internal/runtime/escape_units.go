package runtime

import (
	"strings"
	"unicode/utf8"
)

// commandOutputLimit is the output bound of the setup command runner. A
// longer output is cut there, and the command fails.
const commandOutputLimit = 1 << 20

// checkSystemUnits reports each loaded system unit with User= set to the
// account, by name or by number. The pattern "*" matches every loaded
// unit, active or not, such as a timer-activated service that does not
// run now. The user manager of the account runs inside the slice, so it
// does not count. systemctl prints the property names and values without
// translation, so the locale does not matter.
//
// A failed command still gives the findings of its output. The report then
// says that it cannot check the units, with the reason.
func (s escapeScanner) checkSystemUnits(report *routeReport, account escapeAccount) {
	output, err := s.run(s.ctx, s.systemctl, "show", "--property=Id", "--property=User", "--", "*")
	text := string(output)
	if err != nil && len(output) >= commandOutputLimit {
		// The last line can be cut, so it proves nothing.
		text = text[:strings.LastIndex(text, "\n")+1]
	}
	for _, block := range strings.Split(text, "\n\n") {
		var id, user string
		for _, line := range strings.Split(block, "\n") {
			if value, ok := strings.CutPrefix(line, "Id="); ok {
				id = value
			} else if value, ok := strings.CutPrefix(line, "User="); ok {
				user = value
			}
		}
		if account.is(user) && id != s.host.managerUnit() {
			report.add(routeSystemUnits, id+" runs as "+user)
		}
	}
	if err == nil {
		return
	}
	if s.stopped(report) {
		// The context ended the command: the incomplete entry says so.
		return
	}
	report.unchecked(routeSystemUnits, "the loaded system units (systemctl show: "+commandFailure(err, output)+")")
}

// commandFailure gives the reason of a failed command: the output limit,
// or the first line of the error and the last output line that is not a
// property.
func commandFailure(err error, output []byte) string {
	if len(output) >= commandOutputLimit {
		return "output exceeds 1 MiB"
	}
	reason := firstLine(err.Error())
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line != "" && !strings.HasPrefix(line, "Id=") && !strings.HasPrefix(line, "User=") {
			return reason + ": " + firstLine(line)
		}
	}
	return reason
}

// firstLine keeps the first line of a command error or output, at most
// 160 bytes, cut at a character boundary.
func firstLine(text string) string {
	text, _, _ = strings.Cut(text, "\n")
	if len(text) <= 160 {
		return text
	}
	cut := 160
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + "..."
}

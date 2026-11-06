package runtime

import (
	"strings"
	"unicode/utf8"
)

// polkitRuleDirs hold the polkit JavaScript rules. The scan reads them
// as text. It does not run them, and it does not ask polkit. A missing
// directory has no rules.
var polkitRuleDirs = []string{"/etc/polkit-1/rules.d", "/run/polkit-1/rules.d", "/usr/local/share/polkit-1/rules.d", "/usr/share/polkit-1/rules.d"}

// polkitActionPrefixes name the actions that start units outside the
// slice, open a host shell or run a program as root.
var polkitActionPrefixes = []string{"org.freedesktop.systemd1.manage-unit", "org.freedesktop.machine1", "org.freedesktop.policykit.exec"}

// checkPolkit reports each rules file that mentions such an action. A rule
// can allow or deny it, so the report says only that the file mentions it.
// The check is best effort: it does not read .policy defaults or rules
// that match actions in another way.
func (s escapeScanner) checkPolkit(report *routeReport) {
	for _, dir := range polkitRuleDirs {
		for _, entry := range s.listPolicyDir(report, routePolkit, dir) {
			if !strings.HasSuffix(entry.name, ".rules") {
				continue
			}
			name := dir + "/" + entry.name
			data, _ := s.readRoutePolicy(report, routePolkit, name, false)
			for _, action := range polkitActionPrefixes {
				if strings.Contains(string(data), action) {
					report.add(routePolkit, name+" mentions "+action)
				}
			}
		}
	}
}

// checkSystemUnits reports each loaded system unit with User= set to the
// account, by name or by number. --all includes the inactive units, such
// as timer and socket activated services and finished oneshot services.
// The user manager of the account runs inside the slice, so it does not
// count. systemctl prints the property names and values without
// translation, so the locale does not matter.
func (s escapeScanner) checkSystemUnits(report *routeReport, account escapeAccount) {
	output, err := s.run(s.ctx, s.systemctl, "show", "--all", "--property=Id", "--property=User", "--", "*")
	if err != nil {
		reason := firstLine(err.Error())
		if line := firstLine(strings.TrimSpace(string(output))); line != "" {
			reason += ": " + line
		}
		report.unchecked(routeSystemUnits, "the loaded system units (systemctl show: "+reason+")")
		return
	}
	for _, block := range strings.Split(string(output), "\n\n") {
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

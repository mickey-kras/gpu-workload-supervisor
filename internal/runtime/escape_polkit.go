package runtime

import "strings"

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
			if s.markStopped(report) {
				return
			}
			s.checkPolkitFile(report, dir+"/"+entry.name)
		}
	}
}

// checkPolkitFile reports each action that one rules file mentions.
func (s escapeScanner) checkPolkitFile(report *routeReport, name string) {
	data, _ := s.readRoutePolicy(report, routePolkit, name, false)
	for _, action := range polkitActionPrefixes {
		if strings.Contains(string(data), action) {
			report.add(routePolkit, name+" mentions "+action)
		}
	}
}

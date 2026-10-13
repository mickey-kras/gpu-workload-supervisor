package runtime

import (
	"fmt"
	"strings"
)

// qualifiedLoadedProperty describes the canonical systemctl show evidence for
// explicitly configured directives that affect paths, dependencies or isolation.
func qualifiedLoadedProperty(key, value string) (string, bool) {
	switch key {
	case "WorkingDirectory", "ProtectHome", "ProtectSystem":
		return value, true
	case "KillSignal":
		return "2", true
	case "NoNewPrivileges", "PrivateTmp":
		if value == "true" {
			return "yes", true
		}
		return "no", true
	case "Wants":
		if len(strings.Fields(value)) == 0 {
			return "", true
		}
		return "network-online.target", true
	}
	return "", false
}

// CheckLoadedLaunchProperties requires effective loaded metadata to agree with
// inspected source. Values and host paths are deliberately excluded from errors.
func CheckLoadedLaunchProperties(observed, expected map[string]string) error {
	for _, key := range []string{"WorkingDirectory", "Wants", "KillSignal", "NoNewPrivileges", "PrivateTmp", "ProtectHome", "ProtectSystem"} {
		want, required := expected[key]
		if !required {
			continue
		}
		got, present := observed[key]
		if key == "Wants" {
			got = strings.Join(strings.Fields(got), " ")
		}
		if !present || got != want {
			return fmt.Errorf("%w: loaded %s differs from inspected source; reload and refresh the binding", ErrLaunchChanged, key)
		}
	}
	return nil
}

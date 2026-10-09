package fixtures

import "strings"

// ok: go.setup-shared-literals
const execStartDirective = "ExecStart="

// ok: go.setup-shared-literals
const typedDirective string = "ExecStart="

const (
	// ok: go.setup-shared-literals
	inspectionFailed = "inspection-failed"
)

func status(line string) string {
	// ruleid: go.setup-shared-literals
	if strings.HasPrefix(line, "ExecStart=") {
		// ruleid: go.setup-shared-literals
		return "ExecStart=" + line
	}
	// ok: go.setup-shared-literals
	if strings.HasPrefix(line, execStartDirective) {
		// ok: go.setup-shared-literals
		return inspectionFailed
	}
	// ruleid: go.setup-shared-literals
	return "inspection-failed"
}

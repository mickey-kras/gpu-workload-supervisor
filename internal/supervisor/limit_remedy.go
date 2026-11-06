package supervisor

import (
	"fmt"
	"strings"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

// remedyState is the state that the next steps of a limit rejection start
// from. latched means the state needs recovery first: setup refuses it.
// kept is the workload that keeps running. user means the user owns the
// state, so the supervisor commands do not apply. keptUnit is the unit of
// kept when kept has limits: its failed checks refuse recover-user for it
// too.
type remedyState struct {
	latched  bool
	kept     control.Workload
	user     bool
	keptUnit string
}

// limitRemedy adds the recovery of a latched state to a rejection of a unit
// with limits, as gpu-mode commands. Each runtime cause carries its own
// remedy, such as the open escape routes to close or the limits to remove
// through setup, so recovery comes first. A rejection of several units has
// one line for each unit, so the steps then go on their own line and apply
// to each unit.
func limitRemedy(rejected error, from remedyState) error {
	if rejected == nil {
		return nil
	}
	steps := from.recoverySteps()
	if len(steps) == 0 {
		return rejected
	}
	separator := "; next steps: "
	if strings.Contains(rejected.Error(), "\n") {
		separator = "\nnext steps: "
	}
	text := steps[len(steps)-1]
	if len(steps) > 1 {
		text = strings.Join(steps[:len(steps)-1], ", ") + ", then " + text
	}
	return fmt.Errorf("%w%sfirst, %s", rejected, separator, text)
}

// unknownUserWorkload reports a latched user-owned state whose running
// workload is unknown: its unit must stop outside gpu-mode first.
func (s remedyState) unknownUserWorkload() bool {
	return s.latched && s.user && s.kept == control.WorkloadUnknown
}

// recoverySteps gives the recovery of a latched state for its owner.
// recover-user needs the workload that runs.
func (s remedyState) recoverySteps() []string {
	switch {
	case !s.latched:
		return nil
	case !s.user:
		return []string{"run `gpu-mode recover`"}
	case s.keptUnit != "":
		return []string{fmt.Sprintf("run `systemctl --user stop %s`", s.keptUnit), "run `gpu-mode -target idle recover-user`"}
	case s.unknownUserWorkload():
		return []string{"stop the running workload unit outside gpu-mode", "run `gpu-mode -target idle recover-user`"}
	default:
		return []string{fmt.Sprintf("run `gpu-mode -target %s recover-user`", s.kept)}
	}
}

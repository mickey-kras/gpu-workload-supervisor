package supervisor

import (
	"errors"
	"fmt"
	"strings"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
)

// remedyState is the state that the next steps of a limit rejection start
// from. latched means the state needs recovery first: setup refuses it.
// kept is the workload that keeps running. user means the user owns the
// state, so the supervisor commands do not apply. keptUnit is the unit of
// kept when kept has limits: the escape route gate refuses recover-user for
// it too.
type remedyState struct {
	latched  bool
	kept     control.Workload
	user     bool
	keptUnit string
}

// limitRemedy adds the next steps to a rejection of a unit with limits, as
// gpu-mode commands. Setup removes limits only from a healthy, stable Idle
// with closed admission, whoever owns it. A user-owned workload other than
// Idle goes to Idle with user-switch, which keeps the user ownership:
// switch needs supervisor ownership, and setup does not need it.
//
// The escape route gate fails every unit with limits, so its only fix is
// to remove the limits. Another cause can carry its own remedy, so recovery
// then comes first. A rejection of several units has one line for each
// unit, so the steps then go on their own line and apply to each unit.
func limitRemedy(rejected error, from remedyState) error {
	if rejected == nil {
		return nil
	}
	steps := from.recoverySteps()
	gate := errors.Is(rejected, gpuruntime.ErrEscapeRoutesUnchecked)
	if gate {
		if kept := from.keptAfterRecovery(); kept != control.WorkloadIdle {
			step := "run `gpu-mode -target idle switch`"
			if from.user {
				step = "run `gpu-mode -target idle user-switch`"
			}
			if kept == control.WorkloadUnknown || from.latched {
				// Recovery often ends in Idle: it keeps only a retained
				// unit. So after recovery, or with an unknown workload,
				// the step is conditional.
				step = "if a workload other than Idle runs then, " + step
			}
			steps = append(steps, step)
		}
		steps = append(steps, "remove the limits through setup")
	}
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
	if !gate {
		text = "first, " + text
	}
	return fmt.Errorf("%w%s%s", rejected, separator, text)
}

// unknownUserWorkload reports a latched user-owned state whose running
// workload is unknown: its unit must stop outside gpu-mode first.
func (s remedyState) unknownUserWorkload() bool {
	return s.latched && s.user && s.kept == control.WorkloadUnknown
}

// keptAfterRecovery is the workload that runs after the recovery steps. The
// gate refuses a kept unit with limits in recovery too, so recovery stops it
// and ends in Idle, whoever owns the state.
func (s remedyState) keptAfterRecovery() control.Workload {
	if (s.latched && s.keptUnit != "") || s.unknownUserWorkload() {
		return control.WorkloadIdle
	}
	return s.kept
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

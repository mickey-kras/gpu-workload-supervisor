package supervisor

import (
	"fmt"
	"strings"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

// remedyState is the state that the next steps of a limit rejection start
// from. latched means the state needs recovery first: setup refuses it.
// kept is the workload that keeps running. user means the user owns the
// state, so the supervisor commands do not apply.
type remedyState struct {
	latched bool
	kept    control.Workload
	user    bool
}

// limitRemedy adds the next steps to a rejection of a unit with limits, as
// gpu-mode commands. Most runtime causes name their own fix, often with
// "or remove the limits through setup". So recovery of a latched state
// comes first. Setup removes limits only from a healthy, stable Idle with
// closed admission, so the steps then say how to reach Idle. A user-owned
// workload goes to Idle with user-switch, which keeps the user ownership:
// switch needs supervisor ownership, and setup does not need it. A
// rejection of several units has one line for each unit, so the steps then
// go on their own line and apply to each unit.
func limitRemedy(rejected error, from remedyState) error {
	if rejected == nil {
		return nil
	}
	var parts []string
	if steps := from.recoverySteps(); len(steps) != 0 {
		text := steps[len(steps)-1]
		if len(steps) > 1 {
			text = strings.Join(steps[:len(steps)-1], ", ") + ", then " + text
		}
		parts = append(parts, "first, "+text)
	}
	if step := from.idleStep(); step != "" {
		parts = append(parts, "to remove the limits through setup, switch to Idle first: "+step)
	}
	if len(parts) == 0 {
		return rejected
	}
	separator := "; next steps: "
	if strings.Contains(rejected.Error(), "\n") {
		separator = "\nnext steps: "
	}
	return fmt.Errorf("%w%s%s", rejected, separator, strings.Join(parts, "; "))
}

// idleStep gives the switch to Idle after the recovery steps, or empty
// when the state is Idle then. Recovery often ends in Idle: it keeps only
// a retained unit. So after recovery, or with an unknown workload, the
// step is conditional.
func (s remedyState) idleStep() string {
	kept := s.keptAfterRecovery()
	if kept == control.WorkloadIdle {
		return ""
	}
	step := "run `gpu-mode -target idle switch`"
	if s.user {
		step = "run `gpu-mode -target idle user-switch`"
	}
	if kept == control.WorkloadUnknown || s.latched {
		step = "if a workload other than Idle runs then, " + step
	}
	return step
}

// unknownUserWorkload reports a latched user-owned state whose running
// workload is unknown: its unit must stop outside gpu-mode first.
func (s remedyState) unknownUserWorkload() bool {
	return s.latched && s.user && s.kept == control.WorkloadUnknown
}

// keptAfterRecovery is the workload that runs after the recovery steps.
// The recovery of a user-owned state with an unknown workload ends in Idle.
func (s remedyState) keptAfterRecovery() control.Workload {
	if s.unknownUserWorkload() {
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
	case s.unknownUserWorkload():
		return []string{"stop the running workload unit outside gpu-mode", "run `gpu-mode -target idle recover-user`"}
	default:
		return []string{fmt.Sprintf("run `gpu-mode -target %s recover-user`", s.kept)}
	}
}

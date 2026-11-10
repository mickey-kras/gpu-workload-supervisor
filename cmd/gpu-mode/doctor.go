package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/deployment"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/lock"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/operator"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/supervisor"
)

const doctorCommand = "doctor"

// errDoctorFailed is the error of a doctor run with a failed check. The
// report on stdout names the check and the next step.
var errDoctorFailed = errors.New("doctor found a problem; follow the next step above")

// doctorReport prints one line for each check and the next step for a
// failure or for a state that needs an operator action.
type doctorReport struct {
	out io.Writer
}

func (r doctorReport) pass(check, detail string) {
	fmt.Fprintf(r.out, "ok    %s: %s\n", check, detail)
}

func (r doctorReport) next(step string) {
	fmt.Fprintf(r.out, "next  %s\n", step)
}

func (r doctorReport) fail(check string, cause error, step string) error {
	fmt.Fprintf(r.out, "FAIL  %s: %v\n", check, cause)
	r.next(step)
	return errDoctorFailed
}

// runDoctor runs the existing checks in order: the operator profile, the
// deployment marker against the activated release, the accepted catalog,
// verify-host on the accepted catalog, then status. It stops at the first
// failed check, because each later check needs the earlier ones. It changes
// state only as status does: status can latch an observation failure.
func runDoctor(newRuntime func(gpuruntime.SystemdConfig) (gpuruntime.Manager, error), runtimeConfig gpuruntime.SystemdConfig, options modeExecution) error {
	report := doctorReport{out: os.Stdout}
	release, err := doctorProfile(report, options.statePath, loadOperatorProfile)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	processLock, err := lock.AcquireContext(ctx, options.statePath+".lock", false)
	if err != nil {
		return err
	}
	defer processLock.Close()
	if err := options.profile.check(options.statePath, loadOperatorProfile); err != nil {
		return report.fail("operator profile", err, "run gpu-mode doctor again")
	}
	if err := deployment.Check(options.statePath, release); err != nil {
		return report.fail("deployment marker", err,
			"open GPU Workload Setup and finish or repeat the activation; see docs/OPERATIONS.md#upgrade-and-interrupted-activation")
	}
	report.pass("deployment marker", "the installed release can use this state")
	runtimeConfig.LimitReserve, runtimeConfig.LimitReserveErr = limitReserveFor(options.statePath, loadOperatorProfile)
	stateStore, err := store.Open(ctx, options.statePath)
	if err != nil {
		return report.fail("state", fmt.Errorf("open state store: %w", err), "check the -state path and its owner and mode")
	}
	defer stateStore.Close()
	controller, err := doctorController(ctx, report, stateStore, newRuntime, runtimeConfig, options)
	if err != nil {
		return err
	}
	state, err := controller.Status(ctx)
	return doctorStatus(report, state, err)
}

// doctorProfile reports the operator profile and gives its activated release
// when the profile applies to statePath.
func doctorProfile(report doctorReport, statePath string, load func() (operator.Profile, error)) (string, error) {
	profile, err := load()
	if errors.Is(err, os.ErrNotExist) {
		report.pass("operator profile", "none; the runtime flags come from the command line")
		return "", nil
	}
	if err != nil {
		return "", report.fail("operator profile", err, profileHint)
	}
	same, err := sameStatePath(statePath, profile.StatePath)
	if err != nil {
		return "", report.fail("operator profile", err, "give -state as an absolute path")
	}
	if !same {
		report.pass("operator profile", "it is for another state path; the checks use -state and the command-line flags")
		return "", nil
	}
	report.pass("operator profile", "release "+profile.ActivatedRelease+" is activated for this state")
	return profile.ActivatedRelease, nil
}

// doctorController checks the accepted catalog and runs verify-host on it,
// then builds the controller for status.
func doctorController(ctx context.Context, report doctorReport, stateStore *store.Store, newRuntime func(gpuruntime.SystemdConfig) (gpuruntime.Manager, error), runtimeConfig gpuruntime.SystemdConfig, options modeExecution) (*supervisor.Controller, error) {
	snapshot, err := stateStore.Catalog(ctx)
	if err == nil {
		_, err = pinCatalog(snapshot)
	}
	if err != nil {
		return nil, report.fail("accepted catalog", err, "open GPU Workload Setup and finish setup, or run gpu-mode -catalog FILE configure")
	}
	report.pass("accepted catalog", "revision "+snapshot.Revision)
	runtimeConfig.Catalog = &snapshot.Catalog
	if err := verifyHost(runtimeConfig, options.actionTimeout, newRuntime); err != nil {
		return nil, report.fail(verifyHostCommand, err,
			"correct the reported host problem, then run doctor again; see docs/DEPLOYMENT.md#qualify-the-host")
	}
	report.pass(verifyHostCommand, "the host supports the accepted catalog")
	runtimeManager, err := newRuntime(runtimeConfig)
	if err != nil {
		return nil, report.fail("runtime", err, "correct the runtime flags or run setup again")
	}
	controller, err := supervisor.New(stateStore, runtimeManager, supervisorConfig(&snapshot, options))
	if err != nil {
		return nil, report.fail("controller", err, "correct the timeout flags")
	}
	return controller, nil
}

// doctorStatus reports status and the next operator step for its state.
func doctorStatus(report doctorReport, state control.State, err error) error {
	if err == nil && state.Health == control.HealthHealthy && state.Phase == control.PhaseStable {
		report.pass("status", stateSummary(state))
		if step := stableNextStep(state); step != "" {
			report.next(step)
		}
		return nil
	}
	if err == nil {
		err = errors.New(stateSummary(state))
	}
	return report.fail("status", err, unstableNextStep(state))
}

func stateSummary(state control.State) string {
	return fmt.Sprintf("owner %s, active %s, phase %s, health %s, admission %s",
		state.Owner, state.ActiveWorkload, state.Phase, state.Health, state.Admission)
}

func stableNextStep(state control.State) string {
	if state.Owner == control.OwnerUser {
		return "the User owns the GPU; to give control back, run gpu-mode -target WORKLOAD return-control"
	}
	if state.ActiveWorkload == control.WorkloadIdle {
		return "the GPU is Idle; to select a workload, run gpu-mode -target WORKLOAD switch"
	}
	return ""
}

func unstableNextStep(state control.State) string {
	if state.Owner == control.OwnerUser {
		return "inspect the runtime, then run gpu-mode -target WORKLOAD recover-user; see docs/OPERATIONS.md#recover-after-a-failed-transfer-or-switch"
	}
	if state.Owner == control.OwnerSupervisor && state.Health != control.HealthError && state.Phase != control.PhaseStable {
		return "a transition did not finish; run gpu-mode reconcile, and if it fails, inspect the runtime before gpu-mode recover"
	}
	return "inspect systemctl --user status UNIT and journalctl --user -u UNIT, correct the cause, then run gpu-mode recover; see docs/OPERATIONS.md#boot-and-explicit-recovery"
}

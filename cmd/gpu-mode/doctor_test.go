package main

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/operator"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/supervisor"
)

type failingPreflightRuntime struct{ stoppedRecoveryRuntime }

func (r *failingPreflightRuntime) Preflight(context.Context) error {
	return errors.New("manager anchor is missing")
}

func setDoctorState(t *testing.T, statePath string, owner control.Owner) {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	state, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	state.Owner, state.ActiveWorkload, state.DesiredWorkload = owner, control.WorkloadIdle, control.WorkloadIdle
	state.Phase, state.Health, state.Admission = control.PhaseStable, control.HealthHealthy, control.AdmissionClosed
	if _, err := s.UpdateState(ctx, state.Version, state); err != nil {
		t.Fatal(err)
	}
}

func runDoctorCommand(t *testing.T, load func() (operator.Profile, error), manager supervisor.Runtime, args ...string) (string, error) {
	t.Helper()
	oldArgs, oldLoad, oldStdout := os.Args, loadOperatorProfile, os.Stdout
	t.Cleanup(func() { os.Args, loadOperatorProfile, os.Stdout = oldArgs, oldLoad, oldStdout })
	output, err := os.CreateTemp(t.TempDir(), "output")
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	os.Stdout, loadOperatorProfile = output, load
	os.Args = append(append([]string{"gpu-mode"}, args...), doctorCommand)
	runErr := runWithRuntimeFactory(func(gpuruntime.SystemdConfig) (supervisor.Runtime, error) { return manager, nil })
	raw, err := os.ReadFile(output.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(raw), runErr
}

func TestDoctorReportsAHealthyIdleDeployment(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.db")
	seedCatalog(t, statePath)
	setDoctorState(t, statePath, control.OwnerSupervisor)
	report, err := runDoctorCommand(t, profileLoader(operator.Profile{}, fs.ErrNotExist), &stoppedRecoveryRuntime{},
		"-state", statePath, "-systemctl", "/usr/bin/systemctl")
	if err != nil {
		t.Fatalf("%s%v", report, err)
	}
	for _, want := range []string{"ok    operator profile: none", "ok    deployment marker", "ok    accepted catalog", "ok    verify-host", "ok    status: owner supervisor, active idle", "next  the GPU is Idle"} {
		if !strings.Contains(report, want) {
			t.Fatalf("report lacks %q:\n%s", want, report)
		}
	}
}

func TestDoctorStopsAtTheFirstFailedCheck(t *testing.T) {
	seeded := filepath.Join(t.TempDir(), "state.db")
	seedCatalog(t, seeded)
	empty := filepath.Join(t.TempDir(), "state.db")
	noProfile := profileLoader(operator.Profile{}, fs.ErrNotExist)
	for _, tc := range []struct {
		name      string
		statePath string
		load      func() (operator.Profile, error)
		manager   supervisor.Runtime
		want      []string
	}{
		{"unreadable profile", seeded, profileLoader(operator.Profile{}, errors.New("untrusted profile")), &stoppedRecoveryRuntime{}, []string{"FAIL  operator profile: untrusted profile", "next  the operator profile"}},
		{"release not activated", seeded, profileLoader(activatedProfile(seeded), nil), &stoppedRecoveryRuntime{}, []string{"ok    operator profile: release 1.0.0", "FAIL  deployment marker", "GPU Workload Setup"}},
		{"no accepted catalog", empty, noProfile, &stoppedRecoveryRuntime{}, []string{"FAIL  accepted catalog", "configure"}},
		{"host check", seeded, noProfile, &failingPreflightRuntime{}, []string{"FAIL  verify-host: manager anchor is missing", "DEPLOYMENT.md#qualify-the-host"}},
		{"unfinished transition", seeded, noProfile, &stoppedRecoveryRuntime{}, []string{"FAIL  status: owner supervisor", "next  a transition did not finish"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := []string{"-state", tc.statePath, "-systemctl", "/usr/bin/systemctl"}
			if tc.name == "release not activated" {
				args = []string{"-systemctl", "/usr/bin/systemctl"}
			}
			report, err := runDoctorCommand(t, tc.load, tc.manager, args...)
			if !errors.Is(err, errDoctorFailed) {
				t.Fatalf("doctor error = %v\n%s", err, report)
			}
			for _, want := range tc.want {
				if !strings.Contains(report, want) {
					t.Fatalf("report lacks %q:\n%s", want, report)
				}
			}
		})
	}
}

func TestDoctorProfileForAnotherState(t *testing.T) {
	var out bytes.Buffer
	release, err := doctorProfile(doctorReport{out: &out}, "/state/state.db", profileLoader(activatedProfile("/other/state.db"), nil))
	if err != nil || release != "" || !strings.Contains(out.String(), "another state path") {
		t.Fatalf("release %q, %v: %s", release, err, out.String())
	}
}

func TestDoctorNextSteps(t *testing.T) {
	user := control.State{Owner: control.OwnerUser, ActiveWorkload: control.WorkloadIdle, Phase: control.PhaseStable, Health: control.HealthHealthy}
	if step := stableNextStep(user); !strings.Contains(step, "return-control") {
		t.Fatalf("stable User = %q", step)
	}
	text := control.State{Owner: control.OwnerSupervisor, ActiveWorkload: control.WorkloadText, Phase: control.PhaseStable, Health: control.HealthHealthy}
	if step := stableNextStep(text); step != "" {
		t.Fatalf("serving text = %q", step)
	}
	if step := unstableNextStep(control.State{Owner: control.OwnerUser, Health: control.HealthError}); !strings.Contains(step, "recover-user") {
		t.Fatalf("failed User = %q", step)
	}
	if step := unstableNextStep(control.State{Owner: control.OwnerSupervisor, Phase: control.PhaseStable, Health: control.HealthError}); !strings.Contains(step, "gpu-mode recover;") {
		t.Fatalf("latched error = %q", step)
	}
	var out bytes.Buffer
	err := doctorStatus(doctorReport{out: &out}, control.State{}, errors.New("observe runtime: probe failed"))
	if !errors.Is(err, errDoctorFailed) || !strings.Contains(out.String(), "FAIL  status: observe runtime: probe failed") {
		t.Fatalf("status error = %v: %s", err, out.String())
	}
}

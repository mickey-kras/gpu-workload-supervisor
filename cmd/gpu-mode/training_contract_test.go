package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/supervisor"
)

type cliTrainingRuntime struct {
	cliCatalogRuntime
	startErr error
	starts   int
	terminal control.TrainingTerminal
}

func (r *cliTrainingRuntime) StartTraining(_ context.Context, _ control.WorkloadProfile, job control.TrainingJob) (string, error) {
	r.starts++
	r.terminal = control.TrainingTerminal{Version: 1, ID: job.ID, Token: job.Token, Fence: job.Fence, RecipeSHA256: job.RecipeSHA256, InvocationID: strings.Repeat("a", 32), Outcome: "completed"}
	return r.terminal.InvocationID, r.startErr
}
func (r *cliTrainingRuntime) TrainingTerminal(context.Context, control.WorkloadProfile, control.TrainingJob) (control.TrainingTerminal, error) {
	return r.terminal, nil
}
func (r *cliTrainingRuntime) ReleaseTraining(context.Context, control.WorkloadProfile) error {
	return nil
}
func (r *cliTrainingRuntime) TrainingMemoryPeak(context.Context, control.WorkloadProfile, control.TrainingJob) (uint64, error) {
	return 1 << 30, nil
}

func cliTrainingFixture(t *testing.T) (*supervisor.Controller, *store.Store, *cliTrainingRuntime) {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	catalog := control.Catalog{Version: 4, Profiles: []control.WorkloadProfile{{ID: "trainer", Label: "Trainer", Adapter: "systemd", Unit: "trainer.service", Cgroup: "/training/trainer.service", RequiredMiB: 1024, BootPolicy: "stop-to-idle", Trainer: &control.TrainerBinding{Version: 1, GPUUUID: "GPU-11111111-1111-1111-1111-111111111111", LaunchFile: "/opt/trainer/unit.service", LaunchSHA256: strings.Repeat("a", 64), Executable: "/opt/trainer/train", ExecutableSHA256: strings.Repeat("b", 64), RecipeFile: "/opt/trainer/recipe.json", RecipeSHA256: strings.Repeat("c", 64), JobFile: "/opt/trainer/job.json", TerminalFile: "/opt/trainer/terminal.json"}}}}
	snap, err := s.ReplaceCatalog(ctx, "", catalog)
	if err != nil {
		t.Fatal(err)
	}
	state, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	state.Owner = control.OwnerSupervisor
	state.ActiveWorkload = control.WorkloadIdle
	state.DesiredWorkload = control.WorkloadIdle
	state.Phase = control.PhaseStable
	state.Health = control.HealthHealthy
	state.Admission = control.AdmissionClosed
	if _, err = s.UpdateState(ctx, state.Version, state); err != nil {
		t.Fatal(err)
	}
	r := &cliTrainingRuntime{}
	c, err := supervisor.New(s, r, supervisor.Config{Catalog: &snap, DrainTimeout: time.Second, VerifyTimeout: time.Second, ActionTimeout: time.Second, CleanupTimeout: time.Second, FinalizeTimeout: time.Second, PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return c, s, r
}

func captureTrainingCommand(t *testing.T, call func() error) ([]byte, error) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "output")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	prior := os.Stdout
	os.Stdout = f
	defer func() { os.Stdout = prior }()
	commandErr := call()
	raw, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	return raw, commandErr
}

func TestCLITrainingLifecycleRedactsTokens(t *testing.T) {
	for _, finish := range []string{"training-reconcile", "training-recover"} {
		t.Run(finish, func(t *testing.T) {
			c, s, r := cliTrainingFixture(t)
			ctx := context.Background()
			options := modeExecution{trainingJob: "job-a", target: "trainer"}
			for _, command := range []string{"training-start", "training-status"} {
				raw, err := captureTrainingCommand(t, func() error { return executeRuntimeCommand(ctx, c, command, options) })
				if err != nil {
					t.Fatal(err)
				}
				stored, err := s.TrainingJob(ctx, "job-a")
				if err != nil {
					t.Fatal(err)
				}
				if stored.Token == "" {
					t.Fatal("store lost completion token")
				}
				if strings.Contains(string(raw), stored.Token) {
					t.Fatal("CLI leaked completion token")
				}
				var displayed control.TrainingJob
				if err = json.Unmarshal(raw, &displayed); err != nil {
					t.Fatal(err)
				}
				if displayed.ID != "job-a" || displayed.Status != "running" || displayed.Token != "" {
					t.Fatalf("incorrect public job: %+v", displayed)
				}
				if strings.Contains(string(raw), "memoryPeakBytes") {
					t.Fatal("status showed a peak that was not measured")
				}
			}
			raw, err := captureTrainingCommand(t, func() error { return executeRuntimeCommand(ctx, c, finish, options) })
			if err != nil {
				t.Fatal(err)
			}
			var final control.State
			if err = json.Unmarshal(raw, &final); err != nil {
				t.Fatal(err)
			}
			if final.ActiveWorkload != control.WorkloadIdle || final.Admission != control.AdmissionClosed {
				t.Fatalf("unsafe finish: %+v", final)
			}
			stored, err := s.TrainingJob(ctx, "job-a")
			if err != nil {
				t.Fatal(err)
			}
			want := "completed"
			if finish == "training-recover" {
				want = "interrupted"
			}
			if stored.Status != want {
				t.Fatalf("status=%s want=%s", stored.Status, want)
			}
			if r.starts != 1 {
				t.Fatalf("launch count=%d", r.starts)
			}
			raw, err = captureTrainingCommand(t, func() error { return executeRuntimeCommand(ctx, c, "training-status", options) })
			if err != nil || !strings.Contains(string(raw), `"memoryPeakBytes":1073741824`) {
				t.Fatalf("status does not show the recorded peak: %s %v", raw, err)
			}
		})
	}
}

func TestCLITrainingErrorsKeepDurableIdentityPrivate(t *testing.T) {
	c, s, r := cliTrainingFixture(t)
	sentinel := errors.New("launch acknowledgement lost")
	r.startErr = sentinel
	raw, err := captureTrainingCommand(t, func() error { return executeTraining(context.Background(), c, "training-start", "job-a", "trainer") })
	if !errors.Is(err, sentinel) {
		t.Fatalf("launch error hidden: %v", err)
	}
	job, err := s.TrainingJob(context.Background(), "job-a")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), job.Token) {
		t.Fatal("failure output leaked token")
	}
	if _, err = captureTrainingCommand(t, func() error { return executeTraining(context.Background(), c, "training-start", "job-a", "trainer") }); !errors.Is(err, store.ErrRequestConflict) {
		t.Fatalf("duplicate launch error hidden: %v", err)
	}
	if r.starts != 1 {
		t.Fatal("CLI replayed ambiguous launch")
	}
}

func TestCLITrainingRejectsBadCommandsAndReportsOutputFailure(t *testing.T) {
	for _, tc := range []struct{ command, id string }{{"training-start", ""}, {"training-unknown", "job-a"}} {
		raw, err := captureTrainingCommand(t, func() error { return executeTraining(context.Background(), nil, tc.command, tc.id, "") })
		if err == nil || len(raw) != 0 {
			t.Fatalf("invalid command accepted or emitted output: %s %v", raw, err)
		}
	}
	c, _, _ := cliTrainingFixture(t)
	closed, err := os.CreateTemp(t.TempDir(), "closed")
	if err != nil {
		t.Fatal(err)
	}
	if err = closed.Close(); err != nil {
		t.Fatal(err)
	}
	prior := os.Stdout
	os.Stdout = closed
	defer func() { os.Stdout = prior }()
	if err = executeTraining(context.Background(), c, "training-status", "missing", ""); err == nil || !errors.Is(err, os.ErrClosed) {
		t.Fatalf("output failure lost: %v", err)
	}
}

func TestCLITrainingFlagsRejectBeforeStateOrRuntimeAccess(t *testing.T) {
	for _, args := range [][]string{{"-training-job", "job-a", "status"}, {"training-status"}, {"-training-job", "job-a", "training-unknown"}} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "must-not-exist.db")
			prior := os.Args
			os.Args = append([]string{"gpu-mode", "-state", path}, args...)
			defer func() { os.Args = prior }()
			if err := run(); err == nil {
				t.Fatal("invalid training flags accepted")
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid flags touched state: %v", err)
			}
		})
	}
}

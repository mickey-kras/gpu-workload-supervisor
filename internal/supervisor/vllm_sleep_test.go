package supervisor

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

type hibernatingFakeRuntime struct {
	*acceptanceRuntime
	actions  []string
	sleeping map[control.Workload]bool
}

func (h *hibernatingFakeRuntime) Sleep(ctx context.Context, id control.Workload) (string, error) {
	h.actions = append(h.actions, "sleep:"+string(id))
	if h.active == id {
		h.sleeping[id] = true
	}
	return "sleeping", h.acceptanceRuntime.Stop(ctx, id)
}

func (h *hibernatingFakeRuntime) Stop(ctx context.Context, id control.Workload) error {
	h.actions = append(h.actions, "stop:"+string(id))
	delete(h.sleeping, id)
	return h.acceptanceRuntime.Stop(ctx, id)
}

func (h *hibernatingFakeRuntime) ReleasedForSetup(ctx context.Context, target control.Workload) error {
	for id, asleep := range h.sleeping {
		if asleep && id != target {
			return errors.New("sleeping service still holds CUDA context")
		}
	}
	return h.acceptanceRuntime.ReleasedFor(ctx, target)
}

func (h *hibernatingFakeRuntime) StartTraining(_ context.Context, _ control.WorkloadProfile, job control.TrainingJob) (string, error) {
	h.actions = append(h.actions, "train:"+string(job.Workload))
	return strings.Repeat("a", 32), nil
}

func (h *hibernatingFakeRuntime) ReleaseTraining(ctx context.Context, p control.WorkloadProfile) error {
	if len(h.sleeping) != 0 {
		return errors.New("GPU compute processes remain after trainer exit")
	}
	return h.ReleasedForSetup(ctx, control.WorkloadIdle)
}

func (h *hibernatingFakeRuntime) Wake(ctx context.Context, id control.Workload) error {
	h.actions = append(h.actions, "wake:"+string(id))
	delete(h.sleeping, id)
	return h.acceptanceRuntime.Start(ctx, id)
}

func TestControllerDispatchesHibernatableRuntimeOnSwitch(t *testing.T) {
	ctx := context.Background()
	original, s, base, _ := acceptanceController(t)
	hr := &hibernatingFakeRuntime{acceptanceRuntime: base, sleeping: make(map[control.Workload]bool)}
	c, err := New(s, hr, original.config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Switch(ctx, "speech", "test"); err != nil {
		t.Fatalf("Switch to speech: %v", err)
	}
	if !slices.Contains(hr.actions, "wake:speech") {
		t.Fatalf("expected wake:speech, got %v", hr.actions)
	}
	hr.actions = nil
	if _, err := c.Switch(ctx, "media", "test"); err != nil {
		t.Fatalf("Switch to media: %v", err)
	}
	if !slices.Contains(hr.actions, "sleep:speech") || !slices.Contains(hr.actions, "wake:media") {
		t.Fatalf("expected sleep:speech and wake:media, got %v", hr.actions)
	}
}

// Starting a service that still runs does not wake it.
func (h *hibernatingFakeRuntime) Start(ctx context.Context, id control.Workload) error {
	if h.sleeping[id] {
		return nil
	}
	return h.acceptanceRuntime.Start(ctx, id)
}

func TestRollbackWakesSleepingSource(t *testing.T) {
	ctx := context.Background()
	original, s, base, _ := acceptanceController(t)
	runtime := &hibernatingFakeRuntime{acceptanceRuntime: base, sleeping: make(map[control.Workload]bool)}
	c, err := New(s, runtime, original.config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Switch(ctx, "speech", "test"); err != nil {
		t.Fatal(err)
	}
	base.failStart = "media"
	state, err := c.Switch(ctx, "media", "test")
	if err == nil || errors.Is(err, ErrVerifyTimeout) || runtime.sleeping["speech"] || state.ActiveWorkload != "speech" {
		t.Fatalf("rollback state = %s, sleeping = %t, error = %v", state.ActiveWorkload, runtime.sleeping["speech"], err)
	}
}

func TestTrainingAndUserHandoffColdStopSleepingServices(t *testing.T) {
	ctx := context.Background()
	original, s, base, _ := acceptanceController(t)
	cat, err := s.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cat.Catalog.Version = 4
	cat.Catalog.Profiles = append(cat.Catalog.Profiles, control.WorkloadProfile{
		ID: "trainer", Label: "Trainer", Adapter: "systemd", Unit: "trainer.service",
		Cgroup: "/training/trainer.service", RequiredMiB: 1024, BootPolicy: "stop-to-idle",
		Trainer: &control.TrainerBinding{
			Version: 1, GPUUUID: "GPU-11111111-1111-1111-1111-111111111111",
			LaunchFile: "/opt/trainer/unit.service", LaunchSHA256: strings.Repeat("a", 64),
			Executable: "/opt/trainer/train", ExecutableSHA256: strings.Repeat("b", 64),
			RecipeFile: "/opt/trainer/recipe.json", RecipeSHA256: strings.Repeat("c", 64),
			JobFile: "/opt/trainer/job.json", TerminalFile: "/opt/trainer/terminal.json",
		},
	})
	snap, err := s.ReplaceCatalog(ctx, cat.Revision, cat.Catalog)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &hibernatingFakeRuntime{acceptanceRuntime: base, sleeping: make(map[control.Workload]bool)}
	cfg := original.config
	cfg.Catalog = &snap
	c, err := New(s, runtime, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Switch(ctx, "speech", "test"); err != nil {
		t.Fatal(err)
	}
	// Park speech in level 1 sleep at Idle, then run finite training.
	if _, err := c.Switch(ctx, control.WorkloadIdle, "idle-policy"); err != nil || !runtime.sleeping["speech"] {
		t.Fatalf("expected sleeping speech at idle: sleeping=%v err=%v", runtime.sleeping, err)
	}
	if _, err := c.StartTraining(ctx, "job-sleep-handoff", "trainer"); err != nil {
		t.Fatalf("StartTraining failed: %v", err)
	}
	if runtime.sleeping["speech"] {
		t.Fatal("StartTraining left sleeping service alive")
	}
	if _, err := c.RecoverTraining(ctx, "job-sleep-handoff"); err != nil {
		t.Fatalf("RecoverTraining failed: %v", err)
	}
	// User ownership handoff also cold-stops sleeping supervisor services.
	if _, err := c.Switch(ctx, "speech", "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.TransferToUser(ctx, control.WorkloadIdle, "user-takeover"); err != nil || runtime.sleeping["speech"] {
		t.Fatalf("TransferToUser left sleeping service: sleeping=%v err=%v", runtime.sleeping, err)
	}
}

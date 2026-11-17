package supervisor

import (
	"context"
	"errors"
	"slices"
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
	return "sleeping", h.Stop(ctx, id)
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

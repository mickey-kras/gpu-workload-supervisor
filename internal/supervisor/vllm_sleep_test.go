package supervisor

import (
	"context"
	"slices"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

type hibernatingFakeRuntime struct {
	*acceptanceRuntime
	actions []string
}

func (h *hibernatingFakeRuntime) Sleep(ctx context.Context, id control.Workload) error {
	h.actions = append(h.actions, "sleep:"+string(id))
	return h.acceptanceRuntime.Stop(ctx, id)
}

func (h *hibernatingFakeRuntime) Wake(ctx context.Context, id control.Workload) error {
	h.actions = append(h.actions, "wake:"+string(id))
	return h.acceptanceRuntime.Start(ctx, id)
}

func TestControllerDispatchesHibernatableRuntimeOnSwitch(t *testing.T) {
	ctx := context.Background()
	original, s, base, _ := acceptanceController(t)
	hr := &hibernatingFakeRuntime{acceptanceRuntime: base}
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

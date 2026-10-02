package supervisor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

func TestReleaseVerificationDeadlineBoundsBlockingProbe(t *testing.T) {
	stateStore := openStore(t)
	runtime := &fakeRuntime{blockRelease: true}
	controller := testController(t, stateStore, runtime)
	controller.config.ActionTimeout = time.Second
	controller.config.VerifyTimeout = 20 * time.Millisecond

	start := time.Now()
	err := controller.waitReleasedFor(context.Background(), control.WorkloadIdle, start.Add(controller.config.VerifyTimeout))
	elapsed := time.Since(start)
	if !errors.Is(err, ErrVerifyTimeout) {
		t.Fatalf("release verification error = %v, want ErrVerifyTimeout", err)
	}
	if elapsed >= 500*time.Millisecond {
		t.Fatalf("release probe exceeded phase deadline: %v", elapsed)
	}
	if runtime.releaseCalls != 1 {
		t.Fatalf("release probe calls = %d, want 1", runtime.releaseCalls)
	}
}

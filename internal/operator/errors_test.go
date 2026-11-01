package operator

import (
	"context"
	"errors"
	"fmt"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/supervisor"
	"golang.org/x/sys/unix"
	"testing"
)

func TestTypedErrors(t *testing.T) {
	// Only the supervisor refusal before any effect is definite. The runtime
	// backstop error after an unload, a rollback or a status latch keeps the
	// code of the failure it joins.
	backstop := fmt.Errorf("vision (gws-owned-vision.service): %w", gpuruntime.ErrLimitsNotEnforced)
	refused := fmt.Errorf("%w: %w", supervisor.ErrLimitedTarget, backstop)
	for _, x := range []struct {
		e error
		c Code
	}{{context.DeadlineExceeded, Timeout}, {store.ErrWrongOwner, WrongOwner}, {supervisor.ErrSupervisorOwned, WrongOwner}, {store.ErrVersionConflict, StaleState}, {store.ErrConfigurationConflict, StaleState}, {unix.EAGAIN, Busy}, {supervisor.ErrTransitionRunning, Busy}, {store.ErrUnstableState, RecoveryRequired}, {errors.New("secret path"), Unavailable}, {refused, IncompatibleConfiguration}, {backstop, Unavailable}, {errors.Join(supervisor.ErrVerifyTimeout, backstop), Timeout}, {errors.Join(supervisor.ErrRecoveryRequired, backstop), RecoveryRequired}} {
		if c := errorCode(x.e); c != x.c {
			t.Fatal(x, c)
		}
	}
}

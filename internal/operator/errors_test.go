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
	// Only the supervisor refusal before any effect is definite. A failed
	// limit check after an effect keeps the code of the failure it joins.
	limits := fmt.Errorf("vision (gws-owned-vision.service): %w", gpuruntime.ErrLimitDrift)
	refused := fmt.Errorf("%w: %w", supervisor.ErrLimitedTarget, gpuruntime.ErrHostCap)
	for _, x := range []struct {
		e error
		c Code
	}{{context.DeadlineExceeded, Timeout}, {store.ErrWrongOwner, WrongOwner}, {supervisor.ErrSupervisorOwned, WrongOwner}, {store.ErrVersionConflict, StaleState}, {store.ErrConfigurationConflict, StaleState}, {unix.EAGAIN, Busy}, {supervisor.ErrTransitionRunning, Busy}, {store.ErrUnstableState, RecoveryRequired}, {errors.New("secret path"), Unavailable}, {refused, IncompatibleConfiguration}, {limits, Unavailable}, {errors.Join(supervisor.ErrVerifyTimeout, limits), Timeout}, {errors.Join(supervisor.ErrRecoveryRequired, limits), RecoveryRequired},
		{fmt.Errorf("%w %q", supervisor.ErrInvalidTarget, "idle"), InvalidRequest}, {supervisor.ErrTrainerTarget, InvalidRequest}} {
		if c := errorCode(x.e); c != x.c {
			t.Fatal(x, c)
		}
	}
}

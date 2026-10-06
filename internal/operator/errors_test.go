package operator

import (
	"context"
	"errors"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/supervisor"
	"golang.org/x/sys/unix"
	"testing"
)

func TestTypedErrors(t *testing.T) {
	for _, x := range []struct {
		e error
		c Code
	}{{context.DeadlineExceeded, Timeout}, {store.ErrWrongOwner, WrongOwner}, {supervisor.ErrSupervisorOwned, WrongOwner}, {store.ErrVersionConflict, StaleState}, {store.ErrConfigurationConflict, StaleState}, {unix.EAGAIN, Busy}, {supervisor.ErrTransitionRunning, Busy}, {store.ErrUnstableState, RecoveryRequired}, {errors.New("secret path"), Unavailable}} {
		if c := errorCode(x.e); c != x.c {
			t.Fatal(x, c)
		}
	}
}

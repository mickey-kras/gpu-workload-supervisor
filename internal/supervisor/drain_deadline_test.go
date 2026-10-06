package supervisor

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestDrainProbeFailureOutranksDeadline(t *testing.T) {
	c := testController(t, openStore(t), &fakeRuntime{})
	probeErr := errors.New("pending work query unavailable")
	probe := func(ctx context.Context) (int, error) {
		time.Sleep(30 * time.Millisecond)
		return 0, probeErr
	}
	err := c.waitForWork(context.Background(), time.Now().Add(10*time.Millisecond), probe)
	if !errors.Is(err, probeErr) || errors.Is(err, ErrDrainTimeout) {
		t.Fatalf("probe failure misreported: %v", err)
	}
}

func TestVerifyTimeoutPrefersParentCancellation(t *testing.T) {
	c := testController(t, openStore(t), &fakeRuntime{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	deadline := time.Now().Add(time.Hour)
	calls := 0
	c.now = func() time.Time {
		calls++
		if calls == 1 {
			return time.Now()
		}
		// The caller cancels as the budget check runs; that is cancellation,
		// not a verification timeout.
		cancel()
		return deadline.Add(time.Hour)
	}
	probe := func(context.Context) error { return nil }
	if err := c.pollUntil(ctx, deadline, probe); !errors.Is(err, context.Canceled) || errors.Is(err, ErrVerifyTimeout) {
		t.Fatalf("poll error = %v", err)
	}
}

func TestDrainPhaseDeadline(t *testing.T) {
	for _, mode := range []string{"blocked query", "late zero", "poll wait", "caller cancellation"} {
		t.Run(mode, func(t *testing.T) {
			c := testController(t, openStore(t), &fakeRuntime{})
			c.config.PollInterval = time.Second
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			deadline := time.Now().Add(20 * time.Millisecond)
			probe := func(ctx context.Context) (int, error) {
				switch mode {
				case "blocked query":
					select {
					case <-ctx.Done():
						return 0, ctx.Err()
					case <-time.After(200 * time.Millisecond):
						return 0, nil
					}
				case "late zero":
					time.Sleep(40 * time.Millisecond)
					return 0, nil
				case "caller cancellation":
					cancel()
					return 0, nil
				default:
					return 1, nil
				}
			}
			start := time.Now()
			err := c.waitForWork(ctx, deadline, probe)
			var want error
			switch mode {
			case "caller cancellation":
				want = context.Canceled
			case "late zero":
				// An observed empty queue completes the drain even when the
				// probe returns exactly at the deadline.
				want = nil
			default:
				want = ErrDrainTimeout
			}
			if !errors.Is(err, want) {
				t.Fatalf("got %v, want %v", err, want)
			}
			// Generous wall-clock bound: scheduling jitter must not flake this,
			// but the drain must not wait out the one-second poll interval.
			if time.Since(start) > time.Second {
				t.Fatalf("drain exceeded deadline: %v", time.Since(start))
			}
		})
	}
}

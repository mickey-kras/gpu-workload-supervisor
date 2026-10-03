package supervisor

import (
	"context"
	"errors"
	"testing"
	"time"
)

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
			want := ErrDrainTimeout
			if mode == "caller cancellation" {
				want = context.Canceled
			}
			if !errors.Is(err, want) {
				t.Fatalf("got %v, want %v", err, want)
			}
			if time.Since(start) > 150*time.Millisecond {
				t.Fatalf("drain exceeded deadline: %v", time.Since(start))
			}
		})
	}
}

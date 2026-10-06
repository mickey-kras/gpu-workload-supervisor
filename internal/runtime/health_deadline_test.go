package runtime

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

type blockedHealthRunner struct{}

func (blockedHealthRunner) Run(ctx context.Context, _ string, _ ...string) ([]byte, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestHealthTimeoutBoundsSystemctl(t *testing.T) {
	for _, target := range []control.Workload{control.WorkloadText, control.WorkloadMedia} {
		t.Run(string(target), func(t *testing.T) {
			config := testConfig()
			config.HealthTimeout = 20 * time.Millisecond
			manager, err := newSystemdManager(config, blockedHealthRunner{}, http.DefaultClient)
			if err != nil {
				t.Fatal(err)
			}
			fixtureCgroups(t, manager)
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			start := time.Now()
			err = manager.Healthy(ctx, target)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("health error = %v", err)
			}
			// Generous wall-clock bound: scheduling jitter must not flake this,
			// but the probe must not wait out the 500ms parent context.
			if elapsed := time.Since(start); elapsed > time.Second {
				t.Fatalf("systemctl exceeded health timeout: %v", elapsed)
			}
		})
	}
}

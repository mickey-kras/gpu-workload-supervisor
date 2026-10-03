package runtime

import (
	"context"
	"errors"
	"os/exec"
	"testing"
	"time"
)

func TestExecRunnerBoundsInheritedPipes(t *testing.T) {
	for _, script := range []string{"sleep 2 & wait", "sleep 2 & exit 0"} {
		t.Run(script, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			start := time.Now()
			_, err := (ExecRunner{}).Run(ctx, "/bin/sh", "-c", script)
			if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, exec.ErrWaitDelay) {
				t.Fatalf("expected bounded wait error, got %v", err)
			}
			if time.Since(start) > time.Second {
				t.Fatalf("inherited pipe delayed return: %v", time.Since(start))
			}
		})
	}
}

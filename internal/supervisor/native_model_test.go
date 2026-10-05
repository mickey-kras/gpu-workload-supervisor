package supervisor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

func TestNamedModelsUseFullReleaseLifecycleWithinRuntime(t *testing.T) {
	for _, family := range []string{"ollama", "llama.cpp", "vllm"} {
		t.Run(family, func(t *testing.T) {
			ctx := context.Background()
			c, s, r, _ := acceptanceController(t)
			cat := c.config.Catalog.Catalog.Clone()
			for _, i := range []int{0, 2} {
				p := &cat.Profiles[i]
				p.NativeModel = &control.NativeModel{Runtime: family, Instance: "local", Endpoint: "http://localhost:9000", Model: string(p.ID), LaunchFile: "/models/" + string(p.ID) + ".service", LaunchSHA256: strings.Repeat("a", 64)}
			}
			snap, err := s.ReplaceCatalog(ctx, c.config.Catalog.Revision, cat)
			if err != nil {
				t.Fatal(err)
			}
			c.config.Catalog = &snap
			if _, err = c.Switch(ctx, "text", "test"); err != nil {
				t.Fatal(err)
			}
			r.calls = nil
			releaseBefore := r.releaseCalls
			state, err := c.Switch(ctx, "speech", "test")
			if err != nil {
				t.Fatal(err)
			}
			stop, start := -1, -1
			for i, call := range r.calls {
				if call == "stop text" {
					stop = i
				}
				if call == "start speech" {
					start = i
				}
			}
			if stop < 0 || start <= stop || r.releaseCalls <= releaseBefore || state.ActiveWorkload != "speech" {
				t.Fatalf("missing release boundary: %v %+v", r.calls, state)
			}
			r.calls = nil
			same, err := c.Switch(ctx, "speech", "test")
			if err != nil || same.ActiveWorkload != state.ActiveWorkload {
				t.Fatalf("same-model switch was not a no-op: %v %v", r.calls, err)
			}
			for _, call := range r.calls {
				if call == "stop speech" || call == "start speech" {
					t.Fatal("same-model request restarted selected service", r.calls)
				}
			}
			for _, target := range []control.Workload{"media", "speech", control.WorkloadIdle} {
				if _, err = c.Switch(ctx, target, "test"); err != nil {
					t.Fatal(target, err)
				}
			}
			if _, err = c.Switch(ctx, "text", "test"); err != nil {
				t.Fatal(err)
			}
			r.calls = nil
			r.blockRelease = true
			c.config.VerifyTimeout = 5 * time.Millisecond
			c.config.CleanupTimeout = 5 * time.Millisecond
			state, err = c.Switch(ctx, "speech", "test")
			if err == nil || state.Admission != control.AdmissionClosed {
				t.Fatal("release failure did not close admission", state, err)
			}
			for _, call := range r.calls {
				if call == "start speech" {
					t.Fatal("started model before GPU release")
				}
			}
		})
	}
}

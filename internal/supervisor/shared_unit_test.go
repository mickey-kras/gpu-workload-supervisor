package supervisor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
)

// sharedUnitRuntime emulates the runtime contract of sibling profiles bound to
// one Ollama unit: switching is model-level, Stop unloads only the profile's
// own model, and release fails while any model other than the sibling
// target's remains loaded, including auto-loaded foreign models.
type sharedUnitRuntime struct {
	calls  []string
	loaded map[string]bool
	chat   bool
}

func (r *sharedUnitRuntime) Observe(context.Context) (gpuruntime.Snapshot, error) {
	return gpuruntime.Snapshot{Workloads: map[control.Workload]gpuruntime.WorkloadObservation{
		"alpha": {Active: r.loaded["a"], Exclusive: true},
		"beta":  {Active: r.loaded["b"], Exclusive: true},
		"chat":  {Active: r.chat, Exclusive: true},
	}}, nil
}

func (r *sharedUnitRuntime) Start(_ context.Context, id control.Workload) error {
	r.calls = append(r.calls, "start "+string(id))
	switch id {
	case "alpha":
		r.loaded = map[string]bool{"a": true}
	case "beta":
		r.loaded = map[string]bool{"b": true}
	case "chat":
		r.chat = true
	}
	return nil
}

func (r *sharedUnitRuntime) Stop(_ context.Context, id control.Workload) error {
	r.calls = append(r.calls, "stop "+string(id))
	switch id {
	case "alpha":
		delete(r.loaded, "a")
	case "beta":
		delete(r.loaded, "b")
	case "chat":
		r.chat = false
	}
	return nil
}

func (r *sharedUnitRuntime) StopForRecovery(ctx context.Context) error {
	for _, id := range []control.Workload{"alpha", "beta", "chat"} {
		if err := r.Stop(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

func (r *sharedUnitRuntime) Healthy(context.Context, control.Workload) error { return nil }
func (r *sharedUnitRuntime) Preflight(context.Context) error                 { return nil }

func (r *sharedUnitRuntime) ReleasedFor(_ context.Context, target control.Workload) error {
	if target != "chat" && r.chat {
		return errors.New("chat is not released")
	}
	allowed := map[control.Workload]string{"alpha": "a", "beta": "b"}[target]
	for model := range r.loaded {
		if model != allowed {
			return fmt.Errorf("shared unit model %s is still loaded", model)
		}
	}
	return nil
}

func sharedUnitController(t *testing.T) (*Controller, *sharedUnitRuntime) {
	t.Helper()
	ctx := context.Background()
	s := openStore(t)
	native := func(runtime, instance, model, endpoint string) *control.NativeModel {
		return &control.NativeModel{Runtime: runtime, Instance: instance, Model: model, Endpoint: endpoint, LaunchFile: "/etc/systemd/user/" + instance + ".service", LaunchSHA256: strings.Repeat("0", 64)}
	}
	sibling := func(id, model string) control.WorkloadProfile {
		return control.WorkloadProfile{ID: control.Workload(id), Label: id, Adapter: "systemd", Unit: "ollama.service", Cgroup: "/workloads/ollama.service", HealthURL: "http://127.0.0.1:11434/health", NativeModel: native("ollama", "local", model, "http://127.0.0.1:11434")}
	}
	cat := control.Catalog{Version: 1, Profiles: []control.WorkloadProfile{
		sibling("alpha", "a"),
		sibling("beta", "b"),
		{ID: "chat", Label: "chat", Adapter: "systemd", Unit: "chat.service", Cgroup: "/workloads/chat.service", HealthURL: "http://127.0.0.1:9100/health", NativeModel: native("llama.cpp", "chat", "c", "http://127.0.0.1:9100")},
	}}
	snap, err := s.ReplaceCatalog(ctx, "", cat)
	if err != nil {
		t.Fatal(err)
	}
	r := &sharedUnitRuntime{loaded: map[string]bool{}}
	n := 0
	c, err := newController(s, r, Config{Catalog: &snap, DrainTimeout: 500 * time.Millisecond, VerifyTimeout: 300 * time.Millisecond, ActionTimeout: 500 * time.Millisecond, CleanupTimeout: 500 * time.Millisecond, FinalizeTimeout: 500 * time.Millisecond, PollInterval: 5 * time.Millisecond}, time.Now, func() (string, error) { n++; return fmt.Sprintf("transition-%d", n), nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	return c, r
}

func TestSharedUnitSwitchSequence(t *testing.T) {
	ctx := context.Background()
	c, r := sharedUnitController(t)
	for _, target := range []control.Workload{"alpha", "beta", control.WorkloadIdle, "chat", "beta"} {
		state, err := c.Switch(ctx, target, "test")
		if err != nil {
			t.Fatal(target, err)
		}
		if state.ActiveWorkload != target {
			t.Fatalf("target %s: state = %#v", target, state)
		}
	}
	joined := strings.Join(r.calls, ",")
	sequence := []string{"start alpha", "stop alpha", "start beta", "stop beta", "start chat", "stop chat", "start beta"}
	at := 0
	for _, call := range r.calls {
		if at < len(sequence) && call == sequence[at] {
			at++
		}
	}
	if at != len(sequence) || r.chat || !r.loaded["b"] || len(r.loaded) != 1 {
		t.Fatalf("switch sequence = %s loaded=%v chat=%v", joined, r.loaded, r.chat)
	}
}

func TestSharedUnitForeignModelBlocksIdleAndCrossRuntimeSwitches(t *testing.T) {
	for _, target := range []control.Workload{control.WorkloadIdle, "chat"} {
		t.Run(string(target), func(t *testing.T) {
			ctx := context.Background()
			c, r := sharedUnitController(t)
			if _, err := c.Switch(ctx, "alpha", "test"); err != nil {
				t.Fatal(err)
			}
			r.loaded["foreign"] = true
			if _, err := c.Switch(ctx, target, "test"); err == nil {
				t.Fatal("foreign loaded model accepted for", target)
			}
			for _, call := range r.calls {
				if call == "start "+string(target) {
					t.Fatal("started target over unreleased foreign model", r.calls)
				}
			}
			if _, err := c.Recover(ctx); err == nil {
				t.Fatal("recovery ignored foreign loaded model")
			}
			delete(r.loaded, "foreign")
			if _, err := c.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			state, err := c.Switch(ctx, target, "test")
			if err != nil || state.ActiveWorkload != target {
				t.Fatalf("switch after clearing foreign model: %v %#v", err, state)
			}
		})
	}
}

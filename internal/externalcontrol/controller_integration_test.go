package externalcontrol

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/lock"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/supervisor"
)

type integrationRuntime struct {
	active         control.Workload
	effects        int
	startEntered   chan struct{}
	cleanupStarted chan struct{}
	allowCleanup   chan struct{}
}

func (r *integrationRuntime) Observe(context.Context) (gpuruntime.Snapshot, error) {
	return gpuruntime.Snapshot{TextActive: r.active == control.WorkloadText, MediaReady: r.active == control.WorkloadMedia}, nil
}
func (r *integrationRuntime) Start(ctx context.Context, w control.Workload) error {
	r.effects++
	if r.cleanupStarted != nil && w == control.WorkloadMedia {
		close(r.startEntered)
		<-ctx.Done()
		return ctx.Err()
	}
	if r.cleanupStarted != nil && w == control.WorkloadText {
		close(r.cleanupStarted)
		select {
		case <-r.allowCleanup:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	r.active = w
	return nil
}
func (r *integrationRuntime) Stop(_ context.Context, w control.Workload) error {
	r.effects++
	if r.active == w {
		r.active = control.WorkloadIdle
	}
	return nil
}
func (r *integrationRuntime) StopForRecovery(ctx context.Context) error {
	return r.Stop(ctx, r.active)
}
func (r *integrationRuntime) Released(context.Context) error                  { return nil }
func (r *integrationRuntime) Healthy(context.Context, control.Workload) error { return nil }

func integratedService(t *testing.T) (*Service, *store.Store, *supervisor.Controller, *integrationRuntime) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	runtime := &integrationRuntime{active: control.WorkloadText}
	c, err := supervisor.New(s, runtime, supervisor.Config{DrainTimeout: time.Second, VerifyTimeout: time.Second, ActionTimeout: time.Second, CleanupTimeout: time.Second, FinalizeTimeout: time.Second, PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	runtime.effects = 0
	svc, err := New(c, resolverFunc(func(context.Context) (string, error) { return "verified", nil }), &auditRecorder{}, Config{Timeout: time.Second, Grants: map[string]Grant{"verified": {AuditID: "broker-1", Status: true, Workloads: []control.Workload{control.WorkloadMedia}}}})
	if err != nil {
		t.Fatal(err)
	}
	if s.DurableStatePath() != path || c.DurableStatePath() != path {
		t.Fatal("backend not bound to store path")
	}
	return svc, s, c, runtime
}
func bodyForState(t *testing.T, state control.State) []byte {
	t.Helper()
	body, err := json.Marshal(Request{APIVersion: APIVersion, Operation: OperationRequestWorkload, Target: control.WorkloadMedia, Expected: &control.Precondition{Incarnation: state.LeaseFence.Incarnation, Version: state.Version}})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestServiceRealControllerBindsGateAndToken(t *testing.T) {
	svc, s, _, runtime := integratedService(t)
	ctx := context.Background()
	state, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := lock.TryAcquire(s.DurableStatePath() + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range [][]byte{[]byte(statusBody), bodyForState(t, state)} {
		if result := svc.Handle(ctx, body); result.Code != CodeBusy || runtime.effects != 0 {
			t.Fatalf("gate mismatch: %#v effects=%d", result, runtime.effects)
		}
	}
	gate.Close()
	result := svc.Handle(ctx, bodyForState(t, state))
	if result.Code != CodeOK || result.Status == nil || result.Status.ActiveWorkload != control.WorkloadMedia {
		t.Fatalf("transition: %#v", result)
	}
	effects := runtime.effects
	result = svc.Handle(ctx, bodyForState(t, state))
	if result.Code != CodeStaleState || runtime.effects != effects {
		t.Fatalf("stale replay executed: %#v", result)
	}
}

func TestServiceRealControllerPreservesUserOwnership(t *testing.T) {
	svc, s, c, runtime := integratedService(t)
	state, err := c.TransferToUser(context.Background(), control.WorkloadText, "human")
	if err != nil {
		t.Fatal(err)
	}
	runtime.effects = 0
	result := svc.Handle(context.Background(), bodyForState(t, state))
	after, err := s.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Code != CodeUserOwned || result.Status != nil || after != state || runtime.effects != 0 {
		t.Fatalf("user boundary: %#v %#v effects=%d", result, after, runtime.effects)
	}
}

func TestServiceRealCancellationWaitsForCleanupAndFailsClosed(t *testing.T) {
	svc, s, _, runtime := integratedService(t)
	runtime.cleanupStarted = make(chan struct{})
	runtime.allowCleanup = make(chan struct{})
	runtime.startEntered = make(chan struct{})
	state, err := s.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan Response, 1)
	body := bodyForState(t, state)
	go func() { done <- svc.Handle(ctx, body) }()
	select {
	case <-runtime.startEntered:
	case <-time.After(time.Second):
		t.Fatal("transition not entered")
	}
	cancel()
	select {
	case <-runtime.cleanupStarted:
	case <-time.After(time.Second):
		t.Fatal("cleanup not entered")
	}
	if result := svc.Handle(context.Background(), []byte(statusBody)); result.Code != CodeBusy {
		t.Fatalf("gate not held through real cleanup: %#v", result)
	}
	select {
	case result := <-done:
		t.Fatalf("returned before cleanup: %#v", result)
	default:
	}
	close(runtime.allowCleanup)
	result := <-done
	if result.Code != CodeCanceled {
		t.Fatalf("cancellation: %#v", result)
	}
	after, err := s.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if after.Health != control.HealthError || after.Admission != control.AdmissionClosed || after.Phase != control.PhaseReconciling {
		t.Fatalf("not fail closed: %#v", after)
	}
	if running, err := s.InProgressTransition(context.Background()); err != nil || running != "" {
		t.Fatalf("cleanup not finalized: %q %v", running, err)
	}
}

type contextAudit struct{}

func (contextAudit) Record(ctx context.Context, _ AuditEvent) error { return ctx.Err() }
func TestContextAwareAuditFailureOverridesCancellationAfterEffects(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(map[bool]string{false: "caller-cancel", true: "deadline"}[timeout], func(t *testing.T) {
			_, b, _, cfg := serviceFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if timeout {
				cfg.Timeout = time.Millisecond
				b.execute = func(ctx context.Context) { <-ctx.Done() }
			} else {
				b.execute = func(context.Context) { cancel() }
			}
			svc, err := New(b, resolverFunc(func(context.Context) (string, error) { return "verified-secret", nil }), contextAudit{}, cfg)
			if err != nil {
				t.Fatal(err)
			}
			result := svc.Handle(ctx, []byte(switchBody))
			if result.Code != CodeUnavailable || result.Status != nil || b.calls != 1 {
				t.Fatalf("audit precedence after possible effects: %#v calls=%d", result, b.calls)
			}
		})
	}
}

func TestInvalidBackendStatusAndCorrelationFailureAreRedacted(t *testing.T) {
	svc, b, _, _ := serviceFixture(t)
	b.state.Owner = "backend-secret"
	if result := svc.Handle(context.Background(), []byte(statusBody)); result.Code != CodeUnavailable || result.Status != nil {
		t.Fatalf("invalid state: %#v", result)
	}
	svc.id = func() (string, error) { return "", errors.New("entropy-secret") }
	b.calls = 0
	if result := svc.Handle(context.Background(), []byte(switchBody)); result.Code != CodeUnavailable || b.calls != 0 {
		t.Fatalf("entropy failure: %#v", result)
	}
}

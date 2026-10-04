package externalcontrol

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/lock"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/supervisor"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type resolverFunc func(context.Context) (string, error)

func (f resolverFunc) Resolve(ctx context.Context) (string, error) { return f(ctx) }

type auditRecorder struct {
	mu     sync.Mutex
	events []AuditEvent
	failAt int
}

func (a *auditRecorder) Record(_ context.Context, event AuditEvent) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, event)
	if len(a.events) == a.failAt {
		return errors.New("audit-secret")
	}
	return nil
}

type testBackend struct {
	path      string
	calls     int
	state     control.State
	err       error
	target    control.Workload
	initiator string
	expected  control.Precondition
	execute   func(context.Context)
}

func (b *testBackend) DurableStatePath() string { return b.path }
func (b *testBackend) Status(ctx context.Context) (control.State, error) {
	b.calls++
	if b.execute != nil {
		b.execute(ctx)
	}
	return b.state, b.err
}
func (b *testBackend) SwitchConditional(ctx context.Context, target control.Workload, initiator string, p control.Precondition) (control.State, error) {
	b.target = target
	b.initiator = initiator
	b.expected = p
	return b.Status(ctx)
}
func serviceFixture(t *testing.T) (*Service, *testBackend, *auditRecorder, Config) {
	t.Helper()
	b := &testBackend{path: filepath.Join(t.TempDir(), "state.db"), state: control.InitialState("inc-1", time.Now())}
	b.state.Phase = control.PhaseStable
	b.state.ActiveWorkload = control.WorkloadIdle
	a := &auditRecorder{}
	cfg := Config{Timeout: time.Second, Grants: map[string]Grant{"verified-secret": {AuditID: "broker-1", Status: true, Workloads: []control.Workload{control.WorkloadMedia}}}}
	s, err := New(b, resolverFunc(func(context.Context) (string, error) { return "verified-secret", nil }), a, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s, b, a, cfg
}

func TestServiceSuccessAndImmutablePolicy(t *testing.T) {
	s, b, a, cfg := serviceFixture(t)
	cfg.Grants["verified-secret"].Workloads[0] = control.WorkloadText
	cfg.Grants["verified-secret"] = Grant{AuditID: "changed", Status: false, Workloads: []control.Workload{control.WorkloadIdle}}
	for _, body := range []string{statusBody, switchBody} {
		r := s.Handle(context.Background(), []byte(body))
		if r.Code != CodeOK || r.Status == nil {
			t.Fatalf("response: %#v", r)
		}
	}
	if b.calls != 2 || b.target != control.WorkloadMedia || b.initiator != "broker-1" || b.expected.Incarnation != "inc-1" || b.expected.Version != 2 {
		t.Fatalf("backend: %#v", b)
	}
	if len(a.events) != 4 || a.events[0].Stage != "admission" || a.events[1].Stage != "outcome" || a.events[0].CorrelationID != a.events[1].CorrelationID || a.events[0].CorrelationID == a.events[2].CorrelationID {
		t.Fatalf("audit: %#v", a.events)
	}
	for _, event := range a.events {
		if event.AuditID != "broker-1" || event.Timestamp.IsZero() || event.Outcome != CodeOK {
			t.Fatalf("audit: %#v", event)
		}
	}
	denied := s.Handle(context.Background(), []byte(strings.Replace(switchBody, "media", "text", 1)))
	if denied.Code != CodeForbidden || denied.Status != nil || b.calls != 2 {
		t.Fatalf("policy broadened: %#v", denied)
	}
}

func TestServiceRejectionsNeverCallBackendOrLeak(t *testing.T) {
	for _, scenario := range []string{"resolver-error", "missing-identity", "unknown-identity", "no-status", "no-workload", "invalid", "forged", "version", "oversize"} {
		t.Run(scenario, func(t *testing.T) {
			_, b, a, cfg := serviceFixture(t)
			body := statusBody
			want := CodeForbidden
			resolver := resolverFunc(func(context.Context) (string, error) { return "verified-secret", nil })
			switch scenario {
			case "resolver-error":
				resolver = func(context.Context) (string, error) { return "credential-secret", errors.New("credential-secret") }
				want = CodeUnauthenticated
			case "missing-identity":
				resolver = func(context.Context) (string, error) { return "", nil }
				want = CodeUnauthenticated
			case "unknown-identity":
				resolver = func(context.Context) (string, error) { return "credential-secret", nil }
			case "no-status":
				cfg.Grants["verified-secret"] = Grant{AuditID: "broker-1"}
			case "no-workload":
				body = strings.Replace(switchBody, "media", "text", 1)
			case "invalid":
				body = `{"apiVersion":"v1","operation":"credential-secret"}`
				want = CodeInvalidRequest
			case "forged":
				body = `{"apiVersion":"v1","operation":"status","identity":"credential-secret"}`
				want = CodeInvalidRequest
			case "version":
				body = `{"apiVersion":"credential-secret","operation":"status"}`
				want = CodeUnsupportedVersion
			case "oversize":
				body = strings.Repeat("credential-secret", 4096)
				want = CodeInvalidRequest
			}
			s, err := New(b, resolver, a, cfg)
			if err != nil {
				t.Fatal(err)
			}
			r := s.Handle(context.Background(), []byte(body))
			if r.Code != want || r.Status != nil || b.calls != 0 {
				t.Fatalf("rejection: %#v, calls %d", r, b.calls)
			}
			data, _ := json.Marshal(struct {
				Response Response
				Events   []AuditEvent
			}{r, a.events})
			if strings.Contains(string(data), "secret") {
				t.Fatalf("leak: %s", data)
			}
			if len(a.events) != 1 || a.events[0].Stage != "outcome" {
				t.Fatalf("audit: %#v", a.events)
			}
		})
	}
}

func TestServiceErrorMappingAndRedaction(t *testing.T) {
	cases := []struct {
		err  error
		code Code
	}{{supervisor.ErrUserOwned, CodeUserOwned}, {store.ErrUserOwned, CodeUserOwned}, {store.ErrVersionConflict, CodeStaleState}, {store.ErrStaleFence, CodeStaleState}, {supervisor.ErrTransitionRunning, CodeBusy}, {store.ErrTransitionRunning, CodeBusy}, {supervisor.ErrRecoveryRequired, CodeRecoveryRequired}, {supervisor.ErrReconcileRequired, CodeRecoveryRequired}, {store.ErrRecoveryRequired, CodeRecoveryRequired}, {context.DeadlineExceeded, CodeTimeout}, {supervisor.ErrDrainTimeout, CodeTimeout}, {supervisor.ErrVerifyTimeout, CodeTimeout}, {context.Canceled, CodeCanceled}, {errors.New("backend-secret"), CodeUnavailable}}
	for _, tc := range cases {
		t.Run(string(tc.code)+tc.err.Error(), func(t *testing.T) {
			s, b, a, _ := serviceFixture(t)
			b.err = errors.Join(tc.err, errors.New("backend-secret"))
			r := s.Handle(context.Background(), []byte(switchBody))
			if r.Code != tc.code || r.Status != nil {
				t.Fatalf("mapping: %#v", r)
			}
			data, _ := json.Marshal(struct {
				Response Response
				Events   []AuditEvent
			}{r, a.events})
			if strings.Contains(string(data), "secret") {
				t.Fatalf("leak: %s", data)
			}
		})
	}
}

func TestServiceAuditFailureBeforeAndAfterOperation(t *testing.T) {
	for _, failAt := range []int{1, 2} {
		t.Run(string(rune('0'+failAt)), func(t *testing.T) {
			s, b, a, _ := serviceFixture(t)
			a.failAt = failAt
			r := s.Handle(context.Background(), []byte(switchBody))
			if r.Code != CodeUnavailable || r.Status != nil || b.calls != failAt-1 {
				t.Fatalf("audit failure: %#v calls=%d", r, b.calls)
			}
		})
	}
	s, b, a, _ := serviceFixture(t)
	a.failAt = 1
	r := s.Handle(context.Background(), []byte(`{"apiVersion":"v1","operation":"secret"}`))
	if r.Code != CodeUnavailable || b.calls != 0 {
		t.Fatalf("rejected audit failure: %#v", r)
	}
}

func TestServiceUsesExactCLIGateForStatusAndSwitch(t *testing.T) {
	for _, body := range []string{statusBody, switchBody} {
		t.Run(body, func(t *testing.T) {
			s, b, _, _ := serviceFixture(t)
			gate, err := lock.TryAcquire(b.path + ".lock")
			if err != nil {
				t.Fatal(err)
			}
			r := s.Handle(context.Background(), []byte(body))
			gate.Close()
			if r.Code != CodeBusy || b.calls != 0 {
				t.Fatalf("gate: %#v calls=%d", r, b.calls)
			}
		})
	}
}

func TestServiceHoldsGateUntilCanceledBackendCleanupReturns(t *testing.T) {
	for _, body := range []string{statusBody, switchBody} {
		t.Run(body, func(t *testing.T) {
			s, b, _, _ := serviceFixture(t)
			entered := make(chan struct{})
			cleanup := make(chan struct{})
			release := make(chan struct{})
			returned := make(chan Response, 1)
			b.err = context.Canceled
			b.execute = func(ctx context.Context) { close(entered); <-ctx.Done(); close(cleanup); <-release }
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go func() { returned <- s.Handle(ctx, []byte(body)) }()
			<-entered
			if r := s.Handle(context.Background(), []byte(switchBody)); r.Code != CodeBusy {
				t.Fatalf("concurrent: %#v", r)
			}
			cancel()
			<-cleanup
			select {
			case r := <-returned:
				t.Fatalf("returned before cleanup: %#v", r)
			default:
			}
			gate, err := lock.TryAcquire(b.path + ".lock")
			if err == nil {
				gate.Close()
				t.Fatal("gate released during cleanup")
			}
			close(release)
			r := <-returned
			if r.Code != CodeCanceled || b.calls != 1 {
				t.Fatalf("cancellation: %#v calls=%d", r, b.calls)
			}
			gate, err = lock.TryAcquire(b.path + ".lock")
			if err != nil {
				t.Fatal(err)
			}
			gate.Close()
		})
	}
}

func TestServiceCanceledAndUnavailableGateHaveNoBackendEffects(t *testing.T) {
	s, b, _, _ := serviceFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := s.Handle(ctx, []byte(switchBody))
	if r.Code != CodeCanceled || b.calls != 0 {
		t.Fatalf("canceled: %#v", r)
	}
	if err := os.Mkdir(b.path+".lock", 0700); err != nil {
		t.Fatal(err)
	}
	r = s.Handle(context.Background(), []byte(statusBody))
	if r.Code != CodeUnavailable || b.calls != 0 {
		t.Fatalf("gate error: %#v", r)
	}
}

func TestServiceConstructorFailsClosed(t *testing.T) {
	for _, scenario := range []string{"backend", "resolver", "audit", "typed-nil", "empty-path", "relative-path", "timeout-zero", "timeout-cap", "empty-principal", "audit-id", "anonymous-id", "workload"} {
		t.Run(scenario, func(t *testing.T) {
			_, b, a, cfg := serviceFixture(t)
			var backend Backend = b
			var sink AuditSink = a
			var resolver Resolver = resolverFunc(func(context.Context) (string, error) { return "verified-secret", nil })
			switch scenario {
			case "backend":
				backend = nil
			case "resolver":
				resolver = nil
			case "audit":
				sink = nil
			case "typed-nil":
				var nilBackend *testBackend
				backend = nilBackend
			case "empty-path":
				b.path = ""
			case "relative-path":
				b.path = "state.db"
			case "timeout-zero":
				cfg.Timeout = 0
			case "timeout-cap":
				cfg.Timeout = MaxTimeout + time.Second
			case "empty-principal":
				cfg.Grants[""] = Grant{AuditID: "broker-2"}
			case "audit-id":
				cfg.Grants["verified-secret"] = Grant{AuditID: "path/secret"}
			case "anonymous-id":
				cfg.Grants["verified-secret"] = Grant{AuditID: "anonymous"}
			case "workload":
				cfg.Grants["verified-secret"] = Grant{AuditID: "broker-1", Workloads: []control.Workload{control.WorkloadUnknown}}
			}
			if _, err := New(backend, resolver, sink, cfg); err == nil {
				t.Fatal("accepted invalid configuration")
			}
		})
	}
}

func TestSafeRuntimeCauseMapsToTimeout(t *testing.T) {
	for _, category := range []error{supervisor.ErrRuntimeObservation, supervisor.ErrHealthCheck, gpuruntime.ErrCapacity} {
		err := gpuruntime.SafeError("probe failed", category, context.DeadlineExceeded)
		if got := errorCode(err); got != CodeTimeout {
			t.Fatalf("%v mapped to %v", category, got)
		}
	}
}

func TestNewWorkloadRequiresExplicitAutomationGrant(t *testing.T) {
	s, b, a, cfg := serviceFixture(t)
	body := strings.Replace(switchBody, "media", "speech", 1)
	denied := s.Handle(context.Background(), []byte(body))
	if denied.Code != CodeForbidden || b.calls != 0 {
		t.Fatalf("new workload inherited legacy grant: %+v calls %d", denied, b.calls)
	}
	cfg.Grants["verified-secret"] = Grant{AuditID: "broker-1", Workloads: []control.Workload{"speech"}}
	explicit, err := New(b, resolverFunc(func(context.Context) (string, error) { return "verified-secret", nil }), a, cfg)
	if err != nil {
		t.Fatal(err)
	}
	allowed := explicit.Handle(context.Background(), []byte(body))
	if allowed.Code != CodeOK || b.target != "speech" {
		t.Fatalf("explicit grant failed: %+v", allowed)
	}
}

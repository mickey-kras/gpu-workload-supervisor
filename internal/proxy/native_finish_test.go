package proxy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

func drainNative(t *testing.T, h *Handler) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := h.DrainNativeCompletions(ctx); err != nil {
		t.Fatal(err)
	}
}

func awaitNative(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("native completion did not reach barrier")
	}
}

type nativeFinishStore struct {
	StateStore
	finish func(context.Context, string, control.Workload, control.Fence, string, store.WorkOutcome) error
}

func (s nativeFinishStore) FinishWorkToken(ctx context.Context, id string, workload control.Workload, fence control.Fence, token string, outcome store.WorkOutcome) error {
	return s.finish(ctx, id, workload, fence, token, outcome)
}

func TestNativeFinishRetriesImmutableProof(t *testing.T) {
	transient := errors.New("sensitive database path/token/request-id must not escape")
	cases := []struct {
		name    string
		errors  []error
		reasons []string
	}{
		{"success", []error{nil}, nil},
		{"transient recovery", []error{transient, nil}, []string{"retrying"}},
		{"uncertain commit", []error{transient, store.ErrWorkAlreadyCompleted}, []string{"retrying"}},
		{"persistent failure", []error{transient, transient, transient}, []string{"retrying", "retrying", "exhausted"}},
		{"stale fence", []error{store.ErrStaleFence}, []string{"rejected"}},
		{"wrong workload", []error{store.ErrWorkloadMismatch}, []string{"rejected"}},
		{"reused request id", []error{store.ErrRegistrationTokenMismatch}, []string{"rejected"}},
		{"retained row removed", []error{sql.ErrNoRows}, []string{"rejected"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			fence := admittedState(control.OwnerSupervisor).LeaseFence
			start := time.Now()
			h := &Handler{workload: "media", store: nativeFinishStore{finish: func(ctx context.Context, id string, workload control.Workload, gotFence control.Fence, token string, outcome store.WorkOutcome) error {
				if id != "immutable-id" || workload != "media" || gotFence != fence || token != "immutable-token" || outcome != store.WorkCompleted {
					t.Error("retry changed the verified completion tuple")
				}
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > observedFinishAttemptTimeout || deadline.After(start.Add(observedFinishTimeout)) {
					t.Error("attempt escaped its bounded lifetime")
				}
				calls++
				if calls > len(tc.errors) {
					t.Fatal("unexpected retry")
				}
				return tc.errors[calls-1]
			}}}
			var reasons []string
			h.finishObserved(context.Background(), "immutable-id", fence, "immutable-token", func(f NativeCompletionFailure) {
				reasons = append(reasons, f.Reason)
				if f.Attempts != len(reasons) {
					t.Error("incorrect sanitized attempt count")
				}
			})
			if calls != len(tc.errors) || !reflect.DeepEqual(reasons, tc.reasons) {
				t.Fatalf("calls=%d reasons=%v, want %v", calls, reasons, tc.reasons)
			}
		})
	}
}

func TestNativeFinishCancellationAndAttemptTimeout(t *testing.T) {
	for _, phase := range []string{"before attempt", "in write", "backoff", "committed during cancellation", "attempt timeout"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if phase == "before attempt" {
				cancel()
			}
			calls := 0
			var failures []NativeCompletionFailure
			h := &Handler{store: nativeFinishStore{finish: func(attempt context.Context, _ string, _ control.Workload, _ control.Fence, _ string, _ store.WorkOutcome) error {
				calls++
				switch phase {
				case "in write":
					cancel()
					return attempt.Err()
				case "committed during cancellation":
					cancel()
					return nil
				case "attempt timeout":
					if calls == 1 {
						<-attempt.Done()
						return attempt.Err()
					}
					return nil
				default:
					return errors.New("temporary")
				}
			}}}
			h.finishObserved(ctx, "id", control.Fence{}, "token", func(f NativeCompletionFailure) {
				failures = append(failures, f)
				if phase == "backoff" {
					cancel()
				}
			})
			switch phase {
			case "before attempt":
				if calls != 0 || len(failures) != 1 || failures[0] != (NativeCompletionFailure{"canceled", 0}) {
					t.Fatalf("calls=%d failures=%v", calls, failures)
				}
			case "committed during cancellation":
				if calls != 1 || len(failures) != 0 {
					t.Fatalf("successful commit reported unresolved: %v", failures)
				}
			case "attempt timeout":
				if calls != 2 || len(failures) != 1 || failures[0].Reason != "retrying" {
					t.Fatalf("per-attempt timeout prevented recovery: %d %v", calls, failures)
				}
			default:
				if calls != 1 || failures[len(failures)-1].Reason != "canceled" {
					t.Fatalf("canceled write retried: %d %v", calls, failures)
				}
			}
		})
	}
}

type nativeRoundTrip func(*http.Request) (*http.Response, error)

func (f nativeRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func nativeRequest(fence control.Fence) *http.Request {
	r := httptest.NewRequest("POST", "/api/generate", strings.NewReader(`{"model":"selected"}`))
	r.Header.Set(DefaultRequestIDHeader, "request-1")
	addLeaseHeaders(r, fence)
	return r
}

func TestNativePersistenceDoesNotBlockForwardingAndBoundsAdmission(t *testing.T) {
	f := newTerminalFixture(t, "ollama", "/api/generate", "application/x-ndjson", "{\"done\":true}\n")
	started, release := make(chan struct{}), make(chan struct{})
	var unblock sync.Once
	defer unblock.Do(func() { close(release) })
	f.handler.nativeFinishes.limit = 1
	var upstreamCalls atomic.Int32
	original := f.handler.proxy.Transport
	f.handler.proxy.Transport = nativeRoundTrip(func(r *http.Request) (*http.Response, error) {
		upstreamCalls.Add(1)
		return original.RoundTrip(r)
	})
	f.handler.store = nativeFinishStore{StateStore: f.store, finish: func(ctx context.Context, id string, w control.Workload, fence control.Fence, token string, outcome store.WorkOutcome) error {
		close(started)
		<-release
		return f.store.FinishWorkToken(ctx, id, w, fence, token, outcome)
	}}
	request := nativeRequest(f.store.state.LeaseFence)
	ctx, disconnect := context.WithCancel(request.Context())
	defer disconnect()
	response := httptest.NewRecorder()
	forwarded := make(chan struct{})
	go func() { f.handler.ServeHTTP(response, request.WithContext(ctx)); close(forwarded) }()
	awaitNative(t, started)
	awaitNative(t, forwarded)
	if response.Body.String() != "{\"done\":true}\n" {
		t.Fatal("finish contention delayed or damaged terminal bytes")
	}
	disconnect() // Verified evidence outlives the downstream request.
	full := httptest.NewRecorder()
	f.handler.ServeHTTP(full, nativeRequest(f.store.state.LeaseFence))
	if full.Code != http.StatusServiceUnavailable || full.Header().Get("Retry-After") != "1" || len(f.store.admitted) != 1 || upstreamCalls.Load() != 1 {
		t.Fatalf("saturation dispatched or admitted execution: status=%d", full.Code)
	}
	unblock.Do(func() { close(release) })
	drainNative(t, f.handler)
	if len(f.store.finished) != 1 || f.handler.nativeFinishes.reserved != 0 {
		t.Fatal("disconnected request lost its proof or reservation")
	}
}

func TestNativeReservationReleasedWithoutProof(t *testing.T) {
	for _, failure := range []string{"admission", "transport", "unsupported response", "no proof"} {
		t.Run(failure, func(t *testing.T) {
			f := newTerminalFixture(t, "ollama", "/api/generate", "application/x-ndjson", "{\"done\":false}\n")
			f.handler.nativeFinishes.limit = 1
			switch failure {
			case "admission":
				f.store.admitErr = store.ErrAdmissionClosed
			case "transport":
				f.handler.proxy.Transport = nativeRoundTrip(func(*http.Request) (*http.Response, error) { return nil, errors.New("transport failed") })
			case "unsupported response":
				f.handler.proxy.Transport = nativeRoundTrip(func(*http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: 503, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("{\"done\":true}\n"))}, nil
				})
			}
			for range 2 {
				response := httptest.NewRecorder()
				f.handler.ServeHTTP(response, nativeRequest(f.store.state.LeaseFence))
				if strings.Contains(response.Body.String(), "proxy_capacity_exceeded") {
					t.Fatal("untransferred reservation leaked")
				}
			}
			drainNative(t, f.handler)
			if len(f.store.finished) != 0 || f.handler.nativeFinishes.reserved != 0 {
				t.Fatal("failure inferred completion or leaked capacity")
			}
		})
	}
}

// An upstream can leave a stream open after the terminal event. Persistence
// frees its proof slot; Server still accounts for that open HTTP producer.
func TestNativePostTerminalStreamRemainsUnderServerCap(t *testing.T) {
	proof, release := make(chan struct{}), make(chan struct{})
	var unblock sync.Once
	defer unblock.Do(func() { close(release) })
	f := newTerminalFixtureWith(t, "ollama", "/api/generate", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprint(w, "{\"done\":true}\n")
		w.(http.Flusher).Flush()
		<-release
	}))
	f.handler.nativeFinishes.limit = 1
	f.handler.store = nativeFinishStore{StateStore: f.store, finish: func(context.Context, string, control.Workload, control.Fence, string, store.WorkOutcome) error {
		close(proof)
		return nil
	}}
	tracked := NewServer(f.handler, 1, 1, "")
	done := make(chan struct{})
	go func() {
		tracked.ServeHTTP(httptest.NewRecorder(), nativeRequest(f.store.state.LeaseFence))
		close(done)
	}()
	awaitNative(t, proof)
	// Drain joins the proof worker while the response is demonstrably open.
	drainNative(t, f.handler)
	select {
	case <-done:
		t.Fatal("stream unexpectedly closed at proof")
	default:
	}
	if f.handler.nativeFinishes.reserved != 0 {
		t.Fatal("persistence retained its slot after completion")
	}
	response := httptest.NewRecorder()
	tracked.ServeHTTP(response, httptest.NewRequest("GET", "/unclassified", nil))
	if response.Code != 503 || !strings.Contains(response.Body.String(), "proxy_capacity_exceeded") {
		t.Fatal("post-terminal stream escaped the ordinary HTTP cap")
	}
	unblock.Do(func() { close(release) })
	awaitNative(t, done)
}

type terminalErrorBody struct {
	closed chan struct{}
	once   sync.Once
	data   string
}

func (b *terminalErrorBody) Read(p []byte) (int, error) {
	<-b.closed
	return copy(p, b.data), io.ErrUnexpectedEOF
}
func (b *terminalErrorBody) Close() error { b.once.Do(func() { close(b.closed) }); return nil }

func TestNativeProofBytesSurviveConcurrentCloseAndReadError(t *testing.T) {
	for _, body := range []string{"{\"done\":true}\n", "{\"done\":false}\n", "not json\n{\"done\":true}\n"} {
		t.Run(body, func(t *testing.T) {
			f := newTerminalFixture(t, "ollama", "/api/generate", "application/json", "")
			r := f.handler.nativeFinishes.reserve()
			response := &http.Response{StatusCode: 200, Header: make(http.Header), Body: &terminalErrorBody{closed: make(chan struct{}), data: body}}
			f.handler.observeNativeTerminal(response, "/api/generate", "request-1", f.store.state.LeaseFence, "test-registration-token", r)
			done := make(chan struct{})
			go func() {
				defer close(done)
				buffer := make([]byte, 100)
				for range 2 {
					if n, err := response.Body.Read(buffer); n != len(body) || err != io.ErrUnexpectedEOF {
						t.Error("observer changed upstream read result")
					}
				}
			}()
			_ = response.Body.Close()
			_ = response.Body.Close()
			awaitNative(t, done)
			r.release()
			drainNative(t, f.handler)
			want := 0
			if body == "{\"done\":true}\n" {
				want = 1
			}
			if len(f.store.finished) != want {
				t.Fatalf("finished=%d want=%d", len(f.store.finished), want)
			}
		})
	}
}

func TestNativeDrainSealsCancelsAndJoins(t *testing.T) {
	f := newTerminalFixture(t, "ollama", "/api/generate", "application/json", "")
	started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var unblock sync.Once
	defer unblock.Do(func() { close(release) })
	var calls atomic.Int32
	f.handler.store = nativeFinishStore{finish: func(ctx context.Context, _ string, _ control.Workload, _ control.Fence, _ string, _ store.WorkOutcome) error {
		calls.Add(1)
		close(started)
		<-ctx.Done()
		close(canceled)
		<-release // Model SQLite's non-interruptible busy handler.
		return ctx.Err()
	}}
	job := f.handler.nativeFinishes.reserve()
	late := f.handler.nativeFinishes.reserve()
	job.submit(f.handler, "id", control.Fence{}, "token")
	awaitNative(t, started)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	drained := make(chan struct{})
	go func() {
		if !errors.Is(f.handler.DrainNativeCompletions(ctx), context.Canceled) {
			t.Error("drain did not report canceled budget")
		}
		close(drained)
	}()
	awaitNative(t, canceled)
	late.submit(f.handler, "late", control.Fence{}, "token")
	late.release()
	if next := f.handler.nativeFinishes.reserve(); next != nil {
		t.Fatal("sealed handler accepted work")
	}
	select {
	case <-drained:
		t.Fatal("drain returned while a write still owned the store")
	default:
	}
	unblock.Do(func() { close(release) })
	awaitNative(t, drained)
	job.submit(f.handler, "duplicate", control.Fence{}, "token")
	job.release()
	drainNative(t, f.handler)
	if calls.Load() != 1 || f.handler.nativeFinishes.reserved != 0 {
		t.Fatal("late proof wrote after drain or leaked capacity")
	}
	if err := (&Handler{}).DrainNativeCompletions(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestNativeReportsOutsideLifecycleLocks(t *testing.T) {
	f := newTerminalFixture(t, "ollama", "/api/generate", "application/json", "")
	reporting, release := make(chan struct{}), make(chan struct{})
	var unblock sync.Once
	defer unblock.Do(func() { close(release) })
	f.handler.nativeFinishes.report = func(failure NativeCompletionFailure) {
		if failure != (NativeCompletionFailure{"rejected", 1}) {
			t.Error("unsanitized failure report")
		}
		close(reporting)
		<-release
	}
	f.handler.store = nativeFinishStore{finish: func(context.Context, string, control.Workload, control.Fence, string, store.WorkOutcome) error {
		return fmt.Errorf("private database path: %w", store.ErrStaleFence)
	}}
	r := f.handler.nativeFinishes.reserve()
	r.submit(f.handler, "private-id", control.Fence{}, "private-token")
	awaitNative(t, reporting)
	// A slow reporter may delay its worker, but never owns the lifecycle lock.
	freed := make(chan struct{})
	go func() { next := f.handler.nativeFinishes.reserve(); next.release(); close(freed) }()
	awaitNative(t, freed)
	unblock.Do(func() { close(release) })
	drainNative(t, f.handler)
}

func TestNativeCompletionLimitConfiguration(t *testing.T) {
	u, _ := url.Parse("http://127.0.0.1:1234")
	if _, err := NewWithContext(context.Background(), &fakeStore{}, Config{Upstream: u, MaxNativeCompletions: -1}); err == nil {
		t.Fatal("negative limit accepted")
	}
	f := newNativeFinishes(2, nil)
	defer f.cancel()
	first, second := f.reserve(), f.reserve()
	if first == nil || second == nil || f.reserve() != nil {
		t.Fatal("configured cap not enforced")
	}
	first.release()
	first.release()
	first.submit(&Handler{}, "released", control.Fence{}, "token")
	third := f.reserve()
	if third == nil || f.reserve() != nil {
		t.Fatal("released slot was reused more than once")
	}
	second.release()
	third.release()
}

func TestNativePersistenceRecoversFromRealSQLiteContention(t *testing.T) {
	ctx := context.Background()
	durable, state, path := nativeDurableFixture(t)
	token, err := durable.AdmitWorkToken(ctx, "contended", "", control.WorkloadText, state.LeaseFence)
	if err != nil {
		t.Fatal(err)
	}
	holder, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	tx, err := holder.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec("UPDATE control_state SET version = version WHERE singleton = 1"); err != nil {
		t.Fatal(err)
	}
	failed := make(chan struct{})
	var once sync.Once
	h := &Handler{nativeModel: &control.NativeModel{}, store: durable, workload: control.WorkloadText}
	h.nativeFinishes = newNativeFinishes(1, func(f NativeCompletionFailure) {
		if f.Reason == "retrying" {
			once.Do(func() { close(failed) })
		}
	})
	defer func() { _ = tx.Rollback(); drainNative(t, h) }()
	r := h.nativeFinishes.reserve()
	response := &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("{\"done\":true}\n"))}
	h.observeNativeTerminal(response, "/api/generate", "contended", state.LeaseFence, token, r)
	start := time.Now()
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	r.release()
	if err != nil || string(body) != "{\"done\":true}\n" || time.Since(start) > time.Second {
		t.Fatal("SQLite contention blocked terminal forwarding")
	}
	// Keep the writer through the first real busy timeout. Cancellation cannot
	// interrupt modernc's BEGIN busy handler; its canceled attempt must not commit.
	select {
	case <-failed:
	case <-time.After(7 * time.Second):
		t.Fatal("SQLite contention did not produce a bounded retry")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	drainNative(t, h)
	if err := durable.FinishWorkToken(ctx, "contended", control.WorkloadText, state.LeaseFence, token, store.WorkCompleted); !errors.Is(err, store.ErrWorkAlreadyCompleted) {
		t.Fatalf("verified completion was not persisted after contention: %v", err)
	}
}

func nativeDurableFixture(t *testing.T) (*store.Store, control.State, string) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	durable, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = durable.Close() })
	state, err := durable.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	state.Phase, state.Admission = control.PhaseStable, control.AdmissionOpen
	state.ActiveWorkload, state.DesiredWorkload = control.WorkloadText, control.WorkloadText
	state, err = durable.UpdateState(ctx, state.Version, state)
	if err != nil {
		t.Fatal(err)
	}
	return durable, state, path
}

func TestNativeRetryPreservesCompetingDurableOutcomes(t *testing.T) {
	for _, race := range []string{"callback abandoned", "uncertain commit", "request ID reused"} {
		t.Run(race, func(t *testing.T) {
			ctx := context.Background()
			durable, state, path := nativeDurableFixture(t)
			token, err := durable.AdmitWorkToken(ctx, "request", "", control.WorkloadText, state.LeaseFence)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			h := &Handler{workload: control.WorkloadText, store: nativeFinishStore{finish: func(attempt context.Context, id string, workload control.Workload, fence control.Fence, gotToken string, outcome store.WorkOutcome) error {
				calls++
				if calls == 1 {
					if race == "uncertain commit" {
						if err := durable.FinishWorkToken(attempt, id, workload, fence, gotToken, outcome); err != nil {
							t.Error(err)
						}
					}
					return errors.New("lost write acknowledgment")
				}
				return durable.FinishWorkToken(attempt, id, workload, fence, gotToken, outcome)
			}}}
			var reports []NativeCompletionFailure
			h.finishObserved(ctx, "request", state.LeaseFence, token, func(f NativeCompletionFailure) {
				reports = append(reports, f)
				if f.Reason != "retrying" || race == "uncertain commit" {
					return
				}
				if err := durable.FinishWorkToken(ctx, "request", control.WorkloadText, state.LeaseFence, token, store.WorkAbandoned); err != nil {
					t.Fatal(err)
				}
				if race == "request ID reused" {
					if count, err := durable.PruneCompletedWork(ctx, time.Now().Add(time.Hour), 1); err != nil || count != 1 {
						t.Fatalf("prune: %d %v", count, err)
					}
					newToken, err := durable.AdmitWorkToken(ctx, "request", "", control.WorkloadText, state.LeaseFence)
					if err != nil || newToken == token {
						t.Fatalf("re-registration: %v", err)
					}
				}
			})
			if calls != 2 {
				t.Fatalf("calls=%d", calls)
			}
			reader, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			var outcome sql.NullString
			if err := reader.QueryRow("SELECT completion_outcome FROM registered_work WHERE request_id = 'request'").Scan(&outcome); err != nil {
				t.Fatal(err)
			}
			switch race {
			case "callback abandoned":
				if outcome.String != string(store.WorkAbandoned) || len(reports) != 1 {
					t.Fatalf("callback overwritten: %v %v", outcome, reports)
				}
			case "uncertain commit":
				if outcome.String != string(store.WorkCompleted) || len(reports) != 1 {
					t.Fatalf("uncertain commit not idempotent: %v %v", outcome, reports)
				}
			case "request ID reused":
				if outcome.Valid || len(reports) != 2 || reports[1].Reason != "rejected" {
					t.Fatalf("old proof finished new registration: %v %v", outcome, reports)
				}
			}
		})
	}
}

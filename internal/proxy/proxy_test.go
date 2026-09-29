package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

type fakeStore struct {
	mu        sync.Mutex
	state     control.State
	admitErr  error
	finishErr error
	admitted  []string
	finished  []string
	outcomes  []store.WorkOutcome
}

func (f *fakeStore) State(context.Context) (control.State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state, nil
}

func (f *fakeStore) AdmitWorkToken(_ context.Context, requestID, _ string, workload control.Workload, fence control.Fence) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.admitErr != nil {
		return "", f.admitErr
	}
	if fence != f.state.LeaseFence {
		return "", store.ErrStaleFence
	}
	if f.state.Admission != control.AdmissionOpen || f.state.Phase != control.PhaseStable || f.state.Health != control.HealthHealthy {
		return "", store.ErrAdmissionClosed
	}
	if f.state.ActiveWorkload != workload || f.state.DesiredWorkload != workload {
		return "", store.ErrWorkloadMismatch
	}
	f.admitted = append(f.admitted, requestID)
	return "test-registration-token", nil
}

func (f *fakeStore) FinishWorkToken(_ context.Context, requestID string, _ control.Workload, _ control.Fence, token string, outcome store.WorkOutcome) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.finishErr != nil {
		return f.finishErr
	}
	if token != "test-registration-token" {
		return store.ErrRegistrationTokenMismatch
	}
	f.finished = append(f.finished, requestID)
	f.outcomes = append(f.outcomes, outcome)
	return nil
}

func TestSupervisorExecutionStaysRegisteredUntilExplicitFinish(t *testing.T) {
	var receivedControlHeader bool
	var receivedToken string
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		receivedControlHeader = request.Header.Get(DefaultFenceIDHeader) != ""
		receivedToken = request.Header.Get(DefaultRegistrationTokenHeader)
		response.Header().Set(DefaultRegistrationTokenHeader, "upstream-spoof")
		response.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(response, "accepted")
	}))
	defer upstream.Close()
	stateStore := &fakeStore{state: admittedState(control.OwnerSupervisor)}
	handler := testHandler(t, stateStore, upstream.URL)
	request := httptest.NewRequest(http.MethodPost, "http://proxy.test/execute", strings.NewReader("{}"))
	addLeaseHeaders(request, stateStore.state.LeaseFence)
	request.Header.Set(DefaultRegistrationTokenHeader, "caller-spoof")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusAccepted || response.Body.String() != "accepted" {
		t.Fatalf("response = %d %q", response.Code, response.Body.String())
	}
	if tokens := response.Header().Values(DefaultRegistrationTokenHeader); len(tokens) != 1 || tokens[0] != "test-registration-token" {
		t.Fatalf("response registration tokens = %#v", tokens)
	}
	if receivedToken != "test-registration-token" {
		t.Fatalf("upstream registration token = %q", receivedToken)
	}
	if receivedControlHeader {
		t.Fatal("control headers reached upstream")
	}
	if len(stateStore.admitted) != 1 || stateStore.admitted[0] != "request-1" {
		t.Fatalf("admitted = %#v", stateStore.admitted)
	}
	if len(stateStore.finished) != 0 {
		t.Fatalf("finished before terminal signal = %#v", stateStore.finished)
	}

	finish := httptest.NewRequest(http.MethodPost, "http://proxy.test"+DefaultCompletionPath,
		strings.NewReader(`{"requestId":"request-1","registrationToken":"test-registration-token","fence":{"incarnation":"11111111-1111-4111-8111-111111111111","epoch":7},"outcome":"completed"}`))
	finishResponse := httptest.NewRecorder()
	handler.ServeHTTP(finishResponse, finish)
	if finishResponse.Code != http.StatusNoContent {
		t.Fatalf("finish status = %d", finishResponse.Code)
	}
	if len(stateStore.finished) != 1 || stateStore.outcomes[0] != store.WorkCompleted {
		t.Fatalf("terminal lifecycle = %#v %#v", stateStore.finished, stateStore.outcomes)
	}
}

func TestUserOwnershipBypassesLeaseRegistration(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	stateStore := &fakeStore{state: admittedState(control.OwnerUser)}
	handler := testHandler(t, stateStore, upstream.URL)
	request := httptest.NewRequest(http.MethodPost, "http://proxy.test/execute", nil)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d", response.Code)
	}
	if len(stateStore.admitted) != 0 || len(stateStore.finished) != 0 {
		t.Fatalf("unexpected lifecycle: %#v %#v", stateStore.admitted, stateStore.finished)
	}
}

func TestReadOnlyRouteStripsControlHeaders(t *testing.T) {
	var receivedControlHeader bool
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		receivedControlHeader = request.Header.Get(DefaultFenceIDHeader) != ""
		response.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	stateStore := &fakeStore{state: admittedState(control.OwnerSupervisor)}
	handler := testHandler(t, stateStore, upstream.URL)
	request := httptest.NewRequest(http.MethodGet, "http://proxy.test/assets/item", nil)
	addLeaseHeaders(request, stateStore.state.LeaseFence)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	if receivedControlHeader {
		t.Fatal("control headers reached upstream")
	}
}

func TestUnclassifiedMutatingRoutesFailClosed(t *testing.T) {
	stateStore := &fakeStore{state: admittedState(control.OwnerSupervisor)}
	handler := testHandler(t, stateStore, "http://127.0.0.1:1")
	for _, target := range []string{"/execute/", "/other", "/a/../execute"} {
		request := httptest.NewRequest(http.MethodPost, "http://proxy.test"+target, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusMethodNotAllowed && response.Code != http.StatusBadRequest {
			t.Fatalf("%s status = %d", target, response.Code)
		}
	}
}

func TestExecutionFailsClosed(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*fakeStore, *http.Request)
		wantStatus int
	}{
		{name: "missing request id", mutate: func(_ *fakeStore, request *http.Request) {
			addLeaseHeaders(request, admittedState(control.OwnerSupervisor).LeaseFence)
			request.Header.Del(DefaultRequestIDHeader)
		}, wantStatus: http.StatusBadRequest},
		{name: "stale fence", mutate: func(_ *fakeStore, request *http.Request) {
			request.Header.Set(DefaultRequestIDHeader, "request-1")
			request.Header.Set(DefaultFenceIDHeader, "stale")
			request.Header.Set(DefaultFenceEpochHeader, "1")
		}, wantStatus: http.StatusConflict},
		{name: "closed admission", mutate: func(stateStore *fakeStore, request *http.Request) {
			stateStore.state.Admission = control.AdmissionClosed
			addLeaseHeaders(request, stateStore.state.LeaseFence)
		}, wantStatus: http.StatusServiceUnavailable},
		{name: "wrong workload", mutate: func(stateStore *fakeStore, request *http.Request) {
			stateStore.state.ActiveWorkload = control.WorkloadText
			stateStore.state.DesiredWorkload = control.WorkloadText
			addLeaseHeaders(request, stateStore.state.LeaseFence)
		}, wantStatus: http.StatusConflict},
		{name: "duplicate request", mutate: func(stateStore *fakeStore, request *http.Request) {
			stateStore.admitErr = store.ErrRequestConflict
			addLeaseHeaders(request, stateStore.state.LeaseFence)
		}, wantStatus: http.StatusConflict},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			upstreamCalls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				upstreamCalls++
				response.WriteHeader(http.StatusOK)
			}))
			defer upstream.Close()
			stateStore := &fakeStore{state: admittedState(control.OwnerSupervisor)}
			handler := testHandler(t, stateStore, upstream.URL)
			request := httptest.NewRequest(http.MethodPost, "http://proxy.test/execute", nil)
			request.Header.Set(DefaultRequestIDHeader, "request-1")
			test.mutate(stateStore, request)
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, request)

			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, test.wantStatus)
			}
			if upstreamCalls != 0 {
				t.Fatalf("upstream calls = %d", upstreamCalls)
			}
		})
	}
}

func TestUpstreamFailureLeavesWorkIncomplete(t *testing.T) {
	stateStore := &fakeStore{state: admittedState(control.OwnerSupervisor)}
	handler := testHandler(t, stateStore, "http://127.0.0.1:1")
	request := httptest.NewRequest(http.MethodPost, "http://proxy.test/execute", nil)
	addLeaseHeaders(request, stateStore.state.LeaseFence)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusBadGateway {
		t.Fatalf("status = %d", response.Code)
	}
	if len(stateStore.admitted) != 1 || len(stateStore.finished) != 0 {
		t.Fatalf("lifecycle = %#v %#v", stateStore.admitted, stateStore.finished)
	}
}

func TestFinishFailureIsFailClosed(t *testing.T) {
	stateStore := &fakeStore{
		state: admittedState(control.OwnerSupervisor), finishErr: errors.New("database unavailable"),
	}
	handler := testHandler(t, stateStore, "http://127.0.0.1:1")
	request := httptest.NewRequest(http.MethodPost, "http://proxy.test"+DefaultCompletionPath,
		strings.NewReader(`{"requestId":"request-1","fence":{"incarnation":"11111111-1111-4111-8111-111111111111","epoch":7},"outcome":"abandoned"}`))
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", response.Code)
	}
}

func testHandler(t *testing.T, stateStore StateStore, upstream string) *Handler {
	t.Helper()
	target, err := url.Parse(upstream)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := New(stateStore, Config{
		Upstream: target, Workload: control.WorkloadMedia,
		ExecutionRoutes: []Route{{Method: http.MethodPost, Path: "/execute"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func admittedState(owner control.Owner) control.State {
	return control.State{
		Owner: owner, DesiredWorkload: control.WorkloadMedia,
		ActiveWorkload: control.WorkloadMedia, Phase: control.PhaseStable,
		Health: control.HealthHealthy, Admission: control.AdmissionOpen,
		LeaseFence: control.Fence{Incarnation: "11111111-1111-4111-8111-111111111111", Epoch: 7},
		Version:    4, UpdatedAt: time.Date(2026, 9, 29, 7, 0, 0, 0, time.UTC),
	}
}

func addLeaseHeaders(request *http.Request, fence control.Fence) {
	request.Header.Set(DefaultRequestIDHeader, "request-1")
	request.Header.Set(DefaultFenceIDHeader, fence.Incarnation)
	request.Header.Set(DefaultFenceEpochHeader, "7")
}

func TestFinishRejectsTrailingJSON(t *testing.T) {
	stateStore := &fakeStore{state: admittedState(control.OwnerSupervisor)}
	handler := testHandler(t, stateStore, "http://127.0.0.1:1")
	request := httptest.NewRequest(http.MethodPost, "http://proxy.test"+DefaultCompletionPath,
		strings.NewReader(`{"requestId":"request-1","fence":{"incarnation":"11111111-1111-4111-8111-111111111111","epoch":7},"outcome":"completed"} {}`))
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", response.Code)
	}
}

func TestRouteCollisionsAreRejected(t *testing.T) {
	target, err := url.Parse("http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	tests := []Config{
		{
			Upstream: target, Workload: control.WorkloadMedia,
			ExecutionRoutes: []Route{{Method: http.MethodPost, Path: DefaultCompletionPath}},
		},
		{
			Upstream: target, Workload: control.WorkloadMedia,
			ExecutionRoutes:   []Route{{Method: http.MethodPost, Path: "/execute"}},
			PassthroughRoutes: []Route{{Method: http.MethodPost, Path: DefaultCompletionPath}},
		},
		{
			Upstream: target, Workload: control.WorkloadMedia,
			ExecutionRoutes:   []Route{{Method: http.MethodPost, Path: "/execute"}},
			PassthroughRoutes: []Route{{Method: http.MethodPost, Path: "/execute"}},
		},
	}
	for index, config := range tests {
		if _, err := New(&fakeStore{}, config); err == nil {
			t.Fatalf("case %d accepted route collision", index)
		}
	}
}

func TestRegistrationTokenProtectsReusedIDThroughHTTPAndStore(t *testing.T) {
	ctx := context.Background()
	stateStore, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer stateStore.Close()
	state, err := stateStore.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	state.DesiredWorkload = control.WorkloadMedia
	state.ActiveWorkload = control.WorkloadMedia
	state.Phase = control.PhaseStable
	state.Health = control.HealthHealthy
	state.Admission = control.AdmissionOpen
	state, err = stateStore.UpdateState(ctx, state.Version, state)
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusAccepted)
	}))
	defer upstream.Close()
	handler := testHandler(t, stateStore, upstream.URL)
	admit := func() string {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, "/execute", nil)
		request.Header.Set(DefaultRequestIDHeader, "reused-id")
		request.Header.Set(DefaultFenceIDHeader, state.LeaseFence.Incarnation)
		request.Header.Set(DefaultFenceEpochHeader, strconv.FormatUint(state.LeaseFence.Epoch, 10))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusAccepted {
			t.Fatalf("admission status = %d: %s", response.Code, response.Body.String())
		}
		token := response.Header().Get(DefaultRegistrationTokenHeader)
		if token == "" {
			t.Fatal("admission response missing registration token")
		}
		return token
	}
	finish := func(token string) int {
		t.Helper()
		body, err := json.Marshal(struct {
			RequestID         string            `json:"requestId"`
			RegistrationToken string            `json:"registrationToken"`
			Fence             control.Fence     `json:"fence"`
			Outcome           store.WorkOutcome `json:"outcome"`
		}{"reused-id", token, state.LeaseFence, store.WorkCompleted})
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, DefaultCompletionPath, strings.NewReader(string(body)))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response.Code
	}
	oldToken := admit()
	if status := finish(oldToken); status != http.StatusNoContent {
		t.Fatalf("first completion status = %d", status)
	}
	if count, err := stateStore.PruneCompletedWork(ctx, time.Now().Add(time.Hour), 256); err != nil || count != 1 {
		t.Fatalf("same-fence prune = %d, %v", count, err)
	}
	newToken := admit()
	if newToken == oldToken {
		t.Fatal("reused request ID received old registration token")
	}
	if status := finish(oldToken); status != http.StatusConflict {
		t.Fatalf("late old-token completion status = %d", status)
	}
	if status := finish(newToken); status != http.StatusNoContent {
		t.Fatalf("new-token completion status = %d", status)
	}
}

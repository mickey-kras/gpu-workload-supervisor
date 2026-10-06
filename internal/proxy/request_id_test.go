package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

func TestExecutionRejectsInvalidRequestIDBeforeRegistration(t *testing.T) {
	for _, tc := range []struct{ name, id, code string }{
		{"over boundary", strings.Repeat("x", 8193), "request_id_too_long"},
		{"original regression", strings.Repeat("x", 65536), "request_id_too_long"},
		{"byte not character bound", strings.Repeat("é", 4097), "request_id_too_long"},
		{"invalid UTF-8", "invalid\xff", "request_id_invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(http.StatusAccepted) }))
			defer upstream.Close()
			s := &fakeStore{state: admittedState(control.OwnerSupervisor)}
			h := testHandler(t, s, upstream.URL)
			request := httptest.NewRequest(http.MethodPost, "/execute", nil)
			addLeaseHeaders(request, s.state.LeaseFence)
			request.Header.Set(DefaultRequestIDHeader, tc.id)
			response := httptest.NewRecorder()
			h.ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), tc.code) {
				t.Errorf("response = %d %s", response.Code, response.Body.String())
			}
			if calls != 0 || len(s.admitted) != 0 {
				t.Fatalf("rejection side effects: upstream=%d registrations=%d", calls, len(s.admitted))
			}
		})
	}
}

func TestBoundaryRequestIDCompletesThroughHTTPAndStore(t *testing.T) {
	for _, tc := range []struct{ name, id string }{
		{"ASCII", strings.Repeat("x", 8192)},
		{"worst JSON escaping", strings.Repeat("<", 8192)},
		{"quotes and backslashes", strings.Repeat("\"\\", 4096)},
		{"UTF-8", strings.Repeat("é", 4096)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			state, err := s.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			state.DesiredWorkload, state.ActiveWorkload = control.WorkloadMedia, control.WorkloadMedia
			state.Phase, state.Health, state.Admission = control.PhaseStable, control.HealthHealthy, control.AdmissionOpen
			state, err = s.UpdateState(ctx, state.Version, state)
			if err != nil {
				t.Fatal(err)
			}
			var receivedID, token string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				receivedID, token = r.Header.Get(DefaultRequestIDHeader), r.Header.Get(DefaultRegistrationTokenHeader)
				w.WriteHeader(http.StatusAccepted)
			}))
			defer upstream.Close()
			h := testHandler(t, s, upstream.URL)
			request := httptest.NewRequest(http.MethodPost, "/execute", nil)
			request.Header.Set(DefaultRequestIDHeader, " "+tc.id+" ")
			request.Header.Set(DefaultFenceIDHeader, state.LeaseFence.Incarnation)
			request.Header.Set(DefaultFenceEpochHeader, strconv.FormatUint(state.LeaseFence.Epoch, 10))
			response := httptest.NewRecorder()
			h.ServeHTTP(response, request)
			if response.Code != http.StatusAccepted || receivedID != tc.id || token == "" {
				t.Fatalf("admission status=%d received ID bytes=%d token=%q", response.Code, len(receivedID), token)
			}
			if response.Header().Get(DefaultRegistrationTokenHeader) != token {
				t.Fatal("response token differs from upstream")
			}
			duplicate := httptest.NewRecorder()
			h.ServeHTTP(duplicate, request)
			if duplicate.Code != http.StatusConflict {
				t.Fatalf("duplicate status=%d", duplicate.Code)
			}
			finish := func(registrationToken string) int {
				body, err := json.Marshal(finishRequest{tc.id, registrationToken, state.LeaseFence, store.WorkCompleted})
				if err != nil {
					t.Fatal(err)
				}
				if len(body) >= 64<<10 {
					t.Fatalf("callback exceeds limit: %d bytes", len(body))
				}
				response := httptest.NewRecorder()
				h.ServeHTTP(response, httptest.NewRequest(http.MethodPost, DefaultCompletionPath, bytes.NewReader(body)))
				return response.Code
			}
			if got := finish("wrong-token"); got != http.StatusConflict {
				t.Fatalf("wrong token status=%d", got)
			}
			if got := finish(token); got != http.StatusNoContent {
				t.Fatalf("finish status=%d", got)
			}
			if got := finish(token); got != http.StatusNoContent {
				t.Fatalf("repeat finish status=%d", got)
			}
			if count, err := s.PruneCompletedWork(ctx, time.Now().Add(time.Hour), 1); err != nil || count != 1 {
				t.Fatalf("terminal row prune=%d, %v", count, err)
			}
		})
	}
}

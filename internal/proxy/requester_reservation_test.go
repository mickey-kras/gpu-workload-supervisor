package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

func reservationHTTP(t *testing.T, s StateStore, config Config) (*Handler, *httptest.Server) {
	t.Helper()
	h, err := NewWithContext(context.Background(), s, config)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	return h, server
}

func demandHTTPStatus(t *testing.T, server *httptest.Server, method, path string) (int, string) {
	t.Helper()
	r := requesterIntegrationRequest(t, server.URL, method, "reservation-request")
	r.URL.Path = path
	response, err := server.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, string(body)
}

func assertDemandReservation(t *testing.T, f *contractFixture, status string, work int) {
	t.Helper()
	var got string
	var registrations int
	if err := f.rows.QueryRow(`SELECT status FROM broker_demands WHERE request_id='reservation-request'`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if err := f.rows.QueryRow(`SELECT COUNT(*) FROM registered_work WHERE request_id='reservation-request'`).Scan(&registrations); err != nil {
		t.Fatal(err)
	}
	if got != status || registrations != work {
		t.Fatalf("demand=%s work=%d want %s/%d", got, registrations, status, work)
	}
}

func TestRequesterNativeCapacityRequeuesBeforeRetry(t *testing.T) {
	f := newContractFixture(t)
	var calls atomic.Int32
	runtime := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); w.WriteHeader(202) }))
	defer runtime.Close()
	c := requesterConfig()
	c.Upstream, _ = url.Parse(runtime.URL)
	h, server := reservationHTTP(t, f.store, c)
	// Occupy the actual native completion admission pool; no runtime admission
	// is allowed while its single persistence slot is reserved.
	h.nativeFinishes = newNativeFinishes(1, nil)
	occupied := h.nativeFinishes.reserve()
	if occupied == nil {
		t.Fatal("missing capacity reservation")
	}
	if status, body := demandHTTPStatus(t, server, http.MethodPost, "/execute"); status != 503 {
		t.Fatalf("%d %s", status, body)
	}
	assertDemandReservation(t, f, "queued", 0)
	if calls.Load() != 0 {
		t.Fatal("capacity rejection dispatched work")
	}
	occupied.release()
	if status, body := demandHTTPStatus(t, server, http.MethodPost, "/execute"); status != 202 {
		t.Fatalf("retry %d %s", status, body)
	}
	assertDemandReservation(t, f, "admitted", 1)
	if calls.Load() != 1 {
		t.Fatal("retry did not dispatch exactly once")
	}
	if status, _ := demandHTTPStatus(t, server, http.MethodPost, "/execute"); status != 409 {
		t.Fatal("admitted work replayed")
	}
}

type reservationRaceStore struct {
	*store.Store
	change func(context.Context) error
}

func (s *reservationRaceStore) ReserveDemand(ctx context.Context, d store.Demand) (string, error) {
	claim, err := s.Store.ReserveDemand(ctx, d)
	if err != nil {
		return "", err
	}
	if s.change != nil {
		if err := s.change(ctx); err != nil {
			return "", err
		}
	}
	return claim, nil
}

func TestRequesterAdmissionRaceRequeuesAndCancels(t *testing.T) {
	for _, race := range []string{"closed admission", "stale fence"} {
		t.Run(race, func(t *testing.T) {
			f := newContractFixture(t)
			s := &reservationRaceStore{Store: f.store, change: func(ctx context.Context) error {
				state, err := f.store.State(ctx)
				if err != nil {
					return err
				}
				if race == "closed admission" {
					state.Admission = control.AdmissionClosed
				} else {
					_, err = f.store.RotateFenceAndCloseAdmission(ctx, state.Version)
					return err
				}
				_, err = f.store.UpdateState(ctx, state.Version, state)
				return err
			}}
			_, server := reservationHTTP(t, s, requesterConfig())
			expected := 503
			if race == "stale fence" {
				expected = 409
			}
			if status, body := demandHTTPStatus(t, server, http.MethodPost, "/execute"); status != expected {
				t.Fatalf("%d %s", status, body)
			}
			assertDemandReservation(t, f, "queued", 0)
			if status, body := demandHTTPStatus(t, server, http.MethodDelete, "/execute"); status != 204 {
				t.Fatalf("cancel %d %s", status, body)
			}
			assertDemandReservation(t, f, "cancelled", 0)
		})
	}
}

func TestRequesterInvalidComfyBodyRequeuesAndCancels(t *testing.T) {
	f := newContractFixture(t)
	c := requesterConfig()
	c.ComfyUIContract = ComfyUIContract
	c.ExecutionRoutes = []Route{{"POST", "/prompt"}}
	c.RequesterPolicy.Requesters[0].Workflows[0].Path = "/prompt"
	_, server := reservationHTTP(t, f.store, c)
	if status, body := demandHTTPStatus(t, server, http.MethodPost, "/prompt"); status != 400 {
		t.Fatalf("%d %s", status, body)
	}
	assertDemandReservation(t, f, "queued", 0)
	if status, body := demandHTTPStatus(t, server, http.MethodDelete, "/prompt"); status != 204 {
		t.Fatalf("cancel %d %s", status, body)
	}
	assertDemandReservation(t, f, "cancelled", 0)
}

type failedRequeueStore struct{ *store.Store }

func (s failedRequeueStore) RequeueDemand(context.Context, string, string) error {
	return errors.New("storage unavailable")
}

func TestRequesterRequeueFailurePreservesReservation(t *testing.T) {
	f := newContractFixture(t)
	h, server := reservationHTTP(t, failedRequeueStore{f.store}, requesterConfig())
	h.nativeFinishes = newNativeFinishes(1, nil)
	occupied := h.nativeFinishes.reserve()
	defer occupied.release()
	if status, body := demandHTTPStatus(t, server, http.MethodPost, "/execute"); status != 503 || body != "{\"error\":\"demand_state_failed\"}\n" {
		t.Fatalf("cleanup failure hidden: %d %s", status, body)
	}
	assertDemandReservation(t, f, "reserved", 0)
}

type ambiguousAdmissionStore struct{ *store.Store }

func (s ambiguousAdmissionStore) AdmitWorkTokenAtCatalog(ctx context.Context, id, job string, workload control.Workload, fence control.Fence, revision string) (string, error) {
	if _, err := s.Store.AdmitWorkTokenAtCatalog(ctx, id, job, workload, fence, revision); err != nil {
		return "", err
	}
	return "", errors.New("commit reply lost")
}

func TestRequesterAmbiguousAdmissionCannotRequeueOrReplay(t *testing.T) {
	f := newContractFixture(t)
	_, server := reservationHTTP(t, ambiguousAdmissionStore{f.store}, requesterConfig())
	if status, _ := demandHTTPStatus(t, server, http.MethodPost, "/execute"); status != 409 {
		t.Fatal(status)
	}
	assertDemandReservation(t, f, "admitted", 1)
	for _, method := range []string{http.MethodPost, http.MethodDelete} {
		if status, _ := demandHTTPStatus(t, server, method, "/execute"); status != 409 {
			t.Fatalf("admitted work changed via %s", method)
		}
	}
}

func TestRequesterCancelledActivationRequeuesDurably(t *testing.T) {
	f := newContractFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	state := f.state
	state.ActiveWorkload = control.WorkloadIdle
	state.DesiredWorkload = control.WorkloadIdle
	state.Admission = control.AdmissionClosed
	var err error
	f.state, err = f.store.UpdateState(ctx, state.Version, state)
	if err != nil {
		t.Fatal(err)
	}
	c := requesterConfig()
	c.ActivateDemand = func(context.Context, control.Workload, control.State, string) (control.Fence, error) {
		cancel()
		return control.Fence{}, context.Canceled
	}
	h, err := NewWithContext(ctx, f.store, c)
	if err != nil {
		t.Fatal(err)
	}
	r := requesterRequest().WithContext(ctx)
	r.Header.Set(DefaultRequestIDHeader, "reservation-request")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 409 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	assertDemandReservation(t, f, "queued", 0)
}

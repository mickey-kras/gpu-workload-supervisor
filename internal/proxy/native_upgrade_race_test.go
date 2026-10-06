package proxy

import (
	"context"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"net/http/httptest"
	"strings"
	"testing"
)

type catalogRaceUpgradeStore struct {
	*nativeStore
	mutate func()
}

func (s *catalogRaceUpgradeStore) State(ctx context.Context) (control.State, error) {
	if s.mutate != nil {
		s.mutate()
		s.mutate = nil
	}
	return s.fakeStore.State(ctx)
}
func TestNativeLegacyNativeUpgradeRace(t *testing.T) {
	s, c, _, calls := nativeProxyFixture(t)
	n := s.catalog.Catalog.Profiles[0].NativeModel
	s.catalog.Catalog.Profiles[0].NativeModel = nil
	wrapped := &catalogRaceUpgradeStore{nativeStore: s, mutate: func() { s.catalog.Revision = "two"; s.catalog.Catalog.Profiles[0].NativeModel = n }}
	h, err := NewWithContext(context.Background(), wrapped, c)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/api/generate", strings.NewReader(`{"model":"other"}`))
	addLeaseHeaders(req, s.state.LeaseFence)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if *calls != 0 {
		t.Fatalf("unpinned legacy handler forwards into changed native catalog: status=%d calls=%d", w.Code, *calls)
	}
}

package proxy

import (
	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

type catalogRaceBody struct {
	io.Reader
	mutate func()
}

func (r *catalogRaceBody) Read(p []byte) (int, error) {
	if r.mutate != nil {
		r.mutate()
		r.mutate = nil
	}
	return r.Reader.Read(p)
}
func (*catalogRaceBody) Close() error { return nil }
func TestNativeCatalogChangesDuringBody(t *testing.T) {
	s, c, _, calls := nativeProxyFixture(t)
	s.state.Owner = control.OwnerUser
	h, err := New(s, c)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/api/generate", nil)
	req.Body = &catalogRaceBody{Reader: strings.NewReader(`{"model":"selected"}`), mutate: func() { s.catalog.Revision = "two"; s.catalog.Catalog.Profiles[0].NativeModel.Model = "replacement" }}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if *calls != 0 {
		t.Fatalf("stale native model forwarded after catalog changed: status=%d calls=%d", w.Code, *calls)
	}
}

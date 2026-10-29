package proxy

import (
	"bytes"
	"context"
	"errors"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"io"
	"net/http"
)

type CatalogStore interface {
	StateStore
	Catalog(context.Context) (control.CatalogSnapshot, error)
	AdmitWorkTokenAtCatalog(context.Context, string, string, control.Workload, control.Fence, string) (string, error)
}

func nativePolicy(ctx context.Context, catalog CatalogStore, config Config) (*control.NativeModel, string, error) {
	if catalog == nil {
		return nil, "", nil
	}
	snapshot, err := catalog.Catalog(ctx)
	if err != nil {
		return nil, "", err
	}
	if snapshot.Revision != "" && !supportedCatalogVersion(snapshot.Catalog.Version) {
		return nil, "", errors.New("unsupported catalog version")
	}
	p, ok := snapshot.Catalog.Profile(config.Workload)
	if !ok || p.NativeModel == nil {
		return nil, snapshot.Revision, nil
	}
	n := *p.NativeModel
	if err := checkNativeUpstream(n, config); err != nil {
		return nil, "", err
	}
	for _, r := range config.ExecutionRoutes {
		if !nativeExecutionRoute(r) {
			return nil, "", errors.New("unsupported native execution route")
		}
	}
	for _, r := range config.ReadOnlyRoutes {
		if r.Method != "GET" || r.Path != "/v1/models" {
			return nil, "", errors.New("unsupported native read route")
		}
	}
	return &n, snapshot.Revision, nil
}

func checkNativeUpstream(n control.NativeModel, config Config) error {
	if n.EnvironmentFile == "" {
		return errors.New("native vLLM binding has no API key file; run setup again for this model")
	}
	if config.Upstream.String() != n.Endpoint || len(config.PassthroughRoutes) != 0 {
		return errors.New("native model requires matching upstream and no passthrough routes")
	}
	return nil
}

// supportedCatalogVersion is the proxy-side belt-and-braces guard: the proxy
// reads the catalog without Validate, so it refuses versions it does not
// understand rather than misinterpreting them.
func supportedCatalogVersion(v int) bool {
	return v == 1 || v == 2 || v == 4
}

func nativeExecutionRoute(r Route) bool {
	if r.Method != "POST" {
		return false
	}
	switch r.Path {
	case "/v1/chat/completions", "/v1/completions", "/v1/embeddings":
		return true
	}
	return false
}
func (h *Handler) checkNativeCatalog(w http.ResponseWriter, r *http.Request) bool {
	if h.catalog == nil {
		return true
	}
	snapshot, err := h.catalog.Catalog(r.Context())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "catalog_unavailable")
		return false
	}
	if snapshot.Revision != h.catalogRevision {
		writeError(w, http.StatusConflict, "configuration_changed")
		return false
	}
	return true
}
func (h *Handler) checkNativeRequest(w http.ResponseWriter, r *http.Request) bool {
	if h.nativeModel == nil {
		return true
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<20))
	if err != nil {
		writeError(w, http.StatusBadGateway, "native_body_read_failed")
		return false
	}
	if control.ValidateModelRequest(body, h.nativeModel.Model) != nil {
		writeError(w, http.StatusBadRequest, "native_model_mismatch")
		return false
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	return true
}

func (h *Handler) admitExecution(ctx context.Context, requestID, jobID string, fence control.Fence) (string, error) {
	if h.catalog == nil {
		return h.store.AdmitWorkToken(ctx, requestID, jobID, h.workload, fence)
	}
	return h.catalog.AdmitWorkTokenAtCatalog(ctx, requestID, jobID, h.workload, fence, h.catalogRevision)
}

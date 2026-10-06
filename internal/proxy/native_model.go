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

func nativePolicy(ctx context.Context, stateStore StateStore, config Config) (*control.NativeModel, string, error) {
	reader, ok := stateStore.(CatalogStore)
	if !ok {
		return nil, "", nil
	}
	snapshot, err := reader.Catalog(ctx)
	if err != nil {
		return nil, "", err
	}
	p, ok := snapshot.Catalog.Profile(config.Workload)
	if !ok || p.NativeModel == nil {
		return nil, snapshot.Revision, nil
	}
	n := *p.NativeModel
	if config.Upstream.String() != n.Endpoint || len(config.PassthroughRoutes) != 0 {
		return nil, "", errors.New("native model requires matching upstream and no passthrough routes")
	}
	for _, r := range config.ExecutionRoutes {
		if !nativeExecutionRoute(n.Runtime, r) {
			return nil, "", errors.New("unsupported native execution route")
		}
	}
	for _, r := range config.ReadOnlyRoutes {
		if r.Method != "GET" || !nativeReadRoute(n.Runtime, r.Path) {
			return nil, "", errors.New("unsupported native read route")
		}
	}
	return &n, snapshot.Revision, nil
}
func nativeExecutionRoute(runtime string, r Route) bool {
	if r.Method != "POST" {
		return false
	}
	switch r.Path {
	case "/v1/chat/completions", "/v1/completions", "/v1/embeddings":
		return true
	case "/api/generate", "/api/chat", "/api/embed", "/api/embeddings":
		return runtime == "ollama"
	}
	return false
}
func nativeReadRoute(runtime, path string) bool {
	if path == "/v1/models" {
		return true
	}
	return runtime == "ollama" && (path == "/api/tags" || path == "/api/ps" || path == "/api/version")
}
func (h *Handler) checkNativeCatalog(w http.ResponseWriter, r *http.Request) bool {
	reader, ok := h.store.(CatalogStore)
	if !ok {
		return true
	}
	snapshot, err := reader.Catalog(r.Context())
	if err != nil {
		writeError(w, 503, "catalog_unavailable")
		return false
	}
	if snapshot.Revision != h.catalogRevision {
		writeError(w, 409, "configuration_changed")
		return false
	}
	return true
}
func (h *Handler) checkNativeRequest(w http.ResponseWriter, r *http.Request) bool {
	if h.nativeModel == nil {
		return true
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<20))
	if err != nil || control.ValidateModelRequest(body, h.nativeModel.Model) != nil {
		writeError(w, 400, "native_model_mismatch")
		return false
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	return true
}

func (h *Handler) admitExecution(ctx context.Context, requestID, jobID string, fence control.Fence) (string, error) {
	admitter, ok := h.store.(CatalogStore)
	if !ok {
		return h.store.AdmitWorkToken(ctx, requestID, jobID, h.workload, fence)
	}
	return admitter.AdmitWorkTokenAtCatalog(ctx, requestID, jobID, h.workload, fence, h.catalogRevision)
}

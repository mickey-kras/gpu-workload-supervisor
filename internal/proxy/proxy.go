package proxy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

const (
	DefaultRequestIDHeader  = "X-Request-ID"
	DefaultFenceIDHeader    = "X-Workload-Lease-Incarnation"
	DefaultFenceEpochHeader = "X-Workload-Lease-Epoch"
	DefaultCompletionPath   = "/_gpu-workload-supervisor/v1/work/finish"
)

type StateStore interface {
	State(context.Context) (control.State, error)
	AdmitWork(context.Context, string, string, control.Workload, control.Fence) error
	FinishWorkFenced(context.Context, string, control.Fence, store.WorkOutcome) error
}

type Route struct {
	Method string
	Path   string
}

type Config struct {
	Upstream          *url.URL
	Workload          control.Workload
	ExecutionRoutes   []Route
	PassthroughRoutes []Route
	CompletionPath    string
	RequestIDHeader   string
	JobIDHeader       string
	FenceIDHeader     string
	FenceEpochHeader  string
	Transport         http.RoundTripper
}

type Handler struct {
	store             StateStore
	proxy             *httputil.ReverseProxy
	workload          control.Workload
	executionRoutes   map[string]struct{}
	passthroughRoutes map[string]struct{}
	completionPath    string
	requestIDHeader   string
	jobIDHeader       string
	fenceIDHeader     string
	fenceEpochHeader  string
}

type finishRequest struct {
	RequestID string            `json:"requestId"`
	Fence     control.Fence     `json:"fence"`
	Outcome   store.WorkOutcome `json:"outcome"`
}

func New(stateStore StateStore, config Config) (*Handler, error) {
	if stateStore == nil {
		return nil, errors.New("state store is required")
	}
	if config.Upstream == nil || config.Upstream.Scheme == "" || config.Upstream.Host == "" {
		return nil, errors.New("absolute upstream URL is required")
	}
	if config.Upstream.Scheme != "http" && config.Upstream.Scheme != "https" {
		return nil, errors.New("upstream scheme must be http or https")
	}
	if config.Workload != control.WorkloadText && config.Workload != control.WorkloadMedia {
		return nil, errors.New("workload must be text or media")
	}
	executionRoutes, err := routeSet(config.ExecutionRoutes, true)
	if err != nil {
		return nil, err
	}
	passthroughRoutes, err := routeSet(config.PassthroughRoutes, false)
	if err != nil {
		return nil, err
	}
	if config.CompletionPath == "" {
		config.CompletionPath = DefaultCompletionPath
	}
	if !canonicalPath(config.CompletionPath) {
		return nil, errors.New("completion path must be canonical and absolute")
	}
	if config.RequestIDHeader == "" {
		config.RequestIDHeader = DefaultRequestIDHeader
	}
	if config.FenceIDHeader == "" {
		config.FenceIDHeader = DefaultFenceIDHeader
	}
	if config.FenceEpochHeader == "" {
		config.FenceEpochHeader = DefaultFenceEpochHeader
	}
	reverseProxy := httputil.NewSingleHostReverseProxy(config.Upstream)
	reverseProxy.Transport = config.Transport
	return &Handler{
		store: stateStore, proxy: reverseProxy, workload: config.Workload,
		executionRoutes: executionRoutes, passthroughRoutes: passthroughRoutes,
		completionPath: config.CompletionPath, requestIDHeader: config.RequestIDHeader,
		jobIDHeader: config.JobIDHeader, fenceIDHeader: config.FenceIDHeader,
		fenceEpochHeader: config.FenceEpochHeader,
	}, nil
}

func (h *Handler) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	if request.Method == http.MethodPost && request.URL.Path == h.completionPath {
		h.finish(response, request)
		return
	}
	if !canonicalPath(request.URL.Path) {
		writeError(response, http.StatusBadRequest, "path_not_canonical")
		return
	}
	key := request.Method + " " + request.URL.Path
	if _, gated := h.executionRoutes[key]; gated {
		h.execute(response, request)
		return
	}
	if isSafeMethod(request.Method) {
		h.proxy.ServeHTTP(response, h.withoutControlHeaders(request))
		return
	}
	if _, allowed := h.passthroughRoutes[key]; allowed {
		h.proxy.ServeHTTP(response, h.withoutControlHeaders(request))
		return
	}
	writeError(response, http.StatusMethodNotAllowed, "route_not_allowed")
}

func (h *Handler) execute(response http.ResponseWriter, request *http.Request) {
	state, err := h.store.State(request.Context())
	if err != nil {
		writeError(response, http.StatusServiceUnavailable, "state_unavailable")
		return
	}
	if state.Owner == control.OwnerUser {
		h.proxy.ServeHTTP(response, h.withoutControlHeaders(request))
		return
	}
	if state.Owner != control.OwnerSupervisor {
		writeError(response, http.StatusServiceUnavailable, "ownership_invalid")
		return
	}
	requestID := strings.TrimSpace(request.Header.Get(h.requestIDHeader))
	if requestID == "" {
		writeError(response, http.StatusBadRequest, "request_id_required")
		return
	}
	fence, err := h.readFence(request)
	if err != nil {
		writeError(response, http.StatusBadRequest, "fence_invalid")
		return
	}
	jobID := ""
	if h.jobIDHeader != "" {
		jobID = strings.TrimSpace(request.Header.Get(h.jobIDHeader))
	}
	if err := h.store.AdmitWork(request.Context(), requestID, jobID, h.workload, fence); err != nil {
		h.writeWorkError(response, err)
		return
	}
	h.proxy.ServeHTTP(response, h.withoutControlHeaders(request))
}

func (h *Handler) finish(response http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(response, request.Body, 64<<10)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	var finish finishRequest
	if err := decoder.Decode(&finish); err != nil {
		writeError(response, http.StatusBadRequest, "finish_request_invalid")
		return
	}
	if finish.Outcome != store.WorkCompleted && finish.Outcome != store.WorkAbandoned {
		writeError(response, http.StatusBadRequest, "outcome_invalid")
		return
	}
	if err := h.store.FinishWorkFenced(request.Context(), finish.RequestID, finish.Fence, finish.Outcome); err != nil {
		h.writeWorkError(response, err)
		return
	}
	response.WriteHeader(http.StatusNoContent)
}

func (h *Handler) readFence(request *http.Request) (control.Fence, error) {
	fence := control.Fence{Incarnation: strings.TrimSpace(request.Header.Get(h.fenceIDHeader))}
	epoch, err := strconv.ParseUint(strings.TrimSpace(request.Header.Get(h.fenceEpochHeader)), 10, 64)
	if err != nil {
		return fence, err
	}
	fence.Epoch = epoch
	return fence, fence.Validate()
}

func (h *Handler) withoutControlHeaders(request *http.Request) *http.Request {
	clone := request.Clone(request.Context())
	clone.Header = request.Header.Clone()
	clone.Header.Del(h.requestIDHeader)
	clone.Header.Del(h.fenceIDHeader)
	clone.Header.Del(h.fenceEpochHeader)
	if h.jobIDHeader != "" {
		clone.Header.Del(h.jobIDHeader)
	}
	return clone
}

func (h *Handler) writeWorkError(response http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrStaleFence), errors.Is(err, store.ErrWorkloadMismatch):
		writeError(response, http.StatusConflict, "lease_rejected")
	case errors.Is(err, store.ErrRequestConflict):
		writeError(response, http.StatusConflict, "request_conflict")
	case errors.Is(err, store.ErrAdmissionClosed):
		writeError(response, http.StatusServiceUnavailable, "admission_closed")
	case errors.Is(err, sql.ErrNoRows):
		writeError(response, http.StatusNotFound, "request_not_found")
	default:
		writeError(response, http.StatusServiceUnavailable, "work_state_failed")
	}
}

func routeSet(routes []Route, required bool) (map[string]struct{}, error) {
	if required && len(routes) == 0 {
		return nil, errors.New("at least one execution route is required")
	}
	result := make(map[string]struct{}, len(routes))
	for _, route := range routes {
		method := strings.ToUpper(strings.TrimSpace(route.Method))
		if method == "" || !canonicalPath(route.Path) {
			return nil, errors.New("routes require a method and canonical absolute path")
		}
		result[method+" "+route.Path] = struct{}{}
	}
	return result, nil
}

func canonicalPath(value string) bool {
	return value != "" && strings.HasPrefix(value, "/") && path.Clean(value) == value
}

func isSafeMethod(method string) bool {
	return method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions
}

func writeError(response http.ResponseWriter, status int, code string) {
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(map[string]string{"error": code})
}

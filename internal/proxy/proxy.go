package proxy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

const (
	DefaultRequestIDHeader   = "X-Request-ID"
	DefaultFenceIDHeader     = "X-Workload-Lease-Incarnation"
	DefaultFenceEpochHeader  = "X-Workload-Lease-Epoch"
	defaultCompletionTimeout = 10 * time.Second
)

type StateStore interface {
	State(context.Context) (control.State, error)
	RegisterWork(context.Context, string, string, control.Workload, control.Fence) error
	CompleteWorkFenced(context.Context, string, control.Fence) error
}

type Route struct {
	Method string
	Path   string
}

type Config struct {
	Upstream          *url.URL
	Workload          control.Workload
	ExecutionRoutes   []Route
	RequestIDHeader   string
	JobIDHeader       string
	FenceIDHeader     string
	FenceEpochHeader  string
	CompletionTimeout time.Duration
	Transport         http.RoundTripper
	ErrorLog          func(error)
}

type Handler struct {
	store             StateStore
	proxy             *httputil.ReverseProxy
	workload          control.Workload
	routes            map[string]struct{}
	requestIDHeader   string
	jobIDHeader       string
	fenceIDHeader     string
	fenceEpochHeader  string
	completionTimeout time.Duration
	errorLog          func(error)
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
	if len(config.ExecutionRoutes) == 0 {
		return nil, errors.New("at least one execution route is required")
	}
	routes := make(map[string]struct{}, len(config.ExecutionRoutes))
	for _, route := range config.ExecutionRoutes {
		method := strings.ToUpper(strings.TrimSpace(route.Method))
		if method == "" || route.Path == "" || !strings.HasPrefix(route.Path, "/") {
			return nil, errors.New("execution routes require a method and absolute path")
		}
		routes[method+" "+route.Path] = struct{}{}
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
	if config.CompletionTimeout <= 0 {
		config.CompletionTimeout = defaultCompletionTimeout
	}
	reverseProxy := httputil.NewSingleHostReverseProxy(config.Upstream)
	reverseProxy.Transport = config.Transport
	return &Handler{
		store: stateStore, proxy: reverseProxy, workload: config.Workload,
		routes: routes, requestIDHeader: config.RequestIDHeader,
		jobIDHeader: config.JobIDHeader, fenceIDHeader: config.FenceIDHeader,
		fenceEpochHeader: config.FenceEpochHeader,
		completionTimeout: config.CompletionTimeout, errorLog: config.ErrorLog,
	}, nil
}

func (h *Handler) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	if _, gated := h.routes[request.Method+" "+request.URL.Path]; !gated {
		h.proxy.ServeHTTP(response, request)
		return
	}
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
	if err := h.store.RegisterWork(request.Context(), requestID, jobID, h.workload, fence); err != nil {
		h.writeAdmissionError(response, err)
		return
	}
	var once sync.Once
	complete := func() {
		once.Do(func() {
			ctx, cancel := context.WithTimeout(context.Background(), h.completionTimeout)
			defer cancel()
			if err := h.store.CompleteWorkFenced(ctx, requestID, fence); err != nil && h.errorLog != nil {
				h.errorLog(fmt.Errorf("complete request %q: %w", requestID, err))
			}
		})
	}
	defer complete()
	h.proxy.ServeHTTP(response, h.withoutControlHeaders(request))
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

func (h *Handler) writeAdmissionError(response http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrStaleFence), errors.Is(err, store.ErrWorkloadMismatch):
		writeError(response, http.StatusConflict, "lease_rejected")
	case errors.Is(err, store.ErrAdmissionClosed):
		writeError(response, http.StatusServiceUnavailable, "admission_closed")
	case errors.Is(err, sql.ErrNoRows):
		writeError(response, http.StatusConflict, "request_conflict")
	default:
		writeError(response, http.StatusServiceUnavailable, "registration_failed")
	}
}

func writeError(response http.ResponseWriter, status int, code string) {
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(map[string]string{"error": code})
}

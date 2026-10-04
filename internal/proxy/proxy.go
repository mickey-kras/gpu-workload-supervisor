package proxy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/httptransport"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/lock"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

const (
	DefaultRequestIDHeader         = "X-Request-ID"
	DefaultFenceIDHeader           = "X-Workload-Lease-Incarnation"
	DefaultFenceEpochHeader        = "X-Workload-Lease-Epoch"
	DefaultRegistrationTokenHeader = "X-Workload-Registration-Token"
	DefaultCompletionPath          = "/_gpu-workload-supervisor/v1/work/finish"
)

type StateStore interface {
	AcquireUserExecution(context.Context, bool) (*lock.File, error)
	State(context.Context) (control.State, error)
	AdmitWorkToken(context.Context, string, string, control.Workload, control.Fence) (string, error)
	FinishWorkToken(context.Context, string, control.Workload, control.Fence, string, store.WorkOutcome) error
}

type Route struct {
	Method string
	Path   string
}

type Config struct {
	Upstream          *url.URL
	Workload          control.Workload
	ExecutionRoutes   []Route
	ReadOnlyRoutes    []Route
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
	readOnlyRoutes    map[string]struct{}
	passthroughRoutes map[string]struct{}
	completionPath    string
	requestIDHeader   string
	jobIDHeader       string
	fenceIDHeader     string
	fenceEpochHeader  string
}

type registrationTransport struct {
	base             http.RoundTripper
	token            string
	requestID        string
	fence            control.Fence
	requestIDHeader  string
	fenceIDHeader    string
	fenceEpochHeader string
}

func (t registrationTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	forwarded := request.Clone(request.Context())
	forwarded.Header.Set(t.requestIDHeader, t.requestID)
	forwarded.Header.Set(t.fenceIDHeader, t.fence.Incarnation)
	forwarded.Header.Set(t.fenceEpochHeader, strconv.FormatUint(t.fence.Epoch, 10))
	forwarded.Header.Set(DefaultRegistrationTokenHeader, t.token)
	return t.base.RoundTrip(forwarded)
}

type finishRequest struct {
	RequestID         string            `json:"requestId"`
	RegistrationToken string            `json:"registrationToken"`
	Fence             control.Fence     `json:"fence"`
	Outcome           store.WorkOutcome `json:"outcome"`
}

func ValidateConfig(config Config) error {
	_, _, _, _, err := validateConfig(config)
	return err
}

func New(stateStore StateStore, config Config) (*Handler, error) {
	if stateStore == nil {
		return nil, errors.New("state store is required")
	}
	executionRoutes, readOnlyRoutes, passthroughRoutes, completionPath, err := validateConfig(config)
	if err != nil {
		return nil, err
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
	if reverseProxy.Transport == nil {
		reverseProxy.Transport = httptransport.NewDirect()
	}
	reverseProxy.ModifyResponse = func(response *http.Response) error {
		response.Header.Del(DefaultRegistrationTokenHeader)
		return nil
	}
	return &Handler{
		store: stateStore, proxy: reverseProxy, workload: config.Workload,
		executionRoutes: executionRoutes, readOnlyRoutes: readOnlyRoutes, passthroughRoutes: passthroughRoutes,
		completionPath: completionPath, requestIDHeader: config.RequestIDHeader,
		jobIDHeader: config.JobIDHeader, fenceIDHeader: config.FenceIDHeader,
		fenceEpochHeader: config.FenceEpochHeader,
	}, nil
}

func validateConfig(config Config) (map[string]struct{}, map[string]struct{}, map[string]struct{}, string, error) {
	if config.Upstream == nil || config.Upstream.Scheme == "" || config.Upstream.Host == "" {
		return nil, nil, nil, "", errors.New("absolute upstream URL is required")
	}
	if config.Upstream.Scheme != "http" && config.Upstream.Scheme != "https" {
		return nil, nil, nil, "", errors.New("upstream scheme must be http or https")
	}
	if !control.ValidWorkloadID(config.Workload) {
		return nil, nil, nil, "", errors.New("workload must be a valid workload ID")
	}
	if err := validateConfiguredHeaderNames(config); err != nil {
		return nil, nil, nil, "", err
	}
	if err := validateDistinctControlHeaders(config); err != nil {
		return nil, nil, nil, "", err
	}
	executionRoutes, err := routeSet(config.ExecutionRoutes, true)
	if err != nil {
		return nil, nil, nil, "", err
	}
	readOnlyRoutes, err := readOnlyRouteSet(config.ReadOnlyRoutes)
	if err != nil {
		return nil, nil, nil, "", err
	}
	passthroughRoutes, err := routeSet(config.PassthroughRoutes, false)
	if err != nil {
		return nil, nil, nil, "", err
	}
	completionPath := config.CompletionPath
	if completionPath == "" {
		completionPath = DefaultCompletionPath
	}
	if !canonicalPath(completionPath) {
		return nil, nil, nil, "", errors.New("completion path must be canonical and absolute")
	}
	completionKey := http.MethodPost + " " + completionPath
	if err := validateRouteCollisions([]map[string]struct{}{executionRoutes, readOnlyRoutes, passthroughRoutes}, completionKey); err != nil {
		return nil, nil, nil, "", err
	}
	return executionRoutes, readOnlyRoutes, passthroughRoutes, completionPath, nil
}

func validateConfiguredHeaderNames(config Config) error {
	for _, header := range []string{config.RequestIDHeader, config.JobIDHeader, config.FenceIDHeader, config.FenceEpochHeader} {
		if header != "" && !validHeaderName(header) {
			return errors.New("configured header name must be an HTTP token")
		}
	}
	return nil
}

func readOnlyRouteSet(routes []Route) (map[string]struct{}, error) {
	result, err := routeSet(routes, false)
	if err != nil {
		return nil, err
	}
	for _, route := range routes {
		if !isSafeMethod(strings.ToUpper(strings.TrimSpace(route.Method))) {
			return nil, errors.New("read-only routes require GET, HEAD or OPTIONS")
		}
	}
	return result, nil
}

func validateRouteCollisions(routeSets []map[string]struct{}, completionKey string) error {
	seen := make(map[string]struct{})
	for _, routes := range routeSets {
		for key := range routes {
			if key == completionKey {
				return errors.New("completion path collides with a configured route")
			}
			if _, exists := seen[key]; exists {
				return errors.New("configured routes overlap")
			}
			seen[key] = struct{}{}
		}
	}
	return nil
}

// validHeaderName implements RFC 9110's token grammar. net/http's validator
// is internal; canonicalization alone does not reject invalid field names.
func validHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for _, ch := range name {
		if ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", ch) {
			continue
		}
		return false
	}
	return true
}

func validateDistinctControlHeaders(config Config) error {
	requestIDHeader, fenceIDHeader, fenceEpochHeader := config.RequestIDHeader, config.FenceIDHeader, config.FenceEpochHeader
	if requestIDHeader == "" {
		requestIDHeader = DefaultRequestIDHeader
	}
	if fenceIDHeader == "" {
		fenceIDHeader = DefaultFenceIDHeader
	}
	if fenceEpochHeader == "" {
		fenceEpochHeader = DefaultFenceEpochHeader
	}
	headers := []string{requestIDHeader, fenceIDHeader, fenceEpochHeader, DefaultRegistrationTokenHeader}
	for i, header := range headers {
		if i < 3 && reservedTransportHeader(header) {
			return errors.New("control header cannot be a hop-by-hop or transport header")
		}
		for _, previous := range headers[:i] {
			if strings.EqualFold(header, previous) {
				return errors.New("request ID, fence, and registration token headers must be distinct")
			}
		}
	}
	return nil
}

func reservedTransportHeader(header string) bool {
	switch http.CanonicalHeaderKey(header) {
	case "Connection", "Keep-Alive", "Proxy-Connection", "Proxy-Authenticate",
		"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
		"Host", "Content-Length":
		return true
	}
	return false
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
	if _, allowed := h.readOnlyRoutes[key]; allowed {
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
		h.executeUser(response, request)
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
	if err := control.ValidateRequestID(requestID); err != nil {
		code := "request_id_invalid"
		if errors.Is(err, control.ErrRequestIDTooLong) {
			code = "request_id_too_long"
		}
		writeError(response, http.StatusBadRequest, code)
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
	token, err := h.store.AdmitWorkToken(request.Context(), requestID, jobID, h.workload, fence)
	if err != nil {
		h.writeWorkError(response, err)
		return
	}
	forwarded := h.withoutControlHeaders(request)
	proxy := *h.proxy
	// ReverseProxy strips hop-by-hop headers before calling Transport. Add
	// admitted correlation metadata afterwards so a client cannot nominate
	// those headers in Connection to remove them.
	proxy.Transport = registrationTransport{
		base: proxy.Transport, token: token, requestID: requestID, fence: fence,
		requestIDHeader: h.requestIDHeader, fenceIDHeader: h.fenceIDHeader,
		fenceEpochHeader: h.fenceEpochHeader,
	}
	proxy.ModifyResponse = func(upstreamResponse *http.Response) error {
		upstreamResponse.Header.Set(DefaultRegistrationTokenHeader, token)
		return nil
	}
	proxy.ServeHTTP(response, forwarded)
}

func (h *Handler) executeUser(response http.ResponseWriter, request *http.Request) {
	gate, err := h.store.AcquireUserExecution(request.Context(), true)
	if err != nil {
		writeError(response, http.StatusServiceUnavailable, "state_unavailable")
		return
	}
	defer gate.Close()
	// Re-read under the gate: the earlier observation may precede a transfer.
	state, err := h.store.State(request.Context())
	if err != nil || state.Owner != control.OwnerUser {
		writeError(response, http.StatusServiceUnavailable, "ownership_changed")
		return
	}
	if state.Phase != control.PhaseStable || state.Health != control.HealthHealthy {
		writeError(response, http.StatusServiceUnavailable, "admission_closed")
		return
	}
	if state.ActiveWorkload != h.workload || state.DesiredWorkload != h.workload {
		writeError(response, http.StatusConflict, "workload_mismatch")
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
	if err := requireJSONEOF(decoder); err != nil {
		writeError(response, http.StatusBadRequest, "finish_request_invalid")
		return
	}
	if strings.TrimSpace(finish.RequestID) == "" || finish.Fence.Validate() != nil {
		writeError(response, http.StatusBadRequest, "finish_request_invalid")
		return
	}
	if finish.Outcome != store.WorkCompleted && finish.Outcome != store.WorkAbandoned {
		writeError(response, http.StatusBadRequest, "outcome_invalid")
		return
	}
	if err := h.store.FinishWorkToken(request.Context(), finish.RequestID, h.workload, finish.Fence, finish.RegistrationToken, finish.Outcome); err != nil {
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
	clone.Header.Del(DefaultRegistrationTokenHeader)
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
	case errors.Is(err, store.ErrRegistrationTokenMismatch):
		writeError(response, http.StatusConflict, "registration_token_rejected")
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

func requireJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
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

package proxy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/httptransport"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/lock"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/strictjson"
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
	RequesterPolicy      *RequesterPolicy
	ActivateDemand       func(context.Context, control.Workload, control.State, string) (control.Fence, error)
	ComfyUIContract      string
	IngressMode          string
	ExecutionCredential  string
	CompletionCredential string
	Upstream             *url.URL
	Workload             control.Workload
	ExecutionRoutes      []Route
	ReadOnlyRoutes       []Route
	PassthroughRoutes    []Route
	CompletionPath       string
	RequestIDHeader      string
	JobIDHeader          string
	FenceIDHeader        string
	FenceEpochHeader     string
	Transport            http.RoundTripper
	// ResponseHeaderTimeout bounds the header wait after uploading the request.
	// Zero uses httptransport.DefaultResponseHeaderTimeout. A custom Transport
	// owns its timeout policy; this field only configures the default transport.
	ResponseHeaderTimeout time.Duration
	// MaxNativeCompletions bounds admitted native requests awaiting proof plus
	// pending native writes. Zero uses 128. Server caps post-proof HTTP streams.
	MaxNativeCompletions int
	// ReportNativeCompletionFailure runs on the persistence worker, outside
	// response reads and lifecycle locks. It must be concurrency safe and return
	// promptly. The payload contains no raw errors or request metadata.
	ReportNativeCompletionFailure func(NativeCompletionFailure)
}

type Handler struct {
	requesterPolicy      *RequesterPolicy
	activateDemand       func(context.Context, control.Workload, control.State, string) (control.Fence, error)
	nativeFinishes       *nativeFinishes
	comfyUI              *comfyUIAdapter
	ingressMode          string
	executionCredential  string
	completionCredential string
	nativeModel          *control.NativeModel
	catalogRevision      string
	store                StateStore
	catalog              CatalogStore
	proxy                *httputil.ReverseProxy
	workload             control.Workload
	executionRoutes      map[string]struct{}
	readOnlyRoutes       map[string]struct{}
	passthroughRoutes    map[string]struct{}
	completionPath       string
	requestIDHeader      string
	jobIDHeader          string
	fenceIDHeader        string
	fenceEpochHeader     string
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

func NewWithContext(ctx context.Context, stateStore StateStore, config Config) (*Handler, error) {
	if stateStore == nil {
		return nil, errors.New("state store is required")
	}
	if config.MaxNativeCompletions < 0 {
		return nil, errors.New("native completion limit must not be negative")
	}
	if config.ResponseHeaderTimeout < 0 {
		return nil, errors.New("response header timeout must not be negative")
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
	catalog, _ := stateStore.(CatalogStore)
	native, revision, err := nativePolicy(ctx, catalog, config)
	if err != nil {
		return nil, err
	}
	if err := validateDemandDependencies(stateStore, config); err != nil {
		return nil, err
	}
	reverseProxy := httputil.NewSingleHostReverseProxy(config.Upstream)
	reverseProxy.Transport = upstreamTransport(config, native)
	if config.IngressMode == IngressAdapter {
		reverseProxy.Transport = ingressTransport{requesterCredentials: requesterSecrets(config.RequesterPolicy), base: reverseProxy.Transport, executionCredential: config.ExecutionCredential, completionCredential: config.CompletionCredential}
	}
	handler := &Handler{
		requesterPolicy: cloneRequesterPolicy(config.RequesterPolicy), activateDemand: config.ActivateDemand,
		ingressMode: config.IngressMode, executionCredential: config.ExecutionCredential, completionCredential: config.CompletionCredential,
		store: stateStore, catalog: catalog, proxy: reverseProxy, workload: config.Workload, nativeModel: native, catalogRevision: revision,
		executionRoutes: executionRoutes, readOnlyRoutes: readOnlyRoutes, passthroughRoutes: passthroughRoutes,
		completionPath: completionPath, requestIDHeader: config.RequestIDHeader,
		jobIDHeader: config.JobIDHeader, fenceIDHeader: config.FenceIDHeader,
		fenceEpochHeader: config.FenceEpochHeader,
	}
	reverseProxy.ModifyResponse = func(response *http.Response) error {
		response.Header.Del(DefaultRegistrationTokenHeader)
		handler.protectIngressResponse(response, "")
		return nil
	}
	if err := handler.configureComfyUI(config); err != nil {
		return nil, err
	}
	if native != nil {
		handler.nativeFinishes = newNativeFinishes(config.MaxNativeCompletions, config.ReportNativeCompletionFailure)
	}
	return handler, nil
}

// upstreamTransport returns the configured or direct transport. For a native
// model it adds the vLLM API key. The ingress transport, when used, wraps it
// and strips client credentials first.
func upstreamTransport(config Config, native *control.NativeModel) http.RoundTripper {
	transport := config.Transport
	if transport == nil {
		direct := httptransport.NewDirect()
		if config.ResponseHeaderTimeout > 0 {
			direct.ResponseHeaderTimeout = config.ResponseHeaderTimeout
		}
		transport = direct
	}
	if native != nil {
		transport = upstreamKeyTransport{keyFile: native.EnvironmentFile, base: transport}
	}
	return transport
}

func validateConfig(config Config) (map[string]struct{}, map[string]struct{}, map[string]struct{}, string, error) {
	if err := validateIngress(config); err != nil {
		return nil, nil, nil, "", err
	}
	if config.Upstream == nil || config.Upstream.Scheme == "" || config.Upstream.Host == "" {
		return nil, nil, nil, "", errors.New("absolute upstream URL is required")
	}
	if config.Upstream.Scheme != "http" && config.Upstream.Scheme != "https" {
		return nil, nil, nil, "", errors.New("upstream scheme must be http or https")
	}
	if host := config.Upstream.Hostname(); !isLoopbackHost(host) {
		return nil, nil, nil, "", errors.New("upstream host must be loopback")
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
	if !CanonicalPath(completionPath) {
		return nil, nil, nil, "", errors.New("completion path must be canonical and absolute")
	}
	completionKey := http.MethodPost + " " + completionPath
	if err := validateRouteCollisions([]map[string]struct{}{executionRoutes, readOnlyRoutes, passthroughRoutes}, completionKey); err != nil {
		return nil, nil, nil, "", err
	}
	return executionRoutes, readOnlyRoutes, passthroughRoutes, completionPath, nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
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
	if config.JobIDHeader != "" {
		headers = append(headers, config.JobIDHeader)
	}
	for i, header := range headers {
		if i < 3 && reservedTransportHeader(header) {
			return errors.New("control header cannot be a hop-by-hop or transport header")
		}
		for _, previous := range headers[:i] {
			if strings.EqualFold(header, previous) {
				return errors.New("request ID, job ID, fence, and registration token headers must be distinct")
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
	if h.requesterExecution(request) {
		h.executeRequester(response, request)
		return
	}
	if !h.authorizeIngress(response, request) {
		return
	}
	if request.Method == http.MethodPost && request.URL.Path == h.completionPath {
		h.finish(response, request)
		return
	}
	if !h.checkNativeCatalog(response, request) {
		return
	}
	if !CanonicalPath(request.URL.Path) {
		writeError(response, http.StatusBadRequest, "path_not_canonical")
		return
	}
	if (h.nativeModel != nil || h.comfyUI != nil) && (request.URL.RawQuery != "" || request.Header.Get("Content-Encoding") != "") {
		writeError(response, http.StatusBadRequest, "native_request_invalid")
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
	methods := h.allowedMethods(request.URL.Path)
	if len(methods) == 0 {
		writeError(response, http.StatusNotFound, "route_not_allowed")
		return
	}
	response.Header().Set("Allow", strings.Join(methods, ", "))
	writeError(response, http.StatusMethodNotAllowed, "route_not_allowed")
}

func (h *Handler) allowedMethods(requestPath string) []string {
	var methods []string
	if requestPath == h.completionPath {
		methods = append(methods, http.MethodPost)
	}
	for _, routes := range []map[string]struct{}{h.executionRoutes, h.readOnlyRoutes, h.passthroughRoutes} {
		for key := range routes {
			method, routePath, _ := strings.Cut(key, " ")
			if routePath == requestPath {
				methods = append(methods, method)
			}
		}
	}
	slices.Sort(methods)
	return methods
}

func (h *Handler) execute(response http.ResponseWriter, request *http.Request) {
	if !h.checkNativeRequest(response, request) {
		return
	}
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
	if h.comfyUI != nil {
		h.executeComfyUI(response, request, requestID, fence)
		return
	}
	h.executeRegistered(response, request, requestID, fence)
}

func (h *Handler) executeRegistered(response http.ResponseWriter, request *http.Request, requestID string, fence control.Fence) {
	var reservation *nativeFinishReservation
	if h.nativeFinishes != nil {
		reservation = h.nativeFinishes.reserve()
		if reservation == nil {
			if !h.releaseUnadmittedDemand(response, request) {
				return
			}
			capacityUnavailable(response)
			return
		}
	}
	// ReverseProxy may return without a response body (e.g. transport failure).
	// Its producer owns the reservation until verified proof transfers it.
	defer reservation.release()
	jobID := ""
	if h.jobIDHeader != "" {
		jobID = strings.TrimSpace(request.Header.Get(h.jobIDHeader))
	}
	token, err := h.admitExecution(request.Context(), requestID, jobID, fence)
	if err != nil {
		if !h.releaseUnadmittedDemand(response, request) {
			return
		}
		h.writeWorkError(response, err)
		return
	}
	forwarded := h.withoutControlHeaders(request)
	proxy := *h.proxy
	// ReverseProxy strips hop-by-hop headers before calling Transport. Add
	// admitted correlation metadata afterwards so a client cannot nominate
	// those headers in Connection to remove them.
	proxy.Transport = executionTransport{base: registrationTransport{
		base: proxy.Transport, token: token, requestID: requestID, fence: fence,
		requestIDHeader: h.requestIDHeader, fenceIDHeader: h.fenceIDHeader,
		fenceEpochHeader: h.fenceEpochHeader,
	}}
	proxy.ModifyResponse = func(upstreamResponse *http.Response) error {
		upstreamResponse.Header.Set(DefaultRegistrationTokenHeader, token)
		h.observeNativeTerminal(upstreamResponse, request.URL.Path, requestID, fence, token, reservation)
		h.protectIngressResponse(upstreamResponse, token)
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
	if !h.checkNativeCatalog(response, request) {
		return
	}
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
	if h.comfyUI != nil {
		h.executeComfyUIUser(response, request)
		return
	}
	proxy := *h.proxy
	proxy.Transport = executionTransport{base: proxy.Transport}
	proxy.ServeHTTP(response, h.withoutControlHeaders(request))
}

func (h *Handler) finish(response http.ResponseWriter, request *http.Request) {
	var finish finishRequest
	if err := strictjson.DecodeLimited(request.Body, 64<<10, &finish); err != nil {
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
	if h.ingressMode == IngressAdapter {
		clone = clone.WithContext(ingressTrailerContext(request))
		stripIngressHeaders(clone.Header, false)
		stripIngressHeaders(clone.Trailer, true)
	}
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
	if errors.Is(err, store.ErrVersionConflict) {
		writeError(response, http.StatusConflict, "configuration_changed")
		return
	}
	switch {
	case errors.Is(err, store.ErrWorkAlreadyCompleted):
		// The request ID is the idempotency key: a finish retried after a lost
		// 204 replays as success instead of request_not_found.
		response.WriteHeader(http.StatusNoContent)
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

func routeSet(routes []Route, required bool) (map[string]struct{}, error) {
	if required && len(routes) == 0 {
		return nil, errors.New("at least one execution route is required")
	}
	result := make(map[string]struct{}, len(routes))
	for _, route := range routes {
		method := strings.ToUpper(strings.TrimSpace(route.Method))
		if method == "" || !CanonicalPath(route.Path) {
			return nil, errors.New("routes require a method and canonical absolute path")
		}
		if _, err := http.NewRequest(method, "/", nil); err != nil {
			return nil, errors.New("route method must be an HTTP token")
		}
		result[method+" "+route.Path] = struct{}{}
	}
	return result, nil
}

func CanonicalPath(value string) bool {
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

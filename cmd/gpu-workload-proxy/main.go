package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/lock"
	workloadproxy "github.com/mickey-kras/gpu-workload-supervisor/internal/proxy"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

type routesFlag []workloadproxy.Route

// activeHandler prevents new proxy work after shutdown begins and reports when
// every admitted request has left its handler, even if Shutdown times out.
type activeHandler struct {
	handler         http.Handler
	mu              sync.Mutex
	active          int
	ordinary        int
	completion      int
	maxOrdinary     int
	maxCompletion   int
	completionPath  string
	stopping        bool
	drained         chan struct{}
	hijacked        map[net.Conn]struct{}
	closingHijacked bool
}

// trackingWriter records connections that leave net/http's ownership on
// Hijack. ReverseProxy uses ResponseController.Hijack for upgrades.
type trackingWriter struct {
	http.ResponseWriter
	owner *activeHandler
	conn  net.Conn
}

func (w *trackingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *trackingWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err != nil {
		return nil, nil, err
	}
	w.owner.mu.Lock()
	if w.owner.hijacked == nil {
		w.owner.hijacked = make(map[net.Conn]struct{})
	}
	w.owner.hijacked[conn] = struct{}{}
	w.conn = conn
	closing := w.owner.closingHijacked
	w.owner.mu.Unlock()
	if closing {
		_ = conn.Close()
	}
	return conn, rw, nil
}

func (h *activeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	if h.stopping {
		h.mu.Unlock()
		http.Error(w, "proxy is shutting down", http.StatusServiceUnavailable)
		return
	}
	isCompletion := r.Method == http.MethodPost && r.URL.Path == h.completionPath
	if isCompletion {
		if h.maxCompletion > 0 && h.completion >= h.maxCompletion {
			h.mu.Unlock()
			capacityUnavailable(w)
			return
		}
		h.completion++
	} else {
		if h.maxOrdinary > 0 && h.ordinary >= h.maxOrdinary {
			h.mu.Unlock()
			capacityUnavailable(w)
			return
		}
		h.ordinary++
	}
	h.active++
	h.mu.Unlock()
	writer := &trackingWriter{ResponseWriter: w, owner: h}
	defer func() {
		h.mu.Lock()
		delete(h.hijacked, writer.conn)
		h.active--
		if isCompletion {
			h.completion--
		} else {
			h.ordinary--
		}
		if h.stopping && h.active == 0 {
			close(h.drained)
		}
		h.mu.Unlock()
	}()
	h.handler.ServeHTTP(writer, r)
}

func capacityUnavailable(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "1")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte("{\"error\":\"proxy_capacity_exceeded\"}\n"))
}

func (h *activeHandler) closeHijacked() error {
	h.mu.Lock()
	h.closingHijacked = true
	conns := make([]net.Conn, 0, len(h.hijacked))
	for conn := range h.hijacked {
		conns = append(conns, conn)
	}
	h.mu.Unlock()
	var err error
	for _, conn := range conns {
		err = errors.Join(err, conn.Close())
	}
	return err
}

func (h *activeHandler) stop() <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.stopping {
		h.stopping = true
		h.drained = make(chan struct{})
		if h.active == 0 {
			close(h.drained)
		}
	}
	return h.drained
}

func shutdownAndDrain(server *http.Server, handler *activeHandler, timeout time.Duration) error {
	drained := handler.stop()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	err := server.Shutdown(ctx)
	if err != nil {
		// Shutdown leaves active connections open when its deadline expires.
		// Close cancels them; a handler may still need time to return.
		err = errors.Join(err, server.Close(), handler.closeHijacked())
	} else {
		select {
		case <-drained:
			return nil
		case <-ctx.Done():
			err = errors.Join(ctx.Err(), server.Close(), handler.closeHijacked())
		}
	}
	<-drained
	return err
}

func (value *routesFlag) String() string {
	items := make([]string, 0, len(*value))
	for _, route := range *value {
		items = append(items, route.Method+":"+route.Path)
	}
	return strings.Join(items, ",")
}

func (value *routesFlag) Set(input string) error {
	method, path, ok := strings.Cut(input, ":")
	if !ok || strings.TrimSpace(method) == "" || !canonicalPath(path) {
		return errors.New("route must use METHOD:/absolute/path")
	}
	*value = append(*value, workloadproxy.Route{Method: strings.ToUpper(strings.TrimSpace(method)), Path: path})
	return nil
}

func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

type proxyServerSettings struct {
	statePath, listen                                                       string
	readHeaderTimeout, idleTimeout, shutdownTimeout, completedWorkRetention time.Duration
	maxInflight, maxCompletionInflight                                      int
}

func run() error {
	flags := flag.NewFlagSet("gpu-workload-proxy", flag.ContinueOnError)
	statePath := flags.String("state", defaultStatePath(), "SQLite state path")
	listen := flags.String("listen", "127.0.0.1:8090", "HTTP listen address")
	upstreamValue := flags.String("upstream", "", "absolute upstream URL")
	workloadValue := flags.String("workload", "", "required workload: text or media")
	requestIDHeader := flags.String("request-id-header", workloadproxy.DefaultRequestIDHeader, "request ID header")
	jobIDHeader := flags.String("job-id-header", "", "optional job ID header")
	fenceIDHeader := flags.String("fence-id-header", workloadproxy.DefaultFenceIDHeader, "lease incarnation header")
	fenceEpochHeader := flags.String("fence-epoch-header", workloadproxy.DefaultFenceEpochHeader, "lease epoch header")
	completionPath := flags.String("completion-path", workloadproxy.DefaultCompletionPath, "local terminal work endpoint path")
	readHeaderTimeout := flags.Duration("read-header-timeout", 10*time.Second, "HTTP header read timeout")
	idleTimeout := flags.Duration("idle-timeout", 2*time.Minute, "HTTP idle timeout")
	shutdownTimeout := flags.Duration("shutdown-timeout", 30*time.Second, "graceful shutdown timeout")
	completedWorkRetention := flags.Duration("completed-work-retention", 30*24*time.Hour, "time to keep completed work records")
	maxInflight := flags.Int("max-inflight", 128, "maximum concurrent non-completion proxy requests")
	maxCompletionInflight := flags.Int("max-completion-inflight", 16, "maximum concurrent completion reports, reserved from proxy traffic")
	var routes routesFlag
	var readOnlyRoutes routesFlag
	var passthroughRoutes routesFlag
	flags.Var(&routes, "execute-route", "gated execution route as METHOD:/absolute/path; repeatable")
	flags.Var(&readOnlyRoutes, "read-only-route", "verified read-only route as GET|HEAD|OPTIONS:/absolute/path; repeatable")
	flags.Var(&passthroughRoutes, "passthrough-route", "explicit ungated mutating route as METHOD:/absolute/path; repeatable")
	if err := flags.Parse(os.Args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if *completedWorkRetention <= 0 {
		return errors.New("completed-work-retention must be positive")
	}
	if *maxInflight <= 0 || *maxCompletionInflight <= 0 {
		return errors.New("max-inflight and max-completion-inflight must be positive")
	}
	if err := validateLoopbackAddress(*listen); err != nil {
		return err
	}
	if *upstreamValue == "" {
		return errors.New("upstream is required")
	}
	upstream, err := url.Parse(*upstreamValue)
	if err != nil {
		return fmt.Errorf("parse upstream: %w", err)
	}
	if upstream.Scheme != "http" && upstream.Scheme != "https" || upstream.Host == "" {
		return errors.New("upstream must be an absolute http or https URL")
	}
	workload := control.Workload(*workloadValue)
	if workload != control.WorkloadText && workload != control.WorkloadMedia {
		return errors.New("workload must be text or media")
	}
	if len(routes) == 0 {
		return errors.New("at least one execution route is required")
	}
	if !canonicalPath(*completionPath) {
		return errors.New("completion path must be canonical and absolute")
	}
	proxyConfig := workloadproxy.Config{
		Upstream: upstream, Workload: workload,
		ExecutionRoutes: routes, ReadOnlyRoutes: readOnlyRoutes, PassthroughRoutes: passthroughRoutes,
		CompletionPath: *completionPath, RequestIDHeader: *requestIDHeader,
		JobIDHeader: *jobIDHeader, FenceIDHeader: *fenceIDHeader,
		FenceEpochHeader: *fenceEpochHeader,
	}
	if err := workloadproxy.ValidateConfig(proxyConfig); err != nil {
		return err
	}
	return serveProxy(proxyConfig, proxyServerSettings{
		statePath: *statePath, listen: *listen,
		readHeaderTimeout: *readHeaderTimeout, idleTimeout: *idleTimeout,
		shutdownTimeout: *shutdownTimeout, completedWorkRetention: *completedWorkRetention,
		maxInflight: *maxInflight, maxCompletionInflight: *maxCompletionInflight,
	})
}

func serveProxy(proxyConfig workloadproxy.Config, settings proxyServerSettings) error {
	// Hold the shared lock until every in-flight handler has finished. Recovery
	// takes its exclusive counterpart before it can abandon unresolved work.
	proxyLock, err := lock.AcquireShared(settings.statePath + ".proxy.lock")
	if err != nil {
		return fmt.Errorf("acquire proxy lifetime lock: %w", err)
	}
	defer proxyLock.Close()
	stateStore, err := store.Open(context.Background(), settings.statePath)
	if err != nil {
		return fmt.Errorf("open state store: %w", err)
	}
	defer stateStore.Close()
	handler, err := workloadproxy.New(stateStore, proxyConfig)
	if err != nil {
		return err
	}
	tracked := &activeHandler{handler: handler, maxOrdinary: settings.maxInflight,
		maxCompletion: settings.maxCompletionInflight, completionPath: proxyConfig.CompletionPath}
	if tracked.completionPath == "" {
		tracked.completionPath = workloadproxy.DefaultCompletionPath
	}
	server := &http.Server{
		Addr: settings.listen, Handler: tracked, ReadHeaderTimeout: settings.readHeaderTimeout,
		IdleTimeout: settings.idleTimeout,
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	maintenanceCtx, stopMaintenance := context.WithCancel(ctx)
	maintenanceDone := make(chan struct{})
	go func() {
		defer close(maintenanceDone)
		maintainCompletedWork(maintenanceCtx, stateStore, settings.completedWorkRetention, time.Hour, func(err error) {
			log.Printf("completed work retention failed: %v", err)
		})
	}()
	defer func() {
		stopMaintenance()
		<-maintenanceDone
	}()
	result := make(chan error, 1)
	go func() {
		err := server.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		result <- err
	}()
	select {
	case err := <-result:
		return errors.Join(err, shutdownAndDrain(server, tracked, settings.shutdownTimeout))
	case <-ctx.Done():
		return shutdownAndDrain(server, tracked, settings.shutdownTimeout)
	}
}

func defaultStatePath() string {
	if root := os.Getenv("XDG_STATE_HOME"); root != "" {
		return filepath.Join(root, "gpu-workload-supervisor", "state.db")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "state.db"
	}
	return filepath.Join(home, ".local", "state", "gpu-workload-supervisor", "state.db")
}

func validateLoopbackAddress(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("invalid listen address: %w", err)
	}
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return errors.New("listen address must be loopback")
	}
	return nil
}

func canonicalPath(value string) bool {
	return value != "" && strings.HasPrefix(value, "/") && path.Clean(value) == value
}

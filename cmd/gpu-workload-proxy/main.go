package main

import (
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
	"strings"
	"syscall"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/deployment"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/lock"
	workloadproxy "github.com/mickey-kras/gpu-workload-supervisor/internal/proxy"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

type routesFlag []workloadproxy.Route

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
	statePath := flags.String("state", deployment.DefaultStatePath(), "SQLite state path")
	listen := flags.String("listen", "127.0.0.1:8090", "HTTP listen address")
	upstreamValue := flags.String("upstream", "", "absolute upstream URL")
	workloadValue := flags.String("workload", "", "required configured workload ID")
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
	upstream, err := parseUpstream(*upstreamValue)
	if err != nil {
		return err
	}
	workload := control.Workload(*workloadValue)
	if !control.ValidWorkloadID(workload) {
		return errors.New("workload must be a valid workload ID")
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
	return serveProxy(proxyConfig, proxyServerSettings{
		statePath: *statePath, listen: *listen,
		readHeaderTimeout: *readHeaderTimeout, idleTimeout: *idleTimeout,
		shutdownTimeout: *shutdownTimeout, completedWorkRetention: *completedWorkRetention,
		maxInflight: *maxInflight, maxCompletionInflight: *maxCompletionInflight,
	})
}

func parseUpstream(value string) (*url.URL, error) {
	upstream, err := url.Parse(value)
	if err != nil {
		return nil, fmt.Errorf("parse upstream: %w", err)
	}
	if upstream.Scheme != "http" && upstream.Scheme != "https" || upstream.Host == "" {
		return nil, errors.New("upstream must be an absolute http or https URL")
	}
	return upstream, nil
}

func serveProxy(proxyConfig workloadproxy.Config, settings proxyServerSettings) error {
	// Hold the shared lock until every in-flight handler has finished. Recovery
	// takes its exclusive counterpart before it can abandon unresolved work.
	proxyLock, err := lock.AcquireShared(settings.statePath + ".proxy.lock")
	if err != nil {
		return fmt.Errorf("acquire proxy lifetime lock: %w", err)
	}
	defer proxyLock.Close()
	if err := deployment.Check(settings.statePath, ""); err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	stateStore, err := store.Open(ctx, settings.statePath)
	if err != nil {
		return fmt.Errorf("open state store: %w", err)
	}
	defer stateStore.Close()
	handler, err := workloadproxy.NewWithContext(ctx, stateStore, proxyConfig)
	if err != nil {
		return err
	}
	tracked := workloadproxy.NewServer(handler, settings.maxInflight, settings.maxCompletionInflight, proxyConfig.CompletionPath)
	server := &http.Server{
		Addr: settings.listen, Handler: tracked, ReadHeaderTimeout: settings.readHeaderTimeout,
		IdleTimeout: settings.idleTimeout,
	}
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
		return errors.Join(err, tracked.ShutdownAndDrain(server, settings.shutdownTimeout))
	case <-ctx.Done():
		return tracked.ShutdownAndDrain(server, settings.shutdownTimeout)
	}
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

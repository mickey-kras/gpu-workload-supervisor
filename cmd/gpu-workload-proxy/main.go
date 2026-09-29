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
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
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
	var routes routesFlag
	var passthroughRoutes routesFlag
	flags.Var(&routes, "execute-route", "gated execution route as METHOD:/absolute/path; repeatable")
	flags.Var(&passthroughRoutes, "passthrough-route", "explicit ungated mutating route as METHOD:/absolute/path; repeatable")
	if err := flags.Parse(os.Args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
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
		ExecutionRoutes: routes, PassthroughRoutes: passthroughRoutes,
		CompletionPath: *completionPath, RequestIDHeader: *requestIDHeader,
		JobIDHeader: *jobIDHeader, FenceIDHeader: *fenceIDHeader,
		FenceEpochHeader: *fenceEpochHeader,
	}
	if err := workloadproxy.ValidateConfig(proxyConfig); err != nil {
		return err
	}
	stateStore, err := store.Open(context.Background(), *statePath)
	if err != nil {
		return fmt.Errorf("open state store: %w", err)
	}
	defer stateStore.Close()
	handler, err := workloadproxy.New(stateStore, proxyConfig)
	if err != nil {
		return err
	}
	server := &http.Server{
		Addr: *listen, Handler: handler, ReadHeaderTimeout: *readHeaderTimeout,
		IdleTimeout: *idleTimeout,
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
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
		return err
	case <-ctx.Done():
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), *shutdownTimeout)
		defer shutdownCancel()
		return server.Shutdown(shutdownCtx)
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

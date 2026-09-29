package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
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
	if !ok || strings.TrimSpace(method) == "" || !strings.HasPrefix(path, "/") {
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
	readHeaderTimeout := flags.Duration("read-header-timeout", 10*time.Second, "HTTP header read timeout")
	idleTimeout := flags.Duration("idle-timeout", 2*time.Minute, "HTTP idle timeout")
	shutdownTimeout := flags.Duration("shutdown-timeout", 30*time.Second, "graceful shutdown timeout")
	var routes routesFlag
	flags.Var(&routes, "execute-route", "gated execution route as METHOD:/absolute/path; repeatable")
	if err := flags.Parse(os.Args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	upstream, err := url.Parse(*upstreamValue)
	if err != nil {
		return fmt.Errorf("parse upstream: %w", err)
	}
	stateStore, err := store.Open(context.Background(), *statePath)
	if err != nil {
		return fmt.Errorf("open state store: %w", err)
	}
	defer stateStore.Close()
	handler, err := workloadproxy.New(stateStore, workloadproxy.Config{
		Upstream: upstream, Workload: control.Workload(*workloadValue),
		ExecutionRoutes: routes, RequestIDHeader: *requestIDHeader,
		JobIDHeader: *jobIDHeader, FenceIDHeader: *fenceIDHeader,
		FenceEpochHeader: *fenceEpochHeader,
		ErrorLog: func(err error) { log.Print(err) },
	})
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

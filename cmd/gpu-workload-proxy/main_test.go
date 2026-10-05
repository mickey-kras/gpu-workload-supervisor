package main

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/deployment"
	workloadproxy "github.com/mickey-kras/gpu-workload-supervisor/internal/proxy"
)

func TestRoutesFlagRequiresCanonicalMethodAndPath(t *testing.T) {
	var routes routesFlag
	for _, value := range []string{"POST", ":/execute", "POST:execute", "POST:/a/../execute"} {
		if err := routes.Set(value); err == nil {
			t.Fatalf("accepted route %q", value)
		}
	}
	if err := routes.Set(" post :/execute"); err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 || routes[0] != (workloadproxy.Route{Method: "POST", Path: "/execute"}) || routes.String() != "POST:/execute" {
		t.Fatalf("routes = %#v, string = %q", routes, routes.String())
	}
}

func TestProxyFlagValidation(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.db")
	tests := []struct {
		name, want string
		args       []string
	}{
		{"positional", "unexpected positional", []string{"extra"}},
		{"invalid retention", "completed-work-retention must be positive", []string{"-completed-work-retention", "0s"}},
		{"invalid ordinary capacity", "max-inflight and max-completion-inflight must be positive", []string{"-max-inflight", "0"}},
		{"invalid completion capacity", "max-inflight and max-completion-inflight must be positive", []string{"-max-completion-inflight", "0"}},
		{"public listener", "listen address must be loopback", []string{"-listen", "0.0.0.0:8090"}},
		{"malformed listener", "invalid listen address", []string{"-listen", "not-an-address"}},
		{"missing upstream", "upstream is required", nil},
		{"malformed upstream", "parse upstream", []string{"-upstream", "http://%"}},
		{"relative upstream", "upstream must be an absolute", []string{"-upstream", "/path"}},
		{"unknown workload", "workload must be a valid workload ID", []string{"-upstream", "http://127.0.0.1:1", "-workload", "INVALID"}},
		{"no routes", "at least one execution route", []string{"-upstream", "http://127.0.0.1:1", "-workload", "media"}},
		{"invalid finish path", "completion path must be canonical", []string{"-upstream", "http://127.0.0.1:1", "-workload", "media", "-execute-route", "POST:/execute", "-completion-path", "/a/../finish"}},
		{"mutating read-only", "read-only routes require", []string{"-upstream", "http://127.0.0.1:1", "-workload", "media", "-execute-route", "POST:/execute", "-read-only-route", "POST:/monitor"}},
		{"repeated read-only collision", "routes overlap", []string{"-upstream", "http://127.0.0.1:1", "-workload", "media", "-execute-route", "GET:/execute", "-read-only-route", "GET:/monitor", "-read-only-route", "GET:/execute"}},
		{"malformed read-only", "route must use", []string{"-read-only-route", "GET:monitor"}},
		{"route collision", "routes overlap", []string{"-upstream", "http://127.0.0.1:1", "-workload", "media", "-execute-route", "POST:/execute", "-passthrough-route", "POST:/execute"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			before := os.Args
			os.Args = append([]string{"gpu-workload-proxy", "-state", statePath}, test.args...)
			t.Cleanup(func() { os.Args = before })
			if err := run(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("run() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestProxyCannotStartWhenListenerIsOccupied(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	before := os.Args
	os.Args = []string{"gpu-workload-proxy", "-state", filepath.Join(t.TempDir(), "state.db"),
		"-listen", listener.Addr().String(), "-upstream", "http://127.0.0.1:1",
		"-workload", "media", "-execute-route", "POST:/execute"}
	t.Cleanup(func() { os.Args = before })
	if err := run(); err == nil || !strings.Contains(err.Error(), "address already in use") {
		t.Fatalf("run with occupied listener = %v", err)
	}
}

func TestDefaultStatePathAndLoopback(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	if got := deployment.DefaultStatePath(); got != filepath.Join(root, "gpu-workload-supervisor", "state.db") {
		t.Fatalf("state path = %q", got)
	}
	for _, address := range []string{"localhost:8090", "[::1]:8090", "127.0.0.1:8090"} {
		if err := validateLoopbackAddress(address); err != nil {
			t.Fatalf("%s: %v", address, err)
		}
	}
	for _, address := range []string{"example.com:8090", "[::]:8090"} {
		if err := validateLoopbackAddress(address); err == nil {
			t.Fatalf("accepted %s", address)
		}
	}
}

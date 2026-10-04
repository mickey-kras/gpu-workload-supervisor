package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/deployment"
	workloadproxy "github.com/mickey-kras/gpu-workload-supervisor/internal/proxy"
)

func TestManagedProxyRefusesMissingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	if err := deployment.Write(path, deployment.Marker{Version: 1, Release: deployment.Release}); err != nil {
		t.Fatal(err)
	}
	if err := serveProxy(workloadproxy.Config{}, proxyServerSettings{statePath: path}); err == nil {
		t.Fatal("accepted missing managed state")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("proxy created missing managed state", err)
	}
}

func TestLegacyProxyMayInitializeWithoutMarker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	// Invalid proxy configuration stops startup after the permitted legacy opener.
	if err := serveProxy(workloadproxy.Config{}, proxyServerSettings{statePath: path}); err == nil {
		t.Fatal("accepted invalid proxy configuration")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("legacy database was not initialized", err)
	}
}

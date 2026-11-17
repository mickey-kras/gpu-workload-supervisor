package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

// The policy tick runs the limit stop. With no unit with limits in
// the catalog, it removes a stale limit stop record.
func TestCLIIdlePolicyTickClearsStaleLimitRecord(t *testing.T) {
	previous := os.Args
	defer func() { os.Args = previous }()
	path := filepath.Join(t.TempDir(), "state.db")
	seedCatalog(t, path)
	ctx := context.Background()
	s, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkLimitFailure(ctx, "vision", true, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	os.Args = []string{"gpu-mode", "-state", path, "-systemctl", "/usr/bin/true", "idle-policy-tick"}
	if err := run(); err != nil {
		t.Fatal(err)
	}
	s, err = store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, ok, err := s.LimitStop(ctx); ok || err != nil {
		t.Fatalf("stale record after the tick = %v, %v", ok, err)
	}
}

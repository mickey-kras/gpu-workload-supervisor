package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/setup"
)

// Use the real process boundary: an unsafe systemctl must never get the chance
// to supply a plausible manager cgroup or create the marker.
func TestRenderOwnedRejectsUntrustedSystemctlBeforeExecution(t *testing.T) {
	for _, mode := range []string{"absolute", "relative", "PATH", "relative PATH", "relative PATH with ErrDot disabled", "user-owned executable", "writable executable", "writable ancestor", "alias"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			// A root-run test may have a trusted TMPDIR. Make the fixture
			// untrusted explicitly instead of relying on ambient ancestors.
			if err := os.Chmod(root, 0777); err != nil {
				t.Fatal(err)
			}
			bin := filepath.Join(root, "bin")
			if err := os.Mkdir(bin, 0755); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(root, "executed")
			t.Setenv("GWS_SYSTEMCTL_MARKER", marker)
			script := filepath.Join(bin, "systemctl")
			if err := os.WriteFile(script, []byte("#!/bin/sh\n: > \"$GWS_SYSTEMCTL_MARKER\"\nprintf 'ControlGroup=/user.slice/user-1000.slice/user@1000.service\\n'\n"), 0755); err != nil {
				t.Fatal(err)
			}
			path := script
			switch mode {
			case "relative":
				t.Chdir(root)
				path = "bin/systemctl"
			case "PATH":
				t.Setenv("PATH", bin)
				path = ""
			case "relative PATH", "relative PATH with ErrDot disabled":
				t.Chdir(root)
				t.Setenv("PATH", "bin")
				path = ""
				if mode == "relative PATH with ErrDot disabled" {
					t.Setenv("GODEBUG", "execerrdot=0")
				}
			case "user-owned executable":
				if os.Geteuid() == 0 {
					t.Skip("requires an unprivileged file owner")
				}
			case "writable executable":
				if err := os.Chmod(script, 0777); err != nil {
					t.Fatal(err)
				}
			case "writable ancestor":
				if err := os.Chmod(bin, 0777); err != nil {
					t.Fatal(err)
				}
			case "alias":
				path = filepath.Join(root, "systemctl-alias")
				if err := os.Symlink(script, path); err != nil {
					t.Fatal(err)
				}
			}
			input, err := json.Marshal(map[string]any{
				"systemctlPath": path,
				"draft": setup.Draft{ID: "vision", Label: "Vision", App: "vllm", Binding: &setup.DraftBinding{
					Instance: "owned", Owned: &setup.DraftOwnedLaunch{ModelPath: "/models/vision", Port: 9100},
				}},
			})
			if err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			err = systemActions().renderOwned(context.Background(), root, bytes.NewReader(input), &output)
			if _, markerErr := os.Stat(marker); !errors.Is(markerErr, os.ErrNotExist) {
				t.Fatalf("rejected systemctl executed: marker error %v, render error %v", markerErr, err)
			}
			if err == nil || output.Len() != 0 {
				t.Fatalf("untrusted systemctl accepted: %v, output %q", err, output.String())
			}
		})
	}
}

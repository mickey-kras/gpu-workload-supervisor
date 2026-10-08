package runtime

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

func TestLaunchSemicolonArgumentsRejectedBeforeLoadedCommandVerification(t *testing.T) {
	for _, app := range []string{"llama.cpp", "vllm"} {
		for _, field := range []string{"model path", "alias"} {
			t.Run(app+"/"+field, func(t *testing.T) {
				n := control.NativeModel{Runtime: app, Endpoint: "http://localhost:9000", Model: "selected"}
				raw := string(nativeLaunchFixture(t, app, n.Endpoint, n.Model))
				command := strings.TrimSpace(strings.Split(raw, "ExecStart=")[1])
				args := strings.Fields(command)
				modelPath := args[2]
				if field == "model path" {
					badPath := modelPath + ";part"
					if app == "vllm" {
						if err := os.Mkdir(badPath, 0700); err != nil {
							t.Fatal(err)
						}
					} else {
						if err := os.WriteFile(badPath, []byte("model"), 0600); err != nil {
							t.Fatal(err)
						}
					}
					raw = strings.Replace(raw, modelPath, badPath, 1)
				} else {
					n.Model = "a;b"
					raw = strings.Replace(raw, "selected", n.Model, 1)
				}
				command = strings.TrimSpace(strings.Split(raw, "ExecStart=")[1])
				loaded := "{ path=" + args[0] + " ; argv[]=" + command + " ; ignore_errors=no ; }"
				if !errors.Is(CheckLoadedLaunchCommand(loaded, command), ErrLaunchChanged) {
					t.Fatal("fixture did not reproduce the loaded-command delimiter mismatch")
				}
				if err := qualifyFixtureLaunch([]byte(raw), n); !errors.Is(err, ErrLaunchUnsupported) {
					t.Fatalf("owned source accepted an unverifiable command: %v", err)
				}
				path := filepath.Join(t.TempDir(), "model.service")
				if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
					t.Fatal(err)
				}
				if _, err := inspectAutomaticLaunchSourcesWithValidator(path, app, nil, fixtureExecutableValidator); !errors.Is(err, ErrLaunchUnsupported) {
					t.Fatalf("adopted source accepted an unverifiable command: %v", err)
				}
				unchanged, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(unchanged, []byte(raw)) {
					t.Fatal("rejected source was modified")
				}
			})
		}
	}
}

func TestLaunchSemicolonPreparationAndDropInPathsRejected(t *testing.T) {
	n := control.NativeModel{Runtime: "ollama", Endpoint: "http://localhost:9000", Model: "selected"}
	raw := nativeLaunchFixture(t, n.Runtime, n.Endpoint, n.Model)
	if err := qualifyFixtureLaunch(append(raw, []byte("ExecStartPre=/usr/bin/test -f /models/a;b.gguf\n")...), n); !errors.Is(err, ErrLaunchUnsupported) {
		t.Fatalf("delimiter in preparation operand accepted: %v", err)
	}
	path := filepath.Join(t.TempDir(), "model.service")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"10-options.conf", "10;options.conf"} {
		drop := filepath.Join(filepath.Dir(path), name)
		if err := os.WriteFile(drop, []byte("[Service]\nLimitNOFILE=65536\n"), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := inspectAutomaticLaunchSourcesWithValidator(path, n.Runtime, []string{drop}, fixtureExecutableValidator)
		if name == "10-options.conf" && err != nil {
			t.Fatalf("ordinary drop-in rejected: %v", err)
		}
		if name == "10;options.conf" && !errors.Is(err, ErrLaunchUnsupported) {
			t.Fatalf("delimiter in drop-in path accepted: %v", err)
		}
	}
}

//go:build systemd_integration

package supervisor

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
)

func TestSystemdOwnedVLLMArtifactSandbox(t *testing.T) {
	systemdCommand(t, "show", "--property=ControlGroup", "--value", "--", "-.slice")
	dir := t.TempDir()
	model := filepath.Join(dir, "model")
	if err := os.Mkdir(model, 0o700); err != nil {
		t.Fatal(err)
	}
	workload := control.Workload(fmt.Sprintf("sandbox-%d-%d", os.Getpid(), time.Now().UnixNano()))
	unit := control.OwnedUnitName(workload)
	executable := filepath.Join(dir, "vllm")
	weights := filepath.Join(model, "model.safetensors")
	template := filepath.Join(model, "chat_template.jinja")
	key := filepath.Join(dir, "vllm.env")
	launch := filepath.Join(dir, unit)
	const templateContents = "{{ messages }}"
	// Runs as the same user who owns the writable fixtures. Each distinct exit
	// status identifies the operation if the namespace allows an artifact write.
	const probe = `#!/bin/sh
set -eu
model=$2
weights=$model/model.safetensors
template=$model/chat_template.jinja
key=${0%/*}/vllm.env
[ "$(cat "$weights")" = weights ] || exit 10
[ -r "$template" ] && [ -r "$key" ] || exit 11
[ -n "${VLLM_API_KEY:-}" ] || exit 12
if touch "$model/new.safetensors"; then exit 20; fi
if ( : > "$weights" ); then exit 21; fi
if mv -f "$model/replacement" "$weights"; then exit 22; fi
if rm -f "$weights"; then exit 23; fi
if ( : > "$template" ); then exit 24; fi
if ( : > "$key" ); then exit 25; fi
if ( : > "$0" ); then exit 26; fi
[ -n "${XDG_CACHE_HOME:-}" ] && [ -n "${TMPDIR:-}" ] || exit 30
( mkdir -p "$XDG_CACHE_HOME" "$TMPDIR" &&
  printf cache > "$XDG_CACHE_HOME/qualification-cache" &&
  printf temp > "$TMPDIR/qualification-temp" &&
  [ "$(cat "$XDG_CACHE_HOME/qualification-cache")" = cache ] &&
  [ "$(cat "$TMPDIR/qualification-temp")" = temp ] ) || exit 31
`
	artifacts := map[string]string{
		weights:                             "weights",
		filepath.Join(model, "replacement"): "replacement",
		template:                            templateContents,
		key:                                 "VLLM_API_KEY=fixture-only-key\n",
		executable:                          probe,
	}
	for path, contents := range artifacts {
		mode := os.FileMode(0o600)
		if path == executable {
			mode = 0o700
		}
		if err := os.WriteFile(path, []byte(contents), mode); err != nil {
			t.Fatal(err)
		}
	}
	profile := control.WorkloadProfile{ID: workload, Unit: unit, NativeModel: &control.NativeModel{
		Runtime: "vllm", Instance: "fixture", LaunchFile: launch, ChatTemplate: template,
		ChatTemplateSHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(templateContents))),
		EnvironmentFile:    key,
		Owned:              &control.OwnedLaunch{ModelPath: model, Executable: executable, Port: 9100, ArtifactSandbox: true},
	}}
	rendered, err := gpuruntime.RenderOwnedUnit(profile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(launch, rendered, 0o600); err != nil {
		t.Fatal(err)
	}
	systemdCommand(t, "link", launch)
	t.Cleanup(func() {
		systemdCommand(t, "stop", unit)
		systemdCommand(t, "disable", unit)
		systemdCommand(t, "daemon-reload")
	})
	systemdCommand(t, "daemon-reload")
	systemdCommand(t, "start", unit)

	deadline := time.Now().Add(5 * time.Second)
	for {
		state := systemdCommand(t, "show", "--property=ActiveState", "--value", unit)
		if state == "failed" || state == "inactive" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("vLLM sandbox probe did not exit: %s", state)
		}
		time.Sleep(10 * time.Millisecond)
	}
	result := systemdCommand(t, "show", "--property=Result", "--property=ExecMainCode", "--property=ExecMainStatus", unit)
	for _, property := range []string{"Result=success", "ExecMainCode=1", "ExecMainStatus=0"} {
		if !strings.Contains("\n"+result+"\n", "\n"+property+"\n") {
			t.Fatalf("vLLM sandbox probe failed (see fixture exit statuses):\n%s", result)
		}
	}
	for path, want := range artifacts {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Errorf("artifact changed: %s (%v)", path, err)
		}
	}
	if _, err := os.Stat(filepath.Join(model, "new.safetensors")); !os.IsNotExist(err) {
		t.Fatalf("unexpected artifact creation: %v", err)
	}
}

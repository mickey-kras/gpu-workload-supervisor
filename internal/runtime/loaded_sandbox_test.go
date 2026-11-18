package runtime

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

type sandboxDriftRunner struct {
	CommandRunner
	started bool
}

func (r *sandboxDriftRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := r.CommandRunner.Run(ctx, name, args...)
	if slices.Contains(args, "start") {
		r.started = true
	}
	if r.started && slices.Contains(args, "--property=ProtectSystem") {
		return []byte(strings.Replace(string(out), "ProtectSystem=strict", "ProtectSystem=no", 1)), err
	}
	return out, err
}

func TestOwnedSandboxReadback(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	m, r := newSleepTestManager(t, "http://127.0.0.1:9111")
	p := m.config.Catalog.Profiles[0]
	ctx := context.Background()
	if err := m.verifyNativeBinding(ctx, p); err != nil {
		t.Fatal(err)
	}
	command := r.calls[len(r.calls)-1]
	good := string(r.outputs[command])
	// systemctl show keeps PrivateUsers boolean even on releases that also
	// expose PrivateUsersEx, prints mode in octal and expands %t at load.
	const writable = "/run/user/1000/gws-d253cbad16732b9d520413ee"
	for _, line := range []string{
		"Type=exec", "PrivateUsers=yes", "ProtectSystem=strict", "RuntimeDirectoryMode=0700",
		"ReadOnlyPaths=" + p.NativeModel.Owned.ModelPath, "ReadWritePaths=" + writable,
		"BindReadOnlyPaths=" + p.NativeModel.Owned.ModelPath + ":" + writable + "/model:rbind",
		"RuntimeDirectory=gws-d253cbad16732b9d520413ee",
	} {
		if !slices.Contains(strings.Split(good, "\n"), line) {
			t.Fatalf("readback expectation differs from systemctl show for %s", line)
		}
	}
	properties, err := ParseUnitProperties(r.outputs[command])
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"XDG_CACHE_HOME", "XDG_CONFIG_HOME", "TMPDIR", "FLASHINFER_WORKSPACE_BASE", "HF_HOME", "VLLM_CACHE_ROOT", "CUDA_CACHE_PATH", "TRITON_CACHE_DIR"} {
		if !slices.Contains(strings.Fields(properties["Environment"]), name+"="+writable) {
			t.Fatalf("missing expanded %s in loaded environment", name)
		}
	}
	for _, key := range []string{"Type", "ProtectSystem", "PrivateUsers", "ReadOnlyPaths", "ReadWritePaths", "BindReadOnlyPaths", "RuntimeDirectory", "RuntimeDirectoryMode"} {
		t.Run(key, func(t *testing.T) {
			if !slices.Contains(strings.Fields(command), "--property="+key) {
				t.Fatalf("missing %s in systemctl show", key)
			}
			for _, changed := range []string{"Missing=", key + "=unexpected "} {
				r.outputs[command] = []byte(strings.Replace(good, "\n"+key+"=", "\n"+changed, 1))
				if err := m.Healthy(ctx, p.ID); !errors.Is(err, ErrLaunchChanged) || !strings.Contains(err.Error(), key) {
					t.Fatalf("health accepted missing or changed %s: %v", key, err)
				}
			}
			r.outputs[command] = []byte(good)
		})
	}

	m.runner = &sandboxDriftRunner{CommandRunner: m.runner}
	err = m.Start(ctx, p.ID)
	if !errors.Is(err, ErrLaunchChanged) || !strings.Contains(err.Error(), "ProtectSystem") {
		t.Fatalf("post-start drift returned %v", err)
	}
	if !slices.Contains(r.calls, "/usr/bin/true --user stop -- "+p.Unit) {
		t.Fatal("failed sandbox readback left the started unit running")
	}
}

func TestOwnedSandboxLegacyBindingCanStop(t *testing.T) {
	m, r := newSleepTestManager(t, "http://127.0.0.1:9111")
	p := m.config.Catalog.Profiles[0]
	p.NativeModel.Owned.ArtifactSandbox = false
	if err := m.Start(context.Background(), p.ID); !errors.Is(err, ErrLaunchUnsupported) || !strings.Contains(err.Error(), "run setup") {
		t.Fatalf("legacy binding start = %v", err)
	}
	if slices.Contains(r.calls, "/usr/bin/true --user start -- "+p.Unit) {
		t.Fatal("legacy unit started")
	}
	writeEvents(t, m.cgroups.root, strings.TrimPrefix(p.Cgroup, "/"), "populated 0\n")
	if err := m.Stop(context.Background(), p.ID); err != nil {
		t.Fatalf("legacy binding stop = %v", err)
	}
}

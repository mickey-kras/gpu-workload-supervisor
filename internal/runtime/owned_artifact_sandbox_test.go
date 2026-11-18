package runtime

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

func TestOwnedArtifactSandboxRenderAndParse(t *testing.T) {
	root := t.TempDir()
	model := filepath.Join(root, "model")
	if err := os.Mkdir(model, 0700); err != nil {
		t.Fatal(err)
	}
	p := ownedRenderProfile("vllm", "vision", &control.OwnedLaunch{ModelPath: model, Port: 9100, ArtifactSandbox: true})
	n := p.NativeModel
	if err := os.WriteFile(n.ChatTemplate, []byte("template"), 0600); err != nil {
		t.Fatal(err)
	}
	runtimeRoot := filepath.Join(root, "run")
	t.Setenv("XDG_RUNTIME_DIR", runtimeRoot)

	n.Owned.ArtifactSandbox = false
	legacy, err := RenderOwnedUnit(p)
	if err != nil {
		t.Fatal(err)
	}
	n.Owned.ArtifactSandbox = true
	raw, err := RenderOwnedUnit(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		"PrivateUsers=yes", "ProtectSystem=strict", "ReadOnlyPaths=" + model,
		"ReadWritePaths=%t/gws-owned-vision-inference", "RuntimeDirectory=gws-owned-vision-inference",
		"RuntimeDirectoryMode=0700", "Environment=TMPDIR=%t/gws-owned-vision-inference",
		"Environment=VLLM_CACHE_ROOT=%t/gws-owned-vision-inference",
	} {
		if !strings.Contains(string(raw), "\n"+line+"\n") || strings.Contains(string(legacy), "\n"+line+"\n") {
			t.Errorf("sandbox line %q has incorrect placement", line)
		}
	}
	parsed, err := parseOwnedLaunchUnit(raw, *n)
	if err != nil {
		t.Fatal(err)
	}
	writable := filepath.Join(runtimeRoot, "gws-owned-vision-inference")
	if parsed.loadedProperties["ReadOnlyPaths"] != model || parsed.loadedProperties["ReadWritePaths"] != writable ||
		parsed.environmentValues["TMPDIR"] != writable || parsed.environmentValues["VLLM_CACHE_ROOT"] != writable {
		t.Fatalf("sandbox binding differs: properties=%v environment=%v", parsed.loadedProperties, parsed.environmentValues)
	}
	for _, modified := range []string{string(legacy), string(raw) + "ReadWritePaths=/\n", strings.Replace(string(raw), "ProtectSystem=strict", "ProtectSystem=full", 1)} {
		if _, err := parseOwnedLaunchUnit([]byte(modified), *n); !errors.Is(err, ErrLaunchUnsupported) {
			t.Fatalf("modified sandbox unit accepted: %v", err)
		}
	}

	outside := filepath.Join(root, "outside.jinja")
	if err := os.Rename(n.ChatTemplate, outside); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, n.ChatTemplate); err != nil {
		t.Fatal(err)
	}
	if _, err := parseOwnedLaunchUnit(raw, *n); !errors.Is(err, ErrLaunchUnsupported) {
		t.Fatalf("template alias outside model directory accepted: %v", err)
	}
}

func TestOwnedArtifactSandboxRejectsModelOverlappingWritablePath(t *testing.T) {
	root := t.TempDir()
	model := filepath.Join(root, "gws-owned-vision-inference")
	if err := os.Mkdir(model, 0700); err != nil {
		t.Fatal(err)
	}
	p := ownedRenderProfile("vllm", "vision", &control.OwnedLaunch{ModelPath: model, Port: 9100, ArtifactSandbox: true})
	if err := os.WriteFile(p.NativeModel.ChatTemplate, []byte("template"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", root)
	raw, err := RenderOwnedUnit(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseOwnedLaunchUnit(raw, *p.NativeModel); !errors.Is(err, ErrLaunchUnsupported) {
		t.Fatalf("model overlapping writable path accepted: %v", err)
	}
}

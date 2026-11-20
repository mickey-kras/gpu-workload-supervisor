package runtime

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

func TestAdoptingGeneratedArtifactSandboxRequiresManagedBinding(t *testing.T) {
	h := newHardenedFixture(t)
	p := ownedRenderProfile("vllm", "vision", &control.OwnedLaunch{
		ModelPath: filepath.Dir(h.template), Alias: "selected", Port: 9100, ArtifactSandbox: true,
	})
	n := h.bind(*p.NativeModel)
	n.LaunchFile = filepath.Join(t.TempDir(), p.Unit)
	p.NativeModel = &n
	for _, sandbox := range []bool{true, false} {
		n.Owned.ArtifactSandbox = sandbox
		raw, err := RenderOwnedUnit(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(n.LaunchFile, raw, 0600); err != nil {
			t.Fatal(err)
		}
		adopted := n
		adopted.Owned = nil
		_, err = inspectQualifiedNativeLaunch(n.LaunchFile, adopted, func(string) error { return nil })
		if sandbox {
			if !errors.Is(err, ErrLaunchUnsupported) || !strings.Contains(err.Error(), "keep that binding or select a separately qualified service to adopt") {
				t.Fatalf("sandbox adoption diagnostic = %v", err)
			}
		} else if err != nil {
			t.Fatalf("adopting compatible earlier unit = %v", err)
		}
	}
}

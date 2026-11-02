package setup

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestCustomStoppedExecutableRecoveryKeepsQualificationAndConsentSeparate(t *testing.T) {
	for _, app := range []string{"vllm"} {
		t.Run(app, func(t *testing.T) {
			name := app
			root := t.TempDir()
			path := filepath.Join(root, name)
			raw := []byte("fixture executable must never run")
			if err := os.WriteFile(path, raw, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0777); err != nil {
				t.Fatal(err)
			}
			request := ProbeRequest{App: app, Reference: path, ReferenceKind: "application"}
			blocked, err := Probe(context.Background(), request)
			if err != nil || !blocked.Installed || blocked.Recognized || blocked.Binding != nil || blocked.ConfigurationStatus != inspectionFailedStatus || len(blocked.Diagnostics) == 0 {
				t.Fatal("actual untrusted executable was hidden or qualified", blocked, err)
			}
			if blocked.Reference != path || blocked.LifecycleControl != "unverified" || len(blocked.Models) != 0 {
				t.Fatal("selection lost or runtime operation implied", blocked)
			}
			// Select a separate fixture for positive boundary wiring. No trusted
			// vLLM is installed in CI; the validator substitute does not
			// prove native runtime trust or override the unsafe selection above.
			replacement := filepath.Join(t.TempDir(), name)
			if err := os.WriteFile(replacement, raw, 0700); err != nil {
				t.Fatal(err)
			}
			request.Reference = replacement
			selected, err := probeReferenceWithExecutableValidator(context.Background(), request, candidate(request), func(kind, chosen string) error {
				if kind != app || chosen != replacement {
					t.Fatal("selection did not reach the trust boundary unchanged")
				}
				return nil
			})
			if err != nil || !selected.Installed || !selected.Recognized || selected.InstanceStatus != "installed" || selected.ConfigurationStatus != "model-required" || selected.LifecycleControl != "unverified" || selected.Endpoint != "" {
				t.Fatal("installed/configured/reachable states conflated", selected, err)
			}
			draft := Draft{ID: "chosen", App: app, Label: "Existing application", Reference: replacement, ReferenceKind: "application", Binding: selected.Binding, Model: "existing:latest"}
			draft.Binding.Owned.ModelPath = testModelPath("recovery-" + app)
			profile, rendered, err := OwnedProfile(draft, "/manager", root)
			if err != nil || profile.NativeModel.Owned.Executable != replacement || len(rendered) == 0 {
				t.Fatal("selection failed to reach the managed preview", err)
			}
			if _, err := os.Stat(profile.NativeModel.LaunchFile); !os.IsNotExist(err) {
				t.Fatal("preview created a service before activation consent", err)
			}
			for _, selectedPath := range []string{path, replacement} {
				if got, err := os.ReadFile(selectedPath); err != nil || !bytes.Equal(got, raw) {
					t.Fatal("probe or preview changed the selected application", err)
				}
			}
		})
	}
}

package runtime

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

func TestExecutableRejectsReplaceableOriginalAncestorBeforeStart(t *testing.T) {
	attacker := t.TempDir()
	alias := filepath.Join(attacker, "installation")
	// A public trusted target makes this test independent of root-only fixtures.
	// The leaf remains a regular executable according to Probe's Lstat rule.
	if err := os.Symlink("/usr/bin", alias); err != nil {
		t.Fatal(err)
	}
	selected := filepath.Join(alias, "true")
	if info, err := os.Lstat(selected); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("invalid alias fixture: %v", err)
	}
	if _, err := validateExecutable(selected); err == nil || !strings.Contains(err.Error(), "original executable path component") || strings.Contains(err.Error(), attacker) {
		t.Fatalf("replaceable original path trusted or disclosed: %v", err)
	}
	raw := []byte("[Service]\nExecStart=" + selected + " x\n")
	u, err := parseExternalLaunchSources([][]byte{raw}, "llama.cpp")
	if err != nil {
		t.Fatal(err)
	}
	p := control.WorkloadProfile{ID: "selected", Unit: "selected.service", NativeModel: &control.NativeModel{Runtime: "llama.cpp", LaunchFile: filepath.Join(t.TempDir(), "selected.service")}}
	p.NativeModel.LaunchSHA256 = fmt.Sprintf("%x", sha256.Sum256(raw))
	if err := os.WriteFile(p.NativeModel.LaunchFile, raw, 0600); err != nil {
		t.Fatal(err)
	}
	r := &identityRunner{}
	m := &SystemdManager{config: SystemdConfig{Catalog: &control.Catalog{Profiles: []control.WorkloadProfile{p}}}, runner: r}
	requireTrustFailure := func(phase string, err error) {
		t.Helper()
		if err == nil || !errors.Is(err, ErrLaunchUnsupported) || !strings.Contains(err.Error(), "executable trust") || strings.Contains(err.Error(), attacker) || strings.Contains(err.Error(), selected) {
			t.Fatalf("%s lost public trust refusal or disclosed selected path: %v", phase, err)
		}
		reason := strings.Contains(err.Error(), "original executable path component")
		if phase == "after-alias-swap" {
			reason = reason || strings.Contains(err.Error(), "executable ancestor component") || strings.Contains(err.Error(), "executable target is not root-owned")
		}
		if !reason {
			t.Fatalf("%s lost phase-appropriate executable trust reason: %v", phase, err)
		}
	}
	for _, phase := range []string{"trusted-target", "after-alias-swap"} {
		if phase == "after-alias-swap" {
			replacement := filepath.Join(attacker, "replacement")
			if err := os.Mkdir(replacement, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(replacement, "true"), []byte("untrusted replacement, never executed"), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(alias); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(replacement, alias); err != nil {
				t.Fatal(err)
			}
		}
		requireTrustFailure(phase, qualifyParsedLaunch(u, control.NativeModel{Runtime: "llama.cpp"}, validateNativeExecutable))
		requireTrustFailure(phase, m.Start(context.Background(), p.ID))
		if len(r.calls) != 0 {
			t.Fatalf("%s contacted systemd before executable proof: %v", phase, r.calls)
		}
	}
	// Existing trusted merged-/usr aliases retain support and original settings.
	for _, path := range []string{"/usr/bin/true", "/bin/true"} {
		if _, err := validateExecutable(path); err != nil {
			t.Fatalf("trusted alias rejected: %s: %v", path, err)
		}
	}
}

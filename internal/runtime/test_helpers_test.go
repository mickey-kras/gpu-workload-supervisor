package runtime

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

const computeAppsCommand = "/usr/bin/true --query-compute-apps=pid,process_name,used_memory --format=csv,noheader,nounits -i 0"

func first(m *SystemdManager, _ string) *SystemdManager { return m }

func remove(t *testing.T, root, name string) {
	t.Helper()
	if err := os.RemoveAll(filepath.Join(root, name)); err != nil {
		t.Fatal(err)
	}
}

// These wrappers give tests direct access to production checks that the
// runtime reaches through larger call paths.

func inspectAutomaticLaunch(raw []byte, app string, validate func(string) error) (AutomaticLaunch, error) {
	u, err := parseLaunchUnit(raw)
	if err != nil {
		return AutomaticLaunch{}, err
	}
	if err := u.validatePreCommands(validate); err != nil {
		return AutomaticLaunch{}, err
	}
	return inspectParsedAutomaticLaunch(u, raw, app, validate)
}

func expandAccountHome(value string) (string, error) {
	snapshot := accountHomeSnapshot{read: localAccountHome}
	return snapshot.expand(value)
}

func verifyNativeLaunchWithValidator(n control.NativeModel, validate func(string) error) error {
	_, err := readVerifiedNativeLaunch(n, validate)
	return err
}

func verifyOwnedSpec(p control.WorkloadProfile) error {
	return verifyOwnedSpecWithValidator(p, validateNativeExecutable)
}

func (m *SystemdManager) preflightOwned(ctx context.Context, unitDir string) error {
	return m.preflightOwnedWithRemovals(ctx, unitDir, nil)
}

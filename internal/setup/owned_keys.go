package setup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/vllmkey"
)

// pinOwnedFiles pins the launch file and the key file of an owned profile to
// the setup home. The catalog layer checks only the suffix (it is
// host-independent); here the exact home is known, so the pin happens before
// anything is written or committed.
func pinOwnedFiles(p control.WorkloadProfile, home string, plan unitPlan) error {
	if p.NativeModel.LaunchFile != filepath.Join(ownedUnitDirectory(home), p.Unit) {
		return fmt.Errorf(ownedUnitErrorFormat, ErrOwnedLaunchFileOutsideHome, p.Unit)
	}
	if !p.NativeModel.Hardened() {
		return nil
	}
	if p.NativeModel.EnvironmentFile != OwnedEnvironmentFile(home, p.ID) {
		return fmt.Errorf(ownedUnitErrorFormat, ErrOwnedEnvironmentFileOutsideHome, p.Unit)
	}
	plan.keys[p.Unit] = p.NativeModel.EnvironmentFile
	return nil
}

// ensureOwnedKey creates the private key file of an owned vLLM unit before
// the unit that names it is written. It keeps a usable key file.
func ensureOwnedKey(plan unitPlan, name string) error {
	path, ok := plan.keys[name]
	if !ok {
		return nil
	}
	_, err := os.Lstat(path)
	missing := errors.Is(err, os.ErrNotExist)
	ensureErr := vllmkey.Ensure(path)
	// A key file that this apply created is recorded even when a later step
	// of Ensure failed, so the rollback removes it.
	if _, err := os.Lstat(path); missing && err == nil && plan.createdKeys != nil {
		plan.createdKeys[name] = true
	}
	if ensureErr != nil {
		return fmt.Errorf("%s: %w", name, ensureErr)
	}
	return nil
}

// removeCreatedKeys removes the key files that a failed apply created. After
// a crash before the commit, a created key file stays without a journal
// entry. It is harmless: the next apply for the workload reuses the usable
// file, and removing the workload removes it.
func removeCreatedKeys(plan unitPlan) {
	for name := range plan.createdKeys {
		_ = os.Remove(plan.keys[name])
	}
}

// removeOwnedKey removes the private key file of a removed owned unit. It
// keeps the file when a binding of the committed catalog still names it, for
// example a workload that moved to an existing launch with the same key file.
func removeOwnedKey(home, name string, catalogs ...control.Catalog) error {
	id := strings.TrimSuffix(strings.TrimPrefix(name, control.OwnedUnitFilePrefix), ".service")
	path := OwnedEnvironmentFile(home, control.Workload(id))
	for _, catalog := range catalogs {
		for _, p := range catalog.Profiles {
			if p.NativeModel != nil && p.NativeModel.EnvironmentFile == path {
				return nil
			}
		}
	}
	err := os.Remove(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove the key file of %s: %w", name, err)
	}
	return nil
}

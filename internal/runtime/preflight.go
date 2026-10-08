package runtime

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

var ErrOrphanedOwnedUnit = errors.New("supervisor-owned unit not in catalog")

func (m *SystemdManager) Preflight(ctx context.Context) error {
	return m.preflight(ctx, nil)
}

// PreflightWithOwnedRemovals permits only exact accepted unit contents that
// setup will remove after committing the replacement catalog.
func (m *SystemdManager) PreflightWithOwnedRemovals(ctx context.Context, removals map[string]string) error {
	return m.preflight(ctx, removals)
}

func (m *SystemdManager) preflight(ctx context.Context, removals map[string]string) error {
	if err := m.preflightBindings(ctx); err != nil {
		return err
	}
	if err := m.verifyManagerCgroup(ctx); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, p := range m.config.Catalog.Profiles {
		if seen[p.Unit] {
			continue
		}
		seen[p.Unit] = true
		if err := m.preflightWorkloadCgroup(ctx, p.Unit, p.Cgroup); err != nil {
			return err
		}
	}
	unitDir, err := m.ownedUnitDirectory()
	if err != nil {
		return err
	}
	return m.preflightOwnedWithRemovals(ctx, unitDir, removals)
}

// PreflightAdopted verifies existing bindings during a setup preview. Owned
// units may not exist yet; Apply must run full Preflight after rendering them.
func (m *SystemdManager) PreflightAdopted(ctx context.Context, removals map[string]string) error {
	if err := m.verifyManagerCgroup(ctx); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, p := range m.config.Catalog.Profiles {
		if p.NativeModel != nil && p.NativeModel.Owned != nil {
			if err := m.verifyOwnedSpec(p); err != nil {
				return err
			}
			continue
		}
		if err := m.verifyNativeBinding(ctx, p); err != nil {
			return err
		}
		if seen[p.Unit] {
			continue
		}
		seen[p.Unit] = true
		if err := m.preflightWorkloadCgroup(ctx, p.Unit, p.Cgroup); err != nil {
			return err
		}
	}
	unitDir, err := m.ownedUnitDirectory()
	if err != nil {
		return err
	}
	return m.preflightOwnedWithRemovals(ctx, unitDir, removals)
}

// lookupAccountID is a seam for account-resolution faults.
var lookupAccountID = user.LookupId

// ownedUnitDirectory resolves the setup-managed unit directory from the
// account record of the running euid — the same source setup uses — never
// from the ambient HOME, and fails closed when the account is unresolvable.
func (m *SystemdManager) ownedUnitDirectory() (string, error) {
	if m.config.OwnedUnitDir != "" {
		return m.config.OwnedUnitDir, nil
	}
	account, err := lookupAccountID(strconv.Itoa(os.Geteuid()))
	if err != nil || account.HomeDir == "" {
		return "", fmt.Errorf("owned unit directory unresolvable for euid %d: %w", os.Geteuid(), err)
	}
	return filepath.Join(account.HomeDir, ".config/systemd/user"), nil
}

// preflightOwned fails closed on supervisor-owned unit files the current
// catalog cannot account for. It reads the directory only; unit contents are
// verified elsewhere by digest.
func (m *SystemdManager) preflightOwned(ctx context.Context, unitDir string) error {
	return m.preflightOwnedWithRemovals(ctx, unitDir, nil)
}

func (m *SystemdManager) preflightOwnedWithRemovals(_ context.Context, unitDir string, removals map[string]string) error {
	if unitDir == "" {
		return nil
	}
	entries, err := os.ReadDir(unitDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	managed, adopted := m.ownedBindings()
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "gws-owned-") || !strings.HasSuffix(name, ".service") || managed[name] {
			continue
		}
		path := filepath.Join(unitDir, name)
		if matchesUnitDigest(path, adopted[path]) || matchesUnitDigest(path, removals[name]) {
			continue
		}
		return ErrOrphanedOwnedUnit
	}
	return nil
}

func matchesUnitDigest(path, proof string) bool {
	if proof == "" {
		return false
	}
	data, err := os.ReadFile(path)
	return err == nil && fmt.Sprintf("%x", sha256.Sum256(data)) == proof
}

func (m *SystemdManager) preflightWorkloadCgroup(ctx context.Context, unit, group string) error {
	state, err := m.unitState(ctx, unit)
	if err != nil {
		return err
	}
	if !state.hasCgroup || (state.active == "active" && state.cgroup == "") {
		return errors.New("workload ControlGroup metadata unavailable")
	}
	// systemd can remove an empty cgroup after either a clean stop or a
	// crash. Neither this nor populated groups prove workload release;
	// recovery must still stop units and verify release independently.
	allowRemoved := (state.active == "inactive" && state.sub == "dead") ||
		(state.active == "failed" && state.sub == "failed")
	if err := m.cgroups.check(group, allowRemoved); err != nil && !errors.Is(err, errCgroupPopulated) {
		return err
	}
	return nil
}

func (m *SystemdManager) preflightBindings(ctx context.Context) error {
	if m.config.Catalog != nil {
		for _, p := range m.config.Catalog.Profiles {
			if err := m.verifyOwnedSpec(p); err != nil {
				return err
			}
			if err := m.verifyNativeBinding(ctx, p); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *SystemdManager) ownedBindings() (map[string]bool, map[string]string) {
	managed := map[string]bool{}
	// An owned profile converted to adopted keeps its launch file on disk; an
	// exact binding (this path with a proven fingerprint) accounts for it,
	// while a drifted fingerprint stays fail-closed.
	adopted := map[string]string{}
	if m.config.Catalog != nil {
		for _, p := range m.config.Catalog.Profiles {
			if p.NativeModel == nil {
				continue
			}
			if p.NativeModel.Owned != nil {
				managed[p.Unit] = true
				continue
			}
			if p.AdoptedOwnedFile() {
				adopted[p.NativeModel.LaunchFile] = p.NativeModel.LaunchSHA256
			}
		}
	}
	return managed, adopted
}

func (m *SystemdManager) verifyOwnedSpec(p control.WorkloadProfile) error {
	validate := m.nativeExecutableValidator
	if validate == nil {
		validate = validateNativeExecutable
	}
	return verifyOwnedSpecWithValidator(p, validate)
}

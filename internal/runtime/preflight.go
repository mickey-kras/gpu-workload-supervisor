package runtime

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var ErrOrphanedOwnedUnit = errors.New("supervisor-owned unit not in catalog")

func (m *SystemdManager) Preflight(ctx context.Context) error {
	if m.config.Catalog != nil {
		for _, p := range m.config.Catalog.Profiles {
			if err := verifyOwnedSpec(p); err != nil {
				return err
			}
			if err := m.verifyNativeBinding(ctx, p); err != nil {
				return err
			}
		}
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
	return m.preflightOwned(ctx, m.ownedUnitDirectory())
}

func (m *SystemdManager) ownedUnitDirectory() string {
	if m.config.OwnedUnitDir != "" {
		return m.config.OwnedUnitDir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config/systemd/user")
}

// preflightOwned fails closed on supervisor-owned unit files the current
// catalog cannot account for. It reads the directory only; unit contents are
// verified elsewhere by digest.
func (m *SystemdManager) preflightOwned(_ context.Context, unitDir string) error {
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
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "gws-owned-") || !strings.HasSuffix(name, ".service") || managed[name] {
			continue
		}
		path := filepath.Join(unitDir, name)
		if sha, ok := adopted[path]; ok {
			data, err := os.ReadFile(path)
			if err == nil && fmt.Sprintf("%x", sha256.Sum256(data)) == sha {
				continue
			}
		}
		return ErrOrphanedOwnedUnit
	}
	return nil
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

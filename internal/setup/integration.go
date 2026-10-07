package setup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type integration struct {
	Version int    `json:"version"`
	Unit    string `json:"unit"`
	Target  string `json:"target"`
}

const (
	reconcileUnit = "gpu-workload-supervisor-reconcile.service"
	idleTimerUnit = "gpu-workload-supervisor-idle.timer"
)

// unitEnablement describes one packaged user unit's enablement: the unit
// name, the refusal-message label (stable: the packaged lifecycle fixture
// matches on it), the wants directory, and the ownership record file.
type unitEnablement struct {
	unit           string
	label          string
	wantsDirectory string
	recordName     string
}

var (
	reconcileEnablement = unitEnablement{reconcileUnit, "reconciliation", "default.target.wants", "integration.json"}
	idleTimerEnablement = unitEnablement{idleTimerUnit, "idle timer", "timers.target.wants", "integration-idle.json"}
)

func (b Backend) enableReconciliation(ctx context.Context, home, systemctl string) error {
	return b.enableUserUnit(ctx, home, systemctl, reconcileEnablement)
}

// enableIdleTimer mirrors the reconciliation enablement exactly: the packaged
// timer is only link-enabled (no --now, no daemon), preexisting user overrides
// are refused, and ownership is recorded for symmetric removal.
func (b Backend) enableIdleTimer(ctx context.Context, home, systemctl string) error {
	return b.enableUserUnit(ctx, home, systemctl, idleTimerEnablement)
}

func (b Backend) enableUserUnit(ctx context.Context, home, systemctl string, e unitEnablement) error {
	userDir := filepath.Join(home, ".config/systemd/user")
	if err := mkdirTrusted(userDir); err != nil {
		return err
	}
	userUnit := filepath.Join(userDir, e.unit)
	if _, err := os.Lstat(userUnit); err == nil {
		return fmt.Errorf("user %s unit exists; refusing override", e.label)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	wants := filepath.Join(userDir, e.wantsDirectory)
	if err := mkdirTrusted(wants); err != nil {
		return err
	}
	target := "/usr/lib/systemd/user/" + e.unit
	link := filepath.Join(wants, e.unit)
	if info, err := os.Lstat(link); err == nil {
		destination, err := os.Readlink(link)
		if err != nil || info.Mode()&os.ModeSymlink == 0 || destination != target {
			return fmt.Errorf("unowned %s enablement exists", e.label)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// Record planned ownership before the standard unit manager creates the link.
	if err := writeJSON(filepath.Join(home, ".config/gpu-workload-supervisor", e.recordName), integration{1, e.unit, target}); err != nil {
		return err
	}
	// Subprocess output is untrusted terminal input (control characters, unit
	// payload echoes); the error carries only the exit status.
	if _, err := b.runCommand(ctx, systemctl, "--user", "enable", e.unit); err != nil {
		return fmt.Errorf("enable %s: %w", e.label, err)
	}
	return nil
}

// RemoveIntegration removes only the recorded enablement links. It does not stop
// any service and preserves profiles, state, audit, models and user workload units.
func RemoveIntegration(home string) error {
	return SystemBackend().RemoveIntegration(context.Background(), home)
}

// RemoveIntegration is all-or-nothing: both integrations (records and links)
// are validated and the idle timer is stopped before either owned link is
// deleted, so a failed removal leaves the installation fully intact.
func (b Backend) RemoveIntegration(ctx context.Context, home string) error {
	reconcile, err := inspectIntegration(home, reconcileEnablement)
	if err != nil {
		return err
	}
	idle, err := inspectIntegration(home, idleTimerEnablement)
	if err != nil {
		return err
	}
	// A login may have activated the idle timer: removing only the wants link
	// leaves the loaded unit firing every 60 seconds until the user manager
	// exits, and the preserved operator profile keeps the service condition
	// true. Stop the timer before deleting anything; if it was never enabled
	// there is nothing to stop. (The reconcile unit is a login-triggered
	// oneshot, so nothing recurring persists for it.)
	if idle.recorded {
		if _, err := b.runCommand(ctx, "/usr/bin/systemctl", "--user", "stop", idleTimerUnit); err != nil {
			return fmt.Errorf("stop idle timer: %w", err)
		}
	}
	if err := idle.remove(); err != nil {
		return err
	}
	return reconcile.remove()
}

// pendingRemoval is a validated integration awaiting deletion.
type pendingRemoval struct {
	recordPath string
	linkPath   string
	recorded   bool
	hasLink    bool
}

// inspectIntegration validates the ownership record and the recorded link
// without touching anything; a foreign or modified link is reported, never
// removed.
func inspectIntegration(home string, e unitEnablement) (pendingRemoval, error) {
	root := filepath.Join(home, ".config/gpu-workload-supervisor")
	if err := TrustedDirectory(root); err != nil {
		return pendingRemoval{}, err
	}
	recordPath := filepath.Join(root, e.recordName)
	data, err := privateRead(recordPath)
	if errors.Is(err, os.ErrNotExist) {
		return pendingRemoval{}, nil
	}
	if err != nil {
		return pendingRemoval{}, err
	}
	var owned integration
	if err := json.Unmarshal(data, &owned); err != nil {
		return pendingRemoval{}, err
	}
	if owned.Version != 1 || owned.Unit != e.unit || owned.Target != "/usr/lib/systemd/user/"+e.unit {
		return pendingRemoval{}, errors.New("invalid integration ownership record")
	}
	directory := filepath.Join(home, ".config/systemd/user", e.wantsDirectory)
	if err := TrustedDirectory(directory); err != nil {
		return pendingRemoval{}, err
	}
	linkPath := filepath.Join(directory, e.unit)
	target, err := os.Readlink(linkPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return pendingRemoval{}, err
	}
	removal := pendingRemoval{recordPath: recordPath, linkPath: linkPath, recorded: true, hasLink: err == nil}
	if removal.hasLink && target != owned.Target {
		return pendingRemoval{}, errors.New("modified integration link is preserved")
	}
	return removal, nil
}

func (p pendingRemoval) remove() error {
	if !p.recorded {
		return nil
	}
	if p.hasLink {
		if err := os.Remove(p.linkPath); err != nil {
			return err
		}
	}
	return os.Remove(p.recordPath)
}

package runtime

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

var ErrTemporaryInvocationChanged = errors.New("temporary discovery invocation changed or start evidence is missing; inspect and stop the service manually, then retry cleanup")

func (m *SystemdManager) temporaryProperties(ctx context.Context, unit string) (map[string]string, error) {
	raw, err := m.runner.Run(ctx, m.config.SystemctlPath, "--user", "show", "--property=Id,LoadState,ActiveState,SubState,ControlGroup,Slice,FragmentPath,DropInPaths,NeedDaemonReload,InvocationID", "--no-pager", "--", unit)
	if err != nil {
		return nil, SafeError("temporary cleanup observation failed", err)
	}
	return ParseUnitProperties(raw)
}

func (m *SystemdManager) verifyTemporaryCleanup(ctx context.Context, v control.TemporaryDiscoveryCandidate) (map[string]string, error) {
	if !automaticUnitName.MatchString(v.Unit) || strings.HasPrefix(v.Unit, control.OwnedUnitFilePrefix) || validateCgroup(v.Cgroup) != nil || v.SystemdSlice != "app.slice" {
		return nil, ErrLaunchUnsupported
	}
	if err := control.ValidateLaunchSources(v.LaunchFile, v.DropIns); err != nil {
		return nil, err
	}
	sources, err := readLaunchSources(v.LaunchFile, v.DropIns)
	if err != nil {
		return nil, err
	}
	if fmt.Sprintf("%x", sha256.Sum256(sources[0])) != v.LaunchSHA256 {
		return nil, ErrLaunchChanged
	}
	service, err := m.temporaryProperties(ctx, v.Unit)
	if err != nil {
		return nil, err
	}
	if err := CheckNativeBindingSources(service, v.Unit, v.LaunchFile, v.DropIns); err != nil {
		return nil, err
	}
	if service["NeedDaemonReload"] != "no" || service["Slice"] != "app.slice" {
		return nil, ErrLaunchChanged
	}
	root, err := m.temporaryProperties(ctx, "-.slice")
	if err != nil {
		return nil, err
	}
	slice, err := m.temporaryProperties(ctx, "app.slice")
	if err != nil {
		return nil, err
	}
	group, err := ResolveAutomaticCgroup(v.Unit, service, slice, root)
	if err != nil {
		return nil, err
	}
	if group != v.Cgroup {
		return nil, ErrLaunchChanged
	}
	if err := m.cgroups.check(root["ControlGroup"], false); err != nil {
		return nil, err
	}
	return service, nil
}

// StopTemporaryDiscovery only cleans up an existing recorded invocation.
func (m *SystemdManager) StopTemporaryDiscovery(ctx context.Context, v control.TemporaryDiscoveryCandidate, invocation string) error {
	properties, err := m.verifyTemporaryCleanup(ctx, v)
	if err != nil {
		return err
	}
	if properties["ActiveState"] != "inactive" || properties["SubState"] != "dead" {
		if !validInvocationID(invocation) || properties["InvocationID"] != invocation || properties["ActiveState"] != "active" || properties["SubState"] != "running" || properties["ControlGroup"] != v.Cgroup {
			return ErrTemporaryInvocationChanged
		}
		if err := m.runSystemctl(ctx, "stop", v.Unit); err != nil {
			return err
		}
		properties, err = m.verifyTemporaryCleanup(ctx, v)
		if err != nil {
			return err
		}
		if properties["ActiveState"] != "inactive" || properties["SubState"] != "dead" {
			return ErrTemporaryInvocationChanged
		}
	}
	return m.cgroups.empty(v.Cgroup)
}

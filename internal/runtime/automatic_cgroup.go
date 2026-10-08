package runtime

import (
	"context"
	"errors"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"path"
	"regexp"
	"strconv"
	"strings"
)

var automaticUnitName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*\.service$`)

// ResolveAutomaticCgroup accepts a reported group, or systemd's supported
// ordinary service placement under the loaded app.slice. Templates, escaped
// names and other slices require an explicit qualified binding.
func ResolveAutomaticCgroup(unit string, service, slice, manager map[string]string) (string, error) {
	if !automaticUnitName.MatchString(unit) || strings.HasPrefix(unit, "cgroup.") || strings.HasPrefix(unit, "cpu.") || strings.HasPrefix(unit, "cpuacct.") || strings.HasPrefix(unit, "memory.") || strings.HasPrefix(unit, "io.") || strings.HasPrefix(unit, "pids.") || strings.HasPrefix(unit, "cpuset.") || strings.HasPrefix(unit, "blkio.") || strings.HasPrefix(unit, "devices.") || strings.HasPrefix(unit, "freezer.") || strings.HasPrefix(unit, "net_cls.") || strings.HasPrefix(unit, "net_prio.") || strings.HasPrefix(unit, "hugetlb.") || strings.HasPrefix(unit, "rdma.") || strings.HasPrefix(unit, "misc.") || strings.HasPrefix(unit, "bpf-firewall.") || strings.HasPrefix(unit, "bpf-devices.") || strings.HasPrefix(unit, "bpf-foreign.") || strings.HasPrefix(unit, "bpf-socket-bind.") || strings.HasPrefix(unit, "bpf-restrict-network-interfaces.") || strings.HasPrefix(unit, "perf_event.") || strings.HasPrefix(unit, "debug.") || strings.HasPrefix(unit, "dmem.") || service["Id"] != unit || service["LoadState"] != "loaded" {
		return "", errors.New("select a loaded ordinary service installation")
	}
	root := manager["ControlGroup"]
	if manager["LoadState"] != "loaded" || manager["ActiveState"] != "active" || manager["SubState"] != "active" || root == "/" || !strings.HasPrefix(root, "/") || path.Clean(root) != root {
		return "", errors.New("systemd manager placement evidence unavailable")
	}
	group := service["ControlGroup"]
	if group != "" {
		if !strings.HasPrefix(group, root+"/") || path.Clean(group) != group {
			return "", errors.New("service placement is outside the user manager")
		}
		return group, nil
	}
	if service["ActiveState"] != "inactive" || service["SubState"] != "dead" || service["Slice"] != "app.slice" || slice["Id"] != "app.slice" || slice["LoadState"] != "loaded" || slice["ActiveState"] != "active" || slice["SubState"] != "active" || slice["ControlGroup"] != root+"/app.slice" {
		return "", errors.New("stopped installation placement cannot be verified; configure its supported app.slice launch and retry")
	}
	return slice["ControlGroup"] + "/" + unit, nil
}

// SupportedSystemdPlacementVersion restricts predicted stopped placement to qualified
// implementations; the systemd naming algorithm is not a public API.
func SupportedSystemdPlacementVersion(output []byte) (uint16, error) {
	fields := strings.Fields(string(output))
	if len(fields) < 2 || fields[0] != "systemd" {
		return 0, errors.New("systemd version evidence unavailable")
	}
	version, err := strconv.ParseUint(fields[1], 10, 16)
	if err != nil || (version != 252 && version != 255 && version != 259) {
		return 0, errors.New("stopped placement is not qualified for this systemd version; configure an installation with reported ControlGroup evidence")
	}
	return uint16(version), nil
}
func (m *SystemdManager) verifyAutomaticPlacement(ctx context.Context, p control.WorkloadProfile) error {
	if p.SystemdSlice == "" {
		return nil
	}
	out, err := m.runner.Run(ctx, m.config.SystemctlPath, "--version")
	if err != nil {
		return err
	}
	version, err := SupportedSystemdPlacementVersion(out)
	if err != nil {
		return err
	}
	if version != p.SystemdVersion {
		return errors.New("systemd version changed; refresh installation configuration")
	}
	read := func(unit string) (map[string]string, error) {
		out, err := m.runner.Run(ctx, m.config.SystemctlPath, "--user", "show", "--property=Id,LoadState,ControlGroup,ActiveState,SubState,Slice", "--no-pager", "--", unit)
		if err != nil {
			return nil, err
		}
		return ParseUnitProperties(out)
	}
	service, err := read(p.Unit)
	if err != nil {
		return err
	}
	root, err := read("-.slice")
	if err != nil {
		return err
	}
	slice, err := read(p.SystemdSlice)
	if err != nil {
		return err
	}
	group, err := ResolveAutomaticCgroup(p.Unit, service, slice, root)
	if err != nil {
		return err
	}
	if group != p.Cgroup {
		return errors.New("automatic service placement changed; refresh setup")
	}
	return nil
}

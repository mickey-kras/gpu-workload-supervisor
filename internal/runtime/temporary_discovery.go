package runtime

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

const temporaryApplicationSlice = "app.slice"

var ErrTemporaryInvocationChanged = errors.New("temporary discovery invocation changed or start evidence is ambiguous; refusing to stop this service. Inspect the unit and the persisted session evidence, stop it manually if appropriate, then retry explicit cleanup")

func (m *SystemdManager) temporaryProperties(ctx context.Context, unit string) (map[string]string, error) {
	out, err := m.runner.Run(ctx, m.config.SystemctlPath, "--user", "show", "--property=Id,LoadState,ActiveState,SubState,ControlGroup,Slice,FragmentPath,DropInPaths,NeedDaemonReload,InvocationID,ActiveEnterTimestampMonotonic,ExecStart,ExecStartPre,Environment", "--no-pager", "--", unit)
	if err != nil {
		return nil, SafeError("temporary discovery unit observation failed", err)
	}
	return ParseUnitProperties(out)
}

func (m *SystemdManager) verifyTemporaryBinding(ctx context.Context, v control.TemporaryDiscoveryCandidate) (map[string]string, error) {
	return m.verifyTemporaryBindingMode(ctx, v, true)
}

// Cleanup checks exact invocation ownership and cgroup release even when GPU
// observation or newly qualified environment settings are no longer available.
// Those admission proofs cannot be used to obstruct stopping our invocation.
func (m *SystemdManager) verifyTemporaryBindingMode(ctx context.Context, v control.TemporaryDiscoveryCandidate, admission bool) (map[string]string, error) {
	if !automaticUnitName.MatchString(v.Unit) || strings.HasPrefix(v.Unit, control.OwnedUnitFilePrefix) || validateCgroup(v.Cgroup) != nil || v.SystemdSlice != temporaryApplicationSlice {
		return nil, ErrLaunchUnsupported
	}
	validate := m.nativeExecutableValidator
	if validate == nil {
		validate = validateNativeExecutable
	}
	paths := make([]string, len(v.DropIns))
	for i, source := range v.DropIns {
		paths[i] = source.Path
	}
	launch, err := inspectAutomaticLaunchSourcesWithValidator(v.LaunchFile, "ollama", paths, validate)
	if err != nil {
		return nil, err
	}
	if launch.SHA256 != v.LaunchSHA256 || launch.Endpoint != v.Endpoint || !control.EqualLaunchSources(launch.DropIns, v.DropIns) {
		return nil, ErrLaunchChanged
	}
	if admission {
		if launch.GPUUUID != v.GPUUUID {
			return nil, ErrLaunchChanged
		}
		if err := m.verifyProfileGPU(ctx, control.WorkloadProfile{NativeModel: &control.NativeModel{GPUUUID: v.GPUUUID}}); err != nil {
			return nil, err
		}
	}
	service, err := m.temporaryProperties(ctx, v.Unit)
	if err != nil {
		return nil, err
	}
	if admission {
		if err := CheckLoadedEnvironment(service["Environment"], launch.Environment); err != nil {
			return nil, err
		}
	}
	if err := CheckLoadedLaunchCommand(service["ExecStart"], launch.Command); err != nil {
		return nil, err
	}
	if err := CheckLoadedPreCommands(service["ExecStartPre"], launch.PreCommands); err != nil {
		return nil, err
	}
	if service["NeedDaemonReload"] != "no" {
		return nil, ErrLaunchChanged
	}
	if err = CheckNativeBindingSources(service, v.Unit, v.LaunchFile, v.DropIns); err != nil {
		return nil, err
	}
	if err := m.verifyTemporaryPlacement(ctx, v, service); err != nil {
		return nil, err
	}
	return service, nil
}

func (m *SystemdManager) verifyTemporaryPlacement(ctx context.Context, v control.TemporaryDiscoveryCandidate, service map[string]string) error {
	out, err := m.runner.Run(ctx, m.config.SystemctlPath, "--version")
	if err != nil {
		return err
	}
	version, err := SupportedSystemdPlacementVersion(out)
	if err != nil {
		return err
	}
	if version != v.SystemdVersion {
		return ErrLaunchChanged
	}
	root, err := m.temporaryProperties(ctx, "-.slice")
	if err != nil {
		return err
	}
	slice, err := m.temporaryProperties(ctx, temporaryApplicationSlice)
	if err != nil {
		return err
	}
	if service["Slice"] != temporaryApplicationSlice {
		return ErrLaunchChanged
	}
	group, err := ResolveAutomaticCgroup(v.Unit, service, slice, root)
	if err != nil {
		return err
	}
	if group != v.Cgroup {
		return ErrLaunchChanged
	}
	if err := m.cgroups.check(root["ControlGroup"], false); err != nil {
		return err
	}
	return nil
}

// PrepareTemporaryDiscovery proves prior stopped state and release without
// starting, selecting, downloading, or loading a model.
func (m *SystemdManager) PrepareTemporaryDiscovery(ctx context.Context, v control.TemporaryDiscoveryCandidate) error {
	props, err := m.verifyTemporaryBinding(ctx, v)
	if err != nil {
		return err
	}
	if props["ActiveState"] != "inactive" || props["SubState"] != "dead" {
		return errors.New("temporary discovery requires a stopped service; use read-only discovery for running applications")
	}
	if err = m.cgroups.empty(v.Cgroup); err != nil {
		return err
	}
	return m.ReleasedFor(ctx, control.WorkloadIdle)
}

func (m *SystemdManager) StartTemporaryDiscovery(ctx context.Context, v control.TemporaryDiscoveryCandidate) (control.TemporaryDiscoveryLaunchEvidence, error) {
	evidence := control.TemporaryDiscoveryLaunchEvidence{}
	if err := m.PrepareTemporaryDiscovery(ctx, v); err != nil {
		return evidence, err
	}
	prior, err := m.temporaryProperties(ctx, v.Unit)
	if err != nil {
		return evidence, err
	}
	if prior["ActiveState"] != "inactive" || prior["SubState"] != "dead" {
		return evidence, ErrTemporaryInvocationChanged
	}
	evidence.PriorInvocationID = prior["InvocationID"]
	output, startErr := m.runner.Run(ctx, m.config.SystemctlPath, "--user", "--show-transaction", "--job-mode=fail", "start", "--", v.Unit)
	anchor := regexp.MustCompile(`(?m)^Enqueued anchor job ([1-9][0-9]*) ` + regexp.QuoteMeta(v.Unit) + `/start\.$`).FindSubmatch(output)
	if len(anchor) == 2 {
		evidence.JobID = string(anchor[1])
	}
	// Read evidence independently of cancellation: a command deadline may fire
	// after systemd accepted the job. The caller also has bounded cleanup.

	observeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	props, err := m.verifyTemporaryBindingMode(observeCtx, v, false)
	if err != nil {
		return evidence, err
	}
	id := props["InvocationID"]
	if props["ActiveState"] != "active" || props["SubState"] != "running" || !validInvocationID(id) || id == evidence.PriorInvocationID || evidence.JobID == "" || props["ActiveEnterTimestampMonotonic"] == "" || props["ActiveEnterTimestampMonotonic"] == "0" || props["ActiveEnterTimestampMonotonic"] == prior["ActiveEnterTimestampMonotonic"] || props["ControlGroup"] != v.Cgroup {
		return evidence, ErrTemporaryInvocationChanged
	}
	evidence.InvocationID = id
	evidence.ActivationTimestamp = props["ActiveEnterTimestampMonotonic"]
	// Retain independently verified invocation evidence before the extra
	// admission checks, so a GPU/env observation failure can still be cleaned up.
	if _, err := m.verifyTemporaryBinding(observeCtx, v); err != nil {
		return evidence, errors.Join(startErr, err)
	}
	return evidence, startErr
}
func validInvocationID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, r := range id {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return id != strings.Repeat("0", 32)
}

// StopTemporaryDiscovery refuses an unknown or replaced invocation. A stopped
// service can be finalized read-only after exact binding and release checks.
func (m *SystemdManager) StopTemporaryDiscovery(ctx context.Context, v control.TemporaryDiscoveryCandidate, invocation string) error {
	props, err := m.verifyTemporaryBindingMode(ctx, v, false)
	if err != nil {
		return err
	}
	if props["ActiveState"] == "inactive" && props["SubState"] == "dead" {
		if err = m.cgroups.empty(v.Cgroup); err != nil {
			return err
		}
		return m.ReleasedFor(ctx, control.WorkloadIdle)
	}
	if !validInvocationID(invocation) || props["InvocationID"] != invocation || props["ActiveState"] != "active" || props["SubState"] != "running" || props["ControlGroup"] != v.Cgroup {
		return ErrTemporaryInvocationChanged
	}
	if err = m.runSystemctl(ctx, "stop", v.Unit); err != nil {
		return err
	}
	props, err = m.verifyTemporaryBindingMode(ctx, v, false)
	if err != nil {
		return err
	}
	if props["ActiveState"] != "inactive" || props["SubState"] != "dead" {
		return ErrTemporaryInvocationChanged
	}
	if err = m.cgroups.empty(v.Cgroup); err != nil {
		return err
	}
	return m.ReleasedFor(ctx, control.WorkloadIdle)
}

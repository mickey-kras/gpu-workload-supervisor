package runtime

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

var ErrOwnedRender = errors.New("owned launch cannot be rendered to the supported unit subset")

// Owned binaries are packaged executables; the deterministic render names the
// packaged path and the round-trip qualification proves it is trusted. It is a
// variable only so tests can render against fixture executables.
var ownedBinaryDirectory = "/usr/bin"

// RenderOwnedUnit deterministically renders the static systemd user unit the
// supervisor owns for an owned launch profile. The output deliberately uses
// the direct launch grammar plus exactly derived owned dependency directives.
// It carries no [Install] section so nothing auto-starts at login.
func RenderOwnedUnit(p control.WorkloadProfile) ([]byte, error) {
	n := p.NativeModel
	if n == nil || n.Owned == nil {
		return nil, fmt.Errorf("%w: profile %s has no owned launch spec", ErrOwnedRender, p.ID)
	}
	var b strings.Builder
	if !control.ValidOwnedConflicts(p.Unit, n.Owned.Conflicts) {
		return nil, fmt.Errorf("%w: invalid owned conflicts", ErrOwnedRender)
	}
	b.WriteString("[Unit]\nDescription=Supervisor-owned " + n.Runtime + " launch for " + string(p.ID) + "\n")
	b.WriteString(ownedDependencies(p.Unit, n.Owned.Conflicts))
	b.WriteString("\n[Service]\nType=simple\nRestart=no\n")
	// A binding from an earlier release has no hardened settings. It keeps
	// its exact earlier bytes and fingerprint, so setup can prove and remove
	// or replace the unit; qualification rejects it for execution.
	hardened := n.Hardened()
	if hardened {
		b.WriteString("Environment=" + offlineHubName + "=1\n")
		b.WriteString("EnvironmentFile=" + n.EnvironmentFile + "\n")
	}
	var execStart string
	executable := n.Owned.Executable
	if executable == "" {
		executable = ownedBinaryDirectory + "/" + n.Runtime
	}
	if !control.ValidOwnedExecutable(n.Runtime, executable) {
		return nil, fmt.Errorf("%w: unsupported executable", ErrOwnedRender)
	}
	switch n.Runtime {
	case "vllm":
		execStart = executable + " serve " + n.Owned.ModelPath + " --host " + hardenedBindHost + " --port " + strconv.Itoa(int(n.Owned.Port))
		if hardened {
			execStart += " --load-format safetensors --chat-template " + n.ChatTemplate
		}
		if n.Owned.MaxModelLen != 0 {
			execStart += " --max-model-len " + strconv.FormatUint(uint64(n.Owned.MaxModelLen), 10)
		}
		if n.Owned.Alias != "" {
			execStart += " --served-model-name " + n.Owned.Alias
		}
	default:
		return nil, fmt.Errorf("%w: runtime %s", ErrOwnedRender, n.Runtime)
	}
	b.WriteString("ExecStart=" + execStart + "\n")
	return []byte(b.String()), nil
}

// OwnedCgroup derives the workload cgroup from the systemd manager root.
func OwnedCgroup(managerRoot string, p control.WorkloadProfile) string {
	return strings.TrimSuffix(managerRoot, "/") + "/app.slice/" + p.Unit
}

// renderMustQualify is the render⇄verify round-trip invariant: rendered output
// must pass the same qualification as an adopted unit on this host.
func renderMustQualify(raw []byte, n control.NativeModel) error {
	if err := qualifyNativeLaunchWithValidator(raw, n, validateNativeExecutable); err != nil {
		return fmt.Errorf("%w: %v", ErrOwnedRender, err)
	}
	return nil
}

// QualifyOwnedUnit renders an owned profile's unit and qualifies it against
// this host, proving the packaged executable is present and trusted and the
// model path exists with the right type before anything becomes durable.
func QualifyOwnedUnit(p control.WorkloadProfile) error {
	if p.NativeModel == nil || p.NativeModel.Owned == nil {
		return nil
	}
	raw, err := RenderOwnedUnit(p)
	if err != nil {
		return err
	}
	return renderMustQualify(raw, *p.NativeModel)
}

// QualifyOwnedUnitBeforeKey qualifies an owned launch like QualifyOwnedUnit,
// except that its private key file can be missing: apply creates it before
// it writes the unit. An existing key file must be usable.
func QualifyOwnedUnitBeforeKey(p control.WorkloadProfile) error {
	if p.NativeModel == nil || p.NativeModel.Owned == nil {
		return nil
	}
	raw, err := RenderOwnedUnit(p)
	if err != nil {
		return err
	}
	return qualifyOwnedRender(raw, *p.NativeModel, validateNativeExecutable, true)
}

// qualifyOwnedRender qualifies a rendered owned unit. With keyMayBeMissing, a
// missing key file passes, because apply creates it before it writes the
// unit; an existing key file must be usable.
func qualifyOwnedRender(raw []byte, n control.NativeModel, validate func(string) error, keyMayBeMissing bool) error {
	unit, err := parseOwnedLaunchUnit(raw, n)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrOwnedRender, err)
	}
	unit.keyFilePending = keyMayBeMissing && KeyFileMissing(n.EnvironmentFile)
	if err := qualifyParsedLaunch(unit, n, validate); err != nil {
		return fmt.Errorf("%w: %v", ErrOwnedRender, err)
	}
	return nil
}

// KeyFileMissing reports a key file path that names no file. Setup and the
// runtime share this rule for a key file that apply creates.
func KeyFileMissing(path string) bool {
	_, err := os.Lstat(path)
	return path != "" && errors.Is(err, os.ErrNotExist)
}

// A total lexical order gives every conflicting pair one ordering edge and no
// cycles. systemd orders stopping before starting with either edge direction.
func ownedDependencies(unit, peers string) string {
	if peers == "" {
		return ""
	}
	lines := "Conflicts=" + peers + "\n"
	before := []string{}
	for _, peer := range strings.Fields(peers) {
		if peer < unit {
			before = append(before, peer)
		}
	}
	if len(before) > 0 {
		lines += "After=" + strings.Join(before, " ") + "\n"
	}
	return lines
}

// Keep the adopted grammar unchanged. Only an owned spec may contribute these
// exact generated dependency lines; duplicate, extra, moved or changed lines
// are left to the strict parser to reject.
func parseOwnedLaunchUnit(raw []byte, n control.NativeModel) (parsedLaunchUnit, error) {
	if n.Owned == nil {
		return parseLaunchUnit(raw)
	}
	unit := filepath.Base(n.LaunchFile)
	if !control.ValidOwnedConflicts(unit, n.Owned.Conflicts) {
		return parsedLaunchUnit{}, ErrLaunchUnsupported
	}
	dependencies := ownedDependencies(unit, n.Owned.Conflicts)
	if dependencies != "" {
		// Match the renderer's placement directly after Description in [Unit].
		lines := strings.SplitN(string(raw), "\n", 3)
		if len(lines) != 3 || lines[0] != "[Unit]" || !strings.HasPrefix(lines[1], "Description=") || !strings.HasPrefix(lines[2], dependencies) {
			return parsedLaunchUnit{}, ErrLaunchUnsupported
		}
		raw = []byte(lines[0] + "\n" + lines[1] + "\n" + strings.TrimPrefix(lines[2], dependencies))
	}
	return parseLaunchUnit(raw)
}

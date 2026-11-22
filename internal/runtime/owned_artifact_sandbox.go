package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

const sandboxDirectoryMode = "0700"

var sandboxEnvironment = [...]string{
	"XDG_CACHE_HOME", "XDG_CONFIG_HOME", "TMPDIR", "FLASHINFER_WORKSPACE_BASE",
	"HF_HOME", "VLLM_CACHE_ROOT", "CUDA_CACHE_PATH", "TRITON_CACHE_DIR",
}

func ownedArtifactSandbox(n control.NativeModel) (string, error) {
	model := n.Owned.ModelPath
	if !n.Hardened() || !validSandboxPath(model) || filepath.Dir(n.ChatTemplate) != model {
		return "", fmt.Errorf("%w: the artifact sandbox requires a local model directory with its pinned chat template", ErrLaunchUnsupported)
	}
	unit, id := sandboxUnitIdentity(n)
	if !control.ValidInstanceID(string(id)) || control.OwnedUnitName(id) != unit {
		return "", fmt.Errorf("%w: the artifact sandbox requires an owned unit name", ErrLaunchUnsupported)
	}
	dir := sandboxDirectory(unit)
	var b strings.Builder
	// Keep later host mounts from replacing parts of the read-only model tree.
	b.WriteString("PrivateUsers=yes\nProtectSystem=strict\nMountFlags=private\nReadOnlyPaths=" + model + "\n")
	b.WriteString("ReadWritePaths=%t/" + dir + "\nRuntimeDirectory=" + dir + "\nRuntimeDirectoryMode=" + sandboxDirectoryMode + "\n")
	// A bind to a different destination makes systemd fail unit execution
	// when mount namespacing is unavailable instead of ignoring the sandbox.
	b.WriteString("BindReadOnlyPaths=" + sandboxModelBind(model, "%t/"+dir) + "\n")
	for _, name := range sandboxEnvironment {
		b.WriteString("Environment=" + name + "=%t/" + dir + "\n")
	}
	return b.String(), nil
}

func sandboxModelBind(model, runtimeDir string) string {
	return model + ":" + runtimeDir + "/model:rbind"
}

func sandboxUnitIdentity(n control.NativeModel) (string, control.Workload) {
	unit := filepath.Base(n.LaunchFile)
	return unit, control.Workload(strings.TrimSuffix(strings.TrimPrefix(unit, control.OwnedUnitFilePrefix), ".service"))
}

func sandboxDirectory(unit string) string {
	// vLLM creates Unix sockets under TMPDIR. Leave room for its UUID even
	// when a workload uses the longest allowed unit name.
	hash := sha256.Sum256([]byte(unit))
	return fmt.Sprintf("gws-%x", hash[:12])
}

func validSandboxPath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && path != "/" &&
		!strings.ContainsAny(path, launchExpansionCharacters+":;*?[]") &&
		strings.IndexFunc(path, unicode.IsSpace) < 0 && strings.IndexFunc(path, unicode.IsControl) < 0
}

func (u *parsedLaunchUnit) verifyOwnedArtifactSandbox(raw []byte, n control.NativeModel) error {
	unit, id := sandboxUnitIdentity(n)
	expected, err := RenderOwnedUnit(control.WorkloadProfile{ID: id, Unit: unit, NativeModel: &n})
	if err != nil || !bytes.Equal(raw, expected) {
		return ErrLaunchUnsupported
	}
	return u.recordOwnedArtifactSandbox(n)
}

func (u *parsedLaunchUnit) recordOwnedArtifactSandbox(n control.NativeModel) error {
	model := n.Owned.ModelPath
	resolved, err := trustPath(model, kindDir)
	if err != nil {
		return err
	}
	template, err := trustPath(n.ChatTemplate, kindFile)
	if err != nil {
		return err
	}
	if !sandboxContains(resolved, template) {
		return fmt.Errorf("%w: the pinned chat template resolves outside its model directory", ErrLaunchUnsupported)
	}
	runtimeRoot := os.Getenv("XDG_RUNTIME_DIR")
	if runtimeRoot == "" {
		runtimeRoot = "/run/user/" + strconv.Itoa(os.Geteuid())
	}
	dir := sandboxDirectory(filepath.Base(n.LaunchFile))
	writable := filepath.Join(runtimeRoot, dir)
	if !validSandboxPath(runtimeRoot) {
		return fmt.Errorf("%w: invalid systemd user runtime directory", ErrLaunchUnsupported)
	}
	for _, excluded := range []string{writable, "/dev", "/proc", "/sys"} {
		if sandboxContains(excluded, resolved) || sandboxContains(resolved, excluded) {
			return fmt.Errorf("%w: model artifacts overlap a writable systemd path", ErrLaunchUnsupported)
		}
	}

	if u.loadedProperties == nil {
		u.loadedProperties = map[string]string{}
	}
	for key, value := range map[string]string{
		"Type": "exec", "PrivateUsers": "yes", "ProtectSystem": "strict", "ReadOnlyPaths": model,
		"ReadWritePaths": writable, "RuntimeDirectory": dir, "RuntimeDirectoryMode": sandboxDirectoryMode,
		"BindReadOnlyPaths": sandboxModelBind(model, writable),
		"MountFlags":        "private",
	} {
		u.loadedProperties[key] = value
	}
	if u.environmentValues == nil {
		u.environmentValues = map[string]string{}
	}
	for _, name := range sandboxEnvironment {
		if _, found := u.environmentValues[name]; found {
			return environmentError(name, "overrides the generated artifact sandbox")
		}
		u.environmentValues[name] = writable
	}
	return nil
}

func sandboxContains(parent, path string) bool {
	return parent == path || strings.HasPrefix(path, strings.TrimSuffix(parent, "/")+"/")
}

// ErrSandboxNamespace tells that systemd could not create the namespaces of
// the artifact sandbox, usually because the host restricts unprivileged user
// namespaces.
var ErrSandboxNamespace = errors.New("the host does not let the systemd user manager create the artifact sandbox namespaces; install packaging/apparmor/gpu-workload-supervisor-systemd-user, load it with `sudo apparmor_parser --replace`, then restart the user manager")

// exitNamespace is the systemd exit status of a failed namespace setup.
const exitNamespace = "226"

// sandboxStartError names the host fix when the unit failed to set up its
// namespaces. Other start failures keep their original error.
func (m *SystemdManager) sandboxStartError(ctx context.Context, unit string, err error) error {
	out, showErr := m.runner.Run(ctx, m.config.SystemctlPath, systemctlUserFlag, "show", "--property=ExecMainStatus", "--", unit)
	if showErr != nil {
		return err
	}
	values, parseErr := ParseUnitProperties(out)
	if parseErr != nil || values["ExecMainStatus"] != exitNamespace {
		return err
	}
	return errors.Join(ErrSandboxNamespace, err)
}

package runtime

import (
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
	"HOME", "XDG_CACHE_HOME", "XDG_CONFIG_HOME", "TMPDIR",
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
	b.WriteString("PrivateUsers=yes\nProtectSystem=strict\nReadOnlyPaths=" + model + "\n")
	b.WriteString("ReadWritePaths=%t/" + dir + "\nRuntimeDirectory=" + dir + "\nRuntimeDirectoryMode=" + sandboxDirectoryMode + "\n")
	for _, name := range sandboxEnvironment {
		b.WriteString("Environment=" + name + "=%t/" + dir + "\n")
	}
	return b.String(), nil
}

func sandboxUnitIdentity(n control.NativeModel) (string, control.Workload) {
	unit := filepath.Base(n.LaunchFile)
	return unit, control.Workload(strings.TrimSuffix(strings.TrimPrefix(unit, control.OwnedUnitFilePrefix), ".service"))
}

func sandboxDirectory(unit string) string {
	return strings.TrimSuffix(unit, ".service") + "-inference"
}

func validSandboxPath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && path != "/" &&
		!strings.ContainsAny(path, launchExpansionCharacters+";*?[]") &&
		strings.IndexFunc(path, unicode.IsSpace) < 0 && strings.IndexFunc(path, unicode.IsControl) < 0
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
		"PrivateUsers": "yes", "ProtectSystem": "strict", "ReadOnlyPaths": model,
		"ReadWritePaths": writable, "RuntimeDirectory": dir, "RuntimeDirectoryMode": sandboxDirectoryMode,
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

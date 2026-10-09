package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"golang.org/x/sys/unix"
)

var environmentName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Only whole-assignment quotes are accepted. Escapes, expansion, concatenated
// fragments and values containing whitespace remain unsupported; diagnostics
// contain the variable name only, never its value.
func environmentAssignments(value string) ([]string, error) {
	var assignments []string
	for len(strings.TrimSpace(value)) > 0 {
		value = strings.TrimSpace(value)
		assignment := ""
		if value[0] == '\'' || value[0] == '"' {
			quote := value[0]
			value = value[1:]
			end := strings.IndexByte(value, quote)
			if end < 0 {
				return nil, environmentError(environmentHint(value), "unterminated whole-assignment quote")
			}
			assignment, value = value[:end], value[end+1:]
			if value != "" && value[0] != ' ' && value[0] != '\t' {
				return nil, environmentError(environmentHint(assignment), "quoted fragments are unsupported")
			}
		} else {
			end := strings.IndexAny(value, " \t")
			if end < 0 {
				assignment, value = value, ""
			} else {
				assignment, value = value[:end], value[end:]
			}
		}
		name, _, ok := strings.Cut(assignment, "=")
		if !ok || !environmentName.MatchString(name) {
			return nil, environmentError("", "expected NAME=value")
		}
		if strings.ContainsAny(assignment, launchExpansionCharacters+"\r\n\t ") {
			return nil, environmentError(name, "expansion, escapes or whitespace are unsupported; use one literal whole assignment")
		}
		assignments = append(assignments, assignment)
	}
	return assignments, nil
}
func environmentHint(value string) string {
	name, _, ok := strings.Cut(strings.TrimLeft(value, "\"'"), "=")
	if !ok {
		return ""
	}
	if environmentName.MatchString(name) {
		return name
	}
	return ""
}
func environmentError(name, reason string) error {
	if name != "" {
		name = " variable " + name
	}
	return fmt.Errorf("%w: Environment%s: %s", ErrLaunchUnsupported, name, reason)
}

func (u *parsedLaunchUnit) applySafeEnvironment(runtimeName, assignment string) error {
	name, value, ok := strings.Cut(assignment, "=")
	if !ok || !environmentName.MatchString(name) {
		return environmentError("", "expected NAME=value")
	}
	if u.environment == nil {
		u.environment = map[string]bool{}
	}
	if u.environment[name] {
		return environmentError(name, "duplicate assignment")
	}
	u.environment[name] = true
	if u.environmentValues == nil {
		u.environmentValues = map[string]string{}
	}
	u.environmentValues[name] = value
	switch name {
	case "CUDA_VISIBLE_DEVICES":
		if !control.ValidGPUUUID(value) {
			return environmentError(name, "only one full physical GPU UUID is supported; numeric indices, UUID prefixes, lists and MIG mappings cannot prove the configured GPU identity. Select its full UUID and review the configured GPU")
		}
		u.gpuUUID = value
		return nil
	case "HOME", "HF_HOME", "HUGGINGFACE_HUB_CACHE", "TRANSFORMERS_CACHE", "XDG_CACHE_HOME", "TMPDIR", "HF_TOKEN_PATH":
		if err := qualifyEnvironmentPath(value, name == "HF_TOKEN_PATH", false); err != nil {
			return environmentError(name, "requires an existing absolute clean trusted path without links; inspect ownership and permissions of this setting and its ancestors")
		}
		return nil
	case "PATH":
		for _, path := range strings.Split(value, ":") {
			if err := qualifyEnvironmentSearchPath(path); err != nil {
				return environmentError(name, "requires absolute clean root-owned executable directories; aliases must be root-owned and resolve into trusted directories with no group/world writes")
			}
		}
		return nil
	default:
		if runtimeName == "ollama" && strings.HasPrefix(name, "OLLAMA_") {
			if err := u.applyOllamaEnvironment(assignment); err == nil {
				return nil
			}
		}
		return environmentError(name, "this setting is not qualified; choose a supported direct configuration with independently verified settings")
	}
}

// Descriptor-relative metadata inspection does not read credentials or follow
// symlinks. The source principal policy remains unchanged for all path parts.
func qualifyEnvironmentPath(path string, file, rootOnly bool) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsAny(path, launchExpansionCharacters) {
		return ErrLaunchUnsupported
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return ErrLaunchUnsupported
	}
	defer func() { unix.Close(fd) }()
	var rootStat unix.Stat_t
	if unix.Fstat(fd, &rootStat) != nil || rootStat.Uid != 0 || rootStat.Mode&0022 != 0 {
		return ErrLaunchUnsupported
	}
	if file && path == "/" {
		return ErrLaunchUnsupported
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if path == "/" {
		parts = nil
	}
	for i, part := range parts {
		flags := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_CLOEXEC
		leaf := i == len(parts)-1
		if !leaf || !file {
			flags |= unix.O_DIRECTORY
		}
		next, err := unix.Openat(fd, part, flags, 0)
		if err != nil {
			return ErrLaunchUnsupported
		}
		unix.Close(fd)
		fd = next
		var stat unix.Stat_t
		if unix.Fstat(fd, &stat) != nil {
			return ErrLaunchUnsupported
		}
		if rootOnly {
			if stat.Uid != 0 || stat.Mode&0022 != 0 {
				return ErrLaunchUnsupported
			}
		} else if !trustedLaunchSourceComponent(stat, i, len(parts)+1) {
			return ErrLaunchUnsupported
		}
		if leaf && file && (stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&0444 == 0) {
			return ErrLaunchUnsupported
		}
	}
	return nil
}

func qualifyEnvironmentSearchPath(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return ErrLaunchUnsupported
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return ErrLaunchUnsupported
	}
	prefix := "/"
	for _, part := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		if part == "" {
			continue
		}
		prefix = filepath.Join(prefix, part)
		info, err := os.Lstat(prefix)
		if err != nil {
			return ErrLaunchUnsupported
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != 0 {
			return ErrLaunchUnsupported
		}
		if info.Mode()&os.ModeSymlink == 0 && info.Mode().Perm()&0022 != 0 {
			return ErrLaunchUnsupported
		}
	}
	return qualifyEnvironmentPath(resolved, false, true)
}

// CheckLoadedEnvironment requires exact effective values for newly qualified
// settings. Error messages identify only the variable name, never secrets.
func CheckLoadedEnvironment(output string, expected map[string]string) error {
	required := false
	for name := range expected {
		if !strings.HasPrefix(name, "OLLAMA_") {
			required = true
		}
	}
	if !required {
		return nil
	}
	assignments, err := environmentAssignments(output)
	if err != nil {
		return err
	}
	observed := map[string]string{}
	for _, assignment := range assignments {
		name, value, _ := strings.Cut(assignment, "=")
		if _, ok := observed[name]; ok {
			return environmentError(name, "loaded metadata is ambiguous; reload and retry")
		}
		observed[name] = value
	}
	for name, value := range expected {
		if observed[name] != value {
			return environmentError(name, "loaded setting differs from inspected source; reload and refresh the binding")
		}
	}
	for name := range observed {
		if _, ok := expected[name]; !ok {
			return environmentError(name, "loaded setting has no inspected source; reload and refresh the binding")
		}
	}
	return nil
}

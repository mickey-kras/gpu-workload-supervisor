package runtime

import (
	"crypto/sha256"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"golang.org/x/sys/unix"
)

const loopbackHTTPPrefix = "http://"

// AutomaticLaunch is evidence read from a supported direct launch. Inspection
// never rewrites the existing file or runs its command.
type AutomaticLaunch struct {
	HomeWorkingDirectory             string
	WorkingDirectory                 string
	LoadedProperties                 map[string]string
	Environment                      map[string]string
	Endpoint, Model, SHA256, Command string
	GPUUUID                          string
	PreCommands                      []string
	DropIns                          []control.LaunchSource
}

func InspectAutomaticLaunch(path, app string) (AutomaticLaunch, error) {
	return InspectAutomaticLaunchSources(path, app, nil)
}

func InspectAutomaticLaunchSources(path, app string, dropInPaths []string) (AutomaticLaunch, error) {
	validate := validateNativeExecutable
	if app == "comfyui" {
		validate = validateComfyExecutable
	}
	return inspectAutomaticLaunchSourcesWithValidator(path, app, dropInPaths, validate)
}

func inspectAutomaticLaunchSourcesWithValidator(path, app string, dropInPaths []string, validate func(string) error) (AutomaticLaunch, error) {
	return inspectAutomaticLaunchSourcesMode(path, app, dropInPaths, validate, false)
}

// Cleanup may inspect unchanged source without requiring cache/credential paths
// to remain usable. Source and command trust are still mandatory; this mode
// never authorizes execution or admission.
func inspectAutomaticLaunchSourcesMode(path, app string, dropInPaths []string, validate func(string) error, cleanupEnvironment bool) (AutomaticLaunch, error) {
	sources, dropIns, err := readAutomaticLaunchSources(path, dropInPaths)
	if err != nil {
		return AutomaticLaunch{}, err
	}
	u, err := parseExternalLaunchSourcesMode(sources, app, cleanupEnvironment)
	if err != nil {
		var problems launchProblems
		problems.add(err)
		// Validate independent command evidence without deriving a binding from a
		// partially valid environment or directive set.
		if app != "ollama" && u.execStart != "" {
			_, independentErr := inspectParsedAutomaticLaunch(u, sources[0], app, validate)
			problems.add(independentErr)
		} else {
			problems.add(u.validatePreCommands(validate))
			args := strings.Fields(u.execStart)
			if len(args) >= 2 && filepath.IsAbs(args[0]) {
				problems.add(validate(args[0]))
			}
		}
		problems.add(fmt.Errorf("%w: dependent loaded-state and binding checks were not completed because launch validation failed", ErrLaunchUnsupported))
		return AutomaticLaunch{}, problems.err()
	}
	result, err := inspectParsedAutomaticLaunch(u, sources[0], app, validate)
	if err != nil {
		return AutomaticLaunch{}, err
	}
	result.DropIns = dropIns
	return result, err
}

func readAutomaticLaunchSources(path string, dropInPaths []string) ([][]byte, []control.LaunchSource, error) {
	if len(dropInPaths) > 32 {
		return nil, nil, ErrLaunchUnsupported
	}
	var sourceProblems launchProblems
	raw, err := readLaunchSource(path)
	if err != nil {
		sourceProblems.add(fmt.Errorf("launch source: %w", err))
	}
	sources := [][]byte{raw}
	seen := map[string]bool{path: true}
	var dropIns []control.LaunchSource
	for i, path := range dropInPaths {
		if seen[path] || !strings.HasSuffix(path, ".conf") || !control.LaunchGrammarExpressible(path) {
			sourceProblems.add(fmt.Errorf("%w: contributing drop-in %d path or duplicate source is unsupported", ErrLaunchUnsupported, i+1))
			continue
		}
		seen[path] = true
		raw, err := readLaunchSource(path)
		if err != nil {
			sourceProblems.add(fmt.Errorf("contributing drop-in %d: %w", i+1, err))
			continue
		}
		sources = append(sources, raw)
		dropIns = append(dropIns, control.LaunchSource{Path: path, SHA256: fmt.Sprintf("%x", sha256.Sum256(raw))})
	}
	if err := sourceProblems.err(); err != nil {
		return nil, nil, err
	}
	if err := control.ValidateLaunchSources(path, dropIns); err != nil {
		return nil, nil, err
	}
	return sources, dropIns, nil
}

func inspectAutomaticLaunch(raw []byte, app string, validate func(string) error) (AutomaticLaunch, error) {
	u, err := parseLaunchUnit(raw, app)
	if err != nil {
		return AutomaticLaunch{}, err
	}
	if err := u.validatePreCommands(validate); err != nil {
		return AutomaticLaunch{}, err
	}
	return inspectParsedAutomaticLaunch(u, raw, app, validate)
}

func inspectParsedAutomaticLaunch(u parsedLaunchUnit, raw []byte, app string, validate func(string) error) (AutomaticLaunch, error) {
	var problems launchProblems
	problems.add(u.validatePreCommands(validate))
	args := strings.Fields(u.execStart)
	if len(args) < 2 || !filepath.IsAbs(args[0]) {
		problems.add(fmt.Errorf("%w: ExecStart requires an absolute direct command", ErrLaunchUnsupported))
		return AutomaticLaunch{}, problems.err()
	}
	if err := validate(args[0]); err != nil {
		problems.add(fmt.Errorf("%w: executable: %w", ErrLaunchUnsupported, err))
	}
	result := AutomaticLaunch{HomeWorkingDirectory: u.homeWorkingDirectory, WorkingDirectory: u.workingDirectory, LoadedProperties: u.loadedProperties, Environment: u.environmentValues, GPUUUID: u.gpuUUID, Command: u.execStart, PreCommands: u.preCommands, SHA256: fmt.Sprintf("%x", sha256.Sum256(raw))}
	if app == "comfyui" {
		endpoint, err := comfyLaunchEndpointMode(args, u.cleanupEnvironment)
		result.Endpoint = endpoint
		problems.add(err)
		problems.add(validateComfyScript(args[1]))
	} else if app == "ollama" {
		result.Endpoint = loopbackHTTPPrefix + u.host
		problems.add(qualifyParsedMainCommand(u, control.NativeModel{GPUUUID: u.gpuUUID, Runtime: app, Endpoint: result.Endpoint}, func(string) error { return nil }))
	} else {
		f, err := parseServerCommand(args, app)
		problems.add(err)
		if err == nil {
			result.Endpoint = loopbackHTTPPrefix + net.JoinHostPort(f.bindHost, f.port)
			result.Model = f.alias
			if result.Model == "" {
				result.Model = f.model
			}
			problems.add(qualifyParsedMainCommand(u, control.NativeModel{GPUUUID: u.gpuUUID, Runtime: app, Model: result.Model, Endpoint: result.Endpoint}, func(string) error { return nil }))
		}
	}
	if err := problems.err(); err != nil {
		return AutomaticLaunch{}, err
	}
	return result, nil
}

func comfyLaunchEndpoint(args []string) (string, error) { return comfyLaunchEndpointMode(args, false) }

func comfyLaunchEndpointMode(args []string, cleanup bool) (string, error) {
	executable := filepath.Base(args[0])
	if executable != "python" && executable != "python3" && !strings.HasPrefix(executable, "python3.") {
		return "", ErrLaunchUnsupported
	}
	if !filepath.IsAbs(args[1]) || filepath.Base(args[1]) != "main.py" || !strings.EqualFold(filepath.Base(filepath.Dir(args[1])), "ComfyUI") {
		return "", ErrLaunchUnsupported
	}
	host, port, err := comfyLaunchAddressMode(args[2:], cleanup)
	if err != nil {
		return "", err
	}
	ip := net.ParseIP(host)
	number, err := strconv.ParseUint(port, 10, 16)
	if ip == nil || !ip.IsLoopback() || err != nil || number == 0 {
		return "", ErrLaunchUnsupported
	}
	return loopbackHTTPPrefix + net.JoinHostPort(host, port), nil
}

func comfyLaunchAddress(args []string) (string, string, error) {
	return comfyLaunchAddressMode(args, false)
}

func comfyLaunchAddressMode(args []string, cleanup bool) (string, string, error) {
	host, port := "127.0.0.1", "8188"
	if len(args) > 256 {
		return "", "", fmt.Errorf("%w: ComfyUI argument count exceeds supported limit", ErrLaunchUnsupported)
	}
	var problems launchProblems
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		key := args[i]
		if seen[key] && key != "--extra-model-paths-config" {
			return "", "", comfyDuplicateOption(key)
		}
		seen[key] = true
		consumed, err := qualifyComfyOption(key, args[i+1:], cleanup)
		if err != nil {
			problems.add(err)
			if consumed == 0 {
				return "", "", problems.err()
			}
		}
		switch key {
		case "--listen":
			host = args[i+1]
		case "--port":
			port = args[i+1]
		}
		i += consumed
	}
	if err := problems.err(); err != nil {
		return "", "", err
	}
	return host, port, nil
}

func comfyDuplicateOption(key string) error {
	switch key {
	case "--listen", "--port", "--disable-auto-launch", "--enable-manager", "--output-directory", "--temp-directory", "--user-directory":
		return comfyOptionError(key, "is repeated; use one unambiguous assignment")
	default:
		return comfyUnsupportedOption(key)
	}
}

func qualifyComfyOption(key string, values []string, cleanup bool) (int, error) {
	switch key {
	case "--listen", "--port":
		if len(values) == 0 || strings.HasPrefix(values[0], "--") {
			return 0, comfyOptionError(key, "requires an explicit value")
		}
		return 1, nil
	case "--output-directory", "--temp-directory", "--user-directory":
		if len(values) == 0 || strings.HasPrefix(values[0], "--") {
			return 0, comfyOptionError(key, "requires an existing trusted absolute directory")
		}
		if err := qualifyComfyOptionPath(values[0], false, cleanup); err != nil {
			return 1, comfyOptionError(key, "requires an existing absolute clean trusted directory without links; inspect this setting and its ancestors")
		}
		return 1, nil
	case "--extra-model-paths-config":
		return qualifyComfyConfigurationFiles(values, cleanup)
	case "--disable-auto-launch", "--enable-manager":
		return 0, nil
	default:
		return 0, comfyUnsupportedOption(key)
	}
}

func qualifyComfyConfigurationFiles(values []string, cleanup bool) (int, error) {
	count := 0
	for _, value := range values {
		if strings.HasPrefix(value, "--") {
			break
		}
		count++
	}
	if count == 0 {
		return 0, comfyOptionError("--extra-model-paths-config", "requires at least one trusted configuration file")
	}
	if count > 32 {
		return count, comfyOptionError("--extra-model-paths-config", "configuration file count exceeds supported limit; paths were not inspected")
	}
	var problems launchProblems
	for _, value := range values[:count] {
		if err := qualifyComfyOptionPath(value, true, cleanup); err != nil {
			problems.add(comfyOptionError("--extra-model-paths-config", "requires existing absolute clean trusted regular configuration files without links; inspect this setting and its ancestors"))
		}
	}
	return count, problems.err()
}

func comfyOptionError(key, reason string) error {
	return fmt.Errorf("%w: ComfyUI option %s %s", ErrLaunchUnsupported, key, reason)
}

func comfyUnsupportedOption(key string) error {
	// Only static upstream option names are safe diagnostic labels. Unknown
	// tokens can contain credentials or private paths and are never echoed.
	switch key {
	case "--base-directory", "--input-directory", "--auto-launch", "--cuda-device", "--default-device", "--disable-all-custom-nodes", "--whitelist-custom-nodes", "--enable-manager-legacy-ui", "--disable-manager-ui", "--front-end-version", "--front-end-root", "--models-directory", "--database-url", "--tls-keyfile", "--tls-certfile":
		return comfyOptionError(key, "is not qualified; retain the existing arguments and review a supported direct configuration")
	default:
		return fmt.Errorf("%w: unrecognized ComfyUI option or positional argument; review the supported direct options", ErrLaunchUnsupported)
	}
}

func validateComfyExecutable(path string) error {
	resolved, err := validateExecutable(path)
	if err != nil {
		return fmt.Errorf("%w: interpreter trust: %w", ErrLaunchUnsupported, err)
	}
	base, target := filepath.Base(path), filepath.Base(resolved)
	if base == target {
		return nil
	}
	if (base == "python" || base == "python3") && strings.HasPrefix(target, "python3.") {
		return nil
	}
	return ErrLaunchUnsupported
}

// validateComfyScript uses the deployment trust principal rule: root or the
// desktop account may own code and directories; other principals cannot write
// or replace them. Descriptor-relative no-follow opens reject symlink swaps and
// special files without reading or executing the application.
func validateComfyScript(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return ErrLaunchUnsupported
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return ErrLaunchUnsupported
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for i, part := range parts {
		flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
		leaf := i == len(parts)-1
		if !leaf {
			flags |= unix.O_DIRECTORY
		}
		next, err := unix.Openat(fd, part, flags, 0)
		unix.Close(fd)
		if err != nil {
			return fmt.Errorf("%w: application script component %d cannot be opened without following links; inspect this component for missing access or symbolic links", ErrLaunchUnsupported, i+1)
		}
		fd = next
		var stat unix.Stat_t
		if err := unix.Fstat(fd, &stat); err != nil {
			unix.Close(fd)
			return ErrLaunchUnsupported
		}
		if !trustedComfyComponent(stat, i, len(parts)) {
			unix.Close(fd)
			return fmt.Errorf("%w: application script trust: %w", ErrLaunchUnsupported, sourceComponentError(stat, i, len(parts)))
		}
	}
	unix.Close(fd)
	return nil
}

func trustedComfyComponent(stat unix.Stat_t, index, count int) bool {
	trustedOwner := stat.Uid == 0 || stat.Uid == uint32(os.Geteuid())
	writable := stat.Mode&0022 != 0
	// A root-owned sticky ancestor cannot replace the subsequently checked private directory.
	if index < count-2 && stat.Uid == 0 && stat.Mode&unix.S_ISVTX != 0 {
		writable = false
	}
	leaf := index == count-1
	return trustedOwner && !writable && (!leaf || (stat.Mode&unix.S_IFMT == unix.S_IFREG && stat.Mode&0444 != 0))
}

func qualifyComfyOptionPath(path string, file, cleanup bool) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsAny(path, launchExpansionCharacters) {
		return ErrLaunchUnsupported
	}
	if cleanup {
		return nil
	}
	return qualifyEnvironmentPath(path, file, false)
}

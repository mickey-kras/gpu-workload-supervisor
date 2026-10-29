package runtime

import (
	"crypto/sha256"
	"fmt"
	"net"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

const (
	flagBaseDirectory   = "--base-directory"
	flagOutputDirectory = "--output-directory"
	flagTempDirectory   = "--temp-directory"
	flagUserDirectory   = "--user-directory"

	loopbackHTTPPrefix        = "http://"
	flagExtraModelPathsConfig = "--extra-model-paths-config"
	flagListen                = "--listen"
	flagPort                  = "--port"
)

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
	// Hardened vLLM settings read from the launch.
	ChatTemplate, ChatTemplateSHA256 string
	EnvironmentFile                  string
}

func InspectAutomaticLaunchSources(path, app string, dropInPaths []string) (AutomaticLaunch, error) {
	validate := validateNativeExecutable
	if app == "comfyui" {
		validate = validateComfyExecutable
	}
	return inspectAutomaticLaunchSourcesWithValidator(path, app, dropInPaths, validate)
}

// PreviewAutomaticGPUCorrection validates the complete effective launch with
// the proposed final drop-in, without writing files or running the application.
func PreviewAutomaticGPUCorrection(path, app string, dropInPaths []string, uuid string) (AutomaticLaunch, error) {
	if !control.ValidGPUUUID(uuid) {
		return AutomaticLaunch{}, ErrGPUIdentityUnresolved
	}
	// The proposed correction contributes one additional drop-in.
	if len(dropInPaths) >= 32 {
		return AutomaticLaunch{}, ErrLaunchUnsupported
	}
	sources, dropIns, err := readAutomaticLaunchSources(path, dropInPaths)
	if err != nil {
		return AutomaticLaunch{}, err
	}
	sources = append(sources, []byte("[Service]\nEnvironment=\"CUDA_VISIBLE_DEVICES="+uuid+"\"\n"))
	u, err := parseExternalLaunchSources(sources)
	if err != nil {
		return AutomaticLaunch{}, err
	}
	validate := validateNativeExecutable
	if app == "comfyui" {
		validate = validateComfyExecutable
	}
	result, err := inspectParsedAutomaticLaunch(u, sources[0], app, validate)
	result.DropIns = dropIns
	return result, err
}

func inspectAutomaticLaunchSourcesWithValidator(path, app string, dropInPaths []string, validate func(string) error) (AutomaticLaunch, error) {
	sources, dropIns, err := readAutomaticLaunchSources(path, dropInPaths)
	if err != nil {
		return AutomaticLaunch{}, err
	}
	u, err := parseExternalLaunchSources(sources)
	if err != nil {
		var problems launchProblems
		problems.add(err)
		// Validate independent command evidence without deriving a binding from a
		// partially valid environment or directive set.
		if u.execStart != "" {
			_, independentErr := inspectParsedAutomaticLaunch(u, sources[0], app, validate)
			problems.add(independentErr)
		} else {
			problems.add(u.validatePreCommands(validate))
			args := splitExecWords(u.execStart)
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

func inspectParsedAutomaticLaunch(u parsedLaunchUnit, raw []byte, app string, validate func(string) error) (AutomaticLaunch, error) {
	var problems launchProblems
	problems.add(u.validatePreCommands(validate))
	args := splitExecWords(u.execStart)
	if len(args) < 2 || !filepath.IsAbs(args[0]) {
		problems.add(fmt.Errorf("%w: ExecStart requires an absolute direct command", ErrLaunchUnsupported))
		return AutomaticLaunch{}, problems.err()
	}
	if err := validate(args[0]); err != nil {
		problems.add(fmt.Errorf("%w: executable: %w", ErrLaunchUnsupported, err))
	}
	result := AutomaticLaunch{HomeWorkingDirectory: u.homeWorkingDirectory, WorkingDirectory: u.workingDirectory, LoadedProperties: u.loadedProperties, Environment: u.environmentValues, GPUUUID: u.gpuUUID, Command: u.execStart, PreCommands: u.preCommands, SHA256: fmt.Sprintf("%x", sha256.Sum256(raw))}
	if app == "comfyui" {
		if u.environmentFile != "" {
			problems.add(fmt.Errorf("%w: EnvironmentFile is supported only for the vLLM API key", ErrLaunchUnsupported))
		}
		endpoint, err := comfyLaunchEndpoint(args)
		result.Endpoint = endpoint
		problems.add(err)
		problems.add(validateComfyScript(args[1]))
	} else {
		problems.add(inspectServerLaunch(u, args, app, &result))
	}
	if err := problems.err(); err != nil {
		return AutomaticLaunch{}, err
	}
	return result, nil
}

// inspectServerLaunch reads the binding of an existing vLLM launch, including
// its hardened settings, and qualifies the launch with that binding.
func inspectServerLaunch(u parsedLaunchUnit, args []string, app string, result *AutomaticLaunch) error {
	f, err := parseServerCommand(args, app)
	if err != nil {
		return err
	}
	result.Endpoint = loopbackHTTPPrefix + net.JoinHostPort(f.bindHost, f.port)
	result.Model = f.alias
	if result.Model == "" {
		result.Model = f.model
	}
	result.ChatTemplate, result.EnvironmentFile = f.chatTemplate, u.environmentFile
	if f.chatTemplate != "" {
		// A template that cannot be read is reported by the qualification.
		if digest, err := ChatTemplateSHA256(f.chatTemplate); err == nil {
			result.ChatTemplateSHA256 = digest
		}
	}
	n := control.NativeModel{GPUUUID: u.gpuUUID, Runtime: app, Model: result.Model, Endpoint: result.Endpoint, ChatTemplate: result.ChatTemplate, ChatTemplateSHA256: result.ChatTemplateSHA256, EnvironmentFile: result.EnvironmentFile}
	return qualifyParsedMainCommand(u, n, func(string) error { return nil })
}

func comfyLaunchEndpoint(args []string) (string, error) {
	executable := filepath.Base(args[0])
	if executable != "python" && executable != "python3" && !strings.HasPrefix(executable, "python3.") {
		return "", ErrLaunchUnsupported
	}
	if !filepath.IsAbs(args[1]) || filepath.Base(args[1]) != "main.py" || !strings.EqualFold(filepath.Base(filepath.Dir(args[1])), "ComfyUI") {
		return "", ErrLaunchUnsupported
	}
	host, port, err := comfyLaunchAddress(args[2:])
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
	host, port := "127.0.0.1", "8188"
	if len(args) > 256 {
		return "", "", fmt.Errorf("%w: ComfyUI argument count exceeds supported limit", ErrLaunchUnsupported)
	}
	var problems launchProblems
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		key, values, attached := comfyOptionArguments(args[i], args[i+1:])
		if err := comfyDuplicateOption(key, seen); err != nil {
			return "", "", err
		}
		consumed, err := qualifyComfyOption(key, values)
		if err != nil {
			problems.add(err)
			if consumed == 0 {
				return "", "", problems.err()
			}
		}
		switch key {
		case flagListen:
			host = values[0]
		case flagPort:
			port = values[0]
		}
		if !attached {
			i += consumed
		}
	}
	if err := problems.err(); err != nil {
		return "", "", err
	}
	return host, port, nil
}

func comfyOptionArguments(key string, following []string) (string, []string, bool) {
	name, value, attached := strings.Cut(key, "=")
	if attached {
		switch name {
		case flagListen, flagPort, flagBaseDirectory, flagOutputDirectory, flagTempDirectory, flagUserDirectory, flagExtraModelPathsConfig:
			return name, []string{value}, true
		}
	}
	return key, following, false
}

func comfyDuplicateOption(key string, seen map[string]bool) error {
	switch key {
	case flagListen, flagPort, "--disable-auto-launch", "--enable-manager", flagBaseDirectory, flagOutputDirectory, flagTempDirectory, flagUserDirectory:
		if seen[key] {
			return comfyOptionError(key, "is repeated; use one unambiguous assignment")
		}
		seen[key] = true
	}
	return nil
}

func qualifyComfyOption(key string, values []string) (int, error) {
	switch key {
	case flagListen, flagPort:
		if len(values) == 0 || strings.HasPrefix(values[0], "--") {
			return 0, comfyOptionError(key, "requires an explicit value")
		}
		return 1, nil
	case flagBaseDirectory, flagOutputDirectory, flagTempDirectory, flagUserDirectory:
		if len(values) == 0 || strings.HasPrefix(values[0], "--") {
			return 0, comfyOptionError(key, "requires an existing trusted absolute directory")
		}
		if err := qualifyComfyOptionPath(values[0], false); err != nil {
			return 1, comfyOptionError(key, err.Error())
		}
		return 1, nil
	case flagExtraModelPathsConfig:
		return qualifyComfyConfigurationFiles(values)
	case "--disable-auto-launch", "--enable-manager":
		return 0, nil
	default:
		return 0, comfyUnsupportedOption(key)
	}
}

func qualifyComfyConfigurationFiles(values []string) (int, error) {
	count := 0
	for _, value := range values {
		if strings.HasPrefix(value, "--") {
			break
		}
		count++
	}
	if count == 0 {
		return 0, comfyOptionError(flagExtraModelPathsConfig, "requires at least one trusted configuration file")
	}
	if count > 32 {
		return count, comfyOptionError(flagExtraModelPathsConfig, "configuration file count exceeds supported limit; paths were not inspected")
	}
	var problems launchProblems
	for _, value := range values[:count] {
		if err := qualifyComfyOptionPath(value, true); err != nil {
			problems.add(comfyOptionError(flagExtraModelPathsConfig, err.Error()))
		}
	}
	return count, problems.err()
}

func comfyOptionError(key, reason string) error {
	return fmt.Errorf("%w: ComfyUI option %s %s", ErrLaunchUnsupported, key, reason)
}

// Unknown options remain verbatim, but must not bypass endpoint or path
// qualification. argparse accepts unique long-option abbreviations, which we
// conservatively reject instead of inferring their meaning across versions.
func comfyUnsupportedOption(key string) error {
	name, _, _ := strings.Cut(key, "=")
	if name == "--" {
		return fmt.Errorf("%w: ComfyUI end-of-options marker is not supported", ErrLaunchUnsupported)
	}
	for _, option := range []string{flagListen, flagPort, "--tls-keyfile", "--tls-certfile", "--cuda-device", "--default-device", flagBaseDirectory, flagOutputDirectory, flagTempDirectory, flagUserDirectory, flagExtraModelPathsConfig} {
		if name == option || (strings.HasPrefix(name, "--") && len(name) > 2 && strings.HasPrefix(option, name)) {
			if strings.HasPrefix(option, "--tls-") {
				return comfyOptionError(option, "changes the endpoint protocol; TLS endpoints are not supported")
			}
			if option == "--cuda-device" || option == "--default-device" {
				return comfyOptionError(option, "overrides CUDA_VISIBLE_DEVICES; use the inspected full physical GPU UUID selection")
			}
			return comfyOptionError(option, "requires its supported full option name and value")
		}
	}
	return nil
}

func validateComfyExecutable(path string) error {
	if _, err := validateExecutable(path); err != nil {
		return fmt.Errorf("%w: interpreter: %w", ErrLaunchUnsupported, err)
	}
	return nil
}

// validateComfyScript checks the application script with the shared path
// trust rules (owner you/root, not world-writable, symlinks resolved).
func validateComfyScript(path string) error {
	if _, err := trustPath(path, kindFile); err != nil {
		return fmt.Errorf("%w: application script: %w", ErrLaunchUnsupported, err)
	}
	return nil
}

func qualifyComfyOptionPath(path string, file bool) error {
	kind := kindDir
	if file {
		kind = kindFile
	}
	_, err := trustPath(path, kind)
	return err
}

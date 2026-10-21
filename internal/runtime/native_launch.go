package runtime

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

const (
	runtimeLlamaCPP    = "llama.cpp"
	flagParallel       = "--parallel"
	flagContBatching   = "--cont-batching"
	flagNoContBatching = "--no-cont-batching"
	flagFlashAttn      = "--flash-attn"
	flagSpecDraftNMin  = "--spec-draft-n-min"
	flagAlias          = "--alias"
	flagCtxSize        = "--ctx-size"
	flagModel          = "--model"
	flagMaxModelLen    = "--max-model-len"
	flagNGPULayers     = "--n-gpu-layers"
)

var ErrModelUnavailable = errors.New("configured local model is unavailable")

var ErrLaunchUnsupported = errors.New("native launch requires a supported direct local command")

// qualifyNativeLaunchWithValidator accepts a deliberately small systemd subset,
// not general unit syntax. Unsupported indirection remains unavailable rather
// than guessed.
func qualifyNativeLaunchWithValidator(raw []byte, n control.NativeModel, validate func(string) error) error {
	unit, err := parseOwnedLaunchUnit(raw, n)
	if err != nil {
		return err
	}
	return qualifyParsedLaunch(unit, n, validate)
}

func qualifyParsedLaunch(unit parsedLaunchUnit, n control.NativeModel, validate func(string) error) error {
	if err := unit.validatePreCommands(validate); err != nil {
		return err
	}
	return qualifyParsedMainCommand(unit, n, validate)
}

// Main-command checks are independent of startup preparation validation.
func qualifyParsedMainCommand(unit parsedLaunchUnit, n control.NativeModel, validate func(string) error) error {
	args := strings.Fields(unit.execStart)
	if len(args) < 2 || !filepath.IsAbs(args[0]) {
		return ErrLaunchUnsupported
	}
	if err := validate(args[0]); err != nil {
		return fmt.Errorf("%w: executable: %w", ErrLaunchUnsupported, err)
	}
	if unit.gpuUUID != n.GPUUUID {
		return fmt.Errorf("%w: CUDA_VISIBLE_DEVICES does not match recorded physical GPU evidence; refresh the binding", ErrLaunchUnsupported)
	}
	endpoint, err := url.Parse(n.Endpoint)
	if err != nil {
		return ErrLaunchUnsupported
	}
	if n.Runtime == "ollama" {
		return qualifyOllamaLaunch(args, unit, endpoint)
	}
	return qualifyServerLaunch(args, n, endpoint)
}

type parsedLaunchUnit struct {
	accountHome          *accountHomeSnapshot
	homeWorkingDirectory string
	workingDirectory     string
	loadedProperties     map[string]string
	execStart            string
	preCommands          []string
	cloudOff             bool
	host                 string
	maxLoadedPinned      bool
	gpuUUID              string
	environment          map[string]bool
	environmentValues    map[string]string
	cleanupEnvironment   bool
}

func parseLaunchUnit(raw []byte, runtimeName string) (parsedLaunchUnit, error) {
	var unit parsedLaunchUnit
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	section := ""
	seen := map[string]bool{}
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if header, ok := launchSectionHeader(line); ok {
			section = header
			continue
		}
		if err := unit.applyLaunchLine(seen, runtimeName, section, line); err != nil {
			return unit, err
		}
	}
	if scanner.Err() != nil {
		return unit, ErrLaunchUnsupported
	}
	return unit, nil
}

func launchSectionHeader(line string) (string, bool) {
	switch line {
	case "[Unit]", "[Service]", "[Install]":
		return line, true
	}
	return "", false
}

func (u *parsedLaunchUnit) applyLaunchLine(seen map[string]bool, runtimeName, section, line string) error {
	key, value, ok := strings.Cut(line, "=")
	if !ok {
		return ErrLaunchUnsupported
	}
	if key != "Environment" && key != "ExecStartPre" && seen[section+key] {
		return ErrLaunchUnsupported
	}
	seen[section+key] = true
	return u.applyDirective(runtimeName, section, key, value)
}

func (u *parsedLaunchUnit) applyDirective(runtimeName, section, key, value string) error {
	switch section + key {
	case "[Unit]Description", "[Install]WantedBy":
	case unitWantsDirective:
		_, valid := compatibleIsolationDirective(section, key, value)
		if !valid {
			return fmt.Errorf("%w: incompatible directive %s", ErrLaunchUnsupported, key)
		}
	case "[Service]Type", "[Service]Restart":
		// Any value accepted; the unit runs as-is.
	case "[Service]ExecStartPre":
		if len(u.preCommands) >= 32 || value == "" {
			return ErrLaunchUnsupported
		}
		u.preCommands = append(u.preCommands, value)
	case "[Service]ExecStart":
		if strings.Contains(value, ";") {
			return fmt.Errorf("%w: semicolon delimiter in ExecStart", ErrLaunchUnsupported)
		}
		u.execStart = value
	case "[Service]Environment":
		return u.applySafeEnvironment(runtimeName, value)
	default:
		if unsupportedOperationalDirective(section, key) {
			return fmt.Errorf("%w: unsupported operational directive %s", ErrLaunchUnsupported, key)
		}
		// Harmless unknown settings need not prevent inspection. Directives that
		// add code, change the effective environment, or remap inspected paths do.
	}
	return nil
}

func unsupportedOperationalDirective(section, key string) bool {
	switch section {
	case "[Service]":
		return unsupportedLaunchDirective(key)
	case "[Unit]":
		// Dependency and propagation edges can activate or control code outside
		// the inspected service. Only the qualified Wants case above is allowed.
		switch key {
		case "FailureAction", "SuccessAction", "StartLimitAction", "JobTimeoutAction":
			// These can terminate the user manager, affecting unrelated workloads.
			return true
		case "Requires", "RequiresOverridable", "Requisite", "RequisiteOverridable", "BindsTo", "BindTo", "PartOf", "Upholds", "Conflicts", "OnFailure", "OnSuccess", "PropagatesReloadTo", "PropagateReloadTo", "ReloadPropagatedFrom", "PropagateReloadFrom", "PropagatesStopTo", "StopPropagatedFrom", "RequiresMountsFor", "WantsMountsFor", "JoinsNamespaceOf":
			return true
		}
	}
	return false
}

func unsupportedLaunchDirective(key string) bool {
	// Only ExecStart and the bounded ExecStartPre grammar above are inspected.
	if strings.HasPrefix(key, "Exec") {
		return true
	}
	switch key {
	case "FailureAction", "StartLimitAction":
		// Legacy [Service] aliases of the manager-wide [Unit] actions.
		return true
	case "Sockets":
		// Socket associations implicitly activate units outside this service.
		return true
	case "EnvironmentFile", "UnsetEnvironment", "PassEnvironment", "PAMName":
		// These can replace or remove recorded GPU/runtime environment values.
		return true
	case "RootDirectory", "RootImage", "BindPaths", "BindReadOnlyPaths", "TemporaryFileSystem", "MountImages", "ExtensionImages", "ExtensionDirectories":
		// A trusted host executable/model path need not name the same file in
		// the service's filesystem namespace.
		return true
	}
	return false
}

// Startup preparation is retained verbatim for adopted units and never run
// during inspection. Only fixed system primitives with a bounded argument
// grammar are supported: trusting an interpreter or wrapper does not bind the
// code it can execute. Every accepted executable must also pass the ordinary
// trust check; expansion and systemd privilege prefixes remain unsupported.
func (u parsedLaunchUnit) validatePreCommands(validate func(string) error) error {
	var problems launchProblems
	for _, command := range u.preCommands {
		if strings.ContainsAny(command, ";|&<>\\$%`\"'") {
			problems.add(fmt.Errorf("%w: ExecStartPre command indirection", ErrLaunchUnsupported))
			continue
		}
		args := splitExecWords(strings.TrimPrefix(command, "-"))
		if len(args) == 0 || !filepath.IsAbs(args[0]) || validate(args[0]) != nil {
			problems.add(fmt.Errorf("%w: ExecStartPre requires a trusted direct executable", ErrLaunchUnsupported))
			continue
		}
		if !supportedPreCommand(args) {
			problems.add(fmt.Errorf("%w: ExecStartPre supports only system true/false with optional --, or test with one absolute file predicate", ErrLaunchUnsupported))
		}
	}
	return problems.err()
}

// Exact system paths avoid accepting a helper merely named test or true.
// File predicates inspect their operand; they cannot execute it or load code.
func supportedPreCommand(args []string) bool {
	switch args[0] {
	case "/usr/bin/true", "/bin/true", "/usr/bin/false", "/bin/false":
		return len(args) == 1 || (len(args) == 2 && args[1] == "--")
	case "/usr/bin/test", "/bin/test":
		if len(args) != 3 || !filepath.IsAbs(args[2]) || filepath.Clean(args[2]) != args[2] {
			return false
		}
		switch args[1] {
		case "-e", "-f", "-d", "-r", "-w", "-x", "-s":
			return true
		}
	}
	return false
}

func (u *parsedLaunchUnit) applyOllamaEnvironment(value string) error {
	if value == "OLLAMA_NO_CLOUD=1" && !u.cloudOff {
		u.cloudOff = true
	} else if strings.HasPrefix(value, "OLLAMA_HOST=") && u.host == "" {
		u.host = strings.TrimPrefix(value, "OLLAMA_HOST=")
	} else if value == "OLLAMA_MAX_LOADED_MODELS=1" && !u.maxLoadedPinned {
		u.maxLoadedPinned = true
	} else {
		return ErrLaunchUnsupported
	}
	return nil
}

func qualifyOllamaLaunch(args []string, unit parsedLaunchUnit, endpoint *url.URL) error {
	if filepath.Base(args[0]) != "ollama" || len(args) != 2 || args[1] != "serve" || !unit.cloudOff || unit.host != endpoint.Host {
		return ErrLaunchUnsupported
	}
	return nil
}

func qualifyServerLaunch(args []string, n control.NativeModel, endpoint *url.URL) error {
	flags, err := parseServerCommand(args, n.Runtime)
	if err != nil {
		return err
	}
	if !filepath.IsAbs(flags.model) || filepath.Clean(flags.model) != flags.model || flags.bindHost != endpoint.Hostname() || flags.port != endpoint.Port() || flags.port == "" {
		return ErrLaunchUnsupported
	}
	info, err := os.Stat(flags.model)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: select an existing local model file or directory and refresh its launch configuration", ErrModelUnavailable)
	}
	if err != nil {
		return fmt.Errorf("%w: model metadata could not be inspected; check access to the configured local model", ErrLaunchUnsupported)
	}
	if (n.Runtime == runtimeLlamaCPP && !info.Mode().IsRegular()) || (n.Runtime == "vllm" && !info.IsDir()) {
		return fmt.Errorf("%w: model has the wrong file or directory type; select a compatible local model", ErrModelUnavailable)
	}
	alias := flags.alias
	if alias == "" {
		alias = flags.model
	}
	if alias != n.Model {
		return ErrLaunchUnsupported
	}
	return nil
}

type serverFlags struct {
	model    string
	alias    string
	bindHost string
	port     string
}

func parseServerCommand(args []string, runtimeName string) (serverFlags, error) {
	var flags serverFlags
	switch runtimeName {
	case runtimeLlamaCPP:
		if filepath.Base(args[0]) != "llama-server" {
			return flags, ErrLaunchUnsupported
		}
		args = args[1:]
	case "vllm":
		if filepath.Base(args[0]) != "vllm" || len(args) < 3 || args[1] != "serve" {
			return flags, ErrLaunchUnsupported
		}
		flags.model = args[2]
		args = args[3:]
	default:
		return flags, ErrLaunchUnsupported
	}
	return flags.parse(args, runtimeName)
}

func (f *serverFlags) parse(args []string, runtimeName string) (serverFlags, error) {
	seen := map[string]bool{}
	for len(args) > 0 {
		key := canonicalServerFlag(args[0], runtimeName)
		args = args[1:]
		if seen[key] {
			return *f, fmt.Errorf("%w: duplicate option %s", ErrLaunchUnsupported, diagnosticServerFlag(key))
		}
		seen[key] = true
		if runtimeName == runtimeLlamaCPP && (key == flagContBatching || key == flagNoContBatching) {
			if seen["continuous-batching"] {
				return *f, ErrLaunchUnsupported
			}
			seen["continuous-batching"] = true
			continue
		}
		if len(args) == 0 {
			return *f, fmt.Errorf("%w: option %s requires a value", ErrLaunchUnsupported, diagnosticServerFlag(key))
		}
		value := args[0]
		args = args[1:]
		if err := f.apply(runtimeName, key, value); err != nil {
			return *f, fmt.Errorf("%w: unsupported option or value %s", err, diagnosticServerFlag(key))
		}
	}
	return *f, nil
}

func diagnosticServerFlag(key string) string {
	switch key {
	case "--host", "--port", flagModel, flagAlias, "--served-model-name", flagParallel, "--spec-draft-n-max", flagSpecDraftNMin, flagFlashAttn, "--spec-type", flagCtxSize, flagNGPULayers, flagMaxModelLen, flagContBatching, flagNoContBatching:
		return key
	}
	return "(unqualified option)"
}

func canonicalServerFlag(key, runtimeName string) string {
	if runtimeName != runtimeLlamaCPP {
		return key
	}
	switch key {
	case "-m":
		return flagModel
	case "-c":
		return flagCtxSize
	case "-ngl":
		return flagNGPULayers
	case "-np":
		return flagParallel
	case "-cb":
		return flagContBatching
	case "-nocb":
		return flagNoContBatching
	case "-fa":
		return flagFlashAttn
	}
	return key
}

func (f *serverFlags) apply(runtimeName, key, value string) error {
	switch key {
	case "--host":
		f.bindHost = value
	case "--port":
		f.port = value
	case flagModel, "-m":
		if runtimeName != runtimeLlamaCPP || f.model != "" {
			return ErrLaunchUnsupported
		}
		f.model = value
	case flagAlias, "--served-model-name":
		if f.alias != "" || (key == flagAlias) != (runtimeName == runtimeLlamaCPP) {
			return ErrLaunchUnsupported
		}
		f.alias = value
	default:
		return validateServerOption(runtimeName, key, value)
	}
	return nil
}

func validateServerOption(runtimeName, key, value string) error {
	switch key {
	case flagParallel, "--spec-draft-n-max", flagSpecDraftNMin:
		return validateLlamaNumericOption(runtimeName, key, value)
	case flagFlashAttn:
		if runtimeName != runtimeLlamaCPP || (value != "on" && value != "off" && value != "auto") {
			return ErrLaunchUnsupported
		}
	case "--spec-type":
		// MTP uses the already bound local model; draft-model and remote sources
		// require separate identity evidence and remain unsupported.
		if runtimeName != runtimeLlamaCPP || (value != "none" && value != "draft-mtp") {
			return ErrLaunchUnsupported
		}
	case flagCtxSize, flagNGPULayers, flagMaxModelLen:
		if (runtimeName == runtimeLlamaCPP) != (key != flagMaxModelLen) {
			return ErrLaunchUnsupported
		}
		if v, err := strconv.ParseUint(value, 10, 32); err != nil || v == 0 {
			return ErrLaunchUnsupported
		}
	default:
		return ErrLaunchUnsupported
	}
	return nil
}

func validateLlamaNumericOption(runtimeName, key, value string) error {
	if runtimeName != runtimeLlamaCPP {
		return ErrLaunchUnsupported
	}
	if key == flagParallel && value == "-1" {
		return nil
	}
	v, err := strconv.ParseUint(value, 10, 32)
	if err != nil || (v == 0 && key != flagSpecDraftNMin) {
		return ErrLaunchUnsupported
	}
	return nil
}

func validateNativeExecutable(path string) error {
	if _, err := validateExecutable(path); err != nil {
		return fmt.Errorf("%w: executable: %w", ErrLaunchUnsupported, err)
	}
	return nil
}

// splitExecWords splits an ExecStart line like systemd: whitespace separates
// words; single/double quotes and backslash escapes group them.
func splitExecWords(command string) []string {
	var words []string
	var current strings.Builder
	inWord := false
	quote := byte(0)
	for i := 0; i < len(command); i++ {
		c := command[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			} else {
				current.WriteByte(c)
			}
		case c == '\'' || c == '"':
			quote = c
			inWord = true
		case c == ' ' || c == '\t':
			if inWord {
				words = append(words, current.String())
				current.Reset()
				inWord = false
			}
		case c == '\\' && i+1 < len(command):
			i++
			current.WriteByte(command[i])
			inWord = true
		default:
			current.WriteByte(c)
			inWord = true
		}
	}
	if inWord {
		words = append(words, current.String())
	}
	return words
}

var loadedPreCommand = regexp.MustCompile(`\{ path=([^ ;]+) ; argv\[\]=([^;]+) ; ignore_errors=(yes|no) ;[^}]*\}`)

// CheckLoadedPreCommands binds systemd's loaded startup preparation to the same
// file whose main command and fingerprint were inspected. Unparsed metadata
// fails closed instead of silently dropping startup commands.
func CheckLoadedPreCommands(output string, commands []string) error {
	matches := loadedPreCommand.FindAllStringSubmatch(output, -1)
	if len(matches) != len(commands) || strings.Trim(loadedPreCommand.ReplaceAllString(output, ""), " ;\t\r\n") != "" {
		return ErrLaunchChanged
	}
	for i, command := range commands {
		ignore := strings.HasPrefix(command, "-")
		args := splitExecWords(strings.TrimPrefix(command, "-"))
		if len(args) == 0 || matches[i][1] != args[0] || !slices.Equal(splitExecWords(matches[i][2]), args) || (matches[i][3] == "yes") != ignore {
			return ErrLaunchChanged
		}
	}
	return nil
}

var loadedMainCommand = regexp.MustCompile(`\{ path=([^ ;]+) ; argv\[\]=([^;]+) ;[^}]*\}`)

func CheckLoadedLaunchCommand(output, command string) error {
	matches := loadedMainCommand.FindAllStringSubmatch(output, -1)
	args := splitExecWords(command)
	if len(matches) != 1 || len(args) == 0 || strings.Trim(loadedMainCommand.ReplaceAllString(output, ""), " ;\t\r\n") != "" || matches[0][1] != args[0] || !slices.Equal(splitExecWords(matches[0][2]), args) {
		return ErrLaunchChanged
	}
	if strings.Contains(output, "ignore_errors=yes") {
		return ErrLaunchChanged
	}
	return nil
}

func ValidateSelectedNativeExecutable(app, path string) error {
	if !control.ValidOwnedExecutable(app, path) {
		return fmt.Errorf("%w: selected application must be a supported direct executable", ErrLaunchUnsupported)
	}
	return validateNativeExecutable(path)
}

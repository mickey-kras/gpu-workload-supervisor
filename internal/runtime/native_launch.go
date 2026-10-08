package runtime

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

const runtimeLlamaCPP = "llama.cpp"

var ErrLaunchUnsupported = errors.New("native launch requires a supported direct local command")

// qualifyNativeLaunchWithValidator accepts a deliberately small systemd subset,
// not general unit syntax. Unsupported indirection remains unavailable rather
// than guessed.
func qualifyNativeLaunchWithValidator(raw []byte, n control.NativeModel, validate func(string) error) error {
	unit, err := parseLaunchUnit(raw, n.Runtime)
	if err != nil {
		return err
	}
	return qualifyParsedLaunch(unit, n, validate)
}

func qualifyParsedLaunch(unit parsedLaunchUnit, n control.NativeModel, validate func(string) error) error {
	if err := unit.validatePreCommands(validate); err != nil {
		return err
	}
	args := strings.Fields(unit.execStart)
	if len(args) < 2 || !filepath.IsAbs(args[0]) {
		return ErrLaunchUnsupported
	}
	if err := validate(args[0]); err != nil {
		return ErrLaunchUnsupported
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
	execStart       string
	preCommands     []string
	cloudOff        bool
	host            string
	maxLoadedPinned bool
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
	if !ok || strings.ContainsAny(value, "\\$%`\"'") {
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
	case "[Service]Type":
		if value != "simple" && value != "exec" {
			return ErrLaunchUnsupported
		}
	case "[Service]Restart":
		if value != "no" {
			return ErrLaunchUnsupported
		}
	case "[Service]ExecStartPre":
		if len(u.preCommands) >= 32 || value == "" {
			return ErrLaunchUnsupported
		}
		u.preCommands = append(u.preCommands, value)
	case "[Service]ExecStart":
		u.execStart = value
	case "[Service]Environment":
		if runtimeName != "ollama" {
			return ErrLaunchUnsupported
		}
		return u.applyOllamaEnvironment(value)
	default:
		return ErrLaunchUnsupported
	}
	return nil
}

// Startup preparation is retained verbatim for adopted units. It is never run
// during inspection. Like the main executable, each direct command must be
// trusted; expansion, shell wrappers and systemd privilege prefixes remain
// outside the supported grammar.
func (u parsedLaunchUnit) validatePreCommands(validate func(string) error) error {
	for _, command := range u.preCommands {
		if strings.ContainsAny(command, ";|&<>\\$%`\"'") {
			return fmt.Errorf("%w: ExecStartPre command indirection", ErrLaunchUnsupported)
		}
		args := strings.Fields(strings.TrimPrefix(command, "-"))
		if len(args) == 0 || !filepath.IsAbs(args[0]) || validate(args[0]) != nil {
			return fmt.Errorf("%w: ExecStartPre requires a trusted direct executable", ErrLaunchUnsupported)
		}
		switch filepath.Base(args[0]) {
		case "sh", "bash", "dash", "zsh", "fish", "env", "systemctl", "systemd-run", "sudo", "su":
			return fmt.Errorf("%w: ExecStartPre wrapper %s", ErrLaunchUnsupported, filepath.Base(args[0]))
		}
	}
	return nil
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
	if err != nil || (n.Runtime == runtimeLlamaCPP && !info.Mode().IsRegular()) || (n.Runtime == "vllm" && !info.IsDir()) {
		return ErrLaunchUnsupported
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
			return *f, fmt.Errorf("%w: duplicate option %s", ErrLaunchUnsupported, key)
		}
		seen[key] = true
		if runtimeName == runtimeLlamaCPP && (key == "--cont-batching" || key == "--no-cont-batching") {
			if seen["continuous-batching"] {
				return *f, ErrLaunchUnsupported
			}
			seen["continuous-batching"] = true
			continue
		}
		if len(args) == 0 {
			return *f, fmt.Errorf("%w: option %s requires a value", ErrLaunchUnsupported, key)
		}
		value := args[0]
		args = args[1:]
		if err := f.apply(runtimeName, key, value); err != nil {
			return *f, fmt.Errorf("%w: unsupported option or value %s", err, key)
		}
	}
	return *f, nil
}

func canonicalServerFlag(key, runtimeName string) string {
	if runtimeName != runtimeLlamaCPP {
		return key
	}
	switch key {
	case "-m":
		return "--model"
	case "-c":
		return "--ctx-size"
	case "-ngl":
		return "--n-gpu-layers"
	case "-np":
		return "--parallel"
	case "-cb":
		return "--cont-batching"
	case "-nocb":
		return "--no-cont-batching"
	case "-fa":
		return "--flash-attn"
	}
	return key
}

func (f *serverFlags) apply(runtimeName, key, value string) error {
	switch key {
	case "--host":
		f.bindHost = value
	case "--port":
		f.port = value
	case "--model", "-m":
		if runtimeName != runtimeLlamaCPP || f.model != "" {
			return ErrLaunchUnsupported
		}
		f.model = value
	case "--alias", "--served-model-name":
		if f.alias != "" || (key == "--alias") != (runtimeName == runtimeLlamaCPP) {
			return ErrLaunchUnsupported
		}
		f.alias = value
	case "--parallel", "--spec-draft-n-max", "--spec-draft-n-min":
		if runtimeName != runtimeLlamaCPP {
			return ErrLaunchUnsupported
		}
		if key == "--parallel" && value == "-1" {
			return nil
		}
		v, err := strconv.ParseUint(value, 10, 32)
		if err != nil || (v == 0 && key != "--spec-draft-n-min") {
			return ErrLaunchUnsupported
		}
	case "--flash-attn":
		if runtimeName != runtimeLlamaCPP || (value != "on" && value != "off" && value != "auto") {
			return ErrLaunchUnsupported
		}
	case "--spec-type":
		// MTP uses the already bound local model; draft-model and remote sources
		// require separate identity evidence and remain unsupported.
		if runtimeName != runtimeLlamaCPP || (value != "none" && value != "draft-mtp") {
			return ErrLaunchUnsupported
		}
	case "--ctx-size", "--n-gpu-layers", "--max-model-len":
		if (runtimeName == runtimeLlamaCPP) != (key != "--max-model-len") {
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

func validateNativeExecutable(path string) error {
	resolved, err := validateExecutable(path)
	if err != nil || filepath.Base(resolved) != filepath.Base(path) {
		return ErrLaunchUnsupported
	}
	return nil
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
		args := strings.Fields(strings.TrimPrefix(command, "-"))
		if len(args) == 0 || matches[i][1] != args[0] || strings.Join(strings.Fields(matches[i][2]), " ") != strings.Join(args, " ") || (matches[i][3] == "yes") != ignore {
			return ErrLaunchChanged
		}
	}
	return nil
}

var loadedMainCommand = regexp.MustCompile(`\{ path=([^ ;]+) ; argv\[\]=([^;]+) ;[^}]*\}`)

func CheckLoadedLaunchCommand(output, command string) error {
	matches := loadedMainCommand.FindAllStringSubmatch(output, -1)
	args := strings.Fields(command)
	if len(matches) != 1 || len(args) == 0 || strings.Trim(loadedMainCommand.ReplaceAllString(output, ""), " ;\t\r\n") != "" || matches[0][1] != args[0] || strings.Join(strings.Fields(matches[0][2]), " ") != strings.Join(args, " ") {
		return ErrLaunchChanged
	}
	if strings.Contains(output, "ignore_errors=yes") {
		return ErrLaunchChanged
	}
	return nil
}

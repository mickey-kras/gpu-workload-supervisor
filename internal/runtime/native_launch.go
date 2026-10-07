package runtime

import (
	"bufio"
	"bytes"
	"errors"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"net/url"
	"os"
	"path/filepath"
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
	if key != "Environment" && seen[section+key] {
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
		if len(args) < 2 {
			return *f, ErrLaunchUnsupported
		}
		key, value := args[0], args[1]
		args = args[2:]
		if seen[key] {
			return *f, ErrLaunchUnsupported
		}
		seen[key] = true
		if err := f.apply(runtimeName, key, value); err != nil {
			return *f, err
		}
	}
	return *f, nil
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

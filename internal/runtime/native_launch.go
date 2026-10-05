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

var ErrLaunchUnsupported = errors.New("native launch requires a supported direct local command")

// qualifyNativeLaunch accepts a deliberately small systemd subset, not general
// unit syntax. Unsupported indirection remains unavailable rather than guessed.
func qualifyNativeLaunch(raw []byte, n control.NativeModel) error {
	return qualifyNativeLaunchWithValidator(raw, n, validateNativeExecutable)
}

func qualifyNativeLaunchWithValidator(raw []byte, n control.NativeModel, validate func(string) error) error {
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	section := ""
	execStart := ""
	seen := map[string]bool{}
	cloudOff := false
	host := ""
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if line == "[Unit]" || line == "[Service]" || line == "[Install]" {
			section = line
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.ContainsAny(value, "\\$%`\"'") {
			return ErrLaunchUnsupported
		}
		if key != "Environment" && seen[section+key] {
			return ErrLaunchUnsupported
		}
		seen[section+key] = true
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
			execStart = value
		case "[Service]Environment":
			if n.Runtime != "ollama" {
				return ErrLaunchUnsupported
			}
			if value == "OLLAMA_NO_CLOUD=1" && !cloudOff {
				cloudOff = true
			} else if strings.HasPrefix(value, "OLLAMA_HOST=") && host == "" {
				host = strings.TrimPrefix(value, "OLLAMA_HOST=")
			} else {
				return ErrLaunchUnsupported
			}
		default:
			return ErrLaunchUnsupported
		}
	}
	if scanner.Err() != nil {
		return ErrLaunchUnsupported
	}
	args := strings.Fields(execStart)
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
		if filepath.Base(args[0]) != "ollama" || len(args) != 2 || args[1] != "serve" || !cloudOff || host != endpoint.Host {
			return ErrLaunchUnsupported
		}
		return nil
	}
	model := ""
	alias := ""
	bindHost := ""
	port := ""
	switch n.Runtime {
	case "llama.cpp":
		if filepath.Base(args[0]) != "llama-server" {
			return ErrLaunchUnsupported
		}
		args = args[1:]
	case "vllm":
		if filepath.Base(args[0]) != "vllm" || len(args) < 3 || args[1] != "serve" {
			return ErrLaunchUnsupported
		}
		model = args[2]
		args = args[3:]
	default:
		return ErrLaunchUnsupported
	}
	seen = map[string]bool{}
	for len(args) > 0 {
		if len(args) < 2 {
			return ErrLaunchUnsupported
		}
		key, value := args[0], args[1]
		args = args[2:]
		if seen[key] {
			return ErrLaunchUnsupported
		}
		seen[key] = true
		switch key {
		case "--host":
			bindHost = value
		case "--port":
			port = value
		case "--model", "-m":
			if n.Runtime != "llama.cpp" || model != "" {
				return ErrLaunchUnsupported
			}
			model = value
		case "--alias", "--served-model-name":
			if alias != "" || (key == "--alias") != (n.Runtime == "llama.cpp") {
				return ErrLaunchUnsupported
			}
			alias = value
		case "--ctx-size", "--n-gpu-layers", "--max-model-len":
			if (n.Runtime == "llama.cpp") != (key != "--max-model-len") {
				return ErrLaunchUnsupported
			}
			if v, err := strconv.ParseUint(value, 10, 32); err != nil || v == 0 {
				return ErrLaunchUnsupported
			}
		default:
			return ErrLaunchUnsupported
		}
	}
	if !filepath.IsAbs(model) || filepath.Clean(model) != model || bindHost != endpoint.Hostname() || port != endpoint.Port() || port == "" {
		return ErrLaunchUnsupported
	}
	info, err := os.Stat(model)
	if err != nil || (n.Runtime == "llama.cpp" && !info.Mode().IsRegular()) || (n.Runtime == "vllm" && !info.IsDir()) {
		return ErrLaunchUnsupported
	}
	if alias == "" {
		alias = model
	}
	if alias != n.Model {
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

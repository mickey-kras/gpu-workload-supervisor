package runtime

import (
	"crypto/sha256"
	"fmt"
	"golang.org/x/sys/unix"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

// AutomaticLaunch is evidence read from a supported direct launch. Inspection
// never rewrites the existing file or runs its command.
type AutomaticLaunch struct {
	Endpoint, Model, SHA256, Command string
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
	if len(dropInPaths) > 32 {
		return AutomaticLaunch{}, ErrLaunchUnsupported
	}
	raw, err := readLaunchSource(path)
	if err != nil {
		return AutomaticLaunch{}, fmt.Errorf("%w: launch source %s unreadable or untrusted", err, path)
	}
	sources := [][]byte{raw}
	seen := map[string]bool{path: true}
	var dropIns []control.LaunchSource
	for _, path := range dropInPaths {
		if seen[path] || !strings.HasSuffix(path, ".conf") || !control.LaunchGrammarExpressible(path) {
			return AutomaticLaunch{}, ErrLaunchUnsupported
		}
		seen[path] = true
		raw, err := readLaunchSource(path)
		if err != nil {
			return AutomaticLaunch{}, fmt.Errorf("%w: drop-in %s unreadable or untrusted", err, path)
		}
		sources = append(sources, raw)
		dropIns = append(dropIns, control.LaunchSource{Path: path, SHA256: fmt.Sprintf("%x", sha256.Sum256(raw))})
	}
	if err := control.ValidateLaunchSources(path, dropIns); err != nil {
		return AutomaticLaunch{}, err
	}
	u, err := parseExternalLaunchSources(sources, app)
	if err != nil {
		return AutomaticLaunch{}, err
	}
	result, err := inspectParsedAutomaticLaunch(u, sources[0], app, validate)
	result.DropIns = dropIns
	return result, err
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
	if err := u.validatePreCommands(validate); err != nil {
		return AutomaticLaunch{}, err
	}
	args := strings.Fields(u.execStart)
	if len(args) < 2 || !filepath.IsAbs(args[0]) || validate(args[0]) != nil {
		return AutomaticLaunch{}, ErrLaunchUnsupported
	}
	var err error
	result := AutomaticLaunch{Command: u.execStart, PreCommands: u.preCommands, SHA256: fmt.Sprintf("%x", sha256.Sum256(raw))}
	if app == "comfyui" {
		result.Endpoint, err = comfyLaunchEndpoint(args)
		if err != nil {
			return result, err
		}
		if err := validateComfyScript(args[1]); err != nil {
			return AutomaticLaunch{}, err
		}
		return result, nil
	}
	if app == "ollama" {
		result.Endpoint = "http://" + u.host
		if err := qualifyParsedLaunch(u, control.NativeModel{Runtime: app, Endpoint: result.Endpoint}, validate); err != nil {
			return AutomaticLaunch{}, err
		}
		return result, nil
	}
	f, err := parseServerCommand(args, app)
	if err != nil {
		return AutomaticLaunch{}, err
	}
	result.Endpoint = "http://" + net.JoinHostPort(f.bindHost, f.port)
	result.Model = f.alias
	if result.Model == "" {
		result.Model = f.model
	}
	err = qualifyParsedLaunch(u, control.NativeModel{Runtime: app, Model: result.Model, Endpoint: result.Endpoint}, validate)
	return result, err
}

func comfyLaunchEndpoint(args []string) (string, error) {
	executable := filepath.Base(args[0])
	if executable != "python" && executable != "python3" && !strings.HasPrefix(executable, "python3.") {
		return "", ErrLaunchUnsupported
	}
	if !filepath.IsAbs(args[1]) || filepath.Base(args[1]) != "main.py" || !strings.EqualFold(filepath.Base(filepath.Dir(args[1])), "ComfyUI") {
		return "", ErrLaunchUnsupported
	}
	host, port := "127.0.0.1", "8188"
	seen := map[string]bool{}
	for i := 2; i < len(args); i++ {
		key := args[i]
		if seen[key] {
			return "", ErrLaunchUnsupported
		}
		seen[key] = true
		switch key {
		case "--listen", "--port":
			if i+1 >= len(args) {
				return "", ErrLaunchUnsupported
			}
			i++
			if key == "--listen" {
				host = args[i]
			} else {
				port = args[i]
			}
		case "--disable-auto-launch":
		default:
			return "", ErrLaunchUnsupported
		}
	}
	ip := net.ParseIP(host)
	number, err := strconv.ParseUint(port, 10, 16)
	if ip == nil || !ip.IsLoopback() || err != nil || number == 0 {
		return "", ErrLaunchUnsupported
	}
	return "http://" + net.JoinHostPort(host, port), nil
}

func validateComfyExecutable(path string) error {
	resolved, err := validateExecutable(path)
	if err != nil {
		return ErrLaunchUnsupported
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
			return ErrLaunchUnsupported
		}
		fd = next
		var stat unix.Stat_t
		if err := unix.Fstat(fd, &stat); err != nil {
			unix.Close(fd)
			return ErrLaunchUnsupported
		}
		trustedOwner := stat.Uid == 0 || stat.Uid == uint32(os.Geteuid())
		writable := stat.Mode&0022 != 0
		// A root-owned sticky ancestor (such as /tmp) cannot replace entries in
		// the subsequently checked private application directory.
		if i < len(parts)-2 && stat.Uid == 0 && stat.Mode&unix.S_ISVTX != 0 {
			writable = false
		}
		if !trustedOwner || writable || (leaf && (stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&0444 == 0)) {
			unix.Close(fd)
			return ErrLaunchUnsupported
		}
	}
	unix.Close(fd)
	return nil
}

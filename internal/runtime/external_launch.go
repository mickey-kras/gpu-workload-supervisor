package runtime

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"golang.org/x/sys/unix"
)

// readLaunchSource opens every path component without following symlinks. Only
// root or the desktop principal may write source files or replace ancestors.
func readLaunchSource(path string) ([]byte, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, ErrLaunchChanged
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, ErrLaunchChanged
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for i, part := range parts {
		leaf := i == len(parts)-1
		flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
		if !leaf {
			flags |= unix.O_DIRECTORY
		}
		next, err := unix.Openat(fd, part, flags, 0)
		unix.Close(fd)
		if err != nil {
			return nil, ErrLaunchChanged
		}
		fd = next
		var stat unix.Stat_t
		if unix.Fstat(fd, &stat) != nil {
			unix.Close(fd)
			return nil, ErrLaunchChanged
		}
		writable := stat.Mode&0022 != 0
		if i < len(parts)-2 && stat.Uid == 0 && stat.Mode&unix.S_ISVTX != 0 {
			writable = false
		}
		if (stat.Uid != 0 && stat.Uid != uint32(os.Geteuid())) || writable || (leaf && (stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&0444 == 0 || stat.Size > 1<<20)) {
			unix.Close(fd)
			return nil, ErrLaunchChanged
		}
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, 1<<20+1))
	if err != nil || len(raw) > 1<<20 {
		return nil, ErrLaunchChanged
	}
	return raw, nil
}

func readLaunchSources(path string, dropIns []control.LaunchSource) ([][]byte, error) {
	if err := control.ValidateLaunchSources(path, dropIns); err != nil {
		return nil, err
	}
	raw, err := readLaunchSource(path)
	if err != nil {
		return nil, fmt.Errorf("%w: launch source %s unreadable or untrusted", err, path)
	}
	sources := [][]byte{raw}
	for _, source := range dropIns {
		raw, err := readLaunchSource(source.Path)
		if err != nil || fmt.Sprintf("%x", sha256.Sum256(raw)) != source.SHA256 {
			return nil, fmt.Errorf("%w: drop-in %s changed, unreadable or untrusted", ErrLaunchChanged, source.Path)
		}
		sources = append(sources, raw)
	}
	return sources, nil
}

// parseExternalLaunchSources models the effective subset rather than rendering
// an adopted service into the owned grammar. Scalars override; command lists
// append, with explicit empty assignments resetting the list.
func parseExternalLaunchSources(sources [][]byte, runtimeName string) (parsedLaunchUnit, error) {
	var unit parsedLaunchUnit
	scalars := map[string]string{}
	var starts, pre, envOrder []string
	env := map[string]string{}
	for _, raw := range sources {
		section := ""
		scanner := bufio.NewScanner(bytes.NewReader(raw))
		scanner.Buffer(make([]byte, 4096), 1<<20)
		pending := ""
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
				continue
			}
			if strings.HasSuffix(line, "\\") {
				pending += strings.TrimSuffix(line, "\\") + " "
				continue
			}
			line = pending + line
			pending = ""
			if header, ok := launchSectionHeader(line); ok {
				section = header
				continue
			}
			key, value, ok := strings.Cut(line, "=")
			key = strings.TrimSpace(key)
			value = strings.TrimSpace(value)
			if !ok {
				return unit, fmt.Errorf("%w: malformed unit directive", ErrLaunchUnsupported)
			}
			switch section + key {
			case "[Service]ExecStart":
				if value == "" {
					starts = nil
				} else {
					starts = append(starts, value)
				}
			case "[Service]ExecStartPre":
				if value == "" {
					pre = nil
				} else {
					pre = append(pre, value)
				}
			case "[Service]Environment":
				if value == "" {
					env = map[string]string{}
					envOrder = nil
					continue
				}
				name, _, ok := strings.Cut(value, "=")
				if !ok || strings.ContainsAny(value, "\\$%`\"'") {
					return unit, fmt.Errorf("%w: unsupported Environment assignment", ErrLaunchUnsupported)
				}
				if _, exists := env[name]; !exists {
					envOrder = append(envOrder, name)
				}
				env[name] = value
			default:
				scalars[section+key] = value
			}
		}
		if scanner.Err() != nil || pending != "" {
			return unit, ErrLaunchUnsupported
		}
	}
	if len(starts) != 1 || len(pre) > 32 {
		return unit, fmt.Errorf("%w: effective ExecStart or ExecStartPre command count", ErrLaunchUnsupported)
	}
	if strings.ContainsAny(starts[0], "\\$%`\"';") {
		return unit, fmt.Errorf("%w: expansion, quoting or semicolon delimiter in ExecStart", ErrLaunchUnsupported)
	}
	unit.execStart = starts[0]
	unit.preCommands = pre
	for _, name := range envOrder {
		if err := unit.applyDirective(runtimeName, "[Service]", "Environment", env[name]); err != nil {
			return unit, fmt.Errorf("%w: unsupported Environment variable %s", err, name)
		}
	}
	for directive, value := range scalars {
		end := strings.Index(directive, "]")
		if end < 0 {
			return unit, fmt.Errorf("%w: directive outside supported section", ErrLaunchUnsupported)
		}
		section, key := directive[:end+1], directive[end+1:]
		if accepted, err := externalDirective(section, key, value); accepted || err != nil {
			if err != nil {
				return unit, err
			}
			continue
		}
		if strings.ContainsAny(value, "\\$%`\"'") {
			return unit, fmt.Errorf("%w: expansion or quoting in %s", ErrLaunchUnsupported, key)
		}
		if err := unit.applyDirective(runtimeName, section, key, value); err != nil {
			return unit, fmt.Errorf("%w: unsupported directive or value %s", err, key)
		}
	}
	return unit, nil
}

var orderingUnit = regexp.MustCompile(`^[A-Za-z0-9_.:-]+\.(service|target|socket|slice)$`)

func externalDirective(section, key, value string) (bool, error) {
	unsupported := func() (bool, error) {
		return true, fmt.Errorf("%w: incompatible directive %s", ErrLaunchUnsupported, key)
	}
	switch section + key {
	case "[Unit]Description", "[Unit]Documentation", "[Install]WantedBy":
		return true, nil
	case "[Unit]After", "[Unit]Before":
		for _, name := range strings.Fields(value) {
			if !orderingUnit.MatchString(name) {
				return unsupported()
			}
		}
		return true, nil
	case "[Service]WorkingDirectory":
		if !filepath.IsAbs(value) || filepath.Clean(value) != value || strings.ContainsAny(value, "\\$%`\"'") {
			return unsupported()
		}
		return true, nil
	case "[Service]TimeoutStartSec", "[Service]TimeoutStopSec", "[Service]RestartSec":
		// A bounded integer duration in seconds needs no systemd expression parsing.
		v, err := strconv.ParseUint(value, 10, 32)
		if err != nil || v == 0 {
			return unsupported()
		}
		return true, nil
	case "[Service]LimitNOFILE", "[Service]LimitMEMLOCK":
		if value == "infinity" {
			return true, nil
		}
		v, err := strconv.ParseUint(value, 10, 64)
		if err != nil || v == 0 {
			return unsupported()
		}
		return true, nil
	case "[Service]StandardOutput", "[Service]StandardError":
		if value != "journal" && value != "null" && value != "inherit" {
			return unsupported()
		}
		return true, nil
	case "[Service]KillMode":
		if value != "control-group" && value != "mixed" {
			return unsupported()
		}
		return true, nil
	case "[Service]RemainAfterExit":
		if value != "no" {
			return unsupported()
		}
		return true, nil
	case "[Service]Slice":
		if value != "app.slice" {
			return unsupported()
		}
		return true, nil
	}
	return false, nil
}

func CheckNativeBindingSources(values map[string]string, unit, launchFile string, dropIns []control.LaunchSource) error {
	if values["FragmentPath"] != launchFile {
		return fmt.Errorf("%w: %s", ErrLaunchChanged, unit)
	}
	paths := strings.Fields(values["DropInPaths"])
	if len(paths) != len(dropIns) {
		return ErrLaunchChanged
	}
	for i, path := range paths {
		if path != dropIns[i].Path {
			return ErrLaunchChanged
		}
	}
	return nil
}

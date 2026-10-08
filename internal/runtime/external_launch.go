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

const launchExpansionCharacters = "\\$%`\"'"

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
		if !trustedLaunchSourceComponent(stat, i, len(parts)) {
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

type externalLaunchDirectives struct {
	scalars               map[string]string
	starts, pre, envOrder []string
	env                   map[string]string
}

// parseExternalLaunchSources models the effective subset rather than rendering
// an adopted service into the owned grammar. Scalars override; command lists
// append, with explicit empty assignments resetting the list.
func parseExternalLaunchSources(sources [][]byte, runtimeName string) (parsedLaunchUnit, error) {
	var unit parsedLaunchUnit
	directives := externalLaunchDirectives{scalars: map[string]string{}, env: map[string]string{}}
	for _, raw := range sources {
		if err := directives.readSource(raw); err != nil {
			return unit, err
		}
	}
	if len(directives.starts) != 1 || len(directives.pre) > 32 {
		return unit, fmt.Errorf("%w: effective ExecStart or ExecStartPre command count", ErrLaunchUnsupported)
	}
	if strings.ContainsAny(directives.starts[0], launchExpansionCharacters+";") {
		return unit, fmt.Errorf("%w: expansion, quoting or semicolon delimiter in ExecStart", ErrLaunchUnsupported)
	}
	unit.execStart = directives.starts[0]
	unit.preCommands = directives.pre
	for _, name := range directives.envOrder {
		if err := unit.applyDirective(runtimeName, "[Service]", "Environment", directives.env[name]); err != nil {
			return unit, fmt.Errorf("%w: unsupported Environment variable %s", err, name)
		}
	}
	for directive, value := range directives.scalars {
		if err := unit.applyExternalScalar(runtimeName, directive, value); err != nil {
			return unit, err
		}
	}
	return unit, nil
}

func (d *externalLaunchDirectives) readSource(raw []byte) error {
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
		if err := d.applyLine(section, line); err != nil {
			return err
		}
	}
	if scanner.Err() != nil || pending != "" {
		return ErrLaunchUnsupported
	}
	return nil
}

func (d *externalLaunchDirectives) applyLine(section, line string) error {
	key, value, ok := strings.Cut(line, "=")
	key = strings.TrimSpace(key)
	value = strings.TrimSpace(value)
	if !ok {
		return fmt.Errorf("%w: malformed unit directive", ErrLaunchUnsupported)
	}
	switch section + key {
	case "[Service]ExecStart":
		d.starts = appendLaunchAssignment(d.starts, value)
	case "[Service]ExecStartPre":
		d.pre = appendLaunchAssignment(d.pre, value)
	case "[Service]Environment":
		return d.applyEnvironment(value)
	default:
		d.scalars[section+key] = value
	}
	return nil
}

func appendLaunchAssignment(commands []string, value string) []string {
	if value == "" {
		return nil
	}
	return append(commands, value)
}

func (d *externalLaunchDirectives) applyEnvironment(value string) error {
	if value == "" {
		d.env = map[string]string{}
		d.envOrder = nil
		return nil
	}
	name, _, ok := strings.Cut(value, "=")
	if !ok || strings.ContainsAny(value, launchExpansionCharacters) {
		return fmt.Errorf("%w: unsupported Environment assignment", ErrLaunchUnsupported)
	}
	if _, exists := d.env[name]; !exists {
		d.envOrder = append(d.envOrder, name)
	}
	d.env[name] = value
	return nil
}

func (unit *parsedLaunchUnit) applyExternalScalar(runtimeName, directive, value string) error {
	end := strings.Index(directive, "]")
	if end < 0 {
		return fmt.Errorf("%w: directive outside supported section", ErrLaunchUnsupported)
	}
	section, key := directive[:end+1], directive[end+1:]
	if accepted, err := externalDirective(section, key, value); accepted || err != nil {
		return err
	}
	if strings.ContainsAny(value, launchExpansionCharacters) {
		return fmt.Errorf("%w: expansion or quoting in %s", ErrLaunchUnsupported, key)
	}
	if err := unit.applyDirective(runtimeName, section, key, value); err != nil {
		return fmt.Errorf("%w: unsupported directive or value %s", err, key)
	}
	return nil
}

func trustedLaunchSourceComponent(stat unix.Stat_t, index, count int) bool {
	writable := stat.Mode&0022 != 0
	if index < count-2 && stat.Uid == 0 && stat.Mode&unix.S_ISVTX != 0 {
		writable = false
	}
	trustedOwner := stat.Uid == 0 || stat.Uid == uint32(os.Geteuid())
	leaf := index == count-1
	return trustedOwner && !writable && (!leaf || (stat.Mode&unix.S_IFMT == unix.S_IFREG && stat.Mode&0444 != 0 && stat.Size <= 1<<20))
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
		if !validOrderingUnits(value) {
			return unsupported()
		}
		return true, nil
	case "[Service]WorkingDirectory":
		if !filepath.IsAbs(value) || filepath.Clean(value) != value || strings.ContainsAny(value, launchExpansionCharacters) {
			return unsupported()
		}
		return true, nil
	case "[Service]TimeoutStartSec", "[Service]TimeoutStopSec", "[Service]RestartSec":
		if !validExternalNumber(value, 32, false) {
			return unsupported()
		}
		return true, nil
	case "[Service]LimitNOFILE", "[Service]LimitMEMLOCK":
		if !validExternalNumber(value, 64, true) {
			return unsupported()
		}
		return true, nil
	case "[Service]StandardOutput", "[Service]StandardError", "[Service]KillMode", "[Service]RemainAfterExit", "[Service]Slice":
		if !validExternalServiceValue(key, value) {
			return unsupported()
		}
		return true, nil
	}
	return false, nil
}

func validOrderingUnits(value string) bool {
	for _, name := range strings.Fields(value) {
		if !orderingUnit.MatchString(name) {
			return false
		}
	}
	return true
}

func validExternalNumber(value string, bits int, allowInfinity bool) bool {
	if allowInfinity && value == "infinity" {
		return true
	}
	v, err := strconv.ParseUint(value, 10, bits)
	return err == nil && v != 0
}

func validExternalServiceValue(key, value string) bool {
	switch key {
	case "StandardOutput", "StandardError":
		return value == "journal" || value == "null" || value == "inherit"
	case "KillMode":
		return value == "control-group" || value == "mixed"
	case "RemainAfterExit":
		return value == "no"
	case "Slice":
		return value == "app.slice"
	}
	return false
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

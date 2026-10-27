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
	"sort"
	"strconv"
	"strings"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

const (
	launchExpansionCharacters = "\\$%`\"'"
	unitWantsDirective        = "[Unit]Wants"
	networkOnlineTarget       = "network-online.target"
)

// readLaunchSource reads a unit or drop-in file after the shared path trust
// rules pass (owner you/root, not world-writable, symlinks resolved).
func readLaunchSource(path string) ([]byte, error) {
	return readTrustedLaunchSource(path, false)
}

func readTrustedLaunchSource(path string, rootOnly bool) ([]byte, error) {
	resolved, err := trustPathCoreMode(path, kindFile, rootOnly)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrLaunchUnsupported, err)
	}
	file, err := os.Open(resolved)
	if err != nil {
		return nil, err
	}
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
		return nil, fmt.Errorf("launch source: %w", err)
	}
	sources := [][]byte{raw}
	for i, source := range dropIns {
		raw, err := readLaunchSource(source.Path)
		if err != nil {
			return nil, fmt.Errorf("contributing drop-in %d: %w", i+1, err)
		}
		if fmt.Sprintf("%x", sha256.Sum256(raw)) != source.SHA256 {
			return nil, fmt.Errorf("%w: contributing drop-in %d content digest changed; review this drop-in and refresh the binding", ErrLaunchChanged, i+1)
		}
		sources = append(sources, raw)
	}
	return sources, nil
}

type externalLaunchDirectives struct {
	accountHome           *accountHomeSnapshot
	scalars               map[string]string
	starts, pre, envOrder []string
	env                   map[string]string
}

// parseExternalLaunchSources models the effective subset rather than rendering
// an adopted service into the owned grammar. Scalars override; command lists
// append, with explicit empty assignments resetting the list.
func parseExternalLaunchSources(sources [][]byte) (parsedLaunchUnit, error) {
	return parseExternalLaunchSourcesWithAccountHome(sources, localAccountHome)
}

func parseExternalLaunchSourcesWithAccountHome(sources [][]byte, readHome func() (string, error)) (parsedLaunchUnit, error) {
	snapshot := &accountHomeSnapshot{read: readHome}
	unit := parsedLaunchUnit{accountHome: snapshot}
	directives := externalLaunchDirectives{scalars: map[string]string{}, env: map[string]string{}, accountHome: snapshot}
	var problems launchProblems
	for _, raw := range sources {
		problems.add(directives.readSource(raw))
	}
	problems.add(unit.applyEffectiveCommands(directives))
	for _, name := range directives.envOrder {
		problems.add(unit.applyDirective("[Service]", "Environment", directives.env[name]))
	}
	keys := make([]string, 0, len(directives.scalars))
	for directive := range directives.scalars {
		keys = append(keys, directive)
	}
	sort.Strings(keys)
	for _, directive := range keys {
		problems.add(unit.applyExternalScalar(directive, directives.scalars[directive]))
	}
	return unit, problems.err()
}

func (d *externalLaunchDirectives) readSource(raw []byte) error {
	section := ""
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	pending := ""
	var problems launchProblems
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
		problems.add(d.applyLine(section, line))
	}
	if scanner.Err() != nil || pending != "" {
		problems.add(ErrLaunchUnsupported)
	}
	return problems.err()
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
	case unitWantsDirective:
		return d.applyWants(value)
	default:
		d.scalars[section+key] = value
	}
	return nil
}

// Dependencies accumulate across sources; empty assignments cannot remove an
// earlier dependency. The supported set has one member, so retain a canonical
// constant-size representation rather than concatenating duplicate lines.
func (d *externalLaunchDirectives) applyWants(value string) error {
	if _, exists := d.scalars[unitWantsDirective]; !exists {
		d.scalars[unitWantsDirective] = ""
	}
	for _, name := range strings.Fields(value) {
		if name != networkOnlineTarget {
			return fmt.Errorf("%w: incompatible directive Wants", ErrLaunchUnsupported)
		}
		d.scalars[unitWantsDirective] = networkOnlineTarget
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
	var problems launchProblems
	for len(strings.TrimSpace(value)) > 0 {
		assignment, remaining, err := nextEnvironmentAssignment(strings.TrimSpace(value))
		if err != nil {
			problems.add(err)
			break
		}
		value = remaining
		assignments, err := externalEnvironmentAssignments(assignment, d.accountHome)
		if err != nil {
			problems.add(err)
			continue
		}
		for _, assignment := range assignments {
			name, _, _ := strings.Cut(assignment, "=")
			if _, exists := d.env[name]; !exists {
				d.envOrder = append(d.envOrder, name)
			}
			d.env[name] = assignment
		}
	}
	return problems.err()
}

func externalEnvironmentAssignments(assignment string, snapshot *accountHomeSnapshot) ([]string, error) {
	if strings.Contains(assignment, "%") {
		expanded, err := snapshot.expand(assignment)
		if err != nil {
			return nil, err
		}
		assignment = expanded
	}
	name, _, ok := strings.Cut(assignment, "=")
	if !ok || !environmentName.MatchString(name) {
		return nil, environmentError("", "expected NAME=value")
	}
	if strings.ContainsAny(assignment, launchExpansionCharacters+"\r\n\t ") {
		return nil, environmentError(name, "expansion, escapes or whitespace are unsupported; use one literal whole assignment")
	}
	return []string{assignment}, nil
}

func (unit *parsedLaunchUnit) applyExternalScalar(directive, value string) error {
	end := strings.Index(directive, "]")
	if end < 0 {
		return fmt.Errorf("%w: directive outside supported section", ErrLaunchUnsupported)
	}
	section, key := directive[:end+1], directive[end+1:]
	if !environmentName.MatchString(key) || len(key) > 64 {
		return fmt.Errorf("%w: unrecognized directive", ErrLaunchUnsupported)
	}
	value, err := unit.qualifyExternalWorkingDirectory(directive, value)
	if err != nil {
		return err
	}
	if accepted, err := externalDirective(section, key, value); accepted || err != nil {
		if err == nil {
			unit.recordLoadedProperty(key, value)
		}
		return err
	}
	if strings.ContainsAny(value, launchExpansionCharacters) {
		return fmt.Errorf("%w: expansion or quoting in %s", ErrLaunchUnsupported, key)
	}
	if err := unit.applyDirective(section, key, value); err != nil {
		return fmt.Errorf("%w: unsupported directive or value %s", err, key)
	}
	return nil
}

var orderingUnit = regexp.MustCompile(`^[A-Za-z0-9_.:-]+\.(service|target|socket|slice)$`)

func externalDirective(section, key, value string) (bool, error) {
	unsupported := func() (bool, error) {
		return true, fmt.Errorf("%w: incompatible directive %s", ErrLaunchUnsupported, key)
	}
	if accepted, valid := compatibleIsolationDirective(section, key, value); accepted {
		if !valid {
			return unsupported()
		}
		return true, nil
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

func expandAccountHomePath(value, home string) (string, error) {
	if !filepath.IsAbs(home) || filepath.Clean(home) != home || strings.ContainsAny(home, launchExpansionCharacters+" ;\t\r\n") {
		return "", fmt.Errorf("%w: local account home cannot be qualified for %%h", ErrLaunchUnsupported)
	}
	var result strings.Builder
	for i := 0; i < len(value); i++ {
		if value[i] != '%' {
			result.WriteByte(value[i])
			continue
		}
		if i+1 >= len(value) || value[i+1] != 'h' || (i > 0 && !strings.ContainsRune(" \t=:\"'", rune(value[i-1]))) || (i+2 < len(value) && !strings.ContainsRune("/ \t\"'", rune(value[i+2]))) {
			return "", fmt.Errorf("%w: only the OS account home %%h path specifier is supported", ErrLaunchUnsupported)
		}
		result.WriteString(home)
		i++
	}
	return result.String(), nil
}

// CheckLoadedAccountHomeDirectory binds %h WorkingDirectory to systemd's
// resolved property; empty means this launch did not request that specifier.
func CheckLoadedAccountHomeDirectory(observed, expected string) error {
	if expected != "" && observed != expected {
		return fmt.Errorf("%w: loaded WorkingDirectory differs from OS account home expansion; reload and refresh the binding", ErrLaunchChanged)
	}
	return nil
}

func (unit *parsedLaunchUnit) applyEffectiveCommands(d externalLaunchDirectives) error {
	var problems launchProblems
	if len(d.pre) > 32 {
		problems.add(fmt.Errorf("%w: effective ExecStartPre command count exceeds supported limit; preparation paths were not inspected", ErrLaunchUnsupported))
	} else {
		unit.preCommands = d.pre
	}
	if len(d.starts) != 1 {
		problems.add(fmt.Errorf("%w: effective ExecStart command count", ErrLaunchUnsupported))
		return problems.err()
	}
	command := d.starts[0]
	if strings.Contains(command, "%") {
		expanded, err := unit.accountHome.expand(command)
		if err != nil {
			problems.add(err)
			return problems.err()
		}
		command = expanded
	}
	if strings.ContainsAny(command, launchExpansionCharacters+";") {
		problems.add(fmt.Errorf("%w: expansion, quoting or semicolon delimiter in ExecStart", ErrLaunchUnsupported))
	} else {
		unit.execStart = command
	}
	return problems.err()
}

func (unit *parsedLaunchUnit) qualifyAccountHomeWorkingDirectory(value string) (string, error) {
	expanded, err := unit.accountHome.expand(value)
	if err != nil {
		return "", err
	}
	unit.homeWorkingDirectory = expanded
	if err := qualifyEnvironmentPath(expanded, false); err != nil {
		return "", fmt.Errorf("%w: WorkingDirectory: %w", ErrLaunchUnsupported, err)
	}
	return expanded, nil
}

func (unit *parsedLaunchUnit) qualifyExternalWorkingDirectory(directive, value string) (string, error) {
	if directive != "[Service]WorkingDirectory" {
		return value, nil
	}
	if strings.Contains(value, "%") {
		expanded, err := unit.qualifyAccountHomeWorkingDirectory(value)
		if err != nil {
			return "", err
		}
		value = expanded
	}
	if err := qualifyEnvironmentPath(value, false); err != nil {
		return "", fmt.Errorf("%w: WorkingDirectory: %w", ErrLaunchUnsupported, err)
	}
	unit.workingDirectory = value
	return value, nil
}

func (unit *parsedLaunchUnit) recordLoadedProperty(key, value string) {
	canonical, ok := qualifiedLoadedProperty(key, value)
	if !ok {
		return
	}
	if unit.loadedProperties == nil {
		unit.loadedProperties = map[string]string{}
	}
	unit.loadedProperties[key] = canonical
}

func compatibleIsolationDirective(section, key, value string) (bool, bool) {
	switch section + key {
	case unitWantsDirective:
		for _, name := range strings.Fields(value) {
			if name != networkOnlineTarget {
				return true, false
			}
		}
		return true, true
	case "[Service]KillSignal":
		return true, value == "SIGINT"
	case "[Service]NoNewPrivileges":
		return true, value == "true"
	case "[Service]PrivateTmp":
		return true, value == "true" || value == "false"
	case "[Service]ProtectHome":
		return true, value == "read-only"
	case "[Service]ProtectSystem":
		return true, value == "strict"
	}
	return false, false
}

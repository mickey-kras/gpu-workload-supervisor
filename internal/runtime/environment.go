package runtime

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

var environmentName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Only whole-assignment quotes are accepted. Escapes, expansion, concatenated
// fragments and values containing whitespace remain unsupported; diagnostics
// contain the variable name only, never its value.
func environmentAssignments(value string) ([]string, error) {
	var assignments []string
	for len(strings.TrimSpace(value)) > 0 {
		value = strings.TrimSpace(value)
		assignment, remaining, err := nextEnvironmentAssignment(value)
		if err != nil {
			return nil, err
		}
		value = remaining
		name, _, ok := strings.Cut(assignment, "=")
		if !ok || !environmentName.MatchString(name) {
			return nil, environmentError("", "expected NAME=value")
		}
		if strings.ContainsAny(assignment, launchExpansionCharacters+"\r\n\t ") {
			return nil, environmentError(name, "expansion, escapes or whitespace are unsupported; use one literal whole assignment")
		}
		assignments = append(assignments, assignment)
	}
	return assignments, nil
}
func nextEnvironmentAssignment(value string) (string, string, error) {
	assignment := ""
	if value[0] == '\'' || value[0] == '"' {
		quote := value[0]
		value = value[1:]
		end := strings.IndexByte(value, quote)
		if end < 0 {
			return "", "", environmentError(environmentHint(value), "unterminated whole-assignment quote")
		}
		assignment, value = value[:end], value[end+1:]
		if value != "" && value[0] != ' ' && value[0] != '\t' {
			return "", "", environmentError(environmentHint(assignment), "quoted fragments are unsupported")
		}
	} else {
		end := strings.IndexAny(value, " \t")
		if end < 0 {
			assignment, value = value, ""
		} else {
			assignment, value = value[:end], value[end:]
		}
	}
	return assignment, value, nil
}

func environmentHint(value string) string {
	name, _, ok := strings.Cut(strings.TrimLeft(value, "\"'"), "=")
	if !ok {
		return ""
	}
	if environmentName.MatchString(name) {
		return name
	}
	return ""
}
func environmentError(name, reason string) error {
	if len(name) > 64 || !environmentName.MatchString(name) {
		name = ""
	}
	if name != "" {
		name = " variable " + name
	}
	return fmt.Errorf("%w: Environment%s: %s", ErrLaunchUnsupported, name, reason)
}

func (u *parsedLaunchUnit) applySafeEnvironment(assignment string) error {
	name, value, ok := strings.Cut(assignment, "=")
	if !ok || !environmentName.MatchString(name) {
		return environmentError("", "expected NAME=value")
	}
	if u.environment == nil {
		u.environment = map[string]bool{}
	}
	if u.environment[name] {
		return environmentError(name, "duplicate assignment")
	}
	u.environment[name] = true
	if u.environmentValues == nil {
		u.environmentValues = map[string]string{}
	}
	u.environmentValues[name] = value
	return u.qualifyEnvironmentValue(name, value)
}

func (u *parsedLaunchUnit) qualifyEnvironmentValue(name, value string) error {
	switch name {
	case "CUDA_VISIBLE_DEVICES":
		return u.applyGPUVisibility(value)
	case "HOME", "HF_HOME", "HUGGINGFACE_HUB_CACHE", "TRANSFORMERS_CACHE", "XDG_CACHE_HOME", "TMPDIR", "HF_TOKEN_PATH":
		return qualifyEnvironmentPath(value, name == "HF_TOKEN_PATH")
	case "PATH", "LD_LIBRARY_PATH", "PYTHONPATH":
		return qualifyEnvironmentSearchPaths(name, value)
	case "LD_PRELOAD", "LD_AUDIT", "PYTHONHOME", "PYTHONUSERBASE", "PYTHONSTARTUP":
		// These settings load code or select an interpreter installation through
		// mechanisms that are not covered by the directory-search checks above.
		// An empty value disables the override and is safe to retain.
		if value != "" {
			return environmentError(name, "nonempty code-loading overrides are unsupported")
		}
		return nil
	default:
		return nil
	}
}

func qualifyEnvironmentSearchPaths(name, value string) error {
	// The dynamic linker accepts semicolons as well as colons. Preserve empty
	// components when splitting because those can enable current-directory lookup.
	if name == "LD_LIBRARY_PATH" {
		value = strings.ReplaceAll(value, ";", ":")
	}
	for _, path := range strings.Split(value, ":") {
		if path == "" {
			return environmentError(name, "empty search path components can select the current directory")
		}
		if err := qualifyEnvironmentSearchPath(path); err != nil {
			return err
		}
	}
	return nil
}

// qualifyEnvironmentPath checks an existing directory (file=false) or regular
// file (file=true) with the shared path trust rules.
func qualifyEnvironmentPath(path string, file bool) error {
	if strings.ContainsAny(path, launchExpansionCharacters) {
		return environmentError(pathName(path), "expansion characters are unsupported; use a literal path")
	}
	kind := kindDir
	if file {
		kind = kindFile
	}
	if _, err := trustPath(path, kind); err != nil {
		return environmentError(pathName(path), reasonText(err))
	}
	return nil
}

// reasonText strips the sentinel prefix trustPath already added.
func reasonText(err error) string {
	return strings.TrimPrefix(err.Error(), ErrLaunchUnsupported.Error()+": ")
}

func pathName(path string) string {
	if len(path) > 64 {
		return ""
	}
	return path
}

func qualifyEnvironmentSearchPath(path string) error {
	if _, err := trustPath(path, kindDir); err != nil {
		return environmentError(pathName(path), reasonText(err))
	}
	return nil
}

// CheckLoadedEnvironment requires exact effective values for newly qualified
// settings. Error messages identify only the variable name, never secrets.
func CheckLoadedEnvironment(output string, expected map[string]string) error {
	if len(expected) == 0 {
		return nil
	}
	assignments, err := environmentAssignments(output)
	if err != nil {
		return err
	}
	observed := map[string]string{}
	var problems launchProblems
	for _, assignment := range assignments {
		name, value, _ := strings.Cut(assignment, "=")
		if _, ok := observed[name]; ok {
			problems.add(environmentError(name, "loaded metadata is ambiguous; reload and retry"))
		}
		observed[name] = value
	}
	names := sortedEnvironmentKeys(expected)
	for _, name := range names {
		if observed[name] != expected[name] {
			problems.add(environmentError(name, "loaded setting differs from inspected source; reload and refresh the binding"))
		}
	}
	names = sortedEnvironmentKeys(observed)
	for _, name := range names {
		if _, ok := expected[name]; !ok {
			problems.add(environmentError(name, "loaded setting has no inspected source; reload and refresh the binding"))
		}
	}
	return problems.err()
}

func sortedEnvironmentKeys(values map[string]string) []string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

package setup

import (
	"context"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

func (b Backend) discoverApplications(ctx context.Context, result *Discovery, units []string) {
	// Only these four local default origins are probed. Non-default instances use
	// an explicit endpoint selection; no port, process, or filesystem scanning.
	for _, r := range []ProbeRequest{{App: "comfyui", Endpoint: "http://127.0.0.1:8188"}, {App: "ollama", Endpoint: "http://127.0.0.1:11434"}, {App: appLlamaCPP, Endpoint: "http://127.0.0.1:8080"}, {App: "vllm", Endpoint: "http://127.0.0.1:8000"}} {
		found, err := b.probeApplication(ctx, r)
		if err != nil {
			found = candidate(r)
			found.InstanceStatus = "unreachable"
			found.NextStep = "Discovery timed out or was cancelled. Check the endpoint and retry."
		}
		result.Applications = append(result.Applications, found)
	}
	for _, unit := range units {
		if ctx.Err() != nil {
			return
		}
		b.discoverUnit(ctx, result, unit)
	}
}

// unitRelevant retains existing profile units under arbitrary names without
// assuming that their unit identity establishes any of the supported applications.
func unitRelevant(unit string, profiles []control.WorkloadProfile) bool {
	lower := strings.ToLower(unit)
	for _, name := range []string{"comfyui", "ollama", "llama", "vllm"} {
		if strings.Contains(lower, name) {
			return true
		}
	}
	for _, profile := range profiles {
		if profile.Unit == unit {
			return true
		}
	}
	return false
}
func (b Backend) discoverUnit(ctx context.Context, result *Discovery, unit string) {
	if !unitRelevant(unit, result.Request.Catalog.Profiles) {
		return
	}
	output, err := b.runCommand(ctx, "/usr/bin/systemctl", "--user", "show", unit, "--property=ExecStart,ControlGroup,ActiveState,SubState", "--no-pager")
	if err != nil || len(output) > 1048576 {
		return
	}
	app := appFromUnit(string(output))
	if app == "" {
		return
	}
	found := candidate(ProbeRequest{App: app, Reference: unit, ReferenceKind: "configuration"})
	found.Reference = ""
	found.ReferenceKind = ""
	found.Unit = unit
	found.Label = appLabel(app) + " - " + unit
	values := unitProperties(string(output))
	found.Cgroup = values["ControlGroup"]
	if models := launchModels(app, string(output)); models != nil {
		found.Models = models
	}
	if values["ActiveState"] == "inactive" && values["SubState"] == "dead" {
		found.InstanceStatus = "not-running"
		found.NextStep = "This instance is stopped. Select its existing launch configuration or start it separately to read its inventory."
	} else {
		found.NextStep = "Select this instance's endpoint or existing launch configuration. Lifecycle control is unverified."
	}
	if app == "comfyui" {
		found.InventoryStatus = "not-applicable"
	}
	result.Units = append(result.Units, unit)
	result.Applications = append(result.Applications, found)
}
func unitProperties(output string) map[string]string {
	values := map[string]string{}
	for _, line := range strings.Split(output, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			values[key] = value
		}
	}
	return values
}

var execPath = regexp.MustCompile(`(?:^|[ {])path=([^ ;]+)\s*;`)
var execArguments = regexp.MustCompile(`argv\[\]=([^;]+)\s*;`)

func appFromUnit(output string) string {
	start := unitProperties(output)["ExecStart"]
	paths := execPath.FindAllStringSubmatch(start, -1)
	if len(paths) != 1 {
		return ""
	}
	switch filepath.Base(paths[0][1]) {
	case "ollama":
		return "ollama"
	case "llama-server":
		return appLlamaCPP
	case "vllm":
		return "vllm"
	}
	executable := filepath.Base(paths[0][1])
	if executable != "python" && executable != "python3" && !strings.HasPrefix(executable, "python3.") {
		return ""
	}
	arguments := execArguments.FindStringSubmatch(start)
	if len(arguments) != 2 {
		return ""
	}
	fields := strings.Fields(arguments[1])
	if len(fields) < 2 {
		return ""
	}
	if len(fields) >= 3 && fields[1] == "-m" && (fields[2] == "vllm.entrypoints.openai.api_server" || fields[2] == "vllm") {
		return "vllm"
	}
	if filepath.Base(fields[1]) == "main.py" && strings.EqualFold(filepath.Base(filepath.Dir(fields[1])), "ComfyUI") {
		return "comfyui"
	}
	return ""
}

// launchModels extracts only simple, explicit launch arguments. Escaped or
// shell-based commands remain manual references instead of being interpreted.
func launchModels(app, output string) []ModelCandidate {
	if app != appLlamaCPP && app != "vllm" || appFromUnit(output) != app {
		return nil
	}
	start := unitProperties(output)["ExecStart"]
	arguments := execArguments.FindStringSubmatch(start)
	if len(arguments) != 2 || strings.ContainsAny(arguments[1], "\\\"'\n") {
		return nil
	}
	model, aliases := launchArguments(app, strings.Fields(arguments[1]))
	if !validModelID(model) {
		return nil
	}
	locality := "unknown"
	if filepath.IsAbs(model) {
		locality = "local"
	}
	return []ModelCandidate{{ID: model, Label: model, Source: "configuration", Loaded: "unknown", Locality: locality, Aliases: aliases}}
}
func launchArguments(app string, fields []string) (string, []string) {
	model := ""
	var aliases []string
	for i := 1; i < len(fields); i++ {
		if value, ok := modelFlagValue(app, fields, i); ok {
			model = value
			i++
			continue
		}
		if strings.HasPrefix(fields[i], "--model=") {
			model = strings.TrimPrefix(fields[i], "--model=")
			continue
		}
		i = consumeAliasFlags(fields, i, &aliases)
	}
	return model, aliases
}
func modelFlagValue(app string, fields []string, i int) (string, bool) {
	if (app == appLlamaCPP && fields[i] == "-m") || fields[i] == "--model" || (app == "vllm" && fields[i] == "serve") {
		if i+1 < len(fields) && !strings.HasPrefix(fields[i+1], "-") {
			return fields[i+1], true
		}
	}
	return "", false
}
func consumeAliasFlags(fields []string, i int, aliases *[]string) int {
	flag := fields[i]
	if flag != "--alias" && flag != "--served-model-name" {
		return i
	}
	for i+1 < len(fields) && !strings.HasPrefix(fields[i+1], "-") {
		i++
		*aliases = append(*aliases, fields[i])
		if flag == "--alias" {
			break
		}
	}
	return i
}

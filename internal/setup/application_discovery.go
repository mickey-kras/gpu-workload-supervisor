package setup

import (
	"context"
	"errors"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

const execStartPropertyPrefix = "ExecStart="
const inspectionFailedStatus = "inspection-failed"

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
	if len(units) > 256 {
		result.Errors = append(result.Errors, "Too many services for bounded application discovery. Choose a supported installation explicitly.")
		return
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
	values, err := b.showApplicationCandidate(ctx, unit)
	if err != nil {
		addFailedUnitInspection(result, unit, values, err)
		return
	}
	app := appFromUnit(execStartPropertyPrefix + values["ExecStart"])
	if app == "" {
		addUnsupportedUnit(result, unit)
		return
	}
	found := candidate(ProbeRequest{App: app, Reference: unit, ReferenceKind: "configuration"})
	found.Reference = ""
	found.ReferenceKind = ""
	found.Unit = unit
	found.Label = appLabel(app) + " - " + unit
	found.Location = values["FragmentPath"]
	found.Cgroup = values["ControlGroup"]
	found.ConfigurationStatus = "unsupported"
	if models := launchModels(app, execStartPropertyPrefix+values["ExecStart"]); models != nil {
		found.Models = models
	}
	if values["ActiveState"] == "inactive" && values["SubState"] == "dead" {
		found.InstanceStatus = "not-running"
	}
	model := ""
	if app == "ollama" {
		model = "selection-pending"
	}
	profile, err := b.configurationFromMetadata(ctx, app, unit, model, values)
	if err != nil {
		found.NextStep = err.Error()
		found.ConfigurationStatus = inspectionFailedStatus
		if errors.Is(err, gpuruntime.ErrModelUnavailable) {
			found.ConfigurationStatus = "model-missing"
			found.InventoryStatus = "missing"
		}
	} else {
		b.recognizeUnit(ctx, &found, app, unit, profile)
	}
	if app == "comfyui" {
		found.InventoryStatus = "not-applicable"
	}
	result.Units = append(result.Units, unit)
	result.Applications = append(result.Applications, found)
}
func addFailedUnitInspection(result *Discovery, unit string, values map[string]string, err error) {
	app := appFromUnit(execStartPropertyPrefix + values["ExecStart"])
	if app == "" && !unitRelevant(unit, result.Request.Catalog.Profiles) {
		return
	}
	if app == "" {
		app = candidateApplicationHint(unit, result.Request.Catalog.Profiles)
	}
	found := candidate(ProbeRequest{App: app, Reference: unit, ReferenceKind: "configuration"})
	found.Unit = unit
	found.InstanceStatus = inspectionFailedStatus
	found.ConfigurationStatus = inspectionFailedStatus
	found.NextStep = "Could not inspect this application: " + err.Error()
	result.Applications = append(result.Applications, found)
}

func addUnsupportedUnit(result *Discovery, unit string) {
	if unitRelevant(unit, result.Request.Catalog.Profiles) {
		found := candidate(ProbeRequest{App: candidateApplicationHint(unit, result.Request.Catalog.Profiles), Reference: unit, ReferenceKind: "configuration"})
		found.Unit = unit
		found.InstanceStatus = "unsupported"
		found.ConfigurationStatus = "unsupported"
		found.NextStep = "This launch is not supported. Choose a supported direct application configuration."
		result.Applications = append(result.Applications, found)
	}
}

func (b Backend) recognizeUnit(ctx context.Context, found *ApplicationCandidate, app, unit string, profile control.WorkloadProfile) {
	found.Recognized = true
	found.ConfigurationStatus = "ready"
	endpoint := ""
	launchFile := ""
	instance := ""
	model := ""
	if profile.NativeModel != nil {
		endpoint = profile.NativeModel.Endpoint
		launchFile = profile.NativeModel.LaunchFile
		instance = profile.NativeModel.Instance
		model = profile.NativeModel.Model
	}
	if profile.LaunchBinding != nil {
		endpoint = profile.LaunchBinding.Endpoint
		launchFile = profile.LaunchBinding.LaunchFile
	}
	found.Endpoint = endpoint
	found.Cgroup = profile.Cgroup
	found.Binding = &DraftBinding{Unit: unit, Cgroup: profile.Cgroup, HealthURL: profile.HealthURL, Instance: instance, Model: model, LaunchFile: launchFile}
	if app == "ollama" {
		found.Binding.Model = ""
		found.ConfigurationStatus = "model-required"
	}
	found.NextStep = "Ready to configure. Finish to review and confirm control of this installation."
	b.discoverUnitInventory(ctx, found, app)
}

func (b Backend) discoverUnitInventory(ctx context.Context, found *ApplicationCandidate, app string) {
	if found.InstanceStatus == "not-running" {
		found.NextStep = "Installed and stopped. Ready to configure."
	} else {
		observed, probeErr := b.probeApplication(ctx, ProbeRequest{App: app, Endpoint: found.Endpoint})
		if probeErr != nil {
			found.NextStep = "Application inventory could not be read. Retry or choose an existing model."
		} else {
			found.InstanceStatus = observed.InstanceStatus
			found.InventoryStatus = observed.InventoryStatus
			if app == "ollama" {
				found.Models = observed.Models
			}
		}
	}
}

func applicationHint(unit string) string {
	lower := strings.ToLower(unit)
	for _, app := range []string{"comfyui", "ollama", "vllm"} {
		if strings.Contains(lower, app) {
			return app
		}
	}
	if strings.Contains(lower, "llama") {
		return appLlamaCPP
	}
	return ""
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
	if len(paths) != 1 || !filepath.IsAbs(paths[0][1]) {
		return ""
	}
	arguments := execArguments.FindAllStringSubmatch(start, -1)
	if len(arguments) != 1 || strings.ContainsAny(arguments[0][1], "\\\"'\n") {
		return ""
	}
	fields := strings.Fields(arguments[0][1])
	if len(fields) < 2 || fields[0] != paths[0][1] {
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

func candidateApplicationHint(unit string, profiles []control.WorkloadProfile) string {
	for _, p := range profiles {
		if p.Unit == unit {
			if p.NativeModel != nil {
				return p.NativeModel.Runtime
			}
			if p.LaunchBinding != nil {
				return p.LaunchBinding.Runtime
			}
		}
	}
	return applicationHint(unit)
}

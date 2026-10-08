package setup

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
)

type PrepareRequest struct {
	Draft Draft `json:"draft"`
}
type PreparedApplication struct {
	Profile control.WorkloadProfile `json:"profile"`
}

var selectedUnitName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*\.service$`)

func Prepare(ctx context.Context, request PrepareRequest) (PreparedApplication, error) {
	return SystemBackend().Prepare(ctx, request)
}
func (b Backend) Prepare(ctx context.Context, request PrepareRequest) (PreparedApplication, error) {
	d := request.Draft
	if err := validateDraft(d, map[string]bool{}); err != nil {
		return PreparedApplication{}, err
	}
	if d.Binding == nil || d.Binding.Unit == "" {
		unit, err := b.unitAtReference(ctx, d)
		if err != nil {
			return PreparedApplication{}, err
		}
		d.Binding = &DraftBinding{Unit: unit}
	}
	if !selectedUnitName.MatchString(d.Binding.Unit) {
		return PreparedApplication{}, errors.New("choose a recognized installed application first")
	}
	found, err := b.automaticConfiguration(ctx, d.App, d.Binding.Unit, d.Model)
	if err != nil {
		return PreparedApplication{}, err
	}
	endpoint, launchFile, instance, model := "", "", "", ""
	if found.NativeModel != nil {
		endpoint = found.NativeModel.Endpoint
		launchFile = found.NativeModel.LaunchFile
		instance = found.NativeModel.Instance
		model = found.NativeModel.Model
	}
	if found.LaunchBinding != nil {
		endpoint = found.LaunchBinding.Endpoint
		launchFile = found.LaunchBinding.LaunchFile
	}
	for _, pair := range [][2]string{{d.Endpoint, endpoint}, {d.Binding.Cgroup, found.Cgroup}, {d.Binding.HealthURL, found.HealthURL}, {d.Binding.LaunchFile, launchFile}, {d.Binding.Instance, instance}, {d.Binding.Model, model}} {
		if pair[0] != "" && pair[0] != pair[1] {
			return PreparedApplication{}, errors.New("installation settings changed or the override does not match its supported launch; refresh and retry")
		}
	}
	if d.App != "comfyui" && d.App != "ollama" && d.Model != "" && d.Model != model {
		return PreparedApplication{}, errors.New("selected model does not match the existing launch; choose its configured model")
	}
	found.ID = control.Workload(d.ID)
	found.Label = d.Label
	if err := (control.Catalog{Version: 1, Profiles: []control.WorkloadProfile{found}}).Validate(); err != nil {
		return PreparedApplication{}, err
	}
	return PreparedApplication{Profile: found}, nil
}

func (b Backend) showAutomatic(ctx context.Context, unit string) (map[string]string, error) {
	out, err := b.runCommand(ctx, "/usr/bin/systemctl", "--user", "show", "--property=Id,LoadState,ExecStart,ControlGroup,ActiveState,SubState,FragmentPath,DropInPaths,NeedDaemonReload,Slice", "--no-pager", "--", unit)
	if err != nil {
		return nil, fmt.Errorf("could not inspect installation %s: %w", unit, err)
	}
	if len(out) > commandOutputLimit {
		return nil, errors.New("systemd installation metadata exceeds supported limit")
	}
	return gpuruntime.ParseUnitProperties(out)
}
func (b Backend) automaticConfiguration(ctx context.Context, app, unit, model string) (control.WorkloadProfile, error) {
	values, err := b.showAutomatic(ctx, unit)
	if err != nil {
		return control.WorkloadProfile{}, err
	}
	return b.configurationFromMetadata(ctx, app, unit, model, values)
}
func (b Backend) configurationFromMetadata(ctx context.Context, app, unit, model string, values map[string]string) (control.WorkloadProfile, error) {
	p := control.WorkloadProfile{Adapter: "systemd", Unit: unit}
	if values["NeedDaemonReload"] != "no" || values["DropInPaths"] != "" {
		return p, errors.New("installation has changed or uses unsupported overrides; reload its supported direct launch and retry")
	}
	if appFromUnit("ExecStart="+values["ExecStart"]) != app {
		return p, errors.New("installation launch does not match the selected application")
	}
	inspect := b.inspectAutomatic
	if inspect == nil {
		inspect = gpuruntime.InspectAutomaticLaunch
	}
	launch, err := inspect(values["FragmentPath"], app)
	if err != nil {
		return p, fmt.Errorf("unsupported existing launch; choose a supported direct local configuration: %w", err)
	}
	args := execArguments.FindAllStringSubmatch(values["ExecStart"], -1)
	if len(args) != 1 || strings.Join(strings.Fields(args[0][1]), " ") != strings.Join(strings.Fields(launch.Command), " ") {
		return p, errors.New("loaded launch configuration differs from its file; reload and retry")
	}
	root, err := b.showAutomatic(ctx, "-.slice")
	if err != nil {
		return p, err
	}
	slice := map[string]string{}
	if values["ControlGroup"] == "" {
		slice, err = b.showAutomatic(ctx, "app.slice")
		if err != nil {
			return p, err
		}
	}
	p.Cgroup, err = gpuruntime.ResolveAutomaticCgroup(unit, values, slice, root)
	if err != nil {
		return p, err
	}
	if values["ControlGroup"] == "" {
		out, err := b.runCommand(ctx, "/usr/bin/systemctl", "--version")
		if err != nil {
			return p, err
		}
		version, err := gpuruntime.SupportedSystemdPlacementVersion(out)
		if err != nil {
			return p, err
		}
		p.SystemdSlice = "app.slice"
		p.SystemdVersion = version
	}
	if err := (ProbeRequest{App: app, Endpoint: launch.Endpoint}).validate(); err != nil {
		return p, err
	}
	if app == "comfyui" {
		p.HealthURL = launch.Endpoint + "/system_stats"
		p.LaunchBinding = &control.LaunchBinding{Runtime: app, Endpoint: launch.Endpoint, LaunchFile: values["FragmentPath"], LaunchSHA256: launch.SHA256}
	} else {
		if app != "ollama" {
			model = launch.Model
		}
		if app == "ollama" && model == "" {
			return p, errors.New("choose an existing local Ollama model; setup will not start Ollama or download models")
		}
		p.HealthURL = launch.Endpoint + "/health"
		if app == "ollama" {
			p.HealthURL = launch.Endpoint + "/api/tags"
		}
		p.NativeModel = &control.NativeModel{Runtime: app, Instance: "instance-" + candidate(ProbeRequest{App: app, Reference: unit, ReferenceKind: "configuration"}).ID, Model: model, Endpoint: launch.Endpoint, LaunchFile: values["FragmentPath"], LaunchSHA256: launch.SHA256}
	}
	return p, nil
}

func (b Backend) unitAtReference(ctx context.Context, d Draft) (string, error) {
	if d.Endpoint == "" && (d.Reference == "" || (d.ReferenceKind != "configuration" && d.ReferenceKind != "application-directory" && d.ReferenceKind != "application")) {
		return "", errors.New("choose a recognized installation or its configuration location")
	}
	out, err := b.runCommand(ctx, "/usr/bin/systemctl", "--user", "list-unit-files", "--type=service", "--no-legend", "--no-pager")
	if err != nil || len(out) > commandOutputLimit {
		return "", errors.New("application services could not be listed; check your desktop session and retry")
	}
	units := serviceUnits(out)
	if len(units) > 256 {
		return "", errors.New("too many services for bounded discovery; select a recognized installation explicitly")
	}
	selected := ""
	for _, unit := range units {
		values, err := b.showAutomatic(ctx, unit)
		if err != nil {
			return "", err
		}
		if appFromUnit("ExecStart="+values["ExecStart"]) != d.App {
			continue
		}
		match := values["FragmentPath"] == d.Reference
		if d.Endpoint != "" {
			inspect := b.inspectAutomatic
			if inspect == nil {
				inspect = gpuruntime.InspectAutomaticLaunch
			}
			launch, err := inspect(values["FragmentPath"], d.App)
			if err != nil {
				continue
			}
			match = strings.TrimSuffix(d.Endpoint, "/") == launch.Endpoint
		}
		if d.ReferenceKind == "application-directory" {
			match = filepath.Dir(values["FragmentPath"]) == d.Reference
			args := execArguments.FindStringSubmatch(values["ExecStart"])
			if len(args) == 2 {
				for _, arg := range strings.Fields(args[1]) {
					if filepath.IsAbs(arg) && filepath.Dir(arg) == d.Reference {
						match = true
					}
				}
			}
		}
		if match {
			if selected != "" {
				return "", errors.New("multiple installations use this location; choose the installation explicitly")
			}
			selected = unit
		}
	}
	if selected == "" {
		return "", errors.New("no supported loaded installation uses this location; install a supported direct user service or choose its configuration")
	}
	return selected, nil
}

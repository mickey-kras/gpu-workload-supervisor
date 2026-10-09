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

const automaticSystemctlPath = "/usr/bin/systemctl"

type PrepareRequest struct {
	Draft Draft `json:"draft"`
}
type PreparedApplication struct {
	Profile control.WorkloadProfile `json:"profile"`
}

var selectedUnitName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*\.service$`)

// ErrOwnedDraftPrepare rejects owned launch drafts: prepare only qualifies
// existing installations; owned profiles are synthesized by OwnedProfile.
var ErrOwnedDraftPrepare = errors.New("owned launch drafts cannot be prepared from an existing installation")

func Prepare(ctx context.Context, request PrepareRequest) (PreparedApplication, error) {
	return SystemBackend().Prepare(ctx, request)
}
func (b Backend) Prepare(ctx context.Context, request PrepareRequest) (PreparedApplication, error) {
	d := request.Draft
	if err := validateDraft(d, map[string]bool{}); err != nil {
		return PreparedApplication{}, err
	}
	if d.Binding != nil && d.Binding.Owned != nil {
		return PreparedApplication{}, ErrOwnedDraftPrepare
	}
	if d.Binding == nil || d.Binding.Unit == "" {
		unit, err := b.unitAtReference(ctx, d)
		if err != nil {
			return PreparedApplication{}, err
		}
		binding := DraftBinding{Unit: unit}
		if d.Binding != nil {
			binding = *d.Binding
			binding.Unit = unit
		}
		d.Binding = &binding
	}
	if !selectedUnitName.MatchString(d.Binding.Unit) {
		return PreparedApplication{}, errors.New("choose a recognized installed application first")
	}
	found, err := b.automaticConfiguration(ctx, d.App, d.Binding.Unit, d.Model)
	if err != nil {
		return PreparedApplication{}, err
	}
	if err := validatePreparedInstallation(d, found); err != nil {
		return PreparedApplication{}, err
	}
	found.ID = control.Workload(d.ID)
	found.Label = d.Label
	if err := (control.Catalog{Version: 1, Profiles: []control.WorkloadProfile{found}}).Validate(); err != nil {
		return PreparedApplication{}, err
	}
	return PreparedApplication{Profile: found}, nil
}

func validatePreparedInstallation(d Draft, found control.WorkloadProfile) error {
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
			return errors.New("installation settings changed or the override does not match its supported launch; refresh and retry")
		}
	}
	if d.App != "comfyui" && d.App != "ollama" && d.Model != "" && d.Model != model {
		return errors.New("selected model does not match the existing launch; choose its configured model")
	}
	return nil
}

func (b Backend) showAutomaticRaw(ctx context.Context, unit string) ([]byte, error) {
	out, err := b.runCommand(ctx, automaticSystemctlPath, "--user", "show", "--property=Id,LoadState,ExecStart,ExecStartPre,Environment,ControlGroup,ActiveState,SubState,FragmentPath,DropInPaths,NeedDaemonReload,Slice", "--no-pager", "--", unit)
	if err != nil {
		return nil, fmt.Errorf("could not inspect installation %s: %w", unit, err)
	}
	if len(out) > commandOutputLimit {
		return nil, errors.New("systemd installation metadata exceeds supported limit")
	}
	return out, nil
}

func (b Backend) showAutomatic(ctx context.Context, unit string) (map[string]string, error) {
	out, err := b.showAutomaticRaw(ctx, unit)
	if err != nil {
		return nil, err
	}
	return gpuruntime.ParseUnitProperties(out)
}

// Candidate classification precedes native-binding validation. Unrelated units
// may legitimately contain preparation metadata outside our supported subset.
func (b Backend) showApplicationCandidate(ctx context.Context, unit string) (map[string]string, error) {
	out, err := b.showAutomaticRaw(ctx, unit)
	if err != nil {
		return nil, err
	}
	evidence := unitProperties(string(out))
	// Keep any supported executable evidence when duplicate main-command
	// metadata is ambiguous; strict validation then reports this candidate.
	supported := ""
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "ExecStart=") && appFromUnit(line) != "" {
			supported = strings.TrimPrefix(line, "ExecStart=")
			break
		}
	}
	if supported == "" {
		return evidence, nil
	}
	evidence["ExecStart"] = supported
	values, err := gpuruntime.ParseUnitProperties(out)
	if err != nil {
		return evidence, errors.New("supported application has ambiguous or unsupported loaded metadata; inspect ExecStartPre and reload its direct launch before retrying")
	}
	return values, nil
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
	if values["NeedDaemonReload"] != "no" {
		return p, errors.New("installation has changed or uses unsupported overrides; reload its supported direct launch and retry")
	}
	if appFromUnit("ExecStart="+values["ExecStart"]) != app {
		return p, errors.New("installation launch does not match the selected application")
	}
	launch, err := b.inspectAutomaticMetadata(values, app)
	if err != nil {
		return p, fmt.Errorf("unsupported existing launch; choose a supported direct local configuration: %w", err)
	}
	if err := gpuruntime.CheckLoadedEnvironment(values["Environment"], launch.Environment); err != nil {
		return p, err
	}
	if err := gpuruntime.CheckNativeBindingSources(values, unit, values["FragmentPath"], launch.DropIns); err != nil {
		return p, errors.New("loaded drop-in configuration differs from inspected source files")
	}
	args := execArguments.FindAllStringSubmatch(values["ExecStart"], -1)
	if len(args) != 1 || strings.Join(strings.Fields(args[0][1]), " ") != strings.Join(strings.Fields(launch.Command), " ") {
		return p, errors.New("loaded launch configuration differs from its file; reload and retry")
	}
	if err := gpuruntime.CheckLoadedPreCommands(values["ExecStartPre"], launch.PreCommands); err != nil {
		return p, errors.New("loaded startup preparation differs from its file; reload and retry")
	}
	if err := b.resolveInstallationPlacement(ctx, unit, values, &p); err != nil {
		return p, err
	}
	if err := (ProbeRequest{App: app, Endpoint: launch.Endpoint}).validate(); err != nil {
		return p, err
	}
	err = bindInstallationLaunch(&p, values, launch, app, unit, model)
	return p, err
}

func (b Backend) inspectAutomaticMetadata(values map[string]string, app string) (gpuruntime.AutomaticLaunch, error) {
	if b.inspectAutomatic != nil {
		return b.inspectAutomatic(values["FragmentPath"], app)
	}
	return gpuruntime.InspectAutomaticLaunchSources(values["FragmentPath"], app, strings.Fields(values["DropInPaths"]))
}

func (b Backend) resolveInstallationPlacement(ctx context.Context, unit string, values map[string]string, p *control.WorkloadProfile) error {
	root, err := b.showAutomatic(ctx, "-.slice")
	if err != nil {
		return err
	}
	slice := map[string]string{}
	if values["ControlGroup"] == "" {
		slice, err = b.showAutomatic(ctx, "app.slice")
		if err != nil {
			return err
		}
	}
	p.Cgroup, err = gpuruntime.ResolveAutomaticCgroup(unit, values, slice, root)
	if err != nil {
		return err
	}
	if values["ControlGroup"] == "" {
		out, err := b.runCommand(ctx, automaticSystemctlPath, "--version")
		if err != nil {
			return err
		}
		version, err := gpuruntime.SupportedSystemdPlacementVersion(out)
		if err != nil {
			return err
		}
		p.SystemdSlice = "app.slice"
		p.SystemdVersion = version
	}
	return nil
}

func bindInstallationLaunch(p *control.WorkloadProfile, values map[string]string, launch gpuruntime.AutomaticLaunch, app, unit, model string) error {
	if app == "comfyui" {
		p.HealthURL = launch.Endpoint + "/system_stats"
		p.LaunchBinding = &control.LaunchBinding{GPUUUID: launch.GPUUUID, Runtime: app, Endpoint: launch.Endpoint, LaunchFile: values["FragmentPath"], LaunchSHA256: launch.SHA256, DropIns: launch.DropIns}
	} else {
		if app != "ollama" {
			model = launch.Model
		}
		if app == "ollama" && model == "" {
			return errors.New("choose an existing local Ollama model; setup will not start Ollama or download models")
		}
		p.HealthURL = launch.Endpoint + "/health"
		if app == "ollama" {
			p.HealthURL = launch.Endpoint + "/api/tags"
		}
		p.NativeModel = &control.NativeModel{GPUUUID: launch.GPUUUID, Runtime: app, Instance: "instance-" + candidate(ProbeRequest{App: app, Reference: unit, ReferenceKind: "configuration"}).ID, Model: model, Endpoint: launch.Endpoint, LaunchFile: values["FragmentPath"], LaunchSHA256: launch.SHA256, DropIns: launch.DropIns}
	}
	return nil
}

func (b Backend) unitAtReference(ctx context.Context, d Draft) (string, error) {
	if d.Endpoint == "" && (d.Reference == "" || (d.ReferenceKind != "configuration" && d.ReferenceKind != "application-directory" && d.ReferenceKind != "application")) {
		return "", errors.New("choose a recognized installation or its configuration location")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	out, err := b.runCommand(ctx, automaticSystemctlPath, "--user", "list-unit-files", "--type=service", "--no-legend", "--no-pager")
	if cancelErr := referenceCancellationError(ctx, err); cancelErr != nil {
		return "", cancelErr
	}
	if err != nil || len(out) > commandOutputLimit {
		return "", errors.New("application services could not be listed; check your desktop session and retry")
	}
	units := serviceUnits(out)
	if len(units) > 256 {
		return "", errors.New("too many services for bounded discovery; select a recognized installation explicitly")
	}
	selection, err := b.scanReferenceUnits(ctx, d, units)
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if !selection.inspected && selection.firstInspectionError != nil {
		return "", fmt.Errorf("application services could not be inspected; check your desktop session and retry: %w", selection.firstInspectionError)
	}
	if selection.selected == "" {
		return "", errors.New("no supported loaded installation uses this location; install a supported direct user service or choose its configuration")
	}
	return selection.selected, nil
}

type referenceSelection struct {
	selected             string
	inspected            bool
	firstInspectionError error
}

func (b Backend) scanReferenceUnits(ctx context.Context, d Draft, units []string) (referenceSelection, error) {
	selection := referenceSelection{}
	for _, unit := range units {
		if err := b.inspectReferenceUnit(ctx, d, unit, &selection); err != nil {
			return selection, err
		}
	}
	return selection, nil
}

func (b Backend) inspectReferenceUnit(ctx context.Context, d Draft, unit string, selection *referenceSelection) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	values, err := b.showApplicationCandidate(ctx, unit)
	if cancelErr := referenceCancellationError(ctx, err); cancelErr != nil {
		return cancelErr
	}
	if err != nil {
		if values != nil {
			selection.inspected = true
		}
		if appFromUnit("ExecStart="+values["ExecStart"]) == d.App {
			matches, matchErr := b.referenceMatches(ctx, d, values)
			if matchErr != nil {
				return matchErr
			}
			if matches {
				return err
			}
		}
		if selection.firstInspectionError == nil {
			selection.firstInspectionError = err
		}
		return nil
	}
	selection.inspected = true
	if appFromUnit("ExecStart="+values["ExecStart"]) != d.App {
		return nil
	}
	match, err := b.referenceMatches(ctx, d, values)
	if err != nil {
		return err
	}
	if match {
		if selection.selected != "" {
			return errors.New("multiple installations use this location; choose the installation explicitly")
		}
		selection.selected = unit
	}
	return nil
}

func (b Backend) referenceMatches(ctx context.Context, d Draft, values map[string]string) (bool, error) {
	match := values["FragmentPath"] == d.Reference
	if d.Endpoint != "" {
		launch, err := b.inspectAutomaticMetadata(values, d.App)
		if cancelErr := referenceCancellationError(ctx, err); cancelErr != nil {
			return false, cancelErr
		}
		if err != nil {
			return false, nil
		}
		match = strings.TrimSuffix(d.Endpoint, "/") == launch.Endpoint
	}
	if d.ReferenceKind == "application-directory" {
		match = directoryReferenceMatches(values, d.Reference)
	}
	return match, nil
}

func directoryReferenceMatches(values map[string]string, reference string) bool {
	match := filepath.Dir(values["FragmentPath"]) == reference
	args := execArguments.FindStringSubmatch(values["ExecStart"])
	if len(args) == 2 {
		for _, arg := range strings.Fields(args[1]) {
			if filepath.IsAbs(arg) && filepath.Dir(arg) == reference {
				match = true
			}
		}
	}
	return match
}

// referenceCancellationError distinguishes a stopped scan from an unavailable
// candidate so cancellation cannot silently become a fallback selection.
func referenceCancellationError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return nil
}

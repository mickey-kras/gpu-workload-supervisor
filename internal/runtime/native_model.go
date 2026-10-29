package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/strictjson"
	"io"
	"net/http"
	"strings"
)

var ErrModelIdentity = errors.New("native model identity mismatch")
var ErrLaunchChanged = errors.New("native launch binding changed")

// InspectQualifiedNativeLaunch checks the supported launch subset before
// returning the fingerprint recorded by onboarding. It does not execute it.
func InspectQualifiedNativeLaunch(path string, binding control.NativeModel) (string, error) {
	return inspectQualifiedNativeLaunch(path, binding, validateNativeExecutable)
}
func inspectQualifiedNativeLaunch(path string, binding control.NativeModel, validate func(string) error) (string, error) {
	if binding.Owned != nil && len(binding.DropIns) > 0 {
		return "", ErrLaunchUnsupported
	}
	sources, err := readLaunchSources(path, binding.DropIns)
	if err != nil {
		return "", err
	}
	var unit parsedLaunchUnit
	if binding.Owned != nil {
		unit, err = parseOwnedLaunchUnit(sources[0], binding)
	} else {
		unit, err = parseExternalLaunchSources(sources)
	}
	if err != nil {
		return "", err
	}
	if err := qualifyParsedLaunch(unit, binding, validate); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(sources[0])), nil
}

func readVerifiedNativeLaunch(n control.NativeModel, validate func(string) error) (parsedLaunchUnit, error) {
	var unit parsedLaunchUnit
	if n.Owned != nil && len(n.DropIns) > 0 {
		return unit, ErrLaunchUnsupported
	}
	sources, err := readLaunchSources(n.LaunchFile, n.DropIns)
	if err != nil {
		return unit, err
	}
	if fmt.Sprintf("%x", sha256.Sum256(sources[0])) != n.LaunchSHA256 {
		return unit, ErrLaunchChanged
	}
	if n.Owned != nil {
		unit, err = parseOwnedLaunchUnit(sources[0], n)
	} else {
		unit, err = parseExternalLaunchSources(sources)
	}
	if err != nil {
		return unit, err
	}
	return unit, qualifyParsedLaunch(unit, n, validate)
}

// verifyOwnedSpecWithValidator is the spec↔fingerprint chain for owned
// launches: the deterministic re-render must exist, match the recorded digest,
// and qualify under the shared grammar before the on-disk binding is checked.
func verifyOwnedSpecWithValidator(p control.WorkloadProfile, validate func(string) error) error {
	if p.NativeModel == nil || p.NativeModel.Owned == nil {
		return nil
	}
	raw, err := RenderOwnedUnit(p)
	if err != nil {
		return err
	}
	if fmt.Sprintf("%x", sha256.Sum256(raw)) != p.NativeModel.LaunchSHA256 {
		return fmt.Errorf("%w: rendered digest differs from the recorded launch fingerprint", ErrOwnedRender)
	}
	if err := qualifyNativeLaunchWithValidator(raw, *p.NativeModel, validate); err != nil {
		return fmt.Errorf("%w: %v", ErrOwnedRender, err)
	}
	return nil
}

func (m *SystemdManager) verifyNativeBinding(ctx context.Context, p control.WorkloadProfile) error {
	if p.Trainer != nil {
		return m.verifyTrainerBinding(ctx, p)
	}
	if err := m.verifyProfileGPU(ctx, p); err != nil {
		return err
	}
	if err := m.verifyAutomaticPlacement(ctx, p); err != nil {
		return err
	}
	if p.NativeModel == nil && p.LaunchBinding == nil {
		return nil
	}
	validate := m.nativeExecutableValidator
	if validate == nil {
		validate = validateNativeExecutable
	}
	launch, launchFile, dropIns, err := m.readProfileLaunch(p, validate)
	if err != nil {
		return err
	}
	args := nativeBindingShowArguments(p.Unit, launch)
	b, err := m.runner.Run(ctx, m.config.SystemctlPath, args...)
	if err != nil {
		return ErrLaunchChanged
	}
	values, err := ParseUnitProperties(b)
	if err != nil {
		return err
	}
	if err := CheckLoadedLaunchProperties(values, launch.loadedProperties); err != nil {
		return err
	}
	if err := CheckLoadedEnvironment(values["Environment"], launch.environmentValues); err != nil {
		return err
	}
	if values["NeedDaemonReload"] != "no" {
		return ErrLaunchChanged
	}
	if err := CheckLoadedLaunchCommand(values["ExecStart"], launch.execStart); err != nil {
		return err
	}
	if err := CheckLoadedPreCommands(values["ExecStartPre"], launch.preCommands); err != nil {
		return err
	}
	return CheckNativeBindingSources(values, p.Unit, launchFile, dropIns)
}

func nativeBindingShowArguments(unit string, launch parsedLaunchUnit) []string {
	args := []string{"--user", "show", "--property=FragmentPath", "--property=DropInPaths", "--property=NeedDaemonReload", "--property=ExecStartPre", "--property=ExecStart"}
	for _, key := range []string{"WorkingDirectory", "Wants", "KillSignal", "NoNewPrivileges", "PrivateTmp", "ProtectHome", "ProtectSystem"} {
		if _, configured := launch.loadedProperties[key]; configured {
			args = append(args, "--property="+key)
		}
	}
	if len(launch.environmentValues) > 0 {
		args = append(args, "--property=Environment")
	}
	args = append(args, "--", unit)
	return args
}

func (m *SystemdManager) readProfileLaunch(p control.WorkloadProfile, validate func(string) error) (parsedLaunchUnit, string, []control.LaunchSource, error) {
	if p.NativeModel != nil {
		launch, err := readVerifiedNativeLaunch(*p.NativeModel, validate)
		return launch, p.NativeModel.LaunchFile, p.NativeModel.DropIns, err
	}
	binding := p.LaunchBinding
	launch, err := m.readApplicationLaunch(*binding, validate)
	return launch, binding.LaunchFile, binding.DropIns, err
}

func (m *SystemdManager) readApplicationLaunch(binding control.LaunchBinding, validate func(string) error) (parsedLaunchUnit, error) {
	sources, err := readLaunchSources(binding.LaunchFile, binding.DropIns)
	if err != nil {
		return parsedLaunchUnit{}, err
	}
	if m.nativeExecutableValidator == nil {
		validate = validateComfyExecutable
	}
	unit, err := parseExternalLaunchSources(sources)
	if err != nil {
		return unit, err
	}
	found, err := inspectParsedAutomaticLaunch(unit, sources[0], binding.Runtime, validate)
	if err != nil {
		return unit, err
	}
	if found.SHA256 != binding.LaunchSHA256 || found.Endpoint != binding.Endpoint || unit.gpuUUID != binding.GPUUUID {
		return unit, ErrLaunchChanged
	}
	return unit, nil
}

// ParseUnitProperties parses systemctl show output into key/value pairs,
// ExecStartPre arrays are printed as one repeated property per command by
// systemctl. Preserve their order; scalar duplicates remain ambiguous.
func ParseUnitProperties(out []byte) (map[string]string, error) {
	values := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			if err := addUnitProperty(values, k, v); err != nil {
				return nil, err
			}
		}
	}
	return values, nil
}

func addUnitProperty(values map[string]string, k, v string) error {
	if previous, exists := values[k]; exists {
		if k != "ExecStartPre" || strings.TrimSpace(previous) == "" || strings.TrimSpace(v) == "" {
			return ErrLaunchChanged
		}
		v = previous + " " + v
	}
	if k == "ExecStartPre" && strings.TrimSpace(v) != "" {
		commands := loadedPreCommand.FindAllStringSubmatch(v, -1)
		if len(commands) == 0 || len(commands) > 32 || strings.Trim(loadedPreCommand.ReplaceAllString(v, ""), " ;\t\r\n") != "" {
			return ErrLaunchChanged
		}
	}
	values[k] = v
	return nil
}

// CheckNativeBinding rejects a loaded unit whose binding does not match the
// catalog's launch file: exact fragment path and no drop-ins. Setup's
// post-reload verification and the runtime preflight share this rule.
func CheckNativeBinding(values map[string]string, unit, launchFile string) error {
	if values["FragmentPath"] != launchFile || values["DropInPaths"] != "" {
		return fmt.Errorf("%w: %s", ErrLaunchChanged, unit)
	}
	return nil
}
func (m *SystemdManager) nativeReady(ctx context.Context, n control.NativeModel) error {
	if err := m.getHealthy(ctx, n.Endpoint+"/health"); err != nil {
		return err
	}
	b, err := m.fetchModelList(ctx, n)
	if err != nil {
		return err
	}
	return verifyModelList(b, n)
}

func (m *SystemdManager) fetchModelList(ctx context.Context, n control.NativeModel) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, n.Endpoint+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, &safeHTTPRequestError{cause: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// 5xx is a transient daemon condition retried by the poll loop; 4xx
		// contradicts the bound model identity and is permanent.
		if resp.StatusCode >= http.StatusInternalServerError {
			return nil, fmt.Errorf("model list status %d", resp.StatusCode)
		}
		return nil, ErrModelIdentity
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 65537))
	if err != nil || len(b) > 65536 {
		return nil, ErrModelIdentity
	}
	return b, nil
}

type modelListPayload struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

func verifyModelList(b []byte, n control.NativeModel) error {
	payload, err := decodeModelList(b)
	if err != nil {
		return ErrModelIdentity
	}
	return verifyServerModelList(payload, n.Model)
}

func decodeModelList(b []byte) (modelListPayload, error) {
	var payload modelListPayload
	if strictjson.Check(json.NewDecoder(bytes.NewReader(b))) != nil || json.Unmarshal(b, &payload) != nil {
		return payload, ErrModelIdentity
	}
	fields, err := modelObjectFields(b, "data")
	if err != nil || checkModelEntryFields(fields["data"], "id") != nil {
		return payload, ErrModelIdentity
	}
	return payload, nil
}

func modelObjectFields(raw []byte, names ...string) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return nil, ErrModelIdentity
	}
	for key := range fields {
		for _, name := range names {
			if key != name && strings.EqualFold(key, name) {
				return nil, ErrModelIdentity
			}
		}
	}
	return fields, nil
}

func checkModelEntryFields(raw []byte, names ...string) error {
	if len(raw) == 0 {
		return nil
	}
	var entries []json.RawMessage
	if json.Unmarshal(raw, &entries) != nil {
		return ErrModelIdentity
	}
	for _, entry := range entries {
		if _, err := modelObjectFields(entry, names...); err != nil {
			return err
		}
	}
	return nil
}

func verifyServerModelList(payload modelListPayload, model string) error {
	if len(payload.Data) != 1 || payload.Data[0].ID != model {
		return ErrModelIdentity
	}
	return nil
}

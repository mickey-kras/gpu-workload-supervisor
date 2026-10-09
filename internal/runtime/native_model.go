package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"io"
	"net/http"
	"strings"
	"time"
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
		unit, err = parseLaunchUnit(sources[0], binding.Runtime)
	} else {
		unit, err = parseExternalLaunchSources(sources, binding.Runtime)
	}
	if err != nil {
		return "", err
	}
	if err := qualifyParsedLaunch(unit, binding, validate); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(sources[0])), nil
}

func verifyNativeLaunchWithValidator(n control.NativeModel, validate func(string) error) error {
	_, err := readVerifiedNativeLaunch(n, validate)
	return err
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
		unit, err = parseLaunchUnit(sources[0], n.Runtime)
	} else {
		unit, err = parseExternalLaunchSources(sources, n.Runtime)
	}
	if err != nil {
		return unit, err
	}
	return unit, qualifyParsedLaunch(unit, n, validate)
}

// verifyOwnedSpec is the spec↔fingerprint chain for owned launches: the
// deterministic re-render must exist, match the recorded digest, and qualify
// under the shared grammar before the on-disk binding is checked.
func verifyOwnedSpec(p control.WorkloadProfile) error {
	return verifyOwnedSpecWithValidator(p, validateNativeExecutable)
}

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
	args := []string{"--user", "show", "--property=FragmentPath", "--property=DropInPaths", "--property=NeedDaemonReload", "--property=ExecStartPre", "--property=ExecStart"}
	for name := range launch.environmentValues {
		if !strings.HasPrefix(name, "OLLAMA_") {
			args = append(args, "--property=Environment")
			break
		}
	}
	args = append(args, "--", p.Unit)
	b, err := m.runner.Run(ctx, m.config.SystemctlPath, args...)
	if err != nil {
		return ErrLaunchChanged
	}
	values, err := ParseUnitProperties(b)
	if err != nil {
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
	unit, err := parseExternalLaunchSources(sources, binding.Runtime)
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
	if n.Runtime != "ollama" {
		if err := m.getHealthy(ctx, n.Endpoint+"/health"); err != nil {
			return err
		}
	}
	b, err := m.fetchModelList(ctx, n)
	if err != nil {
		return err
	}
	return verifyModelList(b, n)
}

func (m *SystemdManager) fetchModelList(ctx context.Context, n control.NativeModel) ([]byte, error) {
	path := "/v1/models"
	if n.Runtime == "ollama" {
		path = "/api/ps"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, n.Endpoint+path, nil)
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
		ID     string `json:"id"`
		Status *struct {
			Value string `json:"value"`
		} `json:"status"`
	} `json:"data"`
	Models []struct {
		Name  string `json:"name"`
		Model string `json:"model"`
	} `json:"models"`
}

func verifyModelList(b []byte, n control.NativeModel) error {
	var payload modelListPayload
	if json.Unmarshal(b, &payload) != nil {
		return ErrModelIdentity
	}
	if n.Runtime == "ollama" {
		return verifyOllamaModelList(payload, n.ComparisonModel())
	}
	return verifyServerModelList(payload, n.Model)
}

func verifyOllamaModelList(payload modelListPayload, model string) error {
	if len(payload.Models) != 1 || (payload.Models[0].Name != model && payload.Models[0].Model != model) || (payload.Models[0].Name != "" && payload.Models[0].Name != model) || (payload.Models[0].Model != "" && payload.Models[0].Model != model) {
		return ErrModelIdentity
	}
	return nil
}

func verifyServerModelList(payload modelListPayload, model string) error {
	if len(payload.Data) != 1 || payload.Data[0].ID != model || (payload.Data[0].Status != nil && payload.Data[0].Status.Value != "loaded") {
		return ErrModelIdentity
	}
	return nil
}
func (m *SystemdManager) startNative(ctx context.Context, p control.WorkloadProfile) error {
	if p.NativeModel.Runtime != "ollama" {
		return nil
	}
	for {
		if err := m.getHealthy(ctx, p.HealthURL); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	if err := m.requireLocalOllama(ctx, *p.NativeModel); err != nil {
		return err
	}
	b, _ := json.Marshal(struct {
		Model     string `json:"model"`
		Stream    bool   `json:"stream"`
		KeepAlive int    `json:"keep_alive"`
	}{Model: p.NativeModel.Model, KeepAlive: -1})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.NativeModel.Endpoint+"/api/generate", bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return m.checkHTTPResponse(req, "preload selected model")
}

func (m *SystemdManager) requireLocalOllama(ctx context.Context, n control.NativeModel) error {
	b, _ := json.Marshal(struct {
		Model string `json:"model"`
	}{n.Model})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.Endpoint+"/api/show", bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.client.Do(req)
	if err != nil {
		return &safeHTTPRequestError{cause: err}
	}
	defer resp.Body.Close()
	b, err = io.ReadAll(io.LimitReader(resp.Body, 65537))
	if err != nil || len(b) > 65536 || resp.StatusCode != 200 {
		return ErrModelIdentity
	}
	var info struct {
		RemoteHost  string                     `json:"remote_host"`
		RemoteModel string                     `json:"remote_model"`
		ModelInfo   map[string]json.RawMessage `json:"model_info"`
	}
	if json.Unmarshal(b, &info) != nil || info.RemoteHost != "" || info.RemoteModel != "" || len(info.ModelInfo) == 0 {
		return ErrModelIdentity
	}
	return nil
}

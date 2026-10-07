package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"golang.org/x/sys/unix"
	"io"
	"net/http"
	"os"
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
	b, err := readNativeLaunch(path)
	if err != nil {
		return "", err
	}
	if err := qualifyNativeLaunchWithValidator(b, binding, validate); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(b)), nil
}
func readNativeLaunch(path string) ([]byte, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, ErrLaunchChanged
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return nil, ErrLaunchChanged
	}
	b, err := io.ReadAll(io.LimitReader(f, 1<<20+1))
	if err != nil || len(b) > 1<<20 {
		return nil, ErrLaunchChanged
	}
	return b, nil
}
func verifyNativeLaunchWithValidator(n control.NativeModel, validate func(string) error) error {
	b, err := readNativeLaunch(n.LaunchFile)
	if err != nil {
		return err
	}
	if fmt.Sprintf("%x", sha256.Sum256(b)) != n.LaunchSHA256 {
		return ErrLaunchChanged
	}
	return qualifyNativeLaunchWithValidator(b, n, validate)
}

// verifyOwnedSpec is the spec↔fingerprint chain for owned launches: the
// deterministic re-render must exist, match the recorded digest, and qualify
// under the shared grammar before the on-disk binding is checked.
func verifyOwnedSpec(p control.WorkloadProfile) error {
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
	return renderMustQualify(raw, *p.NativeModel)
}

func (m *SystemdManager) verifyNativeBinding(ctx context.Context, p control.WorkloadProfile) error {
	if p.NativeModel == nil {
		return nil
	}
	validate := m.nativeExecutableValidator
	if validate == nil {
		validate = validateNativeExecutable
	}
	if err := verifyNativeLaunchWithValidator(*p.NativeModel, validate); err != nil {
		return err
	}
	b, err := m.runner.Run(ctx, m.config.SystemctlPath, "--user", "show", "--property=FragmentPath", "--property=DropInPaths", "--property=NeedDaemonReload", "--", p.Unit)
	if err != nil {
		return ErrLaunchChanged
	}
	values := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			if _, exists := values[k]; exists {
				return ErrLaunchChanged
			}
			values[k] = v
		}
	}
	if values["FragmentPath"] != p.NativeModel.LaunchFile || values["DropInPaths"] != "" || values["NeedDaemonReload"] != "no" {
		return ErrLaunchChanged
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

package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

type CommandRunner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

type SystemdConfig struct {
	TextUnit       string
	MediaUnit      string
	TextHealthURL  string
	MediaHealthURL string
	HealthTimeout  time.Duration
}

type SystemdManager struct {
	config SystemdConfig
	runner CommandRunner
	client *http.Client
}

func NewSystemdManager(config SystemdConfig) (*SystemdManager, error) {
	return newSystemdManager(config, ExecRunner{}, &http.Client{Timeout: config.HealthTimeout})
}

func newSystemdManager(config SystemdConfig, runner CommandRunner, client *http.Client) (*SystemdManager, error) {
	if config.TextUnit == "" || config.MediaUnit == "" {
		return nil, errors.New("text and media units are required")
	}
	if config.TextUnit == config.MediaUnit {
		return nil, errors.New("text and media units must differ")
	}
	if config.HealthTimeout <= 0 {
		return nil, errors.New("health timeout must be greater than zero")
	}
	if runner == nil || client == nil {
		return nil, errors.New("runner and HTTP client are required")
	}
	return &SystemdManager{config: config, runner: runner, client: client}, nil
}

func (m *SystemdManager) Observe(ctx context.Context) (Snapshot, error) {
	text, err := m.active(ctx, m.config.TextUnit)
	if err != nil {
		return Snapshot{}, err
	}
	media, err := m.active(ctx, m.config.MediaUnit)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{TextActive: text, MediaActive: media}, nil
}

func (m *SystemdManager) Start(ctx context.Context, workload control.Workload) error {
	unit, err := m.unit(workload)
	if err != nil {
		return err
	}
	return m.runSystemctl(ctx, "start", unit)
}

func (m *SystemdManager) Stop(ctx context.Context, workload control.Workload) error {
	unit, err := m.unit(workload)
	if err != nil {
		return err
	}
	return m.runSystemctl(ctx, "stop", unit)
}

func (m *SystemdManager) Healthy(ctx context.Context, workload control.Workload) error {
	if workload == control.WorkloadIdle {
		return nil
	}
	url, err := m.healthURL(workload)
	if err != nil {
		return err
	}
	if url == "" {
		return errors.New("health URL is required")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("create health request: %w", err)
	}
	response, err := m.client.Do(request)
	if err != nil {
		return fmt.Errorf("health request: %w", err)
	}
	defer response.Body.Close()
	if _, err := io.Copy(io.Discard, io.LimitReader(response.Body, 4096)); err != nil {
		return fmt.Errorf("read health response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("health status %d", response.StatusCode)
	}
	return nil
}

func (m *SystemdManager) active(ctx context.Context, unit string) (bool, error) {
	output, err := m.runner.Run(ctx, "systemctl", "--user", "show", unit,
		"--property=LoadState", "--property=ActiveState")
	if err != nil {
		return false, fmt.Errorf("inspect %s: %w: %s", unit, err, strings.TrimSpace(string(output)))
	}
	values := map[string]string{}
	for _, line := range strings.Split(string(output), "\\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			values[key] = value
		}
	}
	if values["LoadState"] != "loaded" {
		return false, fmt.Errorf("%s load state is %q", unit, values["LoadState"])
	}
	switch values["ActiveState"] {
	case "active", "activating":
		return true, nil
	case "inactive", "failed", "deactivating":
		return false, nil
	default:
		return false, fmt.Errorf("%s active state is %q", unit, values["ActiveState"])
	}
}

func (m *SystemdManager) runSystemctl(ctx context.Context, action, unit string) error {
	output, err := m.runner.Run(ctx, "systemctl", "--user", action, unit)
	if err != nil {
		return fmt.Errorf("systemctl %s %s: %w: %s", action, unit, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (m *SystemdManager) unit(workload control.Workload) (string, error) {
	switch workload {
	case control.WorkloadText:
		return m.config.TextUnit, nil
	case control.WorkloadMedia:
		return m.config.MediaUnit, nil
	default:
		return "", fmt.Errorf("workload %q has no systemd unit", workload)
	}
}

func (m *SystemdManager) healthURL(workload control.Workload) (string, error) {
	switch workload {
	case control.WorkloadText:
		return m.config.TextHealthURL, nil
	case control.WorkloadMedia:
		return m.config.MediaHealthURL, nil
	default:
		return "", fmt.Errorf("workload %q has no health URL", workload)
	}
}

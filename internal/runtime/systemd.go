package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
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

var systemdUnitPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:_.@-]*\.service$`)

type SystemdConfig struct {
	TextUnit        string
	MediaUnit       string
	TextHealthURL   string
	MediaHealthURL  string
	MediaReleaseURL string
	HealthTimeout   time.Duration
	GPUIndex        int
	ReleaseMaxMiB   uint64
	NvidiaSMIPath   string
	SystemctlPath   string
}

type SystemdManager struct {
	config SystemdConfig
	runner CommandRunner
	client *http.Client
}

func NewSystemdManager(config SystemdConfig) (*SystemdManager, error) {
	client := &http.Client{
		Timeout: config.HealthTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return newSystemdManager(config, ExecRunner{}, client)
}

func newSystemdManager(config SystemdConfig, runner CommandRunner, client *http.Client) (*SystemdManager, error) {
	if config.TextUnit == "" || config.MediaUnit == "" {
		return nil, errors.New("text and media units are required")
	}
	if config.TextUnit == config.MediaUnit {
		return nil, errors.New("text and media units must differ")
	}
	if !systemdUnitPattern.MatchString(config.TextUnit) || !systemdUnitPattern.MatchString(config.MediaUnit) {
		return nil, errors.New("invalid systemd unit name")
	}
	if config.GPUIndex < 0 || config.ReleaseMaxMiB == 0 {
		return nil, errors.New("GPU index and release memory threshold are required")
	}
	resolvedNvidiaSMI, err := validateExecutable(config.NvidiaSMIPath)
	if err != nil {
		return nil, fmt.Errorf("nvidia-smi: %w", err)
	}
	resolvedSystemctl, err := validateExecutable(config.SystemctlPath)
	if err != nil {
		return nil, fmt.Errorf("systemctl: %w", err)
	}
	config.NvidiaSMIPath = resolvedNvidiaSMI
	config.SystemctlPath = resolvedSystemctl
	if config.HealthTimeout <= 0 {
		return nil, errors.New("health timeout must be greater than zero")
	}
	for name, value := range map[string]string{
		"text health":   config.TextHealthURL,
		"media health":  config.MediaHealthURL,
		"media release": config.MediaReleaseURL,
	} {
		if err := validateLoopbackURL(value); err != nil {
			return nil, fmt.Errorf("%s URL: %w", name, err)
		}
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
	return Snapshot{TextActive: text, MediaReady: media}, nil
}

func (m *SystemdManager) Start(ctx context.Context, workload control.Workload) error {
	switch workload {
	case control.WorkloadText:
		return m.runSystemctl(ctx, "start", m.config.TextUnit)
	case control.WorkloadMedia:
		return m.runSystemctl(ctx, "start", m.config.MediaUnit)
	default:
		return fmt.Errorf("workload %q cannot be started", workload)
	}
}

func (m *SystemdManager) Stop(ctx context.Context, workload control.Workload) error {
	switch workload {
	case control.WorkloadText:
		return m.runSystemctl(ctx, "stop", m.config.TextUnit)
	case control.WorkloadMedia:
		return m.releaseMedia(ctx)
	default:
		return fmt.Errorf("workload %q cannot be stopped", workload)
	}
}

func (m *SystemdManager) Healthy(ctx context.Context, workload control.Workload) error {
	switch workload {
	case control.WorkloadIdle:
		return nil
	case control.WorkloadText:
		return m.getHealthy(ctx, m.config.TextHealthURL)
	case control.WorkloadMedia:
		return m.getHealthy(ctx, m.config.MediaHealthURL)
	default:
		return fmt.Errorf("workload %q has no health check", workload)
	}
}

func (m *SystemdManager) Released(ctx context.Context) error {
	output, err := m.runner.Run(ctx, m.config.NvidiaSMIPath, "--query-gpu=memory.used", "--format=csv,noheader,nounits", "-i", strconv.Itoa(m.config.GPUIndex))
	if err != nil {
		return fmt.Errorf("query GPU memory: %w: %s", err, strings.TrimSpace(string(output)))
	}
	used, err := strconv.ParseUint(strings.TrimSpace(string(output)), 10, 64)
	if err != nil {
		return fmt.Errorf("parse GPU memory: %w", err)
	}
	if used > m.config.ReleaseMaxMiB {
		return fmt.Errorf("GPU memory remains above release threshold: %d MiB", used)
	}
	return nil
}

func (m *SystemdManager) active(ctx context.Context, unit string) (bool, error) {
	output, err := m.runner.Run(ctx, m.config.SystemctlPath, "--user", "show",
		"--property=LoadState", "--property=ActiveState", "--property=SubState", "--", unit)
	if err != nil {
		return false, fmt.Errorf("inspect %s: %w: %s", unit, err, strings.TrimSpace(string(output)))
	}
	values := map[string]string{}
	for _, line := range strings.Split(string(output), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			values[key] = value
		}
	}
	if values["LoadState"] != "loaded" {
		return false, fmt.Errorf("%s load state is %q", unit, values["LoadState"])
	}
	switch values["ActiveState"] {
	case "active":
		if values["SubState"] != "running" && values["SubState"] != "exited" {
			return false, fmt.Errorf("%s substate is %q", unit, values["SubState"])
		}
		return true, nil
	case "inactive", "failed", "deactivating", "activating":
		return false, nil
	default:
		return false, fmt.Errorf("%s active state is %q", unit, values["ActiveState"])
	}
}

func (m *SystemdManager) runSystemctl(ctx context.Context, action, unit string) error {
	output, err := m.runner.Run(ctx, m.config.SystemctlPath, "--user", action, "--", unit)
	if err != nil {
		return fmt.Errorf("systemctl %s %s: %w: %s", action, unit, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (m *SystemdManager) getHealthy(ctx context.Context, endpoint string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
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

func (m *SystemdManager) releaseMedia(ctx context.Context) error {
	body := bytes.NewBufferString(`{"unload_models":true,"free_memory":true}`)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, m.config.MediaReleaseURL, body)
	if err != nil {
		return fmt.Errorf("create media release request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := m.client.Do(request)
	if err != nil {
		return fmt.Errorf("media release request: %w", err)
	}
	defer response.Body.Close()
	if _, err := io.Copy(io.Discard, io.LimitReader(response.Body, 4096)); err != nil {
		return fmt.Errorf("read media release response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("media release status %d", response.StatusCode)
	}
	return nil
}

func validateLoopbackURL(value string) error {
	parsed, err := url.Parse(value)
	if err != nil {
		return err
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return errors.New("scheme must be http or https")
	}
	host := parsed.Hostname()
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return errors.New("host must be loopback")
	}
	return nil
}

func validateExecutable(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("path must be absolute")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", errors.New("target must be a regular executable")
	}
	if info.Mode().Perm()&0o022 != 0 {
		return "", errors.New("target must not be group or world writable")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 {
		return "", errors.New("target must be owned by root")
	}
	for directory := filepath.Dir(resolved); ; directory = filepath.Dir(directory) {
		info, err := os.Stat(directory)
		if err != nil {
			return "", err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != 0 || info.Mode().Perm()&0o022 != 0 {
			return "", errors.New("executable path must be rooted in trusted directories")
		}
		if directory == string(filepath.Separator) {
			break
		}
	}
	return resolved, nil
}

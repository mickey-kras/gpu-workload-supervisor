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

type MediaStopMode string

const (
	MediaStopUnload  MediaStopMode = "unload"
	MediaStopService MediaStopMode = "stop-service"
)

func (mode MediaStopMode) Validate() error {
	if mode != "" && mode != MediaStopUnload && mode != MediaStopService {
		return fmt.Errorf("invalid media stop mode %q", mode)
	}
	return nil
}

type SystemdConfig struct {
	MediaStopMode   MediaStopMode
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
	if err := config.MediaStopMode.Validate(); err != nil {
		return nil, err
	}
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
	if err := config.validateEndpoints(); err != nil {
		return nil, err
	}
	if runner == nil || client == nil {
		return nil, errors.New("runner and HTTP client are required")
	}
	return &SystemdManager{config: config, runner: runner, client: client}, nil
}

func (config SystemdConfig) validateEndpoints() error {
	if config.HealthTimeout <= 0 {
		return errors.New("health timeout must be greater than zero")
	}
	endpoints := map[string]string{"text health": config.TextHealthURL, "media health": config.MediaHealthURL}
	if config.MediaStopMode != MediaStopService || config.MediaReleaseURL != "" {
		endpoints["media release"] = config.MediaReleaseURL
	}
	for name, value := range endpoints {
		if err := validateLoopbackURL(value); err != nil {
			return fmt.Errorf("%s URL: %w", name, err)
		}
	}
	return nil
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
	if m.config.MediaStopMode == MediaStopService && text && media {
		return Snapshot{}, errors.New("text and media units are both active")
	}
	return Snapshot{TextActive: text, MediaReady: media, MediaExclusive: m.config.MediaStopMode == MediaStopService}, nil
}

func (m *SystemdManager) Start(ctx context.Context, workload control.Workload) error {
	switch workload {
	case control.WorkloadText:
		return m.startUnit(ctx, m.config.TextUnit, m.config.MediaUnit)
	case control.WorkloadMedia:
		return m.startUnit(ctx, m.config.MediaUnit, m.config.TextUnit)
	default:
		return fmt.Errorf("workload %q cannot be started", workload)
	}
}

func (m *SystemdManager) Stop(ctx context.Context, workload control.Workload) error {
	switch workload {
	case control.WorkloadText:
		return m.stopUnit(ctx, m.config.TextUnit)
	case control.WorkloadMedia:
		if m.config.MediaStopMode == MediaStopService {
			return m.stopUnit(ctx, m.config.MediaUnit)
		}
		state, err := m.unitState(ctx, m.config.MediaUnit)
		if err != nil {
			return err
		}
		if _, err := state.isActive(m.config.MediaUnit); err != nil {
			return err
		}
		if state.active == "inactive" {
			return nil
		}
		return m.releaseMedia(ctx)
	default:
		return fmt.Errorf("workload %q cannot be stopped", workload)
	}
}

// StopForRecovery shuts down both units regardless of the media stop policy.
func (m *SystemdManager) StopForRecovery(ctx context.Context) error {
	if err := m.stopUnit(ctx, m.config.TextUnit); err != nil {
		return err
	}
	return m.stopUnit(ctx, m.config.MediaUnit)
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
	if m.config.MediaStopMode == MediaStopService {
		for _, unit := range []string{m.config.TextUnit, m.config.MediaUnit} {
			if err := m.requireStopped(ctx, unit); err != nil {
				return err
			}
		}
	}
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
	state, err := m.unitState(ctx, unit)
	if err != nil {
		return false, err
	}
	if m.config.MediaStopMode == MediaStopService && !(state.active == "active" && state.sub == "running") && !(state.active == "inactive" && state.sub == "dead") {
		return false, fmt.Errorf("%s is not running or stopped: %s/%s", unit, state.active, state.sub)
	}
	return state.isActive(unit)
}

func (state systemdUnitState) isActive(unit string) (bool, error) {
	switch state.active {
	case "active":
		if state.sub != "running" && state.sub != "exited" {
			return false, fmt.Errorf("%s substate is %q", unit, state.sub)
		}
		return true, nil
	case "inactive", "failed", "deactivating", "activating":
		return false, nil
	default:
		return false, fmt.Errorf("%s active state is %q", unit, state.active)
	}
}

type systemdUnitState struct {
	active string
	sub    string
}

func (m *SystemdManager) unitState(ctx context.Context, unit string) (systemdUnitState, error) {
	output, err := m.runner.Run(ctx, m.config.SystemctlPath, "--user", "show",
		"--property=LoadState", "--property=ActiveState", "--property=SubState", "--", unit)
	if err != nil {
		return systemdUnitState{}, fmt.Errorf("inspect %s: %w: %s", unit, err, strings.TrimSpace(string(output)))
	}
	values := map[string]string{}
	for _, line := range strings.Split(string(output), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			values[key] = value
		}
	}
	if values["LoadState"] != "loaded" {
		return systemdUnitState{}, fmt.Errorf("%s load state is %q", unit, values["LoadState"])
	}
	return systemdUnitState{active: values["ActiveState"], sub: values["SubState"]}, nil
}

func (m *SystemdManager) requireStopped(ctx context.Context, unit string) error {
	state, err := m.unitState(ctx, unit)
	if err != nil {
		return err
	}
	if state.active != "inactive" || state.sub != "dead" {
		return fmt.Errorf("%s is not stopped: %s/%s", unit, state.active, state.sub)
	}
	return nil
}

func (m *SystemdManager) startUnit(ctx context.Context, unit, opposing string) error {
	if m.config.MediaStopMode == MediaStopService {
		if err := m.requireStopped(ctx, opposing); err != nil {
			return err
		}
		if err := m.Released(ctx); err != nil {
			return err
		}
	}
	return m.runSystemctl(ctx, "start", unit)
}

func (m *SystemdManager) stopUnit(ctx context.Context, unit string) error {
	if err := m.runSystemctl(ctx, "stop", unit); err != nil {
		return err
	}
	if m.config.MediaStopMode == MediaStopService {
		return m.requireStopped(ctx, unit)
	}
	return nil
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
	return m.checkHTTPResponse(request, "health")
}

func (m *SystemdManager) releaseMedia(ctx context.Context) error {
	body := bytes.NewBufferString(`{"unload_models":true,"free_memory":true}`)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, m.config.MediaReleaseURL, body)
	if err != nil {
		return fmt.Errorf("create media release request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	return m.checkHTTPResponse(request, "media release")
}

func (m *SystemdManager) checkHTTPResponse(request *http.Request, action string) error {
	response, err := m.client.Do(request)
	if err != nil {
		return fmt.Errorf("%s request: %w", action, err)
	}
	defer response.Body.Close()
	if _, err := io.Copy(io.Discard, io.LimitReader(response.Body, 4096)); err != nil {
		return fmt.Errorf("read %s response: %w", action, err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("%s status %d", action, response.StatusCode)
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

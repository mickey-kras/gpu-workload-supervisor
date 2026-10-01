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
	MediaStopMode       MediaStopMode
	TextUnit            string
	MediaUnit           string
	TextHealthURL       string
	MediaHealthURL      string
	MediaReleaseURL     string
	HealthTimeout       time.Duration
	GPUIndex            int
	TextCgroup          string
	MediaCgroup         string
	TextRequiredMiB     uint64
	MediaRequiredMiB    uint64
	CapacityHeadroomMiB uint64
	NvidiaSMIPath       string
	SystemctlPath       string
}

type SystemdManager struct {
	config  SystemdConfig
	runner  CommandRunner
	client  *http.Client
	cgroups cgroupFS
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
	if err := config.validateUnits(); err != nil {
		return nil, err
	}
	if err := config.validateResources(); err != nil {
		return nil, err
	}
	if config.NvidiaSMIPath != "" || config.TextRequiredMiB != 0 || config.MediaRequiredMiB != 0 {
		resolved, err := validateExecutable(config.NvidiaSMIPath)
		if err != nil {
			return nil, fmt.Errorf("nvidia-smi: %w", err)
		}
		config.NvidiaSMIPath = resolved
	}
	resolvedSystemctl, err := validateExecutable(config.SystemctlPath)
	if err != nil {
		return nil, fmt.Errorf("systemctl: %w", err)
	}
	config.SystemctlPath = resolvedSystemctl
	if err := config.validateEndpoints(); err != nil {
		return nil, err
	}
	if runner == nil || client == nil {
		return nil, errors.New("runner and HTTP client are required")
	}
	return &SystemdManager{config: config, runner: runner, client: client, cgroups: cgroupFS{root: "/sys/fs/cgroup", verify: verifyUnifiedHierarchy}}, nil
}

func (config SystemdConfig) validateUnits() error {
	if err := config.MediaStopMode.Validate(); err != nil {
		return err
	}
	if config.TextUnit == "" || config.MediaUnit == "" {
		return errors.New("text and media units are required")
	}
	if config.TextUnit == config.MediaUnit {
		return errors.New("text and media units must differ")
	}
	if !systemdUnitPattern.MatchString(config.TextUnit) || !systemdUnitPattern.MatchString(config.MediaUnit) {
		return errors.New("invalid systemd unit name")
	}
	return nil
}

func (config SystemdConfig) validateResources() error {
	if config.GPUIndex < 0 {
		return errors.New("GPU index must not be negative")
	}
	for _, group := range []string{config.TextCgroup, config.MediaCgroup} {
		if err := validateCgroup(group); err != nil {
			return err
		}
	}
	if config.TextCgroup == config.MediaCgroup || strings.HasPrefix(config.TextCgroup, config.MediaCgroup+"/") || strings.HasPrefix(config.MediaCgroup, config.TextCgroup+"/") {
		return errors.New("text and media cgroups must be distinct and non-overlapping")
	}
	if config.TextRequiredMiB > ^uint64(0)-config.CapacityHeadroomMiB || config.MediaRequiredMiB > ^uint64(0)-config.CapacityHeadroomMiB {
		return errors.New("capacity requirement plus headroom overflows")
	}
	if config.CapacityHeadroomMiB != 0 && config.TextRequiredMiB == 0 && config.MediaRequiredMiB == 0 {
		return errors.New("capacity headroom requires a measured target requirement")
	}
	return nil
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
		return m.startUnit(ctx, m.config.TextUnit, control.WorkloadText)
	case control.WorkloadMedia:
		return m.startUnit(ctx, m.config.MediaUnit, control.WorkloadMedia)
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
		if err := m.releasedUnit(ctx, m.config.MediaUnit, m.config.MediaCgroup, m.config.MediaStopMode != MediaStopService); err != nil {
			return err
		}
		return m.getHealthy(ctx, m.config.TextHealthURL)
	case control.WorkloadMedia:
		if err := m.releasedUnit(ctx, m.config.TextUnit, m.config.TextCgroup, false); err != nil {
			return err
		}
		return m.getHealthy(ctx, m.config.MediaHealthURL)
	default:
		return fmt.Errorf("workload %q has no health check", workload)
	}
}

var ErrUnloadUnverified = errors.New("live media unload cannot be verified")

func (m *SystemdManager) Released(ctx context.Context) error {
	return m.ReleasedFor(ctx, control.WorkloadIdle)
}

func (m *SystemdManager) ReleasedFor(ctx context.Context, target control.Workload) error {
	if target != control.WorkloadIdle && target != control.WorkloadText && target != control.WorkloadMedia {
		return fmt.Errorf("invalid release target %q", target)
	}
	if err := m.releasedUnit(ctx, m.config.TextUnit, m.config.TextCgroup, false); err != nil {
		return err
	}
	if target == control.WorkloadMedia && m.config.MediaStopMode != MediaStopService {
		state, err := m.unitState(ctx, m.config.MediaUnit)
		if err != nil {
			return err
		}
		// The destination UI may already be running. It need not unload itself
		// before accepting media work, but the opposing text subtree must be empty.
		if state.active == "active" && state.sub == "running" {
			return nil
		}
	}
	return m.releasedUnit(ctx, m.config.MediaUnit, m.config.MediaCgroup, m.config.MediaStopMode != MediaStopService)
}

func (m *SystemdManager) releasedUnit(ctx context.Context, unit, group string, allowUnload bool) error {
	state, err := m.unitState(ctx, unit)
	if err != nil {
		return err
	}
	if allowUnload && state.active == "active" && state.sub == "running" {
		return fmt.Errorf("%w: %s requires runtime-specific proof that work is drained and models/resources are released; HTTP success is insufficient; use -media-stop-mode stop-service or stop the unit explicitly", ErrUnloadUnverified, unit)
	}
	if state.active != "inactive" || state.sub != "dead" {
		return fmt.Errorf("%s is not stopped: %s/%s", unit, state.active, state.sub)
	}
	if !state.hasCgroup || (state.cgroup != "" && state.cgroup != group) {
		return fmt.Errorf("%s ControlGroup does not match configured cgroup %s or metadata is missing", unit, group)
	}
	if err := m.verifyManagerCgroup(ctx); err != nil {
		return err
	}
	if err := m.cgroups.empty(group); err != nil {
		return fmt.Errorf("%s release: %w", unit, err)
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
	active    string
	sub       string
	cgroup    string
	hasCgroup bool
}

func (m *SystemdManager) unitState(ctx context.Context, unit string) (systemdUnitState, error) {
	state, err := m.readUnitState(ctx, unit)
	if err != nil {
		return state, err
	}
	expected := m.config.TextCgroup
	if unit == m.config.MediaUnit {
		expected = m.config.MediaCgroup
	}
	if state.cgroup != "" && state.cgroup != expected {
		return systemdUnitState{}, fmt.Errorf("%s ControlGroup %q does not match configured cgroup %s", unit, state.cgroup, expected)
	}
	return state, nil
}

// The user's root slice identifies the same manager namespace that reports unit
// paths. Unlike /proc/1/ns/cgroup, this evidence is available to an ordinary user.
func (m *SystemdManager) verifyManagerCgroup(ctx context.Context) error {
	state, err := m.readUnitState(ctx, "-.slice")
	if err != nil {
		return fmt.Errorf("inspect systemd manager cgroup anchor: %w", err)
	}
	if state.active != "active" || state.sub != "active" || !state.hasCgroup {
		return errors.New("systemd manager root slice must be loaded/active with ControlGroup metadata")
	}
	if err := validateCgroup(state.cgroup); err != nil {
		return fmt.Errorf("systemd manager cgroup anchor: %w", err)
	}
	for _, group := range []string{m.config.TextCgroup, m.config.MediaCgroup} {
		if !strings.HasPrefix(group, state.cgroup+"/") {
			return fmt.Errorf("configured cgroup %s is not within systemd manager root %s", group, state.cgroup)
		}
	}
	if err := m.cgroups.check(state.cgroup, false); err != nil {
		return fmt.Errorf("systemd manager cgroup anchor is unavailable: %w", err)
	}
	return nil
}

func (m *SystemdManager) readUnitState(ctx context.Context, unit string) (systemdUnitState, error) {
	output, err := m.runner.Run(ctx, m.config.SystemctlPath, "--user", "show",
		"--property=LoadState", "--property=ActiveState", "--property=SubState", "--property=ControlGroup", "--", unit)
	if err != nil {
		return systemdUnitState{}, fmt.Errorf("inspect %s: %w: %s", unit, err, strings.TrimSpace(string(output)))
	}
	values := map[string]string{}
	for _, line := range strings.Split(string(output), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			if _, duplicate := values[key]; duplicate {
				return systemdUnitState{}, fmt.Errorf("%s duplicate systemd property %s", unit, key)
			}
			values[key] = value
		}
	}
	if values["LoadState"] != "loaded" {
		return systemdUnitState{}, fmt.Errorf("%s load state is %q", unit, values["LoadState"])
	}
	_, hasCgroup := values["ControlGroup"]
	return systemdUnitState{active: values["ActiveState"], sub: values["SubState"], cgroup: values["ControlGroup"], hasCgroup: hasCgroup}, nil
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

func (m *SystemdManager) startUnit(ctx context.Context, unit string, workload control.Workload) error {
	if err := m.ReleasedFor(ctx, workload); err != nil {
		return err
	}
	if err := m.capacity(ctx, workload); err != nil {
		return err
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

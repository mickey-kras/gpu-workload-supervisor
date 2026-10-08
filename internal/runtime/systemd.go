package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/httptransport"
)

type CommandRunner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, name, args...)
	// A descendant can inherit stdout/stderr after the command exits or is
	// killed. Bound the pipe drain as well as the command itself.
	command.WaitDelay = 100 * time.Millisecond
	output, err := command.CombinedOutput()
	if err != nil && ctx.Err() != nil {
		err = errors.Join(err, ctx.Err())
	}
	return output, err
}

type SystemdConfig struct {
	Catalog             *control.Catalog
	HealthTimeout       time.Duration
	GPUIndex            int
	CapacityHeadroomMiB uint64
	NvidiaSMIPath       string
	SystemctlPath       string
	// OwnedUnitDir locates supervisor-owned unit files for the preflight
	// orphan scan; empty derives the effective user's systemd user directory.
	OwnedUnitDir string
}

type SystemdManager struct {
	config                    SystemdConfig
	runner                    CommandRunner
	client                    *http.Client
	cgroups                   cgroupFS
	nativeExecutableValidator func(string) error
}

func NewSystemdManager(config SystemdConfig) (*SystemdManager, error) {
	client := &http.Client{
		Timeout:   config.HealthTimeout,
		Transport: httptransport.NewDirect(),
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return newSystemdManager(config, ExecRunner{}, client)
}

func newSystemdManager(config SystemdConfig, runner CommandRunner, client *http.Client) (*SystemdManager, error) {
	if err := config.prepareWorkloads(); err != nil {
		return nil, err
	}
	if config.NvidiaSMIPath != "" || config.measuresCapacity() {
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
	if config.HealthTimeout <= 0 || config.GPUIndex < 0 {
		return nil, errors.New("invalid runtime timeout or GPU index")
	}
	if runner == nil || client == nil {
		return nil, errors.New("runner and HTTP client are required")
	}
	return &SystemdManager{config: config, runner: runner, client: client, cgroups: cgroupFS{root: "/sys/fs/cgroup", verify: verifyUnifiedHierarchy}}, nil
}

func (m *SystemdManager) Start(ctx context.Context, workload control.Workload) error {
	p, ok := m.config.Catalog.Profile(workload)
	if !ok {
		return errors.New("unconfigured workload")
	}
	if err := m.verifyNativeBinding(ctx, p); err != nil {
		return err
	}
	if m.sharedUnitProfile(p) {
		if err := m.evictSharedOllamaUnit(ctx, p); err != nil {
			return err
		}
	}
	if err := m.startUnit(ctx, p.Unit, workload); err != nil {
		return err
	}
	if p.NativeModel != nil {
		return m.startNative(ctx, p)
	}
	return nil
}

// StopForRecovery shuts down every configured unit regardless of adapter policy.
func (m *SystemdManager) StopForRecovery(ctx context.Context) error {
	seen := map[string]bool{}
	for _, p := range m.config.Catalog.Profiles {
		if seen[p.Unit] {
			continue
		}
		seen[p.Unit] = true
		if err := m.runSystemctl(ctx, "stop", p.Unit); err != nil {
			return err
		}
		if err := m.requireStopped(ctx, p.Unit); err != nil {
			return err
		}
	}
	return nil
}

var ErrUnloadUnverified = errors.New("live media unload cannot be verified")

func (m *SystemdManager) releasedUnit(ctx context.Context, unit, group string, allowUnload bool) error {
	state, err := m.unitState(ctx, unit)
	if err != nil {
		return err
	}
	if allowUnload && state.active == "active" && state.sub == "running" {
		return fmt.Errorf("%w: %s requires runtime-specific proof that work is drained and models/resources are released; HTTP success is insufficient; select the stop-service policy or stop the unit explicitly", ErrUnloadUnverified, unit)
	}
	if state.active != "inactive" || state.sub != "dead" {
		return fmt.Errorf("%s is not stopped", unit)
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

type systemdUnitState struct {
	active    string
	sub       string
	cgroup    string
	hasCgroup bool
}

func (m *SystemdManager) profileForUnit(unit string) control.WorkloadProfile {
	for _, p := range m.config.Catalog.Profiles {
		if p.Unit == unit {
			return p
		}
	}
	return control.WorkloadProfile{}
}

func (m *SystemdManager) unitState(ctx context.Context, unit string) (systemdUnitState, error) {
	state, err := m.readUnitState(ctx, unit)
	if err != nil {
		return state, err
	}
	if expected := m.profileForUnit(unit).Cgroup; state.cgroup != "" && state.cgroup != expected {
		return systemdUnitState{}, fmt.Errorf("%s ControlGroup does not match configured cgroup", unit)
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
	for _, p := range m.config.Catalog.Profiles {
		if !strings.HasPrefix(p.Cgroup, state.cgroup+"/") {
			return errors.New("configured cgroup is not within systemd manager root")
		}
		// Owned profiles admit no degrees of freedom: the cgroup must be the
		// exact derivation from the discovered manager root, so a hand-edited
		// profile cannot escape into a foreign or nested prefix.
		if p.NativeModel != nil && p.NativeModel.Owned != nil && p.Cgroup != OwnedCgroup(state.cgroup, p) {
			return errors.New("owned workload cgroup must derive from the systemd manager root")
		}
	}
	if err := m.cgroups.check(state.cgroup, false); err != nil {
		return SafeError("systemd manager cgroup anchor is unavailable", err)
	}
	return nil
}

func (m *SystemdManager) readUnitState(ctx context.Context, unit string) (systemdUnitState, error) {
	output, err := m.runner.Run(ctx, m.config.SystemctlPath, "--user", "show",
		"--property=LoadState", "--property=ActiveState", "--property=SubState", "--property=ControlGroup", "--", unit)
	if err != nil {
		return systemdUnitState{}, fmt.Errorf("inspect %s: %w", unit, SafeError("command failed", err))
	}
	values := map[string]string{}
	for _, line := range strings.Split(string(output), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			if _, duplicate := values[key]; duplicate {
				return systemdUnitState{}, fmt.Errorf("%s duplicate systemd property", unit)
			}
			values[key] = value
		}
	}
	if values["LoadState"] != "loaded" {
		return systemdUnitState{}, fmt.Errorf("%s load state is not loaded", unit)
	}
	_, hasCgroup := values["ControlGroup"]
	return systemdUnitState{active: values["ActiveState"], sub: values["SubState"], cgroup: values["ControlGroup"], hasCgroup: hasCgroup}, nil
}

func (m *SystemdManager) requireStopped(ctx context.Context, unit string) error {
	state, err := m.unitState(ctx, unit)
	if err != nil {
		return err
	}
	if state.active == "failed" && state.sub == "failed" {
		if err := m.resetStoppedFailure(ctx, unit, state); err != nil {
			return err
		}
		state, err = m.unitState(ctx, unit)
		if err != nil {
			return err
		}
	}
	if state.active != "inactive" || state.sub != "dead" {
		return fmt.Errorf("%s is not stopped", unit)
	}
	return nil
}

// A successful stop does not clear systemd's retained failure state. Reset it
// only after verifying the configured subtree has no remaining processes.
func (m *SystemdManager) resetStoppedFailure(ctx context.Context, unit string, state systemdUnitState) error {
	if !state.hasCgroup {
		return errors.New("workload ControlGroup metadata unavailable")
	}
	if err := m.verifyManagerCgroup(ctx); err != nil {
		return err
	}
	if err := m.cgroups.empty(m.profileForUnit(unit).Cgroup); err != nil {
		return err
	}
	return m.runSystemctl(ctx, "reset-failed", unit)
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
	return m.requireStopped(ctx, unit)
}

func (m *SystemdManager) runSystemctl(ctx context.Context, action, unit string) error {
	_, err := m.runner.Run(ctx, m.config.SystemctlPath, "--user", action, "--", unit)
	if err != nil {
		return fmt.Errorf("systemctl %s %s: %w", action, unit, SafeError("command failed", err))
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

func (m *SystemdManager) checkHTTPResponse(request *http.Request, action string) error {
	response, err := m.client.Do(request)
	if err != nil {
		return fmt.Errorf("%s request: %w", action, &safeHTTPRequestError{cause: err})
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

// safeHTTPRequestError keeps error identity without rendering transport messages,
// which can include credentials or redirect URLs even below a url.Error.
type safeHTTPRequestError struct {
	cause error
}

func (e *safeHTTPRequestError) Error() string {
	switch {
	case errors.Is(e.cause, context.Canceled):
		return "HTTP request canceled"
	case errors.Is(e.cause, context.DeadlineExceeded):
		return "HTTP request deadline exceeded"
	}
	var networkError net.Error
	if errors.As(e.cause, &networkError) && networkError.Timeout() {
		return "HTTP request timed out"
	}
	var operationError *net.OpError
	if errors.As(e.cause, &operationError) {
		return "HTTP request network failure"
	}
	return "HTTP request failed"
}

func (e *safeHTTPRequestError) Unwrap() error { return e.cause }

func (config *SystemdConfig) prepareWorkloads() error {
	if config.Catalog == nil {
		return errors.New("workload catalog is required")
	}
	cloned := config.Catalog.Clone()
	config.Catalog = &cloned
	if err := cloned.Validate(); err != nil {
		return err
	}
	for _, p := range cloned.Profiles {
		if p.RequiredMiB > ^uint64(0)-config.CapacityHeadroomMiB {
			return errors.New("capacity requirement plus headroom overflows")
		}
	}
	// Headroom is only ever added to a measured requirement; without one it
	// would be accepted but silently never applied. An explicitly disabled
	// catalog retains this preference for future measured workloads.
	if config.CapacityHeadroomMiB != 0 && !config.Catalog.Disabled && !config.measuresCapacity() {
		return errors.New("capacity headroom requires a measured target requirement")
	}
	return nil
}

func (config SystemdConfig) measuresCapacity() bool {
	for _, p := range config.Catalog.Profiles {
		if p.RequiredMiB != 0 {
			return true
		}
	}
	return false
}

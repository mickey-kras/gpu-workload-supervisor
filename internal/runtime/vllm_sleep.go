package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/strictjson"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/vllmkey"
)

type vllmSleepStatus struct {
	IsSleeping *bool `json:"is_sleeping"`
}

// Sleep offloads vLLM weights to RAM and discards KV cache (/sleep?level=1)
// when the profile enables sleep mode, falling back to Stop if unsupported or failed.
func (m *SystemdManager) Sleep(ctx context.Context, id control.Workload) error {
	p, ok := m.config.Catalog.Profile(id)
	if !ok {
		return errors.New("unconfigured workload")
	}
	if !m.supportsSleepMode(p) {
		return m.Stop(ctx, id)
	}
	st, err := m.unitState(ctx, p.Unit)
	if err != nil {
		return err
	}
	if st.active == "inactive" && st.sub == "dead" {
		return nil
	}
	if st.active == "active" && st.sub == "running" {
		if err := m.verifyNativeBinding(ctx, p); err != nil {
			return err
		}
		if m.requestVLLMSleep(ctx, *p.NativeModel) == nil && m.releasedViaSleep(ctx, p) {
			return nil
		}
	}
	return m.Stop(ctx, id)
}

// Wake resumes a hibernated vLLM unit from host RAM (/wake_up) when active,
// or cold-starts the workload when the unit is stopped.
func (m *SystemdManager) Wake(ctx context.Context, id control.Workload) error {
	p, ok := m.config.Catalog.Profile(id)
	if !ok {
		return errors.New("unconfigured workload")
	}
	if !m.supportsSleepMode(p) {
		return m.Start(ctx, id)
	}
	st, err := m.unitState(ctx, p.Unit)
	if err != nil {
		return err
	}
	if st.active == "active" && st.sub == "running" {
		if err := m.verifyNativeBinding(ctx, p); err != nil {
			return err
		}
		if err := m.prepareStart(ctx, id); err != nil {
			return err
		}
		if p.HasLimits() {
			if err := m.VerifyLimitProtection(ctx, id); err != nil {
				return err
			}
		}
		wakeErr := m.requestVLLMWake(ctx, *p.NativeModel)
		sleeping, stateErr := m.vllmSleeping(ctx, *p.NativeModel)
		if wakeErr == nil && stateErr == nil && !sleeping {
			if err := m.checkStartedUnit(ctx, p); err != nil {
				stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), limitCleanupTimeout)
				defer cancel()
				return errors.Join(err, m.stopUnit(stopCtx, p.Unit))
			}
			return nil
		}
		if stopErr := m.stopUnit(ctx, p.Unit); stopErr != nil {
			return stopErr
		}
	}
	return m.Start(ctx, id)
}

func (m *SystemdManager) supportsSleepMode(p control.WorkloadProfile) bool {
	n := p.NativeModel
	if n == nil || n.Runtime != "vllm" || n.Endpoint == "" {
		return false
	}
	if n.Owned != nil {
		return n.Owned.Serving.SleepMode
	}
	sources, err := readLaunchSources(n.LaunchFile, n.DropIns)
	if err != nil || fmt.Sprintf("%x", sha256.Sum256(sources[0])) != n.LaunchSHA256 {
		return false
	}
	unit, err := parseExternalLaunchSources(sources)
	if err != nil {
		return false
	}
	flags, err := parseServerCommand(strings.Fields(unit.execStart), n.Runtime)
	return err == nil && flags.sleepMode
}

func (m *SystemdManager) isHibernated(ctx context.Context, p control.WorkloadProfile) bool {
	if !m.supportsSleepMode(p) {
		return false
	}
	return m.releasedViaSleep(ctx, p)
}

func (m *SystemdManager) releasedViaSleep(ctx context.Context, p control.WorkloadProfile) bool {
	if !m.supportsSleepMode(p) {
		return false
	}
	st, err := m.unitState(ctx, p.Unit)
	if err != nil || st.active != "active" || st.sub != "running" {
		return false
	}
	if err := m.verifyNativeBinding(ctx, p); err != nil {
		return false
	}
	if !st.hasCgroup || st.cgroup != p.Cgroup {
		return false
	}
	if _, err := m.managerCgroup(ctx); err != nil {
		return false
	}
	if err := m.cgroups.check(p.Cgroup, false); err != nil {
		return false
	}
	return m.verifyVLLMSleeping(ctx, *p.NativeModel) && m.sleepGPUReleased(ctx, p) == nil
}

// A runtime status flag is not GPU release evidence. Every compute process
// still in the sleeping unit must report zero allocated VRAM. Unknown process
// attribution or memory keeps the GPU unavailable and causes a cold stop.
func (m *SystemdManager) sleepGPUReleased(ctx context.Context, p control.WorkloadProfile) error {
	apps, err := m.computeApps(ctx, m.gpuDevice(p))
	if err != nil {
		return err
	}
	for _, app := range apps {
		group, err := m.gpuHost.processCgroup(app.pid)
		if err != nil {
			return gpuCheckFailure("sleeping GPU process ownership could not be read", err)
		}
		if (group == p.Cgroup || strings.HasPrefix(group, p.Cgroup+"/")) && (!app.memoryKnown || app.usedMiB != 0) {
			return errors.New("sleeping workload has not released GPU memory")
		}
	}
	return nil
}

func (m *SystemdManager) requestVLLMSleep(ctx context.Context, n control.NativeModel) error {
	_, err := m.doVLLMControlRequest(ctx, n, http.MethodPost, "/sleep?level=1")
	return err
}

func (m *SystemdManager) requestVLLMWake(ctx context.Context, n control.NativeModel) error {
	_, err := m.doVLLMControlRequest(ctx, n, http.MethodPost, "/wake_up")
	return err
}

func (m *SystemdManager) verifyVLLMSleeping(ctx context.Context, n control.NativeModel) bool {
	sleeping, err := m.vllmSleeping(ctx, n)
	return err == nil && sleeping
}

func (m *SystemdManager) vllmSleeping(ctx context.Context, n control.NativeModel) (bool, error) {
	body, err := m.doVLLMControlRequest(ctx, n, http.MethodGet, "/is_sleeping")
	if err != nil {
		return false, err
	}
	var status vllmSleepStatus
	if err := strictjson.DecodeLimited(bytes.NewReader(body), 1024, &status); err != nil {
		return false, err
	}
	if status.IsSleeping == nil {
		return false, errors.New("vLLM sleep status is missing")
	}
	return *status.IsSleeping, nil
}

func (m *SystemdManager) doVLLMControlRequest(ctx context.Context, n control.NativeModel, method, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, n.Endpoint+path, nil)
	if err != nil {
		return nil, err
	}
	if n.EnvironmentFile != "" {
		key, keyErr := vllmkey.Read(n.EnvironmentFile)
		if keyErr != nil {
			return nil, keyErr
		}
		if listenErr := checkKeyListener(n.Endpoint); listenErr != nil {
			return nil, listenErr
		}
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, &safeHTTPRequestError{cause: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("vLLM control endpoint %s returned status %d", path, resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return nil, err
	}
	if method == http.MethodGet && !json.Valid(raw) {
		return nil, errors.New("invalid JSON from vLLM control endpoint")
	}
	return raw, nil
}

package runtime

import (
	"context"
	"fmt"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"strconv"
	"strings"
)

// ErrGPUIdentityUnresolved distinguishes a discovered numeric CUDA selection
// from an invalid installation. Metadata inspection cannot establish its CUDA
// ordinal-to-physical-UUID mapping, and must not start a runtime to infer it.
var ErrGPUIdentityUnresolved error = unresolvedGPUIdentityError{}

type unresolvedGPUIdentityError struct{}

func (unresolvedGPUIdentityError) Error() string {
	return "CUDA_VISIBLE_DEVICES GPU identity is unresolved; review and approve a change to the external service using the configured GPU's full UUID, then reload systemd and refresh the binding"
}

func (unresolvedGPUIdentityError) Unwrap() error { return ErrLaunchUnsupported }

// applyGPUVisibility records a full GPU UUID when present. Numeric ordinals,
// lists and MIG selections are adopted as-is; GPU pinning stays unverified.
func (u *parsedLaunchUnit) applyGPUVisibility(value string) error {
	if control.ValidGPUUUID(value) {
		u.gpuUUID = value
	}
	return nil
}

func (m *SystemdManager) verifyProfileGPU(ctx context.Context, p control.WorkloadProfile) error {
	uuid := profileGPUUUID(p)
	if uuid == "" {
		return nil
	}
	failure := func(reason string) error {
		return fmt.Errorf("%w: CUDA_VISIBLE_DEVICES %s; verify the configured physical GPU and refresh this installation binding", ErrLaunchUnsupported, reason)
	}
	if !control.ValidGPUUUID(uuid) || m.config.NvidiaSMIPath == "" {
		return failure("physical GPU identity cannot be inspected")
	}
	out, err := m.runner.Run(ctx, m.config.NvidiaSMIPath, "--query-gpu=uuid", "--format=csv,noheader", "-i", strconv.Itoa(m.config.GPUIndex))
	if err != nil || len(out) > 128 || !control.ValidGPUUUID(strings.TrimSpace(string(out))) {
		return failure("physical GPU identity query failed")
	}
	if strings.TrimSpace(string(out)) != uuid {
		return failure("does not select the configured physical GPU")
	}
	return nil
}

func profileGPUUUID(p control.WorkloadProfile) string {
	if p.Trainer != nil {
		return p.Trainer.GPUUUID
	}
	if p.NativeModel != nil {
		return p.NativeModel.GPUUUID
	}
	if p.LaunchBinding != nil {
		return p.LaunchBinding.GPUUUID
	}
	return ""
}

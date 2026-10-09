package runtime

import (
	"context"
	"fmt"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"strconv"
	"strings"
)

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
	if p.NativeModel != nil {
		return p.NativeModel.GPUUUID
	}
	if p.LaunchBinding != nil {
		return p.LaunchBinding.GPUUUID
	}
	return ""
}

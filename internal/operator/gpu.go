package operator

import (
	"context"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/supervisor"
)

// maxGPUProcesses bounds the listed processes, so the response stays below
// MaxResponseBytes. The count is enough to show what blocks the start.
const maxGPUProcesses int = 32

// maxProcessNameRunes bounds one process name in a response.
const maxProcessNameRunes int = 256

// gpuErrorCode maps a GPU check error to its code.
func gpuErrorCode(err error) Code {
	switch {
	case errors.Is(err, gpuruntime.ErrGPUProcessNotOwned):
		return NotPermitted
	case errors.Is(err, gpuruntime.ErrGPUCheckFailed):
		return GPUCheckFailed
	case errors.Is(err, gpuruntime.ErrGPUInUse):
		return GPUInUse
	default:
		return Unavailable
	}
}

// gpuCheck gives the gpuCheck object of a GPU code, or nil for any other
// code.
func gpuCheck(code Code, err error) *GPUCheck {
	switch code {
	case GPUInUse:
		var inUse *gpuruntime.GPUInUseError
		if !errors.As(err, &inUse) || len(inUse.Processes) == 0 {
			return nil
		}
		check := &GPUCheck{}
		for _, p := range inUse.Processes[:min(len(inUse.Processes), maxGPUProcesses)] {
			process := GPUProcess{PID: p.PID, Name: displayText(p.Name), User: displayText(p.User), Closable: p.Closable}
			if p.MemoryKnown {
				used := p.UsedMiB
				process.UsedMiB = &used
			}
			check.Processes = append(check.Processes, process)
		}
		return check
	case GPUCheckFailed:
		var failed *gpuruntime.GPUCheckError
		if errors.As(err, &failed) && failed.Reason != "" {
			return &GPUCheck{Failure: displayText(failed.Reason)}
		}
		return &GPUCheck{Failure: "the GPU process check did not complete"}
	default:
		return nil
	}
}

// displayText drops control and bidirectional override characters and
// bounds the length. A process name comes from another program.
func displayText(s string) string {
	var b strings.Builder
	count := 0
	for _, r := range s {
		if r == utf8.RuneError || unicode.IsControl(r) || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) {
			continue
		}
		if count == maxProcessNameRunes {
			break
		}
		b.WriteRune(r)
		count++
	}
	return strings.TrimSpace(b.String())
}

// respondClose runs Close for one process. A free GPU answers ok with the
// current status. A process that is still there, another process, a failed
// check or a process of another user answers with its code.
func (session Session) respondClose(ctx context.Context, req Request, result Response) Response {
	if code := session.listedTarget(req.Target); code != OK || req.Target == control.WorkloadIdle {
		result.Code = InvalidRequest
		return result
	}
	err := session.Backend.CloseGPUProcess(ctx, req.Target, req.PID)
	if err != nil {
		result.Code = closeErrorCode(err)
		result.GPUCheck = gpuCheck(result.Code, err)
		return result
	}
	state, err := session.Backend.Status(ctx)
	if result.Code = errorCode(err); result.Code != OK {
		return result
	}
	if ctx.Err() != nil {
		result.Code = Timeout
		return result
	}
	if state.Validate() != nil || !token(state.LeaseFence.Incarnation, 128) {
		result.Code = Unavailable
		return result
	}
	result.Status = session.status(state)
	return result
}

func closeErrorCode(err error) Code {
	if code := gpuErrorCode(err); code != Unavailable {
		return code
	}
	if errors.Is(err, supervisor.ErrInvalidTarget) {
		return InvalidRequest
	}
	return errorCode(err)
}

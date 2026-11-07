package setup

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"time"

	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
)

// commandOutputLimit is the output bound of the CommandFunc contract.
const commandOutputLimit = gpuruntime.CommandOutputLimit

type boundedOutput struct{ buffer bytes.Buffer }

func (output *boundedOutput) Write(data []byte) (int, error) {
	available := commandOutputLimit - output.buffer.Len()
	if len(data) > available {
		output.buffer.Write(data[:available])
		return available, errors.New("setup command output exceeds 1 MiB")
	}
	return output.buffer.Write(data)
}
func boundedCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	output := &boundedOutput{}
	command := exec.CommandContext(ctx, name, args...)
	command.Stdout = output
	command.Stderr = output
	command.WaitDelay = 100 * time.Millisecond
	err := command.Run()
	return output.buffer.Bytes(), err
}

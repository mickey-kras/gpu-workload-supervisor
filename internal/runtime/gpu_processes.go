package runtime

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"golang.org/x/sys/unix"
)

// ErrGPUInUse marks a start that a process outside the supervised cgroups
// blocks. External programs are reported, never stopped by a start.
var ErrGPUInUse = errors.New("another program uses the target GPU")

// ErrGPUCheckFailed marks a GPU process check that did not complete. It
// blocks the start: a failed check is never a free GPU.
var ErrGPUCheckFailed = errors.New("the GPU process check did not complete")

// ErrGPUProcessNotOwned refuses to close a process that does not run as
// the current user, or any process when the current user is root.
var ErrGPUProcessNotOwned = errors.New("the GPU process does not run as your user")

// gpuQueryTimeout bounds one nvidia-smi compute process query. A query that
// hangs fails the check instead of using the whole caller budget.
const gpuQueryTimeout time.Duration = 15 * time.Second

// gpuCloseGrace is how long Close waits for the process to exit after
// SIGTERM before it checks the GPU again. It never sends SIGKILL.
const gpuCloseGrace time.Duration = 5 * time.Second

// computeAppsQuery is the one compute process query. The release checks and
// the start check all use it.
var computeAppsQuery = []string{"--query-compute-apps=pid,process_name,used_memory", "--format=csv,noheader,nounits", "-i"}

// GPUProcess is one compute process on the target GPU that no supervised
// unit owns. UID is -1 and User is empty when the process cannot be read,
// for example in another PID namespace or after it ended.
type GPUProcess struct {
	PID         int
	Name        string
	UsedMiB     uint64
	MemoryKnown bool
	UID         int
	User        string
	// Closable is true only when the real, effective and saved user IDs of
	// the process are the current user, and that user is not root.
	Closable bool
}

// GPUInUseError lists the processes that block a start.
type GPUInUseError struct {
	Device    string
	Processes []GPUProcess
}

func (e *GPUInUseError) Error() string {
	parts := make([]string, 0, len(e.Processes))
	for _, p := range e.Processes {
		owner := p.User
		if owner == "" {
			owner = "unknown user"
		}
		memory := "unknown GPU memory"
		if p.MemoryKnown {
			memory = strconv.FormatUint(p.UsedMiB, 10) + " MiB"
		}
		parts = append(parts, fmt.Sprintf("%q (PID %d, %s, %s)", p.Name, p.PID, owner, memory))
	}
	return fmt.Sprintf("%s: GPU %s: %s; close the program, then retry", ErrGPUInUse, e.Device, strings.Join(parts, ", "))
}

func (e *GPUInUseError) Unwrap() error { return ErrGPUInUse }

// GPUCheckError is a failed or timed-out GPU process check. Reason is safe
// to show: it never holds command output.
type GPUCheckError struct {
	Reason string
	cause  error
}

func (e *GPUCheckError) Error() string {
	return fmt.Sprintf("%s: %s; the GPU is not known to be free, retry", ErrGPUCheckFailed, e.Reason)
}

func (e *GPUCheckError) Unwrap() []error {
	if e.cause == nil {
		return []error{ErrGPUCheckFailed}
	}
	return []error{ErrGPUCheckFailed, e.cause}
}

func gpuCheckFailure(reason string, cause error) error {
	return &GPUCheckError{Reason: reason, cause: cause}
}

// pidHandle pins one process for Close, so a reused PID never receives the
// signal.
type pidHandle interface {
	Terminate() error
	// WaitExit reports whether the process exited within the timeout.
	WaitExit(time.Duration) (bool, error)
	Close() error
}

// gpuProcessHost is the host access of the GPU process check. Tests inject
// a procfs tree, user IDs and process handles.
type gpuProcessHost struct {
	proc       string
	currentUID func() int
	userName   func(uid int) string
	open       func(pid int) (pidHandle, error)
	timeout    time.Duration
	grace      time.Duration
}

func defaultGPUProcessHost() gpuProcessHost {
	return gpuProcessHost{proc: "/proc", currentUID: os.Getuid, userName: lookupUserName, open: openPidfd, timeout: gpuQueryTimeout, grace: gpuCloseGrace}
}

func lookupUserName(uid int) string {
	u, err := user.LookupId(strconv.Itoa(uid))
	if err != nil || u.Username == "" {
		return strconv.Itoa(uid)
	}
	return u.Username
}

type pidfd struct{ fd int }

func openPidfd(pid int) (pidHandle, error) {
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return nil, err
	}
	return &pidfd{fd: fd}, nil
}

func (p *pidfd) Terminate() error { return unix.PidfdSendSignal(p.fd, unix.SIGTERM, nil, 0) }

func (p *pidfd) WaitExit(timeout time.Duration) (bool, error) {
	fds := []unix.PollFd{{Fd: int32(p.fd), Events: unix.POLLIN}}
	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false, nil
		}
		n, err := unix.Poll(fds, int(remaining.Milliseconds())+1)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		return n > 0, err
	}
}

func (p *pidfd) Close() error { return unix.Close(p.fd) }

// computeApp is one row of the compute process query.
type computeApp struct {
	pid         int
	name        string
	usedMiB     uint64
	memoryKnown bool
}

// gpuDevice is the GPU of p for nvidia-smi: its GPU UUID, or the configured
// GPU index when it has none.
func (m *SystemdManager) gpuDevice(p control.WorkloadProfile) string {
	if device := profileGPUUUID(p); device != "" {
		return device
	}
	return m.gpuDeviceTarget()
}

// computeApps runs the one compute process query on device. A query that
// fails, times out or prints rows it cannot parse is an error.
func (m *SystemdManager) computeApps(ctx context.Context, device string) ([]computeApp, error) {
	if m.config.NvidiaSMIPath == "" {
		return nil, gpuCheckFailure("no GPU probe (nvidia-smi) is configured", nil)
	}
	timeout := m.gpuHost.timeout
	if timeout <= 0 {
		timeout = gpuQueryTimeout
	}
	queryCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := m.runner.Run(queryCtx, m.config.NvidiaSMIPath, append(append([]string{}, computeAppsQuery...), device)...)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(queryCtx.Err(), context.DeadlineExceeded) {
			return nil, gpuCheckFailure("the nvidia-smi process query timed out", errors.Join(err, context.DeadlineExceeded))
		}
		return nil, gpuCheckFailure("the nvidia-smi process query failed", err)
	}
	return parseComputeApps(out)
}

// parseComputeApps reads "pid, name, used_memory" rows. A name can hold
// commas, so the PID is the first field and the memory the last.
func parseComputeApps(out []byte) ([]computeApp, error) {
	var apps []computeApp
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		first, last := strings.Index(line, ","), strings.LastIndex(line, ",")
		if first < 0 || first == last {
			return nil, gpuCheckFailure("the nvidia-smi process query printed an unexpected row", nil)
		}
		pid, err := strconv.Atoi(strings.TrimSpace(line[:first]))
		if err != nil || pid <= 0 {
			return nil, gpuCheckFailure("the nvidia-smi process query printed an invalid PID", nil)
		}
		app := computeApp{pid: pid, name: strings.TrimSpace(line[first+1 : last])}
		if used, err := strconv.ParseUint(strings.TrimSpace(line[last+1:]), 10, 64); err == nil {
			app.usedMiB, app.memoryKnown = used, true
		}
		apps = append(apps, app)
	}
	if err := scanner.Err(); err != nil {
		return nil, gpuCheckFailure("the nvidia-smi process query output could not be read", err)
	}
	return apps, nil
}

// supervisedCgroup reports whether group is the cgroup of a catalog unit or
// below it.
func (m *SystemdManager) supervisedCgroup(group string) bool {
	for _, p := range m.config.Catalog.Profiles {
		if p.Cgroup != "" && (group == p.Cgroup || strings.HasPrefix(group, p.Cgroup+"/")) {
			return true
		}
	}
	return false
}

// processCgroup reads the unified cgroup path of pid.
func (h gpuProcessHost) processCgroup(pid int) (string, error) {
	raw, err := os.ReadFile(filepath.Join(h.proc, strconv.Itoa(pid), "cgroup"))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if group, ok := strings.CutPrefix(line, "0::"); ok && strings.HasPrefix(group, "/") {
			return group, nil
		}
	}
	return "", errors.New("no unified cgroup entry")
}

// processUIDs reads the real, effective and saved user IDs of pid.
func (h gpuProcessHost) processUIDs(pid int) ([3]int, error) {
	var ids [3]int
	raw, err := os.ReadFile(filepath.Join(h.proc, strconv.Itoa(pid), "status"))
	if err != nil {
		return ids, err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		rest, ok := strings.CutPrefix(line, "Uid:")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) < 3 {
			break
		}
		for i := range ids {
			if ids[i], err = strconv.Atoi(fields[i]); err != nil {
				return ids, err
			}
		}
		return ids, nil
	}
	return ids, errors.New("no Uid entry")
}

// describe fills the user of one unsupervised process. A process it cannot
// read keeps an unknown user and is never closable.
func (h gpuProcessHost) describe(app computeApp) GPUProcess {
	p := GPUProcess{PID: app.pid, Name: app.name, UsedMiB: app.usedMiB, MemoryKnown: app.memoryKnown, UID: -1}
	ids, err := h.processUIDs(app.pid)
	if err != nil {
		return p
	}
	p.UID = ids[0]
	p.User = h.userName(ids[0])
	current := h.currentUID()
	p.Closable = current != 0 && ids[0] == current && ids[1] == current && ids[2] == current
	return p
}

// unsupervisedGPUProcesses lists the compute processes on the GPU of p that
// are outside every supervised cgroup. A process whose cgroup cannot be
// read because it is not visible counts as unsupervised: it can be in
// another PID namespace. Any other read failure fails the check.
func (m *SystemdManager) unsupervisedGPUProcesses(ctx context.Context, p control.WorkloadProfile) (string, []GPUProcess, error) {
	device := m.gpuDevice(p)
	apps, err := m.computeApps(ctx, device)
	if err != nil {
		return device, nil, err
	}
	var found []GPUProcess
	for _, app := range apps {
		group, err := m.gpuHost.processCgroup(app.pid)
		switch {
		case err == nil && m.supervisedCgroup(group):
			continue
		case err != nil && !errors.Is(err, os.ErrNotExist):
			return device, nil, gpuCheckFailure(fmt.Sprintf("the cgroup of GPU process %d could not be read", app.pid), err)
		}
		found = append(found, m.gpuHost.describe(app))
	}
	return device, found, nil
}

// CheckGPUFree is the check before each start, of a service or a trainer.
// It lists the compute processes on the target GPU and ignores those in
// supervised cgroups. Any other process blocks the start with a
// GPUInUseError. A failed or timed-out query blocks it with a
// GPUCheckError. A running supervised workload is not affected.
func (m *SystemdManager) CheckGPUFree(ctx context.Context, workload control.Workload) error {
	p, ok := m.config.Catalog.Profile(workload)
	if !ok {
		return errors.New("unconfigured workload")
	}
	return m.checkProfileGPUFree(ctx, p)
}

func (m *SystemdManager) checkProfileGPUFree(ctx context.Context, p control.WorkloadProfile) error {
	device, found, err := m.unsupervisedGPUProcesses(ctx, p)
	if err != nil {
		return err
	}
	if len(found) != 0 {
		return &GPUInUseError{Device: device, Processes: found}
	}
	return nil
}

// CloseGPUProcess asks one unsupervised process on the GPU of workload to
// exit, then checks the GPU again and returns that result. It only signals
// a process that runs as the current user, never as root, and only with
// SIGTERM. The process is pinned with a pidfd and listed again before the
// signal, so a reused PID is never signalled. A process that already ended
// is not an error: the result is the check again.
func (m *SystemdManager) CloseGPUProcess(ctx context.Context, workload control.Workload, pid int) error {
	p, ok := m.config.Catalog.Profile(workload)
	if !ok {
		return errors.New("unconfigured workload")
	}
	if pid <= 0 {
		return errors.New("invalid PID")
	}
	target, err := m.findGPUProcess(ctx, p, pid)
	if err != nil {
		return err
	}
	if target == nil {
		// The process ended before Close, or a supervised unit owns it.
		return m.checkProfileGPUFree(ctx, p)
	}
	if !target.Closable {
		return ErrGPUProcessNotOwned
	}
	handle, err := m.gpuHost.open(pid)
	if errors.Is(err, unix.ESRCH) {
		return m.checkProfileGPUFree(ctx, p)
	}
	if err != nil {
		return fmt.Errorf("pin GPU process %d: %w", pid, err)
	}
	defer handle.Close()
	// The pidfd now names one process. List it again: an exited process
	// gives ESRCH on the signal, so a listed PID is the pinned process.
	again, err := m.findGPUProcess(ctx, p, pid)
	if err != nil {
		return err
	}
	if again == nil {
		return m.checkProfileGPUFree(ctx, p)
	}
	if !again.Closable || again.Name != target.Name {
		return ErrGPUProcessNotOwned
	}
	if err := handle.Terminate(); err != nil && !errors.Is(err, unix.ESRCH) {
		return fmt.Errorf("send SIGTERM to GPU process %d: %w", pid, err)
	}
	if _, err := handle.WaitExit(m.gpuHost.grace); err != nil {
		return fmt.Errorf("wait for GPU process %d: %w", pid, err)
	}
	return m.checkProfileGPUFree(ctx, p)
}

// findGPUProcess returns the unsupervised process pid on the GPU of p, or
// nil when it is not listed.
func (m *SystemdManager) findGPUProcess(ctx context.Context, p control.WorkloadProfile, pid int) (*GPUProcess, error) {
	_, found, err := m.unsupervisedGPUProcesses(ctx, p)
	if err != nil {
		return nil, err
	}
	for i := range found {
		if found[i].PID == pid {
			return &found[i], nil
		}
	}
	return nil, nil
}

// gpuIdle requires no compute process at all on the GPU of p. The release
// checks use it: they prove release, so supervised processes count too.
func (m *SystemdManager) gpuIdle(ctx context.Context, p control.WorkloadProfile) error {
	apps, err := m.computeApps(ctx, m.gpuDevice(p))
	if err != nil {
		return err
	}
	if len(apps) != 0 {
		return errors.New("GPU compute processes remain")
	}
	return nil
}

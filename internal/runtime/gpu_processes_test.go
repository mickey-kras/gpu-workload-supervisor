package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"golang.org/x/sys/unix"
)

const testUID int = 1000
const otherUID int = 1001

// gpuProcFixture is a procfs tree with the cgroup and user IDs of each PID.
type gpuProcFixture struct {
	t    *testing.T
	root string
}

func (f gpuProcFixture) add(pid int, group string, uid int) {
	f.t.Helper()
	dir := filepath.Join(f.root, strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		f.t.Fatal(err)
	}
	id := strconv.Itoa(uid)
	files := map[string]string{
		"cgroup": "0::" + group + "\n",
		"status": "Name:\tx\nUid:\t" + id + "\t" + id + "\t" + id + "\t" + id + "\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			f.t.Fatal(err)
		}
	}
}

func (f gpuProcFixture) remove(pid int) {
	f.t.Helper()
	if err := os.RemoveAll(filepath.Join(f.root, strconv.Itoa(pid))); err != nil {
		f.t.Fatal(err)
	}
}

// fakePidHandle records the signal and can report an exited process.
type fakePidHandle struct {
	terminated int
	termErr    error
	onTerm     func()
	closed     bool
}

func (h *fakePidHandle) Terminate() error {
	h.terminated++
	if h.onTerm != nil {
		h.onTerm()
	}
	return h.termErr
}
func (h *fakePidHandle) WaitExit(time.Duration) (bool, error) { return true, nil }
func (h *fakePidHandle) Close() error                         { h.closed = true; return nil }

func gpuCheckManager(t *testing.T, apps string) (*SystemdManager, *fakeRunner, gpuProcFixture, *fakePidHandle, *[]int) {
	t.Helper()
	r := stoppedRunner()
	r.outputs[computeAppsCommand] = []byte(apps)
	m := strictManager(t, r)
	f := gpuProcFixture{t: t, root: t.TempDir()}
	handle := &fakePidHandle{}
	var opened []int
	m.gpuHost = gpuProcessHost{proc: f.root, currentUID: func() int { return testUID },
		userName: func(uid int) string {
			return map[int]string{testUID: "alice", otherUID: "bob", 0: "root"}[uid]
		},
		open: func(pid int) (pidHandle, error) {
			opened = append(opened, pid)
			return handle, nil
		}, timeout: time.Second, grace: time.Millisecond}
	return m, r, f, handle, &opened
}

func requireInUse(t *testing.T, err error) *GPUInUseError {
	t.Helper()
	var inUse *GPUInUseError
	if !errors.As(err, &inUse) || !errors.Is(err, ErrGPUInUse) {
		t.Fatalf("GPU check = %v, want GPU in use", err)
	}
	return inUse
}

func TestGPUCheckNoOtherProcess(t *testing.T) {
	m, _, _, _, _ := gpuCheckManager(t, "")
	if err := m.CheckGPUFree(context.Background(), control.WorkloadText); err != nil {
		t.Fatalf("free GPU = %v", err)
	}
	if err := m.Start(context.Background(), control.WorkloadText); err != nil {
		t.Fatalf("start on a free GPU = %v", err)
	}
}

func TestGPUCheckProcessOfYourUserBlocksStart(t *testing.T) {
	m, r, f, _, _ := gpuCheckManager(t, "4242, /usr/bin/python3, 2048\n")
	f.add(4242, "/user.slice/user-1000.slice/session-2.scope", testUID)
	err := m.Start(context.Background(), control.WorkloadText)
	inUse := requireInUse(t, err)
	got := inUse.Processes
	if len(got) != 1 || got[0] != (GPUProcess{PID: 4242, Name: "/usr/bin/python3", UsedMiB: 2048, MemoryKnown: true, UID: testUID, User: "alice", Closable: true}) {
		t.Fatalf("processes = %+v", got)
	}
	if inUse.Device != "0" || !strings.Contains(err.Error(), `"/usr/bin/python3" (PID 4242, alice, 2048 MiB)`) {
		t.Fatalf("message = %v", err)
	}
	for _, call := range r.calls {
		if strings.Contains(call, " start ") {
			t.Fatalf("blocked start ran %q", call)
		}
	}
}

func TestGPUCheckProcessOfAnotherUserCannotBeClosed(t *testing.T) {
	for name, uid := range map[string]int{"other user": otherUID, "root": 0} {
		t.Run(name, func(t *testing.T) {
			m, _, f, handle, opened := gpuCheckManager(t, "77, blender, [N/A]\n")
			f.add(77, "/user.slice/user-1001.slice/session-3.scope", uid)
			got := requireInUse(t, m.CheckGPUFree(context.Background(), control.WorkloadText)).Processes
			if len(got) != 1 || got[0].Closable || got[0].MemoryKnown || got[0].UID != uid {
				t.Fatalf("processes = %+v", got)
			}
			if err := m.CloseGPUProcess(context.Background(), control.WorkloadText, 77); !errors.Is(err, ErrGPUProcessNotOwned) {
				t.Fatalf("close = %v", err)
			}
			if len(*opened) != 0 || handle.terminated != 0 {
				t.Fatalf("process of %s touched", name)
			}
		})
	}
}

func TestGPUCheckRootUserClosesNothing(t *testing.T) {
	m, _, f, handle, _ := gpuCheckManager(t, "77, tool, 10\n")
	m.gpuHost.currentUID = func() int { return 0 }
	f.add(77, "/system.slice/tool.service", 0)
	if got := requireInUse(t, m.CheckGPUFree(context.Background(), control.WorkloadText)).Processes; got[0].Closable {
		t.Fatalf("root process closable: %+v", got)
	}
	if err := m.CloseGPUProcess(context.Background(), control.WorkloadText, 77); !errors.Is(err, ErrGPUProcessNotOwned) || handle.terminated != 0 {
		t.Fatalf("close as root = %v", err)
	}
}

func TestGPUCheckUnreadableProcessBlocksWithUnknownUser(t *testing.T) {
	m, _, _, _, _ := gpuCheckManager(t, "99, hidden, 5\n")
	got := requireInUse(t, m.CheckGPUFree(context.Background(), control.WorkloadText)).Processes
	if len(got) != 1 || got[0].User != "" || got[0].UID != -1 || got[0].Closable {
		t.Fatalf("processes = %+v", got)
	}
}

func TestGPUCheckQueryFailureBlocks(t *testing.T) {
	for name, tc := range map[string]struct {
		err    error
		output string
		reason string
	}{
		"failure":     {err: errors.New("NVML: driver not loaded"), reason: "the nvidia-smi process query failed"},
		"timeout":     {err: context.DeadlineExceeded, reason: "the nvidia-smi process query timed out"},
		"invalid pid": {output: "abc, x, 1\n", reason: "the nvidia-smi process query printed an invalid PID"},
		"bad row":     {output: "123\n", reason: "the nvidia-smi process query printed an unexpected row"},
	} {
		t.Run(name, func(t *testing.T) {
			m, r, _, _, _ := gpuCheckManager(t, tc.output)
			if tc.err != nil {
				r.errs = map[string]error{computeAppsCommand: tc.err}
			}
			err := m.Start(context.Background(), control.WorkloadText)
			var failed *GPUCheckError
			if !errors.As(err, &failed) || !errors.Is(err, ErrGPUCheckFailed) || failed.Reason != tc.reason || errors.Is(err, ErrGPUInUse) {
				t.Fatalf("start = %v", err)
			}
			if strings.Contains(err.Error(), "NVML") {
				t.Fatalf("command output rendered: %v", err)
			}
		})
	}
}

// hangingQueryRunner hangs on the compute process query until its context ends.
type hangingQueryRunner struct{ fakeRunner }

func (r *hangingQueryRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if len(args) > 0 && strings.HasPrefix(args[0], "--query-compute-apps") {
		<-ctx.Done()
		return nil, errors.Join(errors.New("signal: killed"), ctx.Err())
	}
	return r.fakeRunner.Run(ctx, name, args...)
}

func TestGPUCheckQueryTimeoutBlocks(t *testing.T) {
	r := &hangingQueryRunner{fakeRunner: *stoppedRunner()}
	m := strictManager(t, r)
	m.gpuHost.timeout = 10 * time.Millisecond
	err := m.CheckGPUFree(context.Background(), control.WorkloadText)
	var failed *GPUCheckError
	if !errors.As(err, &failed) || failed.Reason != "the nvidia-smi process query timed out" || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("hung query = %v", err)
	}
}

func TestGPUCheckWithoutProbeBlocks(t *testing.T) {
	m, _, _, _, _ := gpuCheckManager(t, "")
	m.config.NvidiaSMIPath = ""
	if err := m.CheckGPUFree(context.Background(), control.WorkloadText); !errors.Is(err, ErrGPUCheckFailed) {
		t.Fatalf("no probe = %v", err)
	}
}

func TestGPUCheckIgnoresSupervisedProcesses(t *testing.T) {
	m, _, f, _, _ := gpuCheckManager(t, "10, vllm, 9000\n11, comfy, 3000\n12, worker, 100\n")
	f.add(10, "/workloads/text.service", testUID)
	f.add(11, "/workloads/media.service/child", testUID)
	// A prefix that is not a path boundary is not supervised.
	f.add(12, "/workloads/text.service-other", testUID)
	got := requireInUse(t, m.CheckGPUFree(context.Background(), control.WorkloadText)).Processes
	if len(got) != 1 || got[0].PID != 12 {
		t.Fatalf("processes = %+v", got)
	}
	f.add(12, "/workloads/text.service/helper", testUID)
	if err := m.CheckGPUFree(context.Background(), control.WorkloadText); err != nil {
		t.Fatalf("supervised processes blocked: %v", err)
	}
}

func TestGPUCheckCgroupReadFailureBlocks(t *testing.T) {
	m, _, f, _, _ := gpuCheckManager(t, "10, x, 1\n")
	f.add(10, "/x", testUID)
	if err := os.WriteFile(filepath.Join(f.root, "10", "cgroup"), []byte("1:cpu:/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.CheckGPUFree(context.Background(), control.WorkloadText); !errors.Is(err, ErrGPUCheckFailed) {
		t.Fatalf("unreadable cgroup = %v", err)
	}
}

func TestGPUCheckUsesProfileGPU(t *testing.T) {
	uuid := "GPU-11111111-2222-3333-4444-555555555555"
	m, r, _, _, _ := gpuCheckManager(t, "")
	r.outputs["/usr/bin/true --query-compute-apps=pid,process_name,used_memory --format=csv,noheader,nounits -i "+uuid] = []byte("5, x, 1\n")
	p := control.WorkloadProfile{ID: "vision", NativeModel: &control.NativeModel{GPUUUID: uuid}}
	if err := m.checkProfileGPUFree(context.Background(), p); requireInUse(t, err).Device != uuid {
		t.Fatalf("device = %v", err)
	}
}

func TestGPUCheckBlocksTrainerStart(t *testing.T) {
	m, r, p, job, command := trainerContractFixture(t)
	r.outputs[command] = []byte(strings.ReplaceAll(strings.ReplaceAll(string(r.outputs[command]), "ActiveState=active", "ActiveState=inactive"), "SubState=exited", "SubState=dead"))
	r.outputs["/usr/bin/true --user show --property=LoadState --property=ActiveState --property=SubState --property=ControlGroup -- training.service"] = stoppedOutput()
	r.outputs["/usr/bin/true --query-compute-apps=pid,process_name,used_memory --format=csv,noheader,nounits -i "+trainerContractGPU] = []byte("31, game, 4000\n")
	m.gpuHost.proc = t.TempDir()
	id, err := m.StartTraining(context.Background(), p, job)
	if requireInUse(t, err); id != "" {
		t.Fatalf("invocation = %q", id)
	}
	for _, call := range r.calls {
		if strings.Contains(call, "start -- training.service") {
			t.Fatal("blocked trainer started")
		}
	}
}

func TestCloseGPUProcessSendsSIGTERMAndChecksAgain(t *testing.T) {
	m, r, f, handle, opened := gpuCheckManager(t, "4242, python3, 2048\n")
	f.add(4242, "/user.slice/session-2.scope", testUID)
	handle.onTerm = func() {
		r.mu.Lock()
		r.outputs[computeAppsCommand] = nil
		r.mu.Unlock()
	}
	if err := m.CloseGPUProcess(context.Background(), control.WorkloadText, 4242); err != nil {
		t.Fatalf("close = %v", err)
	}
	if handle.terminated != 1 || !handle.closed || len(*opened) != 1 || (*opened)[0] != 4242 {
		t.Fatalf("handle = %+v, opened = %v", handle, *opened)
	}
}

func TestCloseGPUProcessStillRunningReportsInUse(t *testing.T) {
	m, _, f, handle, _ := gpuCheckManager(t, "4242, python3, 2048\n")
	f.add(4242, "/user.slice/session-2.scope", testUID)
	requireInUse(t, m.CloseGPUProcess(context.Background(), control.WorkloadText, 4242))
	if handle.terminated != 1 {
		t.Fatalf("terminations = %d", handle.terminated)
	}
}

func TestCloseGPUProcessEndedBeforeClose(t *testing.T) {
	t.Run("not listed", func(t *testing.T) {
		m, _, _, handle, opened := gpuCheckManager(t, "")
		if err := m.CloseGPUProcess(context.Background(), control.WorkloadText, 4242); err != nil {
			t.Fatalf("close = %v", err)
		}
		if len(*opened) != 0 || handle.terminated != 0 {
			t.Fatal("ended process signalled")
		}
	})
	t.Run("ended before pin", func(t *testing.T) {
		m, r, f, handle, _ := gpuCheckManager(t, "4242, python3, 2048\n")
		f.add(4242, "/user.slice/session-2.scope", testUID)
		m.gpuHost.open = func(int) (pidHandle, error) {
			r.mu.Lock()
			r.outputs[computeAppsCommand] = nil
			r.mu.Unlock()
			return nil, unix.ESRCH
		}
		if err := m.CloseGPUProcess(context.Background(), control.WorkloadText, 4242); err != nil || handle.terminated != 0 {
			t.Fatalf("close = %v", err)
		}
	})
	t.Run("ended after pin", func(t *testing.T) {
		m, r, f, handle, _ := gpuCheckManager(t, "4242, python3, 2048\n")
		f.add(4242, "/user.slice/session-2.scope", testUID)
		open := m.gpuHost.open
		m.gpuHost.open = func(pid int) (pidHandle, error) {
			r.mu.Lock()
			r.outputs[computeAppsCommand] = nil
			r.mu.Unlock()
			f.remove(pid)
			return open(pid)
		}
		if err := m.CloseGPUProcess(context.Background(), control.WorkloadText, 4242); err != nil || handle.terminated != 0 || !handle.closed {
			t.Fatalf("close = %v, handle = %+v", err, handle)
		}
	})
	t.Run("ended at signal", func(t *testing.T) {
		m, r, f, handle, _ := gpuCheckManager(t, "4242, python3, 2048\n")
		f.add(4242, "/user.slice/session-2.scope", testUID)
		handle.termErr = unix.ESRCH
		handle.onTerm = func() {
			r.mu.Lock()
			r.outputs[computeAppsCommand] = nil
			r.mu.Unlock()
		}
		if err := m.CloseGPUProcess(context.Background(), control.WorkloadText, 4242); err != nil {
			t.Fatalf("close = %v", err)
		}
	})
}

// A PID reused by another program between the listing and the pin is
// never signalled.
func TestCloseGPUProcessRefusesReusedPID(t *testing.T) {
	m, r, f, handle, _ := gpuCheckManager(t, "4242, python3, 2048\n")
	f.add(4242, "/user.slice/session-2.scope", testUID)
	open := m.gpuHost.open
	m.gpuHost.open = func(pid int) (pidHandle, error) {
		r.mu.Lock()
		r.outputs[computeAppsCommand] = []byte("4242, other, 10\n")
		r.mu.Unlock()
		return open(pid)
	}
	if err := m.CloseGPUProcess(context.Background(), control.WorkloadText, 4242); !errors.Is(err, ErrGPUProcessNotOwned) || handle.terminated != 0 {
		t.Fatalf("reused PID = %v", err)
	}
}

func TestCloseGPUProcessQueryFailureIsShown(t *testing.T) {
	m, r, _, handle, _ := gpuCheckManager(t, "")
	r.errs = map[string]error{computeAppsCommand: errors.New("driver gone")}
	if err := m.CloseGPUProcess(context.Background(), control.WorkloadText, 4242); !errors.Is(err, ErrGPUCheckFailed) || handle.terminated != 0 {
		t.Fatalf("close = %v", err)
	}
}

func TestCloseGPUProcessRejectsInvalidInput(t *testing.T) {
	m, _, _, _, _ := gpuCheckManager(t, "")
	if err := m.CloseGPUProcess(context.Background(), control.WorkloadText, 0); err == nil {
		t.Fatal("PID 0 accepted")
	}
	if err := m.CloseGPUProcess(context.Background(), "vision", 5); err == nil {
		t.Fatal("unconfigured workload accepted")
	}
	if err := m.CheckGPUFree(context.Background(), "vision"); err == nil {
		t.Fatal("unconfigured workload checked")
	}
}

// The real pidfd handle signals and waits for a child process.
func TestPidfdHandleTerminatesChild(t *testing.T) {
	child, err := os.StartProcess("/bin/sleep", []string{"sleep", "30"}, &os.ProcAttr{})
	if err != nil {
		t.Skip("no sleep binary:", err)
	}
	done := make(chan struct{})
	go func() { _, _ = child.Wait(); close(done) }()
	handle, err := openPidfd(child.Pid)
	if err != nil {
		_ = child.Kill()
		t.Skip("pidfd unsupported:", err)
	}
	defer handle.Close()
	if exited, err := handle.WaitExit(time.Millisecond); exited || err != nil {
		t.Fatalf("running child: exited = %v, %v", exited, err)
	}
	if err := handle.Terminate(); err != nil {
		t.Fatal(err)
	}
	if exited, err := handle.WaitExit(5 * time.Second); !exited || err != nil {
		t.Fatalf("terminated child: exited = %v, %v", exited, err)
	}
	<-done
}

func TestLookupUserNameFallsBackToUID(t *testing.T) {
	if got := lookupUserName(2147483000); got != "2147483000" {
		t.Fatalf("unknown uid = %q", got)
	}
}

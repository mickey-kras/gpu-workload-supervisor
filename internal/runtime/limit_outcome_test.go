package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

const textOutcomeCommand = "/usr/bin/true --user show --property=ActiveState,Result,ExecMainCode,ExecMainStatus -- text.service"
const computeAppsCommand = "/usr/bin/true --query-compute-apps=pid --format=csv,noheader,nounits -i 0"

func writeCounters(t *testing.T, root string, files map[string]string) {
	t.Helper()
	dir := filepath.Join(root, "text.service")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, value := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// Each class is a distinct result. A kill comes first. A memory.max event
// is a throttle while the unit runs and an allocation failure once it
// exited with a failure.
func TestLimitOutcomeClasses(t *testing.T) {
	running := "ActiveState=active\nResult=success\nExecMainCode=0\nExecMainStatus=0\n"
	quiet := map[string]string{"memory.events": "low 0\nhigh 0\nmax 0\noom 0\noom_kill 0\n", "pids.events": "max 0\n", "cpu.stat": "usage_usec 9\nnr_throttled 0\n"}
	with := func(name, value string) map[string]string {
		files := map[string]string{}
		for k, v := range quiet {
			files[k] = v
		}
		files[name] = value
		return files
	}
	for name, tc := range map[string]struct {
		files map[string]string
		show  string
		want  string
	}{
		"none":             {quiet, running, LimitOutcomeNone},
		"memory high":      {with("memory.events", "high 3\nmax 0\noom_kill 0\n"), running, LimitOutcomeThrottle},
		"memory max":       {with("memory.events", "high 0\nmax 2\noom_kill 0\n"), running, LimitOutcomeThrottle},
		"cpu quota":        {with("cpu.stat", "nr_throttled 5\n"), running, LimitOutcomeThrottle},
		"tasks":            {with("pids.events", "max 1\n"), running, LimitOutcomeTaskCreation},
		"oom kill counter": {with("memory.events", "max 4\noom_kill 1\n"), running, LimitOutcomeKill},
		"oom kill result":  {quiet, "ActiveState=failed\nResult=oom-kill\nExecMainCode=2\nExecMainStatus=9\n", LimitOutcomeKill},
		"allocation":       {with("memory.events", "max 4\noom_kill 0\n"), "ActiveState=active\nResult=success\nExecMainCode=1\nExecMainStatus=1\n", LimitOutcomeAllocation},
		"clean exit":       {with("memory.events", "max 4\noom_kill 0\n"), "ActiveState=active\nResult=success\nExecMainCode=1\nExecMainStatus=0\n", LimitOutcomeThrottle},
	} {
		r := &fakeRunner{outputs: map[string][]byte{textOutcomeCommand: []byte(tc.show)}}
		m := strictManager(t, r)
		writeCounters(t, filepath.Join(m.cgroups.root, "workloads"), tc.files)
		got := m.LimitOutcome(context.Background(), control.WorkloadText)
		if got.Class != tc.want || strings.Contains(got.Detail, "unread") {
			t.Errorf("%s: outcome = %s", name, got)
		}
	}
}

// A counter or result that cannot be read is named in the detail and
// gives no class.
func TestLimitOutcomeNamesUnreadCounters(t *testing.T) {
	r := &fakeRunner{errs: map[string]error{textOutcomeCommand: errors.New("bus down")}}
	m := strictManager(t, r)
	writeCounters(t, filepath.Join(m.cgroups.root, "workloads"), map[string]string{"memory.events": "garbage\n", "cpu.stat": "nr_throttled x\n"})
	got := m.LimitOutcome(context.Background(), control.WorkloadText)
	if got.Class != LimitOutcomeNone || !strings.Contains(got.String(), "unread memory.events, pids.events, cpu.stat, systemd result") {
		t.Fatalf("outcome = %s", got)
	}
	if got := m.LimitOutcome(context.Background(), "vision"); got.Class != LimitOutcomeNone || got.String() != "none (unconfigured workload)" {
		t.Fatalf("unconfigured outcome = %s", got)
	}
	if got := (LimitOutcome{Class: LimitOutcomeKill}).String(); got != "kill" {
		t.Fatalf("bare outcome = %s", got)
	}
}

// The release check after a limit stop needs stopped units with empty
// cgroups, no compute process on the GPU and the capacity of the unit.
func TestLimitStopReleasedChecksGPUAndCapacity(t *testing.T) {
	ctx := context.Background()
	setup := func(apps string, appsErr error) (*SystemdManager, string) {
		r := stoppedRunner()
		r.outputs[computeAppsCommand] = []byte(apps)
		r.outputs[gpuFreeCommand] = []byte("1000\n")
		r.errs = map[string]error{computeAppsCommand: appsErr}
		m := strictManager(t, r)
		root := filepath.Join(m.cgroups.root)
		writeEvents(t, root, "workloads/text.service", "populated 0\n")
		writeEvents(t, root, "workloads/media.service", "populated 0\n")
		return m, root
	}
	m, root := setup("", nil)
	if err := m.LimitStopReleased(ctx, control.WorkloadText); err != nil {
		t.Fatalf("released = %v", err)
	}
	m.config.Catalog.Profiles[0].RequiredMiB = 4000
	if err := m.LimitStopReleased(ctx, control.WorkloadText); !errors.Is(err, ErrCapacity) {
		t.Fatalf("no capacity = %v", err)
	}
	writeEvents(t, root, "workloads/text.service", "populated 1\n")
	if err := m.LimitStopReleased(ctx, control.WorkloadText); err == nil || errors.Is(err, ErrLimitStopUnreleased) {
		t.Fatalf("populated cgroup = %v", err)
	}
	for name, m := range map[string]*SystemdManager{"process": first(setup("123\n", nil)), "probe": first(setup("", errors.New("no driver")))} {
		if err := m.LimitStopReleased(ctx, control.WorkloadText); !errors.Is(err, ErrLimitStopUnreleased) {
			t.Errorf("%s: released = %v", name, err)
		}
	}
	m, _ = setup("", nil)
	m.config.NvidiaSMIPath = ""
	if err := m.LimitStopReleased(ctx, control.WorkloadText); !errors.Is(err, ErrLimitStopUnreleased) {
		t.Fatalf("no GPU probe = %v", err)
	}
	if err := m.LimitStopReleased(ctx, "vision"); err == nil {
		t.Fatal("unconfigured workload passed")
	}
}

func first(m *SystemdManager, _ string) *SystemdManager { return m }

// The GPU check uses the GPU UUID of the profile when it has one.
func TestLimitStopGPUCheckUsesProfileGPU(t *testing.T) {
	uuid := "GPU-11111111-2222-3333-4444-555555555555"
	r := &fakeRunner{outputs: map[string][]byte{
		"/usr/bin/true --query-compute-apps=pid --format=csv,noheader,nounits -i " + uuid: []byte("42\n"),
		computeAppsCommand: []byte(""),
	}}
	m := strictManager(t, r)
	p := control.WorkloadProfile{ID: "vision", NativeModel: &control.NativeModel{GPUUUID: uuid}}
	if err := m.gpuReleased(context.Background(), p); !errors.Is(err, ErrLimitStopUnreleased) {
		t.Fatalf("GPU of the profile = %v", err)
	}
	if err := m.gpuReleased(context.Background(), control.WorkloadProfile{ID: "text"}); err != nil {
		t.Fatalf("configured GPU index = %v", err)
	}
}

// A failed exit with a memory.max event is an allocation failure, also for
// a signal. A kill comes before it, and it comes before a task failure.
func TestLimitOutcomeFailedExits(t *testing.T) {
	for name, tc := range map[string]struct {
		memory, pids, show, want string
	}{
		"failed exit code": {"max 2\noom_kill 0\n", "max 0\n", "ActiveState=failed\nResult=exit-code\nExecMainCode=1\nExecMainStatus=1\n", LimitOutcomeAllocation},
		"failed state":     {"max 2\noom_kill 0\n", "max 0\n", "ActiveState=failed\nResult=exit-code\nExecMainCode=0\nExecMainStatus=0\n", LimitOutcomeAllocation},
		"signal":           {"max 2\noom_kill 0\n", "max 0\n", "ActiveState=inactive\nResult=signal\nExecMainCode=2\nExecMainStatus=9\n", LimitOutcomeAllocation},
		"before tasks":     {"max 2\noom_kill 0\n", "max 3\n", "ActiveState=failed\nResult=exit-code\nExecMainCode=1\nExecMainStatus=1\n", LimitOutcomeAllocation},
		"kill first":       {"max 2\noom_kill 1\n", "max 3\n", "ActiveState=failed\nResult=oom-kill\nExecMainCode=2\nExecMainStatus=9\n", LimitOutcomeKill},
	} {
		r := &fakeRunner{outputs: map[string][]byte{textOutcomeCommand: []byte(tc.show)}}
		m := strictManager(t, r)
		writeCounters(t, filepath.Join(m.cgroups.root, "workloads"), map[string]string{"memory.events": tc.memory, "pids.events": tc.pids, "cpu.stat": "nr_throttled 0\n"})
		if got := m.LimitOutcome(context.Background(), control.WorkloadText); got.Class != tc.want {
			t.Errorf("%s: outcome = %s", name, got)
		}
	}
}

// The release check after a limit stop fails while a peer unit has failed.
func TestLimitStopReleasedRejectsFailedPeer(t *testing.T) {
	r := stoppedRunner()
	r.outputs[mediaShowCommand] = []byte("LoadState=loaded\nActiveState=failed\nSubState=failed\nControlGroup=\n")
	r.outputs[computeAppsCommand] = []byte("")
	m := strictManager(t, r)
	writeEvents(t, m.cgroups.root, "workloads/text.service", "populated 0\n")
	writeEvents(t, m.cgroups.root, "workloads/media.service", "populated 0\n")
	if err := m.LimitStopReleased(context.Background(), control.WorkloadText); err == nil || !strings.Contains(err.Error(), "not stopped") {
		t.Fatalf("release with a failed peer = %v", err)
	}
}

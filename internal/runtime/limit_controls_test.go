package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"golang.org/x/sys/unix"
)

// A finite slice cpu.max that does not convert to a whole per-second quota
// comes from a non-default CPUQuotaPeriodSec. The refusal names that cause
// and its remedy, not a missing cap.
func TestInexactSliceCPUQuotaNamesThePeriod(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	f.cgroup(t, limitSlice, "cpu.max", "2333 7000\n")
	_, err := f.m.readSliceCPU(limitSlice)
	if !errors.Is(err, ErrHostCap) || strings.Contains(err.Error(), "not a finite quota") || !strings.Contains(err.Error(), "remove CPUQuotaPeriodSec from the slice drop-in") {
		t.Fatalf("slice cpu.max with a non-default period = %v", err)
	}
	f.cgroup(t, limitSlice, "cpu.max", "max 100000\n")
	if _, err := f.m.readSliceCPU(limitSlice); !errors.Is(err, ErrHostCap) || !strings.Contains(err.Error(), "not a finite quota") {
		t.Fatalf("slice cpu.max without a quota = %v", err)
	}
}

// The launch checks need the pids and cpu controllers at the user manager
// root, also for a unit with a CPU quota and no CPU weight.
func TestPidsAndCPUControllersAreRequired(t *testing.T) {
	noWeight := fixtureLimits()
	noWeight.CPUWeight = 0
	loaded := strings.Replace(fixtureLoaded, "CPUWeight=200", "CPUWeight=[not set]", 1)
	for name, c := range map[string]struct {
		controllers string
		want        string
	}{
		"pids not delegated": {"cpu io memory\n", "pids is absent from"},
		"cpu not delegated":  {"io memory pids\n", "cpu is absent from"},
	} {
		f := newLimitFixture(t, noWeight, loaded)
		f.cgroup(t, f.p.Cgroup, "cpu.weight", "100\n")
		f.cgroup(t, limitManagerRoot, "cgroup.controllers", c.controllers)
		err := f.start(t)
		if !errors.Is(err, ErrLimitController) || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "to Delegate= in /etc/systemd/system/user@1000.service.d/50-gws-delegate-io.conf") || f.started() {
			t.Errorf("%s: start = %v", name, err)
		}
	}
	// The same unit starts with every controller.
	f := newLimitFixture(t, noWeight, loaded)
	f.cgroup(t, f.p.Cgroup, "cpu.weight", "100\n")
	if err := f.start(t); err != nil || !f.started() {
		t.Fatalf("start of a unit without CPUWeight = %v", err)
	}
}

// A partition has a partition file and no request queue, so it gets the
// partition error.
func TestPartitionDeviceIsNamed(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	if err := os.RemoveAll(filepath.Join(f.host, "sys/dev/block/259:0/queue")); err != nil {
		t.Fatal(err)
	}
	f.file(t, "/sys/dev/block/259:0/partition", "1\n")
	if _, err := f.m.verifyLimitDevice(fixtureLimits()); !errors.Is(err, ErrLimitDevice) || !strings.Contains(err.Error(), "/dev/nvme0n1 is a partition; set ioDevice and ioDeviceNumber to the whole leaf disk under it (see `lsblk -s`), or remove the limits through setup") {
		t.Fatalf("partition device = %v", err)
	}
}

// A refused symlink or a file where a directory must be stays a definite
// binding failure.
func TestLinkAndNotDirectoryStayDefinite(t *testing.T) {
	for _, errno := range []unix.Errno{unix.ELOOP, unix.ENOTDIR} {
		cause := &os.PathError{Op: "open", Path: "x", Err: errno}
		if bindingReadFailure(cause) || !DefiniteBindingFailure(markBindingRead(ErrLaunchUnsupported, cause)) {
			t.Errorf("%v is not a definite binding failure", errno)
		}
	}
}

// At the schema minimum, the refusal still suggests the minimum value.
func TestReserveSuggestionAtTheMinimum(t *testing.T) {
	c := hostCap{memoryHigh: (control.MinLimitMemoryMiB + DefaultLimitReserve.MemoryMiB) * mebibyte, memoryMax: (control.MinLimitMemoryMiB + DefaultLimitReserve.MemoryMiB) * mebibyte,
		tasks: 10000, cpu: 8000000, io: map[string]uint64{"riops": DefaultLimitReserve.ioCeilingFor(control.MinLimitIOPS)}}
	err := checkReserve(control.ResourceLimits{Version: 1, MemoryMaxMiB: 1024, TasksMax: 64, CPUQuotaPercent: 100}, c, DefaultLimitReserve)
	for _, part := range []string{"use 256 MiB or less for memoryMaxMiB", "set ioReadIOPSMax to 100 operations per second or less"} {
		if !errors.Is(err, ErrLimitReserve) || !strings.Contains(err.Error(), part) {
			t.Fatalf("reserve error = %v, missing %q", err, part)
		}
	}
}

// A missing cgroup hierarchy proves nothing about a control file of the
// user manager root.
func TestManagerFilesWithoutTheHierarchy(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	f.m.cgroups.root = filepath.Join(f.cgroups, "missing")
	if err := f.m.requireManagerFiles(limitManagerRoot, fixtureLimits()); !errors.Is(err, ErrLimitReadback) || errors.Is(err, ErrLimitInterface) {
		t.Fatalf("manager files without the hierarchy = %v", err)
	}
	limitCap := hostCap{slice: limitSlice, io: map[string]uint64{}}
	if err := f.m.readSliceIO(&limitCap, control.ResourceLimits{Version: 1, MemoryMaxMiB: 1024}); err == nil || errors.Is(err, ErrHostCap) {
		t.Fatalf("slice io.max without the hierarchy = %v", err)
	}
}

// The other-device refusal names the ioDevice path of the unit, and the
// second read of a changed slice cap gives the daemon-reload command.
func TestOtherDeviceAndReloadTexts(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	f.cgroup(t, limitSlice, "io.max", "259:0 rbps=1000000000 wbps=max riops=max wiops=max\n8:0 rbps=1048576 wbps=max riops=max wiops=max\n")
	if err := f.m.VerifyLaunchLimits(context.Background(), f.p.ID); !strings.Contains(err.Error(), "caps only its one ioDevice (/dev/nvme0n1)") {
		t.Fatalf("other device refusal = %v", err)
	}
	f = newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	f.edit(f.sliceShow(), "TasksMax=16384", "TasksMax=9")
	f.r.after = map[string]func(){f.sliceShow(): func() {
		f.r.outputs[f.sliceShow()] = []byte(strings.Replace(string(f.r.outputs[f.sliceShow()]), "NeedDaemonReload=no", "NeedDaemonReload=yes", 1))
	}}
	if err := f.m.VerifyLaunchLimits(context.Background(), f.p.ID); !strings.Contains(err.Error(), "run `sudo systemctl daemon-reload` for the system manager") {
		t.Fatalf("slice cap changed with a reload need = %v", err)
	}
}

// With a CPU quota, the readback also needs cpu.max.burst 0, so a burst
// that the delegated unit cgroup sets counts as drift. A kernel without
// burst has no such file.
func TestCPUBurstReadback(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	if err := f.m.readbackLimits(f.p.Cgroup, fixtureLimits(), "iocost"); err != nil {
		t.Fatalf("readback without cpu.max.burst = %v", err)
	}
	f.cgroup(t, f.p.Cgroup, "cpu.max.burst", "0\n")
	if err := f.m.readbackLimits(f.p.Cgroup, fixtureLimits(), "iocost"); err != nil {
		t.Fatalf("readback with no burst = %v", err)
	}
	f.cgroup(t, f.p.Cgroup, "cpu.max.burst", "100000\n")
	if err := f.m.readbackLimits(f.p.Cgroup, fixtureLimits(), "iocost"); !errors.Is(err, ErrLimitDrift) || !strings.Contains(err.Error(), "cpu.max.burst") {
		t.Fatalf("readback with a burst = %v", err)
	}
}

// A failed status read of the resolved leaf proves nothing, and a missing
// leaf stays definite.
func TestResolvedLeafReadMark(t *testing.T) {
	err := checkResolvedLeaf("/"+strings.Repeat("r", 300), kindFile)
	if !errors.Is(err, errBindingRead) || DefiniteBindingFailure(markBindingRead(ErrLaunchUnsupported, err)) {
		t.Fatalf("leaf with a failed status read = %v", err)
	}
	if err := checkResolvedLeaf(filepath.Join(t.TempDir(), "missing"), kindFile); errors.Is(err, errBindingRead) {
		t.Fatalf("missing leaf = %v", err)
	}
}

// A missing or empty NeedDaemonReload is an output problem, not a reload
// need: the readback failure says so and gives no daemon-reload remedy.
func TestMissingNeedDaemonReloadIsNamed(t *testing.T) {
	vendor := "/usr/lib/systemd/system/user-.slice.d/10-defaults.conf"
	for _, value := range []string{"", "NeedDaemonReload=\n", "NeedDaemonReload=no\nNeedDaemonReload=no\n"} {
		for name, edit := range map[string]func(*limitFixture){
			"slice":   func(f *limitFixture) { f.edit(f.sliceShow(), "NeedDaemonReload=no\n", value) },
			"manager": func(f *limitFixture) { f.edit(f.managerShow(), "NeedDaemonReload=no\n", value) },
			"reread": func(f *limitFixture) {
				remove(t, f.host, vendor)
				f.edit(f.dropInReread("user-1000.slice"), "NeedDaemonReload=no\n", value)
			},
		} {
			f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
			edit(f)
			err := f.m.VerifyLaunchLimits(context.Background(), f.p.ID)
			if !errors.Is(err, ErrLimitReadback) || !strings.Contains(err.Error(), "NeedDaemonReload is missing or ambiguous") || strings.Contains(err.Error(), "daemon-reload`") {
				t.Errorf("%s with %q = %v", name, value, err)
			}
		}
	}
}

// Host cap refusals name the setting to fix and where.
func TestHostCapRefusalsNameTheSetting(t *testing.T) {
	capDropIn := "/etc/systemd/system/user-1000.slice.d/50-gws-host-cap.conf"
	for name, c := range map[string]struct {
		edit func(*limitFixture)
		want string
	}{
		"memory high unlimited": {func(f *limitFixture) { f.cgroup(t, limitSlice, "memory.high", "max\n") },
			"memory.high is not a finite limit; set MemoryHigh in " + capDropIn + ", then run `sudo systemctl daemon-reload`"},
		"tasks unlimited": {func(f *limitFixture) { f.cgroup(t, limitSlice, "pids.max", "max\n") }, "set TasksMax in " + capDropIn},
		"cpu unlimited":   {func(f *limitFixture) { f.cgroup(t, limitSlice, "cpu.max", "max 100000\n") }, "set CPUQuota in " + capDropIn},
		"swap allowed":    {func(f *limitFixture) { f.cgroup(t, limitSlice, "memory.swap.max", "max\n") }, "set MemorySwapMax=0 in " + capDropIn},
		"no read cap": {func(f *limitFixture) {
			f.cgroup(t, limitSlice, "io.max", "259:0 rbps=max wbps=max riops=max wiops=max\n")
		},
			"io.max has no rbps cap for 259:0 (/dev/nvme0n1), which ioReadBandwidthMax needs; add an IOReadBandwidthMax line for /dev/nvme0n1 to " + capDropIn +
				", or remove the limits through setup"},
		"unknown drop-in": {func(f *limitFixture) {
			f.edit(f.sliceShow(), "DropInPaths=", "DropInPaths=/etc/systemd/system/user-.slice.d/delegate.conf ")
		}, "unknown drop-in /etc/systemd/system/user-.slice.d/delegate.conf; remove it, or move its settings into " + capDropIn + ", then run `sudo systemctl daemon-reload`"},
		"set-property drop-in": {func(f *limitFixture) {
			f.edit(f.sliceShow(), "DropInPaths=", "DropInPaths=/etc/systemd/system.control/user-1000.slice.d/50-MemoryMax.conf ")
		}, "it came from `systemctl set-property`; remove it"},
		"runtime set-property drop-in": {func(f *limitFixture) {
			f.edit(f.sliceShow(), "DropInPaths=", "DropInPaths=/run/systemd/system.control/user-1000.slice.d/50-MemoryMax.conf ")
		}, "it came from `systemctl set-property --runtime` and goes away at reboot; remove it"},
	} {
		f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
		c.edit(f)
		if err := f.m.VerifyLaunchLimits(context.Background(), f.p.ID); !errors.Is(err, ErrHostCap) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s = %v", name, err)
		}
	}
}

// StartedCause takes the cause from the checkFailed tag by value.
func TestStartedCause(t *testing.T) {
	cause := errors.New("cause")
	if got, ok := StartedCause(checkFailed(cause)); !ok || got != cause {
		t.Fatalf("cause of a tagged error = %v, %v", got, ok)
	}
	for _, err := range []error{cause, fmt.Errorf("%w: %w", cause, ErrLimitCheckFailed), UnitError(control.WorkloadProfile{ID: "x", Unit: "x.service"}, checkFailed(cause))} {
		if got, ok := StartedCause(err); ok || got != err {
			t.Errorf("cause of %v = %v, %v", err, got, ok)
		}
	}
}

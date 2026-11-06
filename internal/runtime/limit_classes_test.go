package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"golang.org/x/sys/unix"
)

// An I/O error while the launch file is read proves no change. Only a file
// above the size limit is a definite change.
func TestLaunchSourceReadFailureIsNotDefinite(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	_, err := readLaunchSource(procMemory)
	if !errors.Is(err, ErrLaunchChanged) || !errors.Is(err, errBindingRead) || DefiniteBindingFailure(err) {
		t.Fatalf("launch source with a failed read = %v", err)
	}
	if err := os.WriteFile(f.p.NativeModel.LaunchFile, make([]byte, 1<<20+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readLaunchSource(f.p.NativeModel.LaunchFile); !errors.Is(err, ErrLaunchChanged) || !DefiniteBindingFailure(err) {
		t.Fatalf("launch source above the size limit = %v", err)
	}
}

// A missing /lib, a /lib that is not a link and a link elsewhere are
// definite. Another read failure of /lib proves nothing.
func TestLibLinkClasses(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	f.edit(f.sliceShow(), "DropInPaths=/usr/lib/", "DropInPaths=/lib/")
	lib := filepath.Join(f.host, "lib")
	if err := f.m.VerifyLaunchLimits(context.Background(), f.p.ID); !errors.Is(err, ErrHostCap) || !strings.Contains(err.Error(), "root-owned /lib link") {
		t.Fatalf("launch checks without /lib = %v", err)
	}
	if err := os.Mkdir(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := f.m.VerifyLaunchLimits(context.Background(), f.p.ID); !errors.Is(err, ErrHostCap) || !strings.Contains(err.Error(), "root-owned /lib link") {
		t.Fatalf("launch checks with a /lib directory = %v", err)
	}
	if err := os.Remove(lib); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("usr/lib", lib); err != nil {
		t.Fatal(err)
	}
	f.m.host.lstat = func(name string) (unix.Stat_t, error) {
		if name == lib {
			return unix.Stat_t{}, unix.EIO
		}
		return f.lstat(name)
	}
	err := f.m.VerifyLaunchLimits(context.Background(), f.p.ID)
	if !errors.Is(err, ErrLimitReadback) || DefiniteLimitFailure(err) || !errors.Is(err, unix.EIO) {
		t.Fatalf("launch checks with a /lib read failure = %v", err)
	}
}

// NeedDaemonReload must be exactly "no". A missing, empty or other value
// proves no wrong cap: it is a readback failure, also in the second read of
// a missing drop-in.
func TestDaemonReloadNeedsExactlyNo(t *testing.T) {
	vendor := "/usr/lib/systemd/system/user-.slice.d/10-defaults.conf"
	for _, value := range []string{"", "NeedDaemonReload=\n", "NeedDaemonReload=maybe\n"} {
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
			if !errors.Is(err, ErrLimitReadback) || DefiniteLimitFailure(err) {
				t.Errorf("%s with %q = %v", name, value, err)
			}
		}
	}
}

// An io.max key that a unit cannot cap is no cap: a finite value of an
// unknown key on another device does not refuse the unit.
func TestUnknownIOKeyIsNoCap(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	f.cgroup(t, limitSlice, "io.max", "259:0 rbps=1000000000 wbps=max riops=max wiops=max foo=7\n8:0 rbps=max wbps=max riops=max wiops=max foo=5\n")
	if err := f.m.VerifyLaunchLimits(context.Background(), f.p.ID); err != nil {
		t.Fatalf("launch checks with an unknown io.max key = %v", err)
	}
	if caps, other := ioMaxCaps("8:0 foo=5 rbps=max\n9:0 foo=7\n", "8:0"); len(caps) != 0 || other != "" {
		t.Fatalf("caps of an unknown key = %v", caps)
	}
}

// The kernel keeps memory.high and memory.max in whole pages. A slice value
// in bytes that is not page aligned matches its rounded cgroup value. A real
// mismatch stays definite.
func TestUnalignedSliceMemoryMatchesItsCgroup(t *testing.T) {
	page := uint64(os.Getpagesize())
	for _, name := range []string{"MemoryHigh", "MemoryMax"} {
		f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
		cgroup := map[string]uint64{"MemoryHigh": 61203283968, "MemoryMax": 64424509440}[name]
		old := name + "=" + strconv.FormatUint(cgroup, 10) + "\n"
		f.edit(f.sliceShow(), old, name+"="+strconv.FormatUint(cgroup+page-1, 10)+"\n")
		if err := f.m.VerifyLaunchLimits(context.Background(), f.p.ID); err != nil {
			t.Fatalf("%s rounded down by the kernel = %v", name, err)
		}
		f.edit(f.sliceShow(), name+"="+strconv.FormatUint(cgroup+page-1, 10)+"\n", name+"="+strconv.FormatUint(cgroup+page, 10)+"\n")
		err := f.m.VerifyLaunchLimits(context.Background(), f.p.ID)
		if !errors.Is(err, ErrHostCap) || !strings.Contains(err.Error(), "but its cgroup holds") {
			t.Fatalf("%s one page above its cgroup = %v", name, err)
		}
	}
}

// A slice MemorySwapMax other than 0 is reported against 0, not against a
// cgroup file. With several mismatches, the first one in the fixed order is
// reported.
func TestSlicePropertyMismatchMessages(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	f.edit(f.sliceShow(), "MemorySwapMax=0", "MemorySwapMax=1")
	err := f.m.VerifyLaunchLimits(context.Background(), f.p.ID)
	if !errors.Is(err, ErrHostCap) || !strings.Contains(err.Error(), `MemorySwapMax of user-1000.slice is "1", but must be 0`) {
		t.Fatalf("slice swap mismatch = %v", err)
	}
	f.edit(f.sliceShow(), "TasksMax=16384", "TasksMax=9")
	for range 20 {
		if err := f.m.VerifyLaunchLimits(context.Background(), f.p.ID); !strings.Contains(err.Error(), "MemorySwapMax of") {
			t.Fatalf("first of two mismatches = %v", err)
		}
	}
}

// The I/O reserve of a very large slice cap does not overflow.
func TestIOReserveDoesNotOverflow(t *testing.T) {
	ceiling := ^uint64(0)
	reserve := ceiling / 10
	c := hostCap{memoryHigh: 64 * gib, memoryMax: 64 * gib, tasks: 10000, cpu: 8000000, io: map[string]uint64{"rbps": ceiling}}
	l := control.ResourceLimits{Version: 1, MemoryMaxMiB: 1024, TasksMax: 64, CPUQuotaPercent: 100, IOReadBandwidthMax: ceiling - reserve}
	if err := checkReserve(l, c); err != nil {
		t.Fatalf("I/O cap at the boundary of a large slice cap = %v", err)
	}
	l.IOReadBandwidthMax++
	if err := checkReserve(l, c); !errors.Is(err, ErrLimitReserve) || !strings.Contains(err.Error(), strconv.FormatUint(ceiling-reserve, 10)) {
		t.Fatalf("I/O cap above the boundary of a large slice cap = %v", err)
	}
}

// Each failed device check has its own message. A device number that is not
// the catalog number shows both numbers.
func TestDeviceMismatchMessages(t *testing.T) {
	for name, c := range map[string]struct {
		edit func(*limitFixture)
		want string
	}{
		"not a block device": {func(f *limitFixture) { f.device.Mode = unix.S_IFCHR | 0o660 }, "/dev/nvme0n1 is not a block device"},
		"other owner":        {func(f *limitFixture) { f.device.Uid++ }, "/dev/nvme0n1 is not owned by root"},
		"other number": {func(f *limitFixture) { f.device.Rdev = unix.Mkdev(259, 1) },
			"/dev/nvme0n1 has device number 259:1, not the catalog ioDeviceNumber 259:0; update the limits for the new number"},
	} {
		f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
		c.edit(f)
		if err := f.m.VerifyLaunchLimits(context.Background(), f.p.ID); !errors.Is(err, ErrLimitDevice) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s = %v", name, err)
		}
	}
}

// The reserve errors give the unit of each I/O cap.
func TestIOReserveMessagesGiveTheUnit(t *testing.T) {
	c := hostCap{memoryHigh: 64 * gib, memoryMax: 64 * gib, tasks: 10000, cpu: 8000000, io: map[string]uint64{"rbps": 100 << 20, "wiops": 1000}}
	err := checkReserve(control.ResourceLimits{Version: 1, MemoryMaxMiB: 1024, TasksMax: 64, CPUQuotaPercent: 100, IOWriteIOPSMax: 1000}, c)
	for _, part := range []string{"set ioReadBandwidthMax to 94371840 bytes per second or less", "use 900 operations per second or less for ioWriteIOPSMax"} {
		if !errors.Is(err, ErrLimitReserve) || !strings.Contains(err.Error(), part) {
			t.Fatalf("reserve error = %v, missing %q", err, part)
		}
	}
}

// The refusal of a slice cap on another device names that device by its
// kernel name when sysfs gives it.
func TestOtherDeviceRefusalGivesTheDeviceName(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	f.cgroup(t, limitSlice, "io.max", "259:0 rbps=1000000000 wbps=max riops=max wiops=max\n8:0 rbps=1048576 wbps=max riops=max wiops=max\n")
	f.file(t, "/sys/dev/block/8:0/uevent", "MAJOR=8\nMINOR=0\nDEVNAME=sda\nDEVTYPE=disk\n")
	err := f.m.VerifyLaunchLimits(context.Background(), f.p.ID)
	if !errors.Is(err, ErrLimitReserve) || !strings.Contains(err.Error(), "the slice caps the I/O of device 8:0 (/dev/sda)") {
		t.Fatalf("other device refusal = %v", err)
	}
}

// The admission error gives the memory that the start needs.
func TestAdmitMemoryGivesTheNeed(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	c := hostCap{slice: limitSlice, memoryHigh: 16 * gib, memoryMax: 16 * gib}
	f.cgroup(t, limitSlice, "memory.current", strconv.FormatUint(8*gib, 10)+"\n")
	err := f.m.admitMemory(c, control.ResourceLimits{Version: 1, MemoryMaxMiB: 8192})
	want := "8192 MiB is in use. The 0 MiB of page cache that the kernel can reclaim does not count. The start needs 10240 MiB: memoryMaxMiB plus a 2048 MiB reserve. The ceiling is 16384 MiB."
	if !errors.Is(err, ErrLimitMemory) || !strings.Contains(err.Error(), want) {
		t.Fatalf("admission error = %v", err)
	}
}

// A failed systemctl start can still leave a start job queued, so Start
// stops the unit on its bounded context. A refusal before the start call
// starts nothing and stops nothing. Each error names the workload and unit.
func TestFailedStartCallOfLimitedUnit(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	startErr := errors.New("bus closed")
	f.r.errs = map[string]error{"/usr/bin/true --user start -- " + f.p.Unit: startErr}
	err := f.start(t)
	prefix := string(f.p.ID) + " (" + f.p.Unit + "): "
	if !errors.Is(err, startErr) || !strings.HasPrefix(err.Error(), prefix) || errors.Is(err, ErrLimitCheckFailed) || !f.stopped() {
		t.Fatalf("start with a failed start call = %v: %v", err, f.r.calls)
	}
	f = newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	f.r.outputs[textShowCommand] = []byte("LoadState=loaded\nActiveState=active\nSubState=running\nControlGroup=" + limitManagerRoot + "/text.service\n")
	err = f.start(t)
	if err == nil || !strings.HasPrefix(err.Error(), prefix) || f.started() || f.stopped() {
		t.Fatalf("start refused before the start call = %v: %v", err, f.r.calls)
	}
}

// The cleanup stop has its own deadline of limitCleanupTimeout, also when
// the start context has none.
func TestStartCleanupStopIsBounded(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	f.cgroup(t, f.p.Cgroup, "memory.high", "8589934592\n")
	before := time.Now()
	if err := f.start(t); !errors.Is(err, ErrLimitCheckFailed) || !f.stopped() {
		t.Fatalf("start with drift = %v", err)
	}
	if f.r.stopDeadline.IsZero() || f.r.stopDeadline.Before(before.Add(limitCleanupTimeout-time.Minute)) || f.r.stopDeadline.After(time.Now().Add(limitCleanupTimeout)) {
		t.Fatalf("cleanup stop deadline = %v, start at %v", f.r.stopDeadline, before)
	}
}

// Each /lib refusal gives its cause.
func TestLibLinkRefusalGivesTheCause(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	f.edit(f.sliceShow(), "DropInPaths=/usr/lib/", "DropInPaths=/lib/")
	lib := filepath.Join(f.host, "lib")
	check := func(want string) {
		t.Helper()
		if err := f.m.VerifyLaunchLimits(context.Background(), f.p.ID); !errors.Is(err, ErrHostCap) || !strings.Contains(err.Error(), want) {
			t.Fatalf("launch checks = %v, missing %q", err, want)
		}
	}
	if err := os.Mkdir(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	check("/lib is not a link")
	if err := os.Remove(lib); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/opt/lib", lib); err != nil {
		t.Fatal(err)
	}
	check(`/lib links to "/opt/lib"`)
	if err := os.Remove(lib); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("usr/lib", lib); err != nil {
		t.Fatal(err)
	}
	f.giveOtherOwner(t, lib)
	check("/lib is owned by uid")
}

// The I/O weight refusal names the device.
func TestIOWeightRefusalNamesTheDevice(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	f.cgroup(t, "", "io.cost.qos", "259:0 enable=0 ctrl=auto\n")
	err := f.m.VerifyLaunchLimits(context.Background(), f.p.ID)
	if !errors.Is(err, ErrLimitDevice) || !strings.Contains(err.Error(), "259:0 (/dev/nvme0n1) does not support I/O weight (no BFQ or iocost)") {
		t.Fatalf("launch checks without a weight provider = %v", err)
	}
}

// A unit that only throttles I/O needs no weight provider and no io.weight
// file. A unit with only IOWeight needs no io.max of its own.
func TestIOThrottleOnlyAndWeightOnly(t *testing.T) {
	throttle := fixtureLimits()
	throttle.IOWeight = 0
	f := newLimitFixture(t, throttle, strings.Replace(fixtureLoaded, "IOWeight=500", "IOWeight=[not set]", 1))
	f.cgroup(t, "", "io.cost.qos", "")
	remove(t, f.cgroups, limitManagerRoot+"/io.weight")
	remove(t, f.cgroups, f.p.Cgroup+"/io.weight")
	remove(t, f.host, "/sys/dev/block/259:0/queue/scheduler")
	if err := f.start(t); err != nil || !f.started() {
		t.Fatalf("start of a unit that only throttles I/O = %v", err)
	}
	weight := fixtureLimits()
	weight.IOReadBandwidthMax = 0
	f = newLimitFixture(t, weight, strings.Replace(fixtureLoaded, "IOReadBandwidthMax=/dev/nvme0n1 536870912\n", "", 1))
	f.cgroup(t, limitSlice, "io.max", "")
	remove(t, f.cgroups, f.p.Cgroup+"/io.max")
	if err := f.start(t); err != nil || !f.started() {
		t.Fatalf("start of a unit with only IOWeight = %v", err)
	}
}

// A failed read of the sysfs uevent of the device proves nothing.
func TestDeviceUeventReadFailureIsNotDefinite(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	uevent := filepath.Join(f.host, "sys/dev/block/259:0/uevent")
	if err := os.Remove(uevent); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(uevent, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.verifyLimitDevice(fixtureLimits()); !errors.Is(err, ErrLimitReadback) || DefiniteLimitFailure(err) {
		t.Fatalf("unreadable uevent = %v", err)
	}
}

// While setup cannot write limits, each remedy that changes the limits
// also gives the remedy that works now.
func TestLimitRemediesIncludeRemoval(t *testing.T) {
	if err := requireCappedControls(control.ResourceLimits{MemoryMaxMiB: 1, TasksMax: 1}); !strings.HasSuffix(err.Error(), "set it in the catalog limits, or remove the limits through setup") {
		t.Fatalf("missing control = %v", err)
	}
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	f.device.Rdev = unix.Mkdev(259, 1)
	if err := f.m.VerifyLaunchLimits(context.Background(), f.p.ID); !strings.HasSuffix(err.Error(), "update the limits for the new number, or remove the limits through setup") {
		t.Fatalf("renumbered device = %v", err)
	}
}

// When the slice cap leaves less than the schema minimum after the reserve,
// the refusal asks for a larger slice cap and suggests no unit value that
// the schema rejects.
func TestReserveRefusalBelowSchemaMinimum(t *testing.T) {
	c := hostCap{memoryHigh: 2200 * mebibyte, memoryMax: 2200 * mebibyte, tasks: 520, cpu: 1000000,
		io: map[string]uint64{"rbps": 1 << 20, "riops": 100}}
	l := control.ResourceLimits{Version: 1, MemoryMaxMiB: 1024, TasksMax: 64, CPUQuotaPercent: 50, IOReadBandwidthMax: 1 << 20}
	err := checkReserve(l, c)
	for _, part := range []string{
		"raise the slice cap to at least 2304 MiB",
		"raise the slice cap to at least 576 tasks",
		"raise the slice cap to at least 101 percent",
		"raise the slice cap to at least 1165084 bytes per second",
		"raise the slice cap to at least 111 operations per second",
	} {
		if !errors.Is(err, ErrLimitReserve) || !strings.Contains(err.Error(), part) {
			t.Fatalf("reserve error = %v, missing %q", err, part)
		}
	}
	if strings.Contains(err.Error(), "or less") {
		t.Fatalf("reserve error suggests a value below the schema minimum: %v", err)
	}
	// The suggested I/O slice cap is the smallest one that fits the minimum.
	for ceiling, minimum := range map[uint64]uint64{1165084: control.MinLimitIOBandwidth, 111: control.MinLimitIOPS} {
		if ceiling-ioReserve(ceiling) < minimum || ceiling-1-ioReserve(ceiling-1) >= minimum {
			t.Fatalf("slice cap %d is not the smallest that leaves %d", ceiling, minimum)
		}
	}
}

// A slice property that differs from its cgroup file only because the cap
// changed during the check proves nothing. The check reads the properties
// again: changed values or a reload need are a readback failure. The same
// values keep the definite failure.
func TestSliceCapChangeDuringCheckIsNotDefinite(t *testing.T) {
	for name, c := range map[string]struct {
		second   func(string) string
		definite bool
	}{
		"tasks changed": {func(show string) string { return strings.Replace(show, "TasksMax=9", "TasksMax=16384", 1) }, false},
		"memory high changed": {func(show string) string {
			return strings.Replace(show, "MemoryHigh=61203283968", "MemoryHigh=61203288064", 1)
		}, false},
		"memory max changed": {func(show string) string {
			return strings.Replace(show, "MemoryMax=64424509440", "MemoryMax=64424513536", 1)
		}, false},
		"swap changed": {func(show string) string { return strings.Replace(show, "MemorySwapMax=0", "MemorySwapMax=1", 1) }, false},
		"quota changed": {func(show string) string {
			return strings.Replace(show, "CPUQuotaPerSecUSec=14s", "CPUQuotaPerSecUSec=15s", 1)
		}, false},
		"reload needed": {func(show string) string {
			return strings.Replace(show, "NeedDaemonReload=no", "NeedDaemonReload=yes", 1)
		}, false},
		"same values": {func(show string) string { return show }, true},
	} {
		f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
		f.edit(f.sliceShow(), "TasksMax=16384", "TasksMax=9")
		f.r.after = map[string]func(){f.sliceShow(): func() { f.r.outputs[f.sliceShow()] = []byte(c.second(string(f.r.outputs[f.sliceShow()]))) }}
		err := f.m.VerifyLaunchLimits(context.Background(), f.p.ID)
		if DefiniteLimitFailure(err) != c.definite || errors.Is(err, ErrHostCap) != c.definite || errors.Is(err, ErrLimitReadback) == c.definite {
			t.Errorf("%s: slice property mismatch = %v", name, err)
		}
	}
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	f.edit(f.sliceShow(), "TasksMax=16384", "TasksMax=9")
	f.r.after = map[string]func(){f.sliceShow(): func() { f.r.errs = map[string]error{f.sliceShow(): errors.New("bus")} }}
	if err := f.m.VerifyLaunchLimits(context.Background(), f.p.ID); !errors.Is(err, ErrLimitReadback) || errors.Is(err, ErrHostCap) || DefiniteLimitFailure(err) {
		t.Fatalf("slice property mismatch with a failed second read = %v", err)
	}
}

// A unit with only IOWeight still needs the io controller and the
// Delegate=io drop-in. The controller errors give the matching remedy.
func TestWeightOnlyUnitNeedsIODelegation(t *testing.T) {
	weight := fixtureLimits()
	weight.IOReadBandwidthMax = 0
	loaded := strings.Replace(fixtureLoaded, "IOReadBandwidthMax=/dev/nvme0n1 536870912\n", "", 1)
	for name, c := range map[string]struct {
		edit func(*limitFixture)
		want error
		text string
	}{
		"io not delegated": {func(f *limitFixture) { f.cgroup(t, limitManagerRoot, "cgroup.controllers", "cpu memory pids\n") },
			ErrLimitController, "check the Delegate=io drop-in of the host protection step. After you add or change it, run `sudo systemctl daemon-reload`, then restart user@1000.service from a verified Idle"},
		"memory not delegated": {func(f *limitFixture) { f.cgroup(t, limitManagerRoot, "cgroup.controllers", "cpu io pids\n") },
			ErrLimitController, "add memory to Delegate= in /etc/systemd/system/user@1000.service.d/50-gws-delegate-io.conf"},
		"delegation drop-in not loaded": {func(f *limitFixture) {
			f.edit(f.managerShow(), "/etc/systemd/system/user@1000.service.d/50-gws-delegate-io.conf", "")
		}, ErrHostCap, "does not load /etc/systemd/system/user@1000.service.d/50-gws-delegate-io.conf"},
	} {
		f := newLimitFixture(t, weight, loaded)
		f.cgroup(t, limitSlice, "io.max", "")
		c.edit(f)
		if err := f.start(t); !errors.Is(err, c.want) || !strings.Contains(err.Error(), c.text) || f.started() {
			t.Errorf("%s: start of a unit with only IOWeight = %v", name, err)
		}
	}
}

// A failed health check of a unit with limits names the workload and the
// unit, so a rollback shows which unit drifted.
func TestHealthLimitErrorNamesTheUnit(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	f.r.outputs[f.unitShow()] = []byte("LoadState=loaded\nActiveState=active\nSubState=running\nControlGroup=" + f.p.Cgroup + "\n")
	f.cgroup(t, f.p.Cgroup, "memory.high", "8589934592\n")
	err := f.m.Healthy(context.Background(), f.p.ID)
	if !errors.Is(err, ErrLimitDrift) || !strings.HasPrefix(err.Error(), string(f.p.ID)+" ("+f.p.Unit+"): ") {
		t.Fatalf("health with drift = %v", err)
	}
}

// A launch error that groups several problems is definite when one problem
// is a definite mismatch. Only a group of read and command failures proves
// nothing.
func TestDefiniteBindingFailureOfAGroup(t *testing.T) {
	eio := &os.PathError{Op: "read", Path: "key", Err: unix.EIO}
	readFailure := markBindingRead(fmt.Errorf("%w: API key file could not be read", ErrLaunchUnsupported), eio)
	commandFailure := SafeError("cannot inspect the launch binding: command failed", ErrLaunchChanged, errBindingCommand)
	mismatch := fmt.Errorf("%w: vLLM must listen only on --host 127.0.0.1", ErrLaunchUnsupported)
	group := func(items ...error) error {
		var problems launchProblems
		for _, item := range items {
			problems.add(item)
		}
		return fmt.Errorf("binding: %w", problems.err())
	}
	if err := group(mismatch, readFailure); !DefiniteBindingFailure(err) {
		t.Fatalf("a mismatch next to a read failure = not definite: %v", err)
	}
	if err := group(readFailure, commandFailure); DefiniteBindingFailure(err) {
		t.Fatalf("read and command failures only = definite: %v", err)
	}
	if !DefiniteBindingFailure(mismatch) || DefiniteBindingFailure(readFailure) {
		t.Fatal("single items are misclassified")
	}
}

// Only the io.cost.qos line of the unit device counts, and only the active
// scheduler counts as BFQ. A missing cgroup hierarchy proves nothing about
// iocost.
func TestIOWeightProviderOfTheDevice(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	f.cgroup(t, "", "io.cost.qos", "8:0 enable=1 ctrl=auto\n259:0 enable=0 ctrl=auto\n")
	if _, err := f.m.ioWeightProvider("259:0"); !errors.Is(err, ErrLimitDevice) {
		t.Fatalf("provider with iocost on another device only = %v", err)
	}
	f.file(t, "/sys/dev/block/259:0/queue/scheduler", "[mq-deadline] bfq none\n")
	if provider, err := f.m.ioWeightProvider("259:0"); !errors.Is(err, ErrLimitDevice) || provider == "bfq" {
		t.Fatalf("provider with an inactive bfq = %q, %v", provider, err)
	}
	f.m.cgroups.root = filepath.Join(f.cgroups, "missing")
	if _, err := f.m.ioWeightProvider("259:0"); !errors.Is(err, ErrLimitReadback) || DefiniteLimitFailure(err) {
		t.Fatalf("provider without the cgroup hierarchy = %v", err)
	}
}

// A malformed slice memory.current proves no memory pressure: it is a
// readback failure.
func TestMalformedMemoryCurrentIsReadback(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	f.cgroup(t, limitSlice, "memory.current", "lots\n")
	c := hostCap{slice: limitSlice, memoryHigh: 16 * gib, memoryMax: 16 * gib}
	if err := f.m.admitMemory(c, control.ResourceLimits{Version: 1, MemoryMaxMiB: 1024}); !errors.Is(err, ErrLimitReadback) || errors.Is(err, ErrLimitMemory) {
		t.Fatalf("admission with a malformed memory.current = %v", err)
	}
}

// While setup cannot write limits, each reserve and device remedy that
// changes the limits also gives the removal of the limits.
func TestReserveRemediesIncludeRemoval(t *testing.T) {
	c := hostCap{memoryHigh: 8 * gib, memoryMax: 8 * gib, tasks: 1000, cpu: 3000000, io: map[string]uint64{"rbps": 100 << 20}}
	err := checkReserve(control.ResourceLimits{Version: 1, MemoryMaxMiB: 7000, TasksMax: 64, CPUQuotaPercent: 100}, c)
	for _, part := range []string{
		"use 6144 MiB or less for memoryMaxMiB, or remove the limits through setup",
		"set ioReadBandwidthMax to 94371840 bytes per second or less, or remove the limits through setup",
	} {
		if !strings.Contains(err.Error(), part) {
			t.Fatalf("reserve error = %v, missing %q", err, part)
		}
	}
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	f.cgroup(t, limitSlice, "io.max", "259:0 rbps=1000000000 wbps=max riops=max wiops=max\n8:0 rbps=1048576 wbps=max riops=max wiops=max\n")
	if err := f.m.VerifyLaunchLimits(context.Background(), f.p.ID); !strings.HasSuffix(err.Error(), "or set ioDevice to that device with its I/O caps, or remove the limits through setup") {
		t.Fatalf("other device refusal = %v", err)
	}
}

package runtime

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"golang.org/x/sys/unix"
)

const (
	limitSlice       = "/user.slice/user-1000.slice"
	limitManagerRoot = limitSlice + "/user@1000.service"
	gib              = 1 << 30
)

// limitRunner records the unit state that systemd would report after start
// and stop.
type limitRunner struct {
	fakeRunner
	unit, cgroup string
	afterStart   func()
	// stopDeadline is the context deadline of the last stop of unit.
	stopDeadline time.Time
	// after runs once after the first call of its command.
	after map[string]func()
	// ignoreCtx runs commands on a spent context, as a runner that does
	// not honour cancellation would.
	ignoreCtx bool
}

func (r *limitRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	call := strings.Join(append([]string{name}, args...), " ")
	if err := ctx.Err(); err != nil && !r.ignoreCtx {
		// A real command fails at once on a spent context and changes
		// nothing.
		r.mu.Lock()
		r.calls = append(r.calls, call)
		r.mu.Unlock()
		return nil, err
	}
	show := "/usr/bin/true --user show --property=LoadState --property=ActiveState --property=SubState --property=ControlGroup -- " + r.unit
	r.mu.Lock()
	switch call {
	case "/usr/bin/true --user start -- " + r.unit:
		r.outputs[show] = []byte("LoadState=loaded\nActiveState=active\nSubState=running\nControlGroup=" + r.cgroup + "\n")
	case "/usr/bin/true --user stop -- " + r.unit:
		r.outputs[show] = stoppedOutput()
		r.stopDeadline, _ = ctx.Deadline()
	}
	r.mu.Unlock()
	out, err := r.fakeRunner.Run(ctx, name, args...)
	if r.afterStart != nil && call == "/usr/bin/true --user start -- "+r.unit {
		r.afterStart()
	}
	if hook := r.after[call]; hook != nil {
		delete(r.after, call)
		hook()
	}
	return out, err
}

func (r *limitRunner) called(call string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Contains(r.calls, call)
}

// limitFixture is a host with a valid host cap, an NVMe leaf disk with
// iocost, and a stopped owned unit with limits whose cgroup holds the
// rendered values.
type limitFixture struct {
	m         *SystemdManager
	r         *limitRunner
	p         control.WorkloadProfile
	cgroups   string
	host      string
	device    unix.Stat_t
	deviceErr error
	// otherOwner lists inodes that ownerOf reports with another owner.
	otherOwner map[uint64]bool
}

// lstat gives the fixture device status for paths under /dev and the real
// status of other host paths.
func (f *limitFixture) lstat(name string) (unix.Stat_t, error) {
	if strings.HasPrefix(name, filepath.Join(f.host, "dev")+"/") {
		return f.device, f.deviceErr
	}
	var stat unix.Stat_t
	err := unix.Lstat(name, &stat)
	return stat, err
}

func (f *limitFixture) ownerOf(stat unix.Stat_t) uint32 {
	if f.otherOwner[stat.Ino] {
		return stat.Uid + 1
	}
	return stat.Uid
}

// giveOtherOwner makes ownerOf report another owner for name, without root.
func (f *limitFixture) giveOtherOwner(t *testing.T, name string) {
	t.Helper()
	var stat unix.Stat_t
	if err := unix.Lstat(name, &stat); err != nil {
		t.Fatal(err)
	}
	if f.otherOwner == nil {
		f.otherOwner = map[uint64]bool{}
	}
	f.otherOwner[stat.Ino] = true
}

func fixtureLimits() control.ResourceLimits {
	return control.ResourceLimits{Version: 1, MemoryHighMiB: 4096, MemoryMaxMiB: 8192, TasksMax: 4096, CPUWeight: 200, CPUQuotaPercent: 400,
		IODevice: "/dev/nvme0n1", IODeviceNumber: "259:0", IOReadBandwidthMax: 512 << 20, IOWeight: 500}
}

func newLimitFixture(t *testing.T, limits control.ResourceLimits, loaded string) *limitFixture {
	t.Helper()
	p, raw := ownedProfileWithLimits(t, limits)
	dir := t.TempDir()
	p.NativeModel.LaunchFile = filepath.Join(dir, p.Unit)
	if err := os.WriteFile(p.NativeModel.LaunchFile, raw, 0600); err != nil {
		t.Fatal(err)
	}
	p.Label, p.Adapter, p.HealthURL = "Vision", "systemd", "http://127.0.0.1:9/health"
	p.Cgroup = OwnedCgroup(limitManagerRoot, p)
	r := &limitRunner{fakeRunner: fakeRunner{outputs: map[string][]byte{textShowCommand: stoppedOutput(), mediaShowCommand: stoppedOutput()}}, unit: p.Unit, cgroup: p.Cgroup}
	m := strictManager(t, r)
	catalog := m.config.Catalog.Clone()
	catalog.Version = control.CatalogVersionLimits
	for i := range catalog.Profiles {
		catalog.Profiles[i].Cgroup = limitManagerRoot + "/" + catalog.Profiles[i].Unit
	}
	catalog.Profiles = append(catalog.Profiles, p)
	m.config.Catalog = &catalog
	m.config.OwnedUnitDir = dir
	m.nativeExecutableValidator = fixtureExecutableValidator
	launch, err := parseOwnedLaunchUnit(raw, *p.NativeModel)
	if err != nil {
		t.Fatal(err)
	}
	r.outputs["/usr/bin/true --user show --property=FragmentPath --property=DropInPaths --property=NeedDaemonReload --property=ExecStartPre --property=ExecStart --property=Environment --property=EnvironmentFiles -- "+p.Unit] =
		[]byte("ExecStart={ path=" + strings.Fields(launch.execStart)[0] + " ; argv[]=" + launch.execStart + " ; }\nFragmentPath=" + p.NativeModel.LaunchFile +
			"\nDropInPaths=\nNeedDaemonReload=no\nEnvironment=HF_HUB_OFFLINE=1\nEnvironmentFiles=" + p.NativeModel.EnvironmentFile + " (ignore_errors=no)\n")
	r.outputs["/usr/bin/true --user show --property=LoadState --property=ActiveState --property=SubState --property=ControlGroup -- "+p.Unit] = stoppedOutput()
	r.outputs[rootShowCommand] = []byte("LoadState=loaded\nActiveState=active\nSubState=active\nControlGroup=" + limitManagerRoot + "\n")
	f := &limitFixture{m: m, r: r, p: p, cgroups: t.TempDir(), host: t.TempDir()}
	f.device = unix.Stat_t{Mode: unix.S_IFBLK | 0o660, Uid: uint32(os.Getuid()), Rdev: unix.Mkdev(259, 0)}
	m.cgroups = cgroupFS{root: f.cgroups, verify: func(int) error { return nil }}
	m.host = limitHost{root: f.host, owner: uint32(os.Getuid()), uid: 1000, ownerOf: f.ownerOf, lstat: f.lstat, hasXattr: hasXattr}
	f.writeCgroups(t)
	f.writeHost(t)
	f.writeShows(loaded)
	return f
}

func (f *limitFixture) writeCgroups(t *testing.T) {
	t.Helper()
	f.cgroup(t, limitManagerRoot, "cgroup.events", "populated 1\n")
	f.cgroup(t, limitManagerRoot, "cgroup.controllers", "cpuset cpu io memory pids\n")
	f.cgroup(t, limitManagerRoot, "memory.swap.max", "max\n")
	f.cgroup(t, limitManagerRoot, "io.weight", "default 100\n")
	for name, value := range map[string]string{"memory.high": "61203283968", "memory.max": "64424509440", "memory.swap.max": "0", "pids.max": "16384",
		"cpu.max": "1400000 100000", "io.max": "259:0 rbps=1000000000 wbps=max riops=max wiops=max", "memory.current": "1073741824",
		"memory.stat": "anon 1073741824\nactive_file 0\ninactive_file 0\nshmem 0"} {
		f.cgroup(t, limitSlice, name, value+"\n")
	}
	f.cgroup(t, "", "io.cost.qos", "8:0 enable=0 ctrl=auto\n259:0 enable=1 ctrl=auto rpct=0.00 rlat=250 wpct=0.00 wlat=250 min=1.00 max=10000.00\n")
	f.cgroup(t, f.p.Cgroup, "cgroup.events", "populated 1\n")
	f.cgroup(t, f.p.Cgroup, "cgroup.controllers", "cpu io memory pids\n")
	for name, value := range map[string]string{"memory.high": "4294967296", "memory.max": "8589934592", "memory.swap.max": "0", "pids.max": "4096",
		"cpu.weight": "200", "cpu.max": "400000 100000", "io.max": "259:0 rbps=536870912 wbps=max riops=max wiops=max", "io.weight": "default 500"} {
		f.cgroup(t, f.p.Cgroup, name, value+"\n")
	}
}

func (f *limitFixture) cgroup(t *testing.T, group, name, value string) {
	t.Helper()
	writeTree(t, f.cgroups, filepath.Join(group, name), value)
}

func (f *limitFixture) file(t *testing.T, name, value string) {
	t.Helper()
	writeTree(t, f.host, name, value)
}

// writeTree writes root/name with mode 0644 and parents with mode 0755.
func writeTree(t *testing.T, root, name, value string) {
	t.Helper()
	dir := root
	for _, component := range strings.Split(strings.Trim(filepath.Dir(name), "/"), "/") {
		if component == "" || component == "." {
			continue
		}
		dir = filepath.Join(dir, component)
		if err := os.Mkdir(dir, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, name), []byte(value), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, name), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f *limitFixture) writeHost(t *testing.T) {
	t.Helper()
	if err := os.Chmod(f.host, 0o755); err != nil {
		t.Fatal(err)
	}
	f.file(t, "/etc/systemd/system/user-1000.slice.d/50-gws-host-cap.conf", "[Slice]\nMemoryHigh=58368M\n")
	f.file(t, "/etc/systemd/system/user@1000.service.d/50-gws-delegate-io.conf", "[Service]\nDelegate=io\n")
	f.file(t, "/usr/lib/systemd/system/user-.slice.d/10-defaults.conf", "[Slice]\nTasksMax=33%\n")
	f.file(t, "/usr/lib/systemd/system/user@.service.d/10-login-barrier.conf", "[Unit]\n")
	f.file(t, "/sys/dev/block/259:0/uevent", "MAJOR=259\nMINOR=0\nDEVNAME=nvme0n1\nDEVTYPE=disk\n")
	f.file(t, "/sys/dev/block/259:0/queue/scheduler", "[none] mq-deadline kyber\n")
	if err := os.Mkdir(filepath.Join(f.host, "sys/dev/block/259:0/slaves"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func (f *limitFixture) sliceShow() string {
	return "/usr/bin/true show --property=" + strings.Join(sliceProperties, " --property=") + " -- user-1000.slice"
}

func (f *limitFixture) managerShow() string {
	return "/usr/bin/true show --property=LoadState --property=DropInPaths --property=NeedDaemonReload -- user@1000.service"
}

func (f *limitFixture) limitShow() string {
	return "/usr/bin/true --user show --property=" + strings.Join(limitPropertyNames, " --property=") + " -- " + f.p.Unit
}

func (f *limitFixture) writeShows(loaded string) {
	f.r.outputs[f.sliceShow()] = []byte("LoadState=loaded\nControlGroup=" + limitSlice +
		"\nDropInPaths=/usr/lib/systemd/system/user-.slice.d/10-defaults.conf /etc/systemd/system/user-1000.slice.d/50-gws-host-cap.conf\n" +
		"NeedDaemonReload=no\nMemoryHigh=61203283968\nMemoryMax=64424509440\nMemorySwapMax=0\nTasksMax=16384\nCPUQuotaPerSecUSec=14s\n")
	f.r.outputs[f.managerShow()] = []byte("LoadState=loaded\nDropInPaths=/usr/lib/systemd/system/user@.service.d/10-login-barrier.conf " +
		"/etc/systemd/system/user@1000.service.d/50-gws-delegate-io.conf\nNeedDaemonReload=no\n")
	f.r.outputs[f.limitShow()] = []byte(loaded)
	// The second read of a missing drop-in sees the same list.
	f.r.outputs[f.dropInReread("user-1000.slice")] = []byte("DropInPaths=/usr/lib/systemd/system/user-.slice.d/10-defaults.conf /etc/systemd/system/user-1000.slice.d/50-gws-host-cap.conf\nNeedDaemonReload=no\n")
	f.r.outputs[f.dropInReread("user@1000.service")] = []byte("DropInPaths=/usr/lib/systemd/system/user@.service.d/10-login-barrier.conf " +
		"/etc/systemd/system/user@1000.service.d/50-gws-delegate-io.conf\nNeedDaemonReload=no\n")
}

// fixtureLoaded is systemctl show output for fixtureLimits, written by hand
// from the systemd formats rather than from the code under test.
const fixtureLoaded = "OOMPolicy=stop\nOOMScoreAdjust=500\nMemoryHigh=4294967296\nMemoryMax=8589934592\nMemorySwapMax=0\nTasksMax=4096\n" +
	"CPUWeight=200\nCPUQuotaPerSecUSec=4s\nIOWeight=500\nIOReadBandwidthMax=/dev/nvme0n1 536870912\n"

func (f *limitFixture) bindingShow() string {
	return "/usr/bin/true --user show --property=FragmentPath --property=DropInPaths --property=NeedDaemonReload --property=ExecStartPre --property=ExecStart --property=Environment --property=EnvironmentFiles -- " + f.p.Unit
}

func (f *limitFixture) unitShow() string {
	return "/usr/bin/true --user show --property=LoadState --property=ActiveState --property=SubState --property=ControlGroup -- " + f.p.Unit
}

func (f *limitFixture) start(t *testing.T) error {
	t.Helper()
	return f.m.Start(context.Background(), f.p.ID)
}

func (f *limitFixture) started() bool {
	return f.r.called("/usr/bin/true --user start -- " + f.p.Unit)
}

func (f *limitFixture) stopped() bool {
	return f.r.called("/usr/bin/true --user stop -- " + f.p.Unit)
}

func TestLimitedUnitStartsAfterAllChecks(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	// An unlimited unit starts without the limit checks.
	f.cgroup(t, f.p.Cgroup, "cgroup.events", "populated 0\n")
	calls := len(f.r.calls)
	if err := f.m.Start(context.Background(), control.WorkloadText); err != nil || slices.Contains(f.r.calls[calls:], f.sliceShow()) {
		t.Fatalf("start of an unlimited unit = %v: %v", err, f.r.calls[calls:])
	}
	f.r.outputs[textShowCommand] = stoppedOutput()
	f.cgroup(t, f.p.Cgroup, "cgroup.events", "populated 1\n")
	if err := f.start(t); err != nil {
		t.Fatalf("start of a unit with valid limits = %v", err)
	}
	if !f.started() || f.stopped() {
		t.Fatalf("calls = %v", f.r.calls)
	}
	for _, call := range []string{f.sliceShow(), f.managerShow(), f.limitShow()} {
		if !f.r.called(call) {
			t.Fatalf("check %q did not run: %v", call, f.r.calls)
		}
	}
	// The health check reads the limits back before the HTTP check. Those
	// checks pass, so the health failure comes from the HTTP check, which
	// fails because nothing listens.
	if err := f.m.checkStartedUnit(context.Background(), f.p); err != nil {
		t.Fatalf("checks of a started unit with exact limits = %v", err)
	}
	err := f.m.Healthy(context.Background(), f.p.ID)
	if err == nil || errors.Is(err, ErrLimitCheckFailed) || errors.Is(err, ErrLimitReadback) || DefiniteLimitFailure(err) || f.stopped() {
		t.Fatalf("health with exact limits = %v", err)
	}
}

// Each check before start fails closed with its error class and starts
// nothing.
func TestLimitedUnitDoesNotStartWhenACheckFails(t *testing.T) {
	cases := map[string]struct {
		edit func(*testing.T, *limitFixture)
		want error
	}{
		"io not delegated": {func(t *testing.T, f *limitFixture) {
			f.cgroup(t, limitManagerRoot, "cgroup.controllers", "cpu memory pids\n")
		}, ErrLimitController},
		"manager controllers unreadable": {func(t *testing.T, f *limitFixture) {
			remove(t, f.cgroups, limitManagerRoot+"/cgroup.controllers")
		}, ErrLimitReadback},
		"no swap accounting": {func(t *testing.T, f *limitFixture) {
			remove(t, f.cgroups, limitManagerRoot+"/memory.swap.max")
		}, ErrLimitInterface},
		"host cap drop-in absent": {func(t *testing.T, f *limitFixture) {
			remove(t, f.host, "/etc/systemd/system/user-1000.slice.d/50-gws-host-cap.conf")
		}, ErrHostCap},
		"host cap drop-in group writable": {func(t *testing.T, f *limitFixture) {
			chmod(t, f.host, "/etc/systemd/system/user-1000.slice.d/50-gws-host-cap.conf", 0o664)
		}, ErrHostCap},
		"host cap parent other writable": {func(t *testing.T, f *limitFixture) {
			chmod(t, f.host, "/etc/systemd/system", 0o757)
		}, ErrHostCap},
		"host cap drop-in symlink": {func(t *testing.T, f *limitFixture) {
			name := filepath.Join(f.host, "etc/systemd/system/user-1000.slice.d/50-gws-host-cap.conf")
			remove(t, f.host, "/etc/systemd/system/user-1000.slice.d/50-gws-host-cap.conf")
			if err := os.Symlink(filepath.Join(f.host, "usr/lib/systemd/system/user-.slice.d/10-defaults.conf"), name); err != nil {
				t.Fatal(err)
			}
		}, ErrHostCap},
		"host cap drop-in directory": {func(t *testing.T, f *limitFixture) {
			remove(t, f.host, "/etc/systemd/system/user-1000.slice.d/50-gws-host-cap.conf")
			if err := os.Mkdir(filepath.Join(f.host, "etc/systemd/system/user-1000.slice.d/50-gws-host-cap.conf"), 0o755); err != nil {
				t.Fatal(err)
			}
		}, ErrHostCap},
		"host root missing": {func(t *testing.T, f *limitFixture) {
			f.m.host.root = filepath.Join(f.host, "missing")
		}, ErrHostCap},
		"host cap not loaded": {func(t *testing.T, f *limitFixture) {
			f.edit(f.sliceShow(), "/etc/systemd/system/user-1000.slice.d/50-gws-host-cap.conf", "")
		}, ErrHostCap},
		"unknown slice drop-in": {func(t *testing.T, f *limitFixture) {
			f.edit(f.sliceShow(), "DropInPaths=", "DropInPaths=/run/systemd/system/user-1000.slice.d/50-raise.conf ")
		}, ErrHostCap},
		"vendor drop-in path not canonical": {func(t *testing.T, f *limitFixture) {
			f.edit(f.sliceShow(), "DropInPaths=", "DropInPaths=/usr/lib/systemd/system/../../../etc/x.conf ")
		}, ErrHostCap},
		"vendor drop-in writable": {func(t *testing.T, f *limitFixture) {
			chmod(t, f.host, "/usr/lib/systemd/system/user-.slice.d/10-defaults.conf", 0o666)
		}, ErrHostCap},
		"slice needs daemon-reload": {func(t *testing.T, f *limitFixture) {
			f.edit(f.sliceShow(), "NeedDaemonReload=no", "NeedDaemonReload=yes")
		}, ErrLimitReadback},
		"manager needs daemon-reload": {func(t *testing.T, f *limitFixture) {
			f.edit(f.managerShow(), "NeedDaemonReload=no", "NeedDaemonReload=yes")
		}, ErrLimitReadback},
		"no iocost for io.weight": {func(t *testing.T, f *limitFixture) {
			remove(t, f.cgroups, limitManagerRoot+"/io.weight")
		}, ErrLimitInterface},
		"manager io.weight unreadable": {func(t *testing.T, f *limitFixture) {
			remove(t, f.cgroups, limitManagerRoot+"/io.weight")
			if err := os.Mkdir(filepath.Join(f.cgroups, limitManagerRoot, "io.weight"), 0o755); err != nil {
				t.Fatal(err)
			}
		}, ErrLimitReadback},

		"slice not loaded": {func(t *testing.T, f *limitFixture) {
			f.edit(f.sliceShow(), "LoadState=loaded", "LoadState=not-found")
		}, ErrHostCap},
		"slice show fails": {func(t *testing.T, f *limitFixture) {
			f.r.errs = map[string]error{f.sliceShow(): errors.New("bus")}
		}, errCommand},
		"manager show fails": {func(t *testing.T, f *limitFixture) {
			f.r.errs = map[string]error{f.managerShow(): errors.New("bus")}
		}, errCommand},
		"loaded OOM score differs": {func(t *testing.T, f *limitFixture) {
			f.edit(f.limitShow(), "OOMScoreAdjust=500", "OOMScoreAdjust=0")
		}, ErrLimitProperties},
		"loaded stray write cap": {func(t *testing.T, f *limitFixture) {
			f.edit(f.limitShow(), "IOWeight=500\n", "IOWeight=500\nIOWriteBandwidthMax=/dev/nvme0n1 1048576\n")
		}, ErrLimitProperties},
		"device not root-owned": {func(t *testing.T, f *limitFixture) {
			f.device.Uid = f.m.host.owner + 1
		}, ErrLimitDevice},
		"slice memory max property differs": {func(t *testing.T, f *limitFixture) {
			f.edit(f.sliceShow(), "MemoryMax=64424509440", "MemoryMax=infinity")
		}, ErrHostCap},
		"slice swap property differs": {func(t *testing.T, f *limitFixture) {
			f.edit(f.sliceShow(), "MemorySwapMax=0", "MemorySwapMax=infinity")
		}, ErrHostCap},
		"slice tasks property differs": {func(t *testing.T, f *limitFixture) {
			f.edit(f.sliceShow(), "TasksMax=16384", "TasksMax=20000")
		}, ErrHostCap},
		"slice quota property differs": {func(t *testing.T, f *limitFixture) {
			f.edit(f.sliceShow(), "CPUQuotaPerSecUSec=14s", "CPUQuotaPerSecUSec=15s")
		}, ErrHostCap},
		"vendor drop-in under /lib without link": {func(t *testing.T, f *limitFixture) {
			f.edit(f.sliceShow(), "DropInPaths=/usr/lib/", "DropInPaths=/lib/")
		}, ErrHostCap},
		"vendor drop-in under /lib linked elsewhere": {func(t *testing.T, f *limitFixture) {
			f.edit(f.sliceShow(), "DropInPaths=/usr/lib/", "DropInPaths=/lib/")
			if err := os.Symlink("etc", filepath.Join(f.host, "lib")); err != nil {
				t.Fatal(err)
			}
		}, ErrHostCap},
		"vendor drop-in outside vendor directory": {func(t *testing.T, f *limitFixture) {
			f.edit(f.sliceShow(), "DropInPaths=/usr/lib/systemd/system/", "DropInPaths=/usr/lib/systemd/user/")
		}, ErrHostCap},
		"slice cgroup repeated": {func(t *testing.T, f *limitFixture) {
			f.edit(f.sliceShow(), "LoadState=loaded\n", "LoadState=loaded\nControlGroup="+limitSlice+"\n")
		}, ErrHostCap},
		"slice outside the manager": {func(t *testing.T, f *limitFixture) {
			// A real slice with trusted files that does not contain the
			// user manager.
			other := "/user.slice/user-1001.slice"
			for _, name := range []string{"memory.high", "memory.max", "memory.swap.max", "pids.max", "cpu.max", "io.max", "memory.current"} {
				data, err := os.ReadFile(filepath.Join(f.cgroups, limitSlice, name))
				if err != nil {
					t.Fatal(err)
				}
				f.cgroup(t, other, name, string(data))
			}
			f.edit(f.sliceShow(), "ControlGroup="+limitSlice, "ControlGroup="+other)
		}, errNotInside},
		"slice memory unlimited": {func(t *testing.T, f *limitFixture) {
			f.cgroup(t, limitSlice, "memory.max", "max\n")
		}, ErrHostCap},
		"slice file writable": {func(t *testing.T, f *limitFixture) {
			chmod(t, f.cgroups, limitSlice+"/memory.max", 0o666)
		}, ErrHostCap},
		"slice file missing": {func(t *testing.T, f *limitFixture) {
			remove(t, f.cgroups, limitSlice+"/pids.max")
		}, ErrHostCap},
		"slice cpu unlimited": {func(t *testing.T, f *limitFixture) {
			f.cgroup(t, limitSlice, "cpu.max", "max 100000\n")
		}, ErrHostCap},
		"slice cpu missing": {func(t *testing.T, f *limitFixture) {
			remove(t, f.cgroups, limitSlice+"/cpu.max")
		}, ErrHostCap},
		"slice swap allowed": {func(t *testing.T, f *limitFixture) {
			f.cgroup(t, limitSlice, "memory.swap.max", "max\n")
		}, ErrHostCap},
		"slice swap file absent": {func(t *testing.T, f *limitFixture) {
			remove(t, f.cgroups, limitSlice+"/memory.swap.max")
		}, ErrHostCap},
		"slice swap file writable": {func(t *testing.T, f *limitFixture) {
			chmod(t, f.cgroups, limitSlice+"/memory.swap.max", 0o666)
		}, ErrHostCap},
		"slice read cap absent": {func(t *testing.T, f *limitFixture) {
			f.cgroup(t, limitSlice, "io.max", "259:0 rbps=max wbps=500000000 riops=max wiops=max\n8:0 rbps=max wbps=max riops=max wiops=max\n")
		}, ErrHostCap},
		"slice caps another device": {func(t *testing.T, f *limitFixture) {
			f.cgroup(t, limitSlice, "io.max", "259:0 rbps=1000000000 wbps=max riops=max wiops=max\n8:0 rbps=1048576 wbps=max riops=max wiops=max\n")
		}, ErrLimitReserve},
		"slice io file missing": {func(t *testing.T, f *limitFixture) {
			remove(t, f.cgroups, limitSlice+"/io.max")
		}, ErrHostCap},
		"slice property differs": {func(t *testing.T, f *limitFixture) {
			f.edit(f.sliceShow(), "MemoryHigh=61203283968", "MemoryHigh=infinity")
		}, ErrHostCap},
		"delegation drop-in not loaded": {func(t *testing.T, f *limitFixture) {
			f.edit(f.managerShow(), "/etc/systemd/system/user@1000.service.d/50-gws-delegate-io.conf", "")
		}, ErrHostCap},
		"delegation drop-in absent": {func(t *testing.T, f *limitFixture) {
			remove(t, f.host, "/etc/systemd/system/user@1000.service.d/50-gws-delegate-io.conf")
		}, ErrHostCap},
		"memory above reserve": {func(t *testing.T, f *limitFixture) {
			f.cgroup(t, limitSlice, "memory.high", "9663676416\n")
			f.edit(f.sliceShow(), "MemoryHigh=61203283968", "MemoryHigh=9663676416")
		}, ErrLimitReserve},
		"tasks above reserve": {func(t *testing.T, f *limitFixture) {
			f.cgroup(t, limitSlice, "pids.max", "4000\n")
			f.edit(f.sliceShow(), "TasksMax=16384", "TasksMax=4000")
		}, ErrLimitReserve},
		"cpu above reserve": {func(t *testing.T, f *limitFixture) {
			f.cgroup(t, limitSlice, "cpu.max", "450000 100000\n")
			f.edit(f.sliceShow(), "CPUQuotaPerSecUSec=14s", "CPUQuotaPerSecUSec=4.500000s")
		}, ErrLimitReserve},
		"io above reserve": {func(t *testing.T, f *limitFixture) {
			f.cgroup(t, limitSlice, "io.max", "259:0 rbps=536870912 wbps=max riops=max wiops=max\n")
		}, ErrLimitReserve},
		"device not a block device": {func(t *testing.T, f *limitFixture) {
			f.device.Mode = unix.S_IFREG | 0o644
		}, ErrLimitDevice},
		"device number differs": {func(t *testing.T, f *limitFixture) {
			f.device.Rdev = unix.Mkdev(259, 1)
		}, ErrLimitDevice},
		"device missing": {func(t *testing.T, f *limitFixture) {
			f.deviceErr = unix.ENOENT
		}, ErrLimitDevice},
		"device name differs": {func(t *testing.T, f *limitFixture) {
			f.file(t, "/sys/dev/block/259:0/uevent", "MAJOR=259\nMINOR=0\nDEVNAME=nvme1n1\n")
		}, ErrLimitDevice},
		"device without queue": {func(t *testing.T, f *limitFixture) {
			if err := os.RemoveAll(filepath.Join(f.host, "sys/dev/block/259:0/queue")); err != nil {
				t.Fatal(err)
			}
		}, ErrLimitDevice},
		"device is a partition": {func(t *testing.T, f *limitFixture) {
			// A partition has no request queue.
			if err := os.RemoveAll(filepath.Join(f.host, "sys/dev/block/259:0/queue")); err != nil {
				t.Fatal(err)
			}
			f.file(t, "/sys/dev/block/259:0/partition", "1\n")
		}, ErrLimitDevice},
		"device is stacked": {func(t *testing.T, f *limitFixture) {
			f.file(t, "/sys/dev/block/259:0/slaves/sda", "")
		}, ErrLimitDevice},
		"device without weight provider": {func(t *testing.T, f *limitFixture) {
			f.cgroup(t, "", "io.cost.qos", "259:0 enable=0 ctrl=auto\n")
		}, ErrLimitDevice},
		"loaded OOM policy differs": {func(t *testing.T, f *limitFixture) {
			f.edit(f.limitShow(), "OOMPolicy=stop", "OOMPolicy=continue")
		}, ErrLimitProperties},
		"loaded quota differs": {func(t *testing.T, f *limitFixture) {
			f.edit(f.limitShow(), "CPUQuotaPerSecUSec=4s", "CPUQuotaPerSecUSec=5s")
		}, ErrLimitProperties},
		"loaded second device": {func(t *testing.T, f *limitFixture) {
			f.edit(f.limitShow(), "IOWeight=500\n", "IOWeight=500\nIOReadBandwidthMax=/dev/sda 1048576\n")
		}, ErrLimitProperties},
		"loaded show fails": {func(t *testing.T, f *limitFixture) {
			f.r.errs = map[string]error{f.limitShow(): errors.New("bus")}
		}, errLoadedShow},
		"slice memory in use": {func(t *testing.T, f *limitFixture) {
			f.cgroup(t, limitSlice, "memory.current", "51539607552\n")
		}, ErrLimitMemory},
		"slice memory above ceiling": {func(t *testing.T, f *limitFixture) {
			f.cgroup(t, limitSlice, "memory.current", "70000000000\n")
		}, ErrLimitMemory},
		"slice memory unknown": {func(t *testing.T, f *limitFixture) {
			remove(t, f.cgroups, limitSlice+"/memory.current")
		}, ErrLimitReadback},
		"manager cgroup unavailable": {func(t *testing.T, f *limitFixture) {
			f.r.errs = map[string]error{rootShowCommand: errors.New("bus")}
		}, errManagerShow},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
			c.edit(t, f)
			err := f.start(t)
			switch c.want {
			case errLoadedShow:
				if err == nil || !strings.Contains(err.Error(), "inspect "+f.p.Unit) || !f.r.called(f.limitShow()) {
					t.Fatalf("start = %v", err)
				}
			case errNotInside:
				if !errors.Is(err, ErrHostCap) || !strings.Contains(err.Error(), "is not inside") {
					t.Fatalf("start = %v", err)
				}
			case errCommand:
				if err == nil || DefiniteLimitFailure(err) || !strings.Contains(err.Error(), "inspect user") {
					t.Fatalf("start = %v", err)
				}
			case errManagerShow:
				if err == nil || !strings.Contains(err.Error(), "systemd manager cgroup anchor") {
					t.Fatalf("start = %v", err)
				}
			default:
				if !errors.Is(err, c.want) {
					t.Fatalf("start = %v, want %v", err, c.want)
				}
			}
			if f.started() {
				t.Fatalf("unit started: %v", f.r.calls)
			}
		})
	}
}

// Markers for cases that fail at a systemctl call, not at a limit check.
var (
	errLoadedShow  = errors.New("loaded show fails")
	errManagerShow = errors.New("manager show fails")
	errCommand     = errors.New("system manager show fails")
	errNotInside   = errors.New("slice outside the manager")
)

func (f *limitFixture) edit(call, old, replacement string) {
	f.r.outputs[call] = []byte(strings.Replace(string(f.r.outputs[call]), old, replacement, 1))
}

func remove(t *testing.T, root, name string) {
	t.Helper()
	if err := os.Remove(filepath.Join(root, name)); err != nil {
		t.Fatal(err)
	}
}

func chmod(t *testing.T, root, name string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(filepath.Join(root, name), mode); err != nil {
		t.Fatal(err)
	}
}

// A unit with limits needs swap accounting, so a missing slice
// memory.swap.max is a missing host file.
func TestHostCapRequiresSliceSwapFile(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	remove(t, f.cgroups, limitSlice+"/memory.swap.max")
	if err := f.start(t); !errors.Is(err, ErrHostCap) || !errors.Is(err, unix.ENOENT) || f.started() {
		t.Fatalf("start without the slice memory.swap.max = %v", err)
	}
}

// A unit without I/O controls needs neither the delegation drop-in nor a
// device; one with only some controls reads back only their controllers.
func TestLimitedUnitWithoutIO(t *testing.T) {
	limits := control.ResourceLimits{Version: 1, MemoryMaxMiB: 8192, TasksMax: 4096, CPUQuotaPercent: 50}
	f := newLimitFixture(t, limits, "OOMPolicy=stop\nOOMScoreAdjust=500\nMemoryHigh=infinity\nMemoryMax=8589934592\nMemorySwapMax=0\n"+
		"TasksMax=4096\nCPUWeight=[not set]\nCPUQuotaPerSecUSec=500ms\nIOWeight=[not set]\n")
	f.cgroup(t, limitSlice, "io.max", "")
	remove(t, f.host, "/etc/systemd/system/user@1000.service.d/50-gws-delegate-io.conf")
	f.edit(f.managerShow(), " /etc/systemd/system/user@1000.service.d/50-gws-delegate-io.conf", "")
	f.cgroup(t, limitManagerRoot, "cgroup.controllers", "cpu memory pids\n")
	f.cgroup(t, f.p.Cgroup, "cgroup.controllers", "cpu memory pids\n")
	f.cgroup(t, f.p.Cgroup, "memory.high", "max\n")
	f.cgroup(t, f.p.Cgroup, "cpu.weight", "100\n")
	f.cgroup(t, f.p.Cgroup, "cpu.max", "50000 100000\n")
	if err := f.start(t); err != nil || !f.started() || f.stopped() {
		t.Fatalf("start of a unit without I/O controls = %v: %v", err, f.r.calls)
	}
}

// After start, drift or a failed readback of a unit that still runs fails
// the check. The probe changes nothing: the supervisor stops the unit.
func TestLimitedUnitFailsReadbackAfterStart(t *testing.T) {
	cases := map[string]struct {
		edit func(*testing.T, *limitFixture)
		want error
	}{
		"memory raised":     {func(t *testing.T, f *limitFixture) { f.cgroup(t, f.p.Cgroup, "memory.max", "max\n") }, ErrLimitDrift},
		"swap allowed":      {func(t *testing.T, f *limitFixture) { f.cgroup(t, f.p.Cgroup, "memory.swap.max", "max\n") }, ErrLimitDrift},
		"tasks raised":      {func(t *testing.T, f *limitFixture) { f.cgroup(t, f.p.Cgroup, "pids.max", "8192\n") }, ErrLimitDrift},
		"cpu raised":        {func(t *testing.T, f *limitFixture) { f.cgroup(t, f.p.Cgroup, "cpu.max", "max 100000\n") }, ErrLimitDrift},
		"weight changed":    {func(t *testing.T, f *limitFixture) { f.cgroup(t, f.p.Cgroup, "cpu.weight", "100\n") }, ErrLimitDrift},
		"io cap removed":    {func(t *testing.T, f *limitFixture) { f.cgroup(t, f.p.Cgroup, "io.max", "") }, ErrLimitDrift},
		"io weight changed": {func(t *testing.T, f *limitFixture) { f.cgroup(t, f.p.Cgroup, "io.weight", "default 100\n") }, ErrLimitDrift},
		"controller absent": {func(t *testing.T, f *limitFixture) {
			f.cgroup(t, f.p.Cgroup, "cgroup.controllers", "cpu memory pids\n")
		}, ErrLimitController},
		"interface file missing": {func(t *testing.T, f *limitFixture) { remove(t, f.cgroups, f.p.Cgroup+"/memory.swap.max") }, ErrLimitInterface},
		"file unreadable": {func(t *testing.T, f *limitFixture) {
			remove(t, f.cgroups, f.p.Cgroup+"/pids.max")
			if err := os.Mkdir(filepath.Join(f.cgroups, f.p.Cgroup, "pids.max"), 0o755); err != nil {
				t.Fatal(err)
			}
		}, ErrLimitReadback},
		"controllers unreadable": {func(t *testing.T, f *limitFixture) {
			remove(t, f.cgroups, f.p.Cgroup+"/cgroup.controllers")
		}, ErrLimitReadback},
		"weight provider gone at the device check": {func(t *testing.T, f *limitFixture) {
			f.cgroup(t, "", "io.cost.qos", "")
		}, ErrLimitDevice},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
			// The edit applies once systemd has started the unit.
			c.edit(t, f)
			p := f.p
			f.r.outputs["/usr/bin/true --user show --property=LoadState --property=ActiveState --property=SubState --property=ControlGroup -- "+p.Unit] =
				[]byte("LoadState=loaded\nActiveState=active\nSubState=running\nControlGroup=" + p.Cgroup + "\n")
			err := f.m.verifyStartedLimits(context.Background(), p)
			if !errors.Is(err, ErrLimitCheckFailed) || !errors.Is(err, c.want) || f.stopped() {
				t.Fatalf("readback = %v, want %v; calls %v", err, c.want, f.r.calls)
			}
		})
	}
}

// Start stops the unit it started when the check after start fails, so a
// rollback start never leaves a unit with failed limits running.
func TestStartStopsUnitWhenReadbackFails(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	f.cgroup(t, f.p.Cgroup, "memory.high", "8589934592\n")
	err := f.start(t)
	if !errors.Is(err, ErrLimitCheckFailed) || !errors.Is(err, ErrLimitDrift) || !strings.Contains(err.Error(), "memory.high") || !f.started() || !f.stopped() {
		t.Fatalf("start with drift = %v: %v", err, f.r.calls)
	}
	// The health check reads back before the HTTP check and stops nothing.
	calls := len(f.r.calls)
	if err := f.m.Healthy(context.Background(), f.p.ID); !errors.Is(err, ErrLimitCheckFailed) || slices.Contains(f.r.calls[calls:], "/usr/bin/true --user stop -- "+f.p.Unit) {
		t.Fatalf("health with drift = %v", err)
	}
}

// A unit that stopped by itself has no cgroup to read. Its health fails the
// usual way, without a limit failure and without another stop.
func TestReadbackOfStoppedUnitIsNotALimitFailure(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	if err := os.RemoveAll(filepath.Join(f.cgroups, f.p.Cgroup)); err != nil {
		t.Fatal(err)
	}
	err := f.m.verifyStartedLimits(context.Background(), f.p)
	if err == nil || errors.Is(err, ErrLimitCheckFailed) || !errors.Is(err, ErrLimitReadback) || f.stopped() {
		t.Fatalf("readback of a stopped unit = %v: %v", err, f.r.calls)
	}
	// An inactive unit whose cgroup still has processes is a failure.
	f.cgroup(t, f.p.Cgroup, "cgroup.events", "populated 1\n")
	f.cgroup(t, f.p.Cgroup, "cgroup.controllers", "cpu io memory pids\n")
	if err := os.Mkdir(filepath.Join(f.cgroups, f.p.Cgroup, "memory.high"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := f.m.verifyStartedLimits(context.Background(), f.p); !errors.Is(err, ErrLimitCheckFailed) || !errors.Is(err, ErrLimitReadback) {
		t.Fatalf("readback of an inactive populated unit = %v", err)
	}
	if err := os.RemoveAll(filepath.Join(f.cgroups, f.p.Cgroup)); err != nil {
		t.Fatal(err)
	}
	// While the unit still runs, the same missing cgroup is a failure.
	f.r.outputs["/usr/bin/true --user show --property=LoadState --property=ActiveState --property=SubState --property=ControlGroup -- "+f.p.Unit] =
		[]byte("LoadState=loaded\nActiveState=active\nSubState=running\nControlGroup=" + f.p.Cgroup + "\n")
	if err := f.m.verifyStartedLimits(context.Background(), f.p); !errors.Is(err, ErrLimitCheckFailed) || f.stopped() {
		t.Fatalf("readback of a running unit without cgroup = %v", err)
	}
}

func TestBFQWeightReadback(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	f.cgroup(t, "", "io.cost.qos", "")
	f.file(t, "/sys/dev/block/259:0/queue/scheduler", "mq-deadline [bfq] none\n")
	f.cgroup(t, f.p.Cgroup, "io.bfq.weight", "default 136\n")
	if err := f.start(t); err != nil || f.stopped() {
		t.Fatalf("start with BFQ = %v", err)
	}
	f.cgroup(t, f.p.Cgroup, "io.bfq.weight", "default 500\n")
	if err := f.m.verifyStartedLimits(context.Background(), f.p); !errors.Is(err, ErrLimitDrift) {
		t.Fatalf("BFQ drift = %v", err)
	}
	for weight, want := range map[uint64]uint64{1: 1, 100: 100, 500: 136, 1000: 181, 10000: 1000} {
		if got := bfqWeight(weight); got != want {
			t.Fatalf("bfqWeight(%d) = %d, want %d", weight, got, want)
		}
	}
}

func TestFormatTimespanMatchesSystemd(t *testing.T) {
	for usec, want := range map[uint64]string{0: "0", 1: "1us", 1500: "1.500ms", 10000: "10ms", 500000: "500ms", 1500000: "1.500000s",
		4000000: "4s", 61500000: "1min 1.500000s", 1024000000: "17min 4s", 3600000000: "1h", 90061000000: "1d 1h 1min 1s"} {
		if got := formatTimespan(usec); got != want {
			t.Fatalf("formatTimespan(%d) = %q, want %q", usec, got, want)
		}
	}
}

func TestExpectedLimitPropertiesForUnsetControls(t *testing.T) {
	got := map[string][]string{}
	for _, property := range expectedLimitProperties(control.ResourceLimits{Version: 1, TasksMax: 64}) {
		got[property.name] = property.want
	}
	for name, want := range map[string][]string{"MemoryHigh": {"infinity"}, "MemoryMax": {"infinity"}, "MemorySwapMax": {"infinity"},
		"CPUWeight": {"[not set]"}, "CPUQuotaPerSecUSec": {"infinity"}, "IOWeight": {"[not set]"}, "IOReadBandwidthMax": nil, "TasksMax": {"64"}} {
		if !slices.Equal(got[name], want) {
			t.Fatalf("%s = %q, want %q", name, got[name], want)
		}
	}
	// The launch rule requires memoryMaxMiB, tasksMax and cpuQuotaPercent,
	// so the readback always reads memory, pids and cpu.
	files := expectedLimitFiles(control.ResourceLimits{Version: 1, MemoryMaxMiB: 512, MemorySwapMaxMiB: 256, TasksMax: 64, CPUQuotaPercent: 50}, "")
	want := []limitFile{{"memory.high", "max", "memory", false}, {"memory.max", "536870912", "memory", false}, {"memory.swap.max", "268435456", "swap accounting", false},
		{"pids.max", "64", "pids", false}, {"cpu.weight", "100", "cpu", false}, {"cpu.max", "50000 100000", "cpu", false}, {"cpu.max.burst", "0", "cpu burst", true}}
	if !slices.Equal(files, want) {
		t.Fatalf("files = %+v", files)
	}
	files = expectedLimitFiles(control.ResourceLimits{Version: 1, MemoryMaxMiB: 512, TasksMax: 64, CPUQuotaPercent: 50, IODeviceNumber: "8:0", IOWriteIOPSMax: 100}, "")
	if io := files[len(files)-1]; io != (limitFile{"io.max", "8:0 rbps=max wbps=max riops=max wiops=100", "I/O throttling", false}) {
		t.Fatalf("io.max file = %+v", io)
	}
}

func TestCheckReserveNamesAllowedValue(t *testing.T) {
	c := hostCap{memoryHigh: 1 * gib, memoryMax: 2 * gib, tasks: 100, cpu: 50000, io: map[string]uint64{}}
	err := checkReserve(control.ResourceLimits{Version: 1, MemoryMaxMiB: 512, TasksMax: 64, CPUQuotaPercent: 1}, c)
	for _, part := range []string{"raise the slice cap to at least 2304 MiB", "raise the slice cap to at least 576 tasks", "raise the slice cap to at least 101 percent"} {
		if !errors.Is(err, ErrLimitReserve) || !strings.Contains(err.Error(), part) {
			t.Fatalf("reserve error = %v, missing %q", err, part)
		}
	}
	c = hostCap{memoryHigh: 4 * gib, memoryMax: 8 * gib, tasks: 1000, cpu: 3000000, io: map[string]uint64{"wiops": 1000}}
	err = checkReserve(control.ResourceLimits{Version: 1, MemoryHighMiB: 3072, MemoryMaxMiB: 1024, TasksMax: 600, CPUQuotaPercent: 300, IOWriteIOPSMax: 1000}, c)
	for _, part := range []string{"use 2048 MiB or less", "use 488 tasks or less", "use 200 percent or less", "use 900 operations per second or less for ioWriteIOPSMax"} {
		if !strings.Contains(err.Error(), part) {
			t.Fatalf("reserve error = %v, missing %q", err, part)
		}
	}
}

func TestTrustedFileRejectsACL(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	name := filepath.Join(f.host, "etc/systemd/system/user-1000.slice.d/50-gws-host-cap.conf")
	// A minimal access ACL: owner rw, one named user r, group r, mask r, other r.
	acl := []byte{2, 0, 0, 0, 1, 0, 6, 0, 0xff, 0xff, 0xff, 0xff, 2, 0, 4, 0, 0xe8, 3, 0, 0, 4, 0, 4, 0, 0xff, 0xff, 0xff, 0xff,
		0x10, 0, 4, 0, 0xff, 0xff, 0xff, 0xff, 0x20, 0, 4, 0, 0xff, 0xff, 0xff, 0xff}
	if err := unix.Setxattr(name, "system.posix_acl_access", acl, 0); err != nil {
		t.Skipf("file system without ACLs: %v", err)
	}
	if err := f.start(t); !errors.Is(err, ErrHostCap) || !strings.Contains(err.Error(), "ACL") {
		t.Fatalf("start with an ACL on the host cap = %v", err)
	}
}

func TestDefaultLimitHostReadsDeviceWithoutFollowingLinks(t *testing.T) {
	h := defaultLimitHost()
	if h.root != "/" || h.owner != 0 || h.uid != os.Getuid() || h.sliceUnit() != "user-"+strconv.Itoa(os.Getuid())+".slice" {
		t.Fatalf("default host = %+v", h)
	}
	stat, err := h.lstat("/dev/null")
	if err != nil || stat.Mode&unix.S_IFMT != unix.S_IFCHR {
		t.Fatalf("stat /dev/null = %+v, %v", stat, err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink("/dev/null", link); err != nil {
		t.Fatal(err)
	}
	if stat, err := h.lstat(link); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFLNK {
		t.Fatalf("stat of a link = %+v, %v", stat, err)
	}
}

// The protection check before a retain repeats every launch check except
// the admission memory check: the running unit already holds its memory.
func TestVerifyLimitProtectionLeavesOutAdmissionMemory(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	ctx := context.Background()
	f.cgroup(t, limitSlice, "memory.current", "60000000000\n")
	if err := f.m.VerifyLimitProtection(ctx, f.p.ID); err != nil {
		t.Fatalf("protection of a running unit = %v", err)
	}
	if err := f.m.VerifyLimitProtection(ctx, control.WorkloadText); err != nil {
		t.Fatalf("protection of an unlimited unit = %v", err)
	}
	f.edit(f.limitShow(), "OOMPolicy=stop", "OOMPolicy=continue")
	if err := f.m.VerifyLimitProtection(ctx, f.p.ID); !errors.Is(err, ErrLimitProperties) || f.stopped() {
		t.Fatalf("protection with changed properties = %v: %v", err, f.r.calls)
	}
}

// Each health check repeats the host protection checks, so no path reports
// a unit with limits healthy after its protection is gone.
func TestHealthFailsWhenProtectionFails(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	if err := f.start(t); err != nil {
		t.Fatal(err)
	}
	remove(t, f.host, "/etc/systemd/system/user-1000.slice.d/50-gws-host-cap.conf")
	err := f.m.Healthy(context.Background(), f.p.ID)
	if !errors.Is(err, ErrLimitCheckFailed) || !errors.Is(err, ErrHostCap) || f.stopped() {
		t.Fatalf("health without host cap = %v: %v", err, f.r.calls)
	}
}

// A limit equal to its cap minus the reserve fits; one more unit does not.
func TestCheckReserveBoundaries(t *testing.T) {
	c := hostCap{memoryHigh: 8 * gib, memoryMax: 10 * gib, tasks: 1000, cpu: 3000000, io: map[string]uint64{"rbps": 100 << 20}}
	exact := control.ResourceLimits{Version: 1, MemoryHighMiB: 6144, MemoryMaxMiB: 6144, TasksMax: 488, CPUQuotaPercent: 200,
		IOReadBandwidthMax: (100 << 20) - (100<<20)/10}
	if err := checkReserve(exact, c); err != nil {
		t.Fatalf("limits at the boundary = %v", err)
	}
	for name, edit := range map[string]func(*control.ResourceLimits){
		"memory high": func(l *control.ResourceLimits) { l.MemoryHighMiB++ },
		"memory max":  func(l *control.ResourceLimits) { l.MemoryMaxMiB++ },
		"tasks":       func(l *control.ResourceLimits) { l.TasksMax++ },
		"cpu":         func(l *control.ResourceLimits) { l.CPUQuotaPercent++ },
		"io":          func(l *control.ResourceLimits) { l.IOReadBandwidthMax++ },
	} {
		over := exact
		edit(&over)
		if err := checkReserve(over, c); !errors.Is(err, ErrLimitReserve) {
			t.Fatalf("%s above the boundary = %v", name, err)
		}
	}
	// The slice memory.max below memory.high is the memory ceiling.
	c.memoryMax = 7 * gib
	if err := checkReserve(control.ResourceLimits{Version: 1, MemoryMaxMiB: 6144, TasksMax: 64, CPUQuotaPercent: 1}, c); !errors.Is(err, ErrLimitReserve) ||
		!strings.Contains(err.Error(), "use 5120 MiB or less") {
		t.Fatalf("reserve against a lower slice memory.max = %v", err)
	}
}

// Slice memory in use plus unit MemoryMax plus the reserve can equal the
// ceiling, the lower of memory.high and memory.max, but not exceed it.
func TestAdmitMemoryBoundary(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	c := hostCap{slice: limitSlice, memoryHigh: 20 * gib, memoryMax: 16 * gib}
	l := control.ResourceLimits{Version: 1, MemoryMaxMiB: 8192}
	for current, ok := range map[uint64]bool{6 * gib: true, 6*gib + 1: false, 16 * gib: false, 17 * gib: false} {
		f.cgroup(t, limitSlice, "memory.current", strconv.FormatUint(current, 10)+"\n")
		if err := f.m.admitMemory(c, l); (err == nil) != ok || (!ok && !errors.Is(err, ErrLimitMemory)) {
			t.Fatalf("admission with %d bytes in use = %v", current, err)
		}
	}
	f.cgroup(t, limitSlice, "memory.current", "lots\n")
	if err := f.m.admitMemory(c, l); !errors.Is(err, ErrLimitReadback) || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("admission with malformed usage = %v", err)
	}
}

// The slice CPU quota must have an exact value in microseconds per second
// that does not overflow.
func TestSliceCPUQuotaIsExact(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	for value, want := range map[string]uint64{"1400000 100000": 14000000, "1 4": 250000, "1 3": 0, "18446744073709 1": 18446744073709000000,
		"18446744073710 1": 0, "max 100000": 0, "100 0": 0, "max": 0, "100000 100000 1": 0} {
		f.cgroup(t, limitSlice, "cpu.max", value+"\n")
		got, err := f.m.readSliceCPU(limitSlice)
		if got != want || (want == 0) != errors.Is(err, ErrHostCap) {
			t.Fatalf("slice cpu.max %q = %d, %v; want %d", value, got, err, want)
		}
	}
}

// A failed read of the weight provider files is a readback failure, not a
// missing provider. A kernel without iocost has no io.cost.qos file.
func TestIOWeightProviderClassifiesReadFailures(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	remove(t, f.cgroups, "io.cost.qos")
	if _, err := f.m.ioWeightProvider("259:0"); !errors.Is(err, ErrLimitDevice) {
		t.Fatalf("provider without io.cost.qos = %v", err)
	}
	if err := os.Mkdir(filepath.Join(f.cgroups, "io.cost.qos"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.ioWeightProvider("259:0"); !errors.Is(err, ErrLimitReadback) {
		t.Fatalf("provider with unreadable io.cost.qos = %v", err)
	}
	remove(t, f.host, "/sys/dev/block/259:0/queue/scheduler")
	if _, err := f.m.ioWeightProvider("259:0"); !errors.Is(err, ErrLimitReadback) {
		t.Fatalf("provider without scheduler = %v", err)
	}
}

// The supervisor runs these checks before it unloads the current workload.
// They leave out the slice memory, which the current workload still holds.
func TestVerifyLaunchLimitsBeforeUnload(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	ctx := context.Background()
	f.cgroup(t, limitSlice, "memory.current", "60000000000\n")
	if err := f.m.VerifyLaunchLimits(ctx, f.p.ID); err != nil {
		t.Fatalf("launch checks with memory in use = %v", err)
	}
	if err := f.m.VerifyLaunchLimits(ctx, control.WorkloadText); err != nil {
		t.Fatalf("launch checks of an unlimited unit = %v", err)
	}
	remove(t, f.host, "/etc/systemd/system/user-1000.slice.d/50-gws-host-cap.conf")
	if err := f.m.VerifyLaunchLimits(ctx, f.p.ID); !errors.Is(err, ErrHostCap) || f.started() {
		t.Fatalf("launch checks without host cap = %v", err)
	}
	f.p.NativeModel.LaunchFile += ".missing"
	f.m.config.Catalog.Profiles[2] = f.p
	if err := f.m.VerifyLaunchLimits(ctx, f.p.ID); !errors.Is(err, ErrLaunchUnsupported) || !DefiniteBindingFailure(err) || errors.Is(err, ErrHostCap) {
		t.Fatalf("launch checks with a changed binding = %v", err)
	}
}

func TestVerifyLimitProtectionReadsBack(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	f.cgroup(t, f.p.Cgroup, "pids.max", "max\n")
	if err := f.m.VerifyLimitProtection(context.Background(), f.p.ID); !errors.Is(err, ErrLimitDrift) || f.stopped() {
		t.Fatalf("protection with drift = %v", err)
	}
}

func TestSliceFileRejectsACL(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	acl := []byte{2, 0, 0, 0, 1, 0, 6, 0, 0xff, 0xff, 0xff, 0xff, 2, 0, 4, 0, 0xe8, 3, 0, 0, 4, 0, 4, 0, 0xff, 0xff, 0xff, 0xff,
		0x10, 0, 4, 0, 0xff, 0xff, 0xff, 0xff, 0x20, 0, 4, 0, 0xff, 0xff, 0xff, 0xff}
	if err := unix.Setxattr(filepath.Join(f.cgroups, limitSlice, "memory.max"), "system.posix_acl_access", acl, 0); err != nil {
		t.Skipf("file system without ACLs: %v", err)
	}
	if err := f.start(t); !errors.Is(err, ErrHostCap) || !strings.Contains(err.Error(), "ACL") {
		t.Fatalf("start with an ACL on a slice file = %v", err)
	}
}

// On a merged /usr host, systemd can name vendor drop-ins under /lib, a
// root-owned link to usr/lib.
func TestVendorDropInUnderMergedLib(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	f.edit(f.sliceShow(), "DropInPaths=/usr/lib/", "DropInPaths=/lib/")
	if err := os.Symlink("usr/lib", filepath.Join(f.host, "lib")); err != nil {
		t.Fatal(err)
	}
	if err := f.start(t); err != nil || !f.started() {
		t.Fatalf("start with a vendor drop-in under /lib = %v", err)
	}
	// The /lib link is read through the host status seam. A read failure
	// proves nothing.
	f.m.host.lstat = func(name string) (unix.Stat_t, error) {
		if name == filepath.Join(f.host, "lib") {
			return unix.Stat_t{}, unix.EACCES
		}
		return f.lstat(name)
	}
	if err := f.m.VerifyLaunchLimits(context.Background(), f.p.ID); !errors.Is(err, ErrLimitReadback) || DefiniteLimitFailure(err) || !strings.Contains(err.Error(), "/lib") {
		t.Fatalf("launch checks with an unreadable /lib link = %v", err)
	}
	// Another owner of the link is definite.
	f.m.host.lstat = f.lstat
	f.giveOtherOwner(t, filepath.Join(f.host, "lib"))
	if err := f.m.VerifyLaunchLimits(context.Background(), f.p.ID); !errors.Is(err, ErrHostCap) || !strings.Contains(err.Error(), "root-owned /lib link") {
		t.Fatalf("launch checks with a /lib link of another owner = %v", err)
	}
}

func TestDefiniteLimitFailure(t *testing.T) {
	for err, want := range map[error]bool{
		nil: false, ErrLimitUnchecked: true, ErrLimitCheckFailed: true, fmt.Errorf("%w: %w", ErrLimitCheckFailed, ErrLimitReadback): true, ErrHostCap: true, ErrLimitDrift: true, ErrLimitProperties: true, ErrLimitDevice: true, ErrLimitReserve: true,
		ErrLimitController: true, ErrLimitInterface: true, ErrLimitReadback: false, ErrLimitMemory: false, errors.New("bus"): false,
		fmt.Errorf("%w: %w", ErrHostCap, context.DeadlineExceeded): false, fmt.Errorf("%w: %w", ErrLimitDrift, context.Canceled): false,
	} {
		if got := DefiniteLimitFailure(err); got != want {
			t.Fatalf("DefiniteLimitFailure(%v) = %v", err, got)
		}
	}
}

// A failed system manager call at a health check is not a definite limit
// failure, so the supervisor retries it.
func TestHealthRetriesTransientCommandFailure(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	f.r.errs = map[string]error{f.sliceShow(): errors.New("bus")}
	if err := f.m.verifyStartedLimits(context.Background(), f.p); err == nil || errors.Is(err, ErrLimitCheckFailed) {
		t.Fatalf("health with a failed show = %v", err)
	}
	// A failed state reread after a readback failure leaves the result open.
	f.r.errs = map[string]error{"/usr/bin/true --user show --property=LoadState --property=ActiveState --property=SubState --property=ControlGroup -- " + f.p.Unit: errors.New("bus")}
	remove(t, f.cgroups, f.p.Cgroup+"/pids.max")
	if err := os.Mkdir(filepath.Join(f.cgroups, f.p.Cgroup, "pids.max"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := f.m.verifyStartedLimits(context.Background(), f.p); !errors.Is(err, ErrLimitReadback) || errors.Is(err, ErrLimitCheckFailed) {
		t.Fatalf("readback with an unknown state = %v", err)
	}
}

func TestTrustedFileRejectsFileCapability(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	name := filepath.Join(f.host, "etc/systemd/system/user-1000.slice.d/50-gws-host-cap.conf")
	// A version 2 capability set with no bits.
	capability := []byte{0, 0, 0, 2, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	if err := unix.Setxattr(name, "security.capability", capability, 0); err != nil {
		t.Skipf("cannot set a file capability here: %v", err)
	}
	if err := f.start(t); !errors.Is(err, ErrHostCap) || !strings.Contains(err.Error(), "file capability") || f.started() {
		t.Fatalf("start with a file capability on the host cap = %v", err)
	}
}

// The capability check also runs without root: the fixture reports a file
// capability on the host cap drop-in through the attribute lookup.
func TestTrustedFileRejectsReportedFileCapability(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	var target unix.Stat_t
	if err := unix.Stat(filepath.Join(f.host, "etc/systemd/system/user-1000.slice.d/50-gws-host-cap.conf"), &target); err != nil {
		t.Fatal(err)
	}
	f.m.host.hasXattr = func(fd int, attribute string) (bool, error) {
		var stat unix.Stat_t
		if err := unix.Fstat(fd, &stat); err == nil && stat.Ino == target.Ino && attribute == "security.capability" {
			return true, nil
		}
		return hasXattr(fd, attribute)
	}
	if err := f.start(t); !errors.Is(err, ErrHostCap) || !strings.Contains(err.Error(), "file capability") || f.started() {
		t.Fatalf("start with a reported file capability on the host cap = %v", err)
	}
}

// The trusted owner is root. A drop-in, a parent directory or a slice file
// with another owner fails the host cap.
func TestHostCapRequiresTrustedOwner(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	f.m.host.owner = uint32(os.Getuid()) + 1
	f.device.Uid = f.m.host.owner
	if err := f.start(t); !errors.Is(err, ErrHostCap) || !strings.Contains(err.Error(), "/systemd/system/") || f.started() {
		t.Fatalf("start with another owner = %v", err)
	}
	for name, owned := range map[string]string{
		"parent directory": "/etc/systemd/system",
		"slice file":       "",
		"lib link":         "lib",
	} {
		t.Run(name, func(t *testing.T) {
			g := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
			target := filepath.Join(g.host, owned)
			switch owned {
			case "":
				target = filepath.Join(g.cgroups, limitSlice, "memory.max")
			case "lib":
				g.edit(g.sliceShow(), "DropInPaths=/usr/lib/", "DropInPaths=/lib/")
				if err := os.Symlink("usr/lib", target); err != nil {
					t.Fatal(err)
				}
			}
			g.giveOtherOwner(t, target)
			if err := g.start(t); !errors.Is(err, ErrHostCap) || g.started() {
				t.Fatalf("start with another owner of the %s = %v", name, err)
			}
		})
	}
}

// A unit without I/O controls does not need the delegation drop-in. When
// systemd lists it anyway, it must still be trusted.
func TestDelegationDropInTrustedWithoutIO(t *testing.T) {
	limits := control.ResourceLimits{Version: 1, MemoryMaxMiB: 8192, TasksMax: 4096, CPUQuotaPercent: 100}
	f := newLimitFixture(t, limits, "OOMPolicy=stop\nOOMScoreAdjust=500\nMemoryHigh=infinity\nMemoryMax=8589934592\nMemorySwapMax=0\n"+
		"TasksMax=4096\nCPUWeight=[not set]\nCPUQuotaPerSecUSec=1s\nIOWeight=[not set]\n")
	f.cgroup(t, limitSlice, "io.max", "")
	f.cgroup(t, f.p.Cgroup, "memory.high", "max\n")
	f.cgroup(t, f.p.Cgroup, "cpu.weight", "100\n")
	f.cgroup(t, f.p.Cgroup, "cpu.max", "100000 100000\n")
	chmod(t, f.host, "/etc/systemd/system/user@1000.service.d/50-gws-delegate-io.conf", 0o666)
	if err := f.start(t); !errors.Is(err, ErrHostCap) || !strings.Contains(err.Error(), "50-gws-delegate-io.conf") || f.started() {
		t.Fatalf("start with a writable delegation drop-in = %v", err)
	}
	chmod(t, f.host, "/etc/systemd/system/user@1000.service.d/50-gws-delegate-io.conf", 0o644)
	if err := f.start(t); err != nil || !f.started() {
		t.Fatalf("start without I/O controls = %v", err)
	}
}

// Only proven mismatches are ErrHostCap. Other open and read errors are
// readback failures that a later probe can pass.
func TestHostFileErrorClasses(t *testing.T) {
	for err, definite := range map[error]bool{unix.ENOENT: true, unix.ELOOP: true, unix.ENOTDIR: true, errUntrusted: true,
		unix.EACCES: false, unix.EIO: false, unix.EMFILE: false} {
		got := hostFileError("/etc/x", err)
		if errors.Is(got, ErrHostCap) != definite || errors.Is(got, ErrLimitReadback) == definite {
			t.Fatalf("hostFileError(%v) = %v", err, got)
		}
	}
}

// A set-property drop-in on a running unit with limits changes its binding:
// that is a definite limit failure.
func TestChangedBindingOfLimitedUnitIsDefinite(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	f.edit(f.bindingShow(), "DropInPaths=", "DropInPaths=/run/user/1000/systemd/system.control/"+f.p.Unit+".d/50-MemoryMax.conf")
	for name, check := range map[string]func() error{
		"health":    func() error { return f.m.Healthy(context.Background(), f.p.ID) },
		"retention": func() error { return f.m.VerifyLimitProtection(context.Background(), f.p.ID) },
	} {
		if err := check(); !errors.Is(err, ErrLimitCheckFailed) || !errors.Is(err, ErrLaunchChanged) {
			t.Fatalf("%s with a set-property drop-in = %v", name, err)
		}
	}
}

// The retention check treats an unreadable limit file of a running unit as
// definite, as the health check does.
func TestRetentionTreatsUnreadableLimitAsDefinite(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	f.r.outputs[f.unitShow()] = []byte("LoadState=loaded\nActiveState=active\nSubState=running\nControlGroup=" + f.p.Cgroup + "\n")
	remove(t, f.cgroups, f.p.Cgroup+"/pids.max")
	if err := os.Mkdir(filepath.Join(f.cgroups, f.p.Cgroup, "pids.max"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := f.m.VerifyLimitProtection(context.Background(), f.p.ID); !errors.Is(err, ErrLimitCheckFailed) || !DefiniteLimitFailure(err) {
		t.Fatalf("retention with an unreadable limit file = %v", err)
	}
}

// Start stops the unit on its own bounded context: a spent start context
// does not keep the unit running.
func TestStartCleanupSurvivesSpentContext(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	f.cgroup(t, f.p.Cgroup, "memory.high", "8589934592\n")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.r.afterStart = cancel
	err := f.m.Start(ctx, f.p.ID)
	if !errors.Is(err, context.Canceled) || !f.stopped() {
		t.Fatalf("start with a spent context = %v: %v", err, f.r.calls)
	}
	if state, stateErr := f.m.unitState(context.Background(), f.p.Unit); stateErr != nil || state.active != "inactive" {
		t.Fatalf("unit after cleanup = %+v, %v", state, stateErr)
	}
}

// Device read failures other than a missing file are readback failures.
func TestDeviceReadFailuresAreNotDefinite(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	f.deviceErr = unix.EIO
	if _, err := f.m.verifyLimitDevice(fixtureLimits()); !errors.Is(err, ErrLimitReadback) || DefiniteLimitFailure(err) {
		t.Fatalf("device stat failure = %v", err)
	}
	f.deviceErr = nil
	slaves := filepath.Join(f.host, "sys/dev/block/259:0/slaves")
	if err := os.Remove(slaves); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(slaves, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.verifyLimitDevice(fixtureLimits()); !errors.Is(err, ErrLimitReadback) {
		t.Fatalf("unreadable slaves = %v", err)
	}
	if err := checkLeafDevice(filepath.Join(f.host, "missing"), "/dev/x"); !errors.Is(err, ErrLimitDevice) {
		t.Fatalf("missing device directory = %v", err)
	}
	if err := os.RemoveAll(filepath.Join(f.host, "sys/dev/block/259:0/queue")); err != nil {
		t.Fatal(err)
	}
	f.file(t, "/sys/dev/block/259:0/queue", "not a directory")
	if _, err := f.m.verifyLimitDevice(fixtureLimits()); !errors.Is(err, ErrLimitDevice) || !strings.Contains(err.Error(), "request queue") {
		t.Fatalf("queue file = %v", err)
	}
}

// A failed read of the manager-root swap file proves nothing; a missing one
// means swap accounting is off.
func TestManagerSwapReadFailureIsNotDefinite(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	remove(t, f.cgroups, limitManagerRoot+"/memory.swap.max")
	if err := os.Mkdir(filepath.Join(f.cgroups, limitManagerRoot, "memory.swap.max"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := f.start(t); !errors.Is(err, ErrLimitReadback) || DefiniteLimitFailure(err) || f.started() {
		t.Fatalf("start with an unreadable swap file = %v", err)
	}
}

// A limit file that goes missing because the group went away is a readback
// failure, not a missing kernel interface.
func TestReadbackGroupGoneIsNotInterfaceMissing(t *testing.T) {
	limits := control.ResourceLimits{Version: 1, MemoryMaxMiB: 8192, TasksMax: 4096}
	f := newLimitFixture(t, limits, "")
	f.cgroup(t, f.p.Cgroup, "memory.high", "max\n")
	opens := 0
	// /proc gives the resolved path, so resolve a symlinked TMPDIR too.
	group, err := filepath.EvalSymlinks(filepath.Join(f.cgroups, f.p.Cgroup))
	if err != nil {
		t.Fatal(err)
	}
	f.m.cgroups.verify = func(fd int) error {
		// The readback opens the unit group first for cgroup.controllers,
		// then for memory.high. The group goes away once its directory is
		// open for memory.high, whatever the depth of the group path.
		if path, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", fd)); err == nil && path == group {
			opens++
			if opens == 2 {
				if err := os.RemoveAll(group); err != nil {
					t.Error(err)
				}
			}
		}
		return nil
	}
	err = f.m.readbackLimits(f.p.Cgroup, limits, "")
	if !errors.Is(err, ErrLimitReadback) || errors.Is(err, ErrLimitInterface) || !strings.Contains(err.Error(), "memory.high") ||
		!errors.Is(err, errCgroupRemoved) {
		t.Fatalf("readback with the group gone = %v", err)
	}
	// With the group present, the same missing file is an interface failure.
	f.m.cgroups.verify = func(int) error { return nil }
	f.cgroup(t, f.p.Cgroup, "cgroup.controllers", "cpu io memory pids\n")
	if err := f.m.readbackLimits(f.p.Cgroup, limits, ""); !errors.Is(err, ErrLimitInterface) {
		t.Fatalf("readback with a missing file = %v", err)
	}
}

// Escape routes are advisory, so no runtime path reads them: a unit with
// limits passes its launch checks, starts, passes the limit checks of a
// health check and is kept on a host where the scan reports routes.
// The cron deny file of the host does not deny the account. The health
// step calls checkStartedUnit, the limit part of Healthy, because the
// fixture has no health endpoint.
func TestLimitedUnitRunsWithoutEscapeRouteCheck(t *testing.T) {
	m, err := newSystemdManager(testConfig(), &fakeRunner{}, http.DefaultClient)
	if err != nil || m.host.ownerOf(unix.Stat_t{Uid: 7}) != 7 {
		t.Fatalf("default runtime = %v", err)
	}
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	f.file(t, "/etc/passwd", "alice:x:1000:1000::/home/alice:/bin/sh\n")
	f.file(t, "/etc/group", "alice:x:1000:\n")
	f.file(t, "/etc/cron.deny", "")
	ctx := context.Background()
	// The checks run in this order: the unit runs after the start.
	for _, step := range []struct {
		name  string
		check func() error
	}{
		{"before unload", func() error { return f.m.VerifyLaunchLimits(ctx, f.p.ID) }},
		{"start", func() error { return f.m.Start(ctx, f.p.ID) }},
		{"health", func() error { return f.m.checkStartedUnit(ctx, f.p) }},
		{"retention", func() error { return f.m.VerifyLimitProtection(ctx, f.p.ID) }},
	} {
		if err := step.check(); err != nil {
			t.Fatalf("%s of a limited unit with open routes = %v", step.name, err)
		}
	}
	if !f.started() {
		t.Fatal("the limited unit did not start")
	}
	for _, call := range f.r.calls {
		if strings.Contains(call, "--property=User") || strings.Contains(call, "sudo") || strings.Contains(call, "pkcheck") {
			t.Fatalf("a limit check ran an escape route command: %s", call)
		}
	}
	// The scan of the same host reports routes.
	scanner := escapeScanner{host: f.m.host, run: f.r.Run, systemctl: "/usr/bin/true", probe: escapeProbe{verify: func(int) error { return nil },
		processGroups: func() ([]uint32, error) { return nil, nil }, socketACL: func(string) (bool, error) { return false, nil }}}
	if routes := scanner.scan(ctx); !slices.Contains(routes, EscapeRoute{Route: "cron", Item: "/etc/cron.deny does not deny alice"}) {
		t.Fatalf("the scan of the fixture host = %+v", routes)
	}
}

// A failed systemctl call proves no binding change: it is not definite.
func TestBindingCommandFailureIsNotDefinite(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	f.r.errs = map[string]error{f.bindingShow(): errors.New("bus")}
	err := f.m.VerifyLimitProtection(context.Background(), f.p.ID)
	if !errors.Is(err, ErrLaunchChanged) || DefiniteBindingFailure(err) || DefiniteLimitFailure(err) || errors.Is(err, ErrLimitCheckFailed) ||
		err.Error() != "cannot inspect the launch binding: command failed" {
		t.Fatalf("binding with a failed command = %v", err)
	}
	if !DefiniteBindingFailure(ErrLaunchUnsupported) || DefiniteBindingFailure(fmt.Errorf("%w: %w", ErrLaunchChanged, context.Canceled)) || DefiniteBindingFailure(nil) {
		t.Fatal("binding failure classes")
	}
}

// The started-unit check reads the I/O weight provider once, in the device
// check. Without a provider it fails as a device failure. A failed read of
// the scheduler file proves no limit, so it is not definite.
func TestStartedCheckWeightProvider(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	f.r.outputs[f.unitShow()] = []byte("LoadState=loaded\nActiveState=active\nSubState=running\nControlGroup=" + f.p.Cgroup + "\n")
	f.cgroup(t, "", "io.cost.qos", "")
	if err := f.m.verifyStartedLimits(context.Background(), f.p); !errors.Is(err, ErrLimitCheckFailed) || !errors.Is(err, ErrLimitDevice) ||
		!strings.Contains(err.Error(), "I/O weight") {
		t.Fatalf("check without a weight provider = %v", err)
	}
	remove(t, f.host, "/sys/dev/block/259:0/queue/scheduler")
	if err := f.m.verifyStartedLimits(context.Background(), f.p); !errors.Is(err, ErrLimitReadback) || DefiniteLimitFailure(err) {
		t.Fatalf("check without a scheduler file = %v", err)
	}
	// With BFQ, the readback also reads io.bfq.weight.
	f.file(t, "/sys/dev/block/259:0/queue/scheduler", "mq-deadline [bfq] none\n")
	if err := f.m.verifyStartedLimits(context.Background(), f.p); !errors.Is(err, ErrLimitInterface) || !strings.Contains(err.Error(), "io.bfq.weight") {
		t.Fatalf("check with BFQ and no io.bfq.weight = %v", err)
	}
	f.cgroup(t, f.p.Cgroup, "io.bfq.weight", "default 136\n")
	if err := f.m.verifyStartedLimits(context.Background(), f.p); err != nil {
		t.Fatalf("check with BFQ = %v", err)
	}
}

// NeedDaemonReload on the slice or the user manager proves no wrong cap. On
// a running unit, it fails the check without a definite failure, so the unit
// is not stopped.
func TestDaemonReloadAfterStartIsNotDefinite(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	f.edit(f.sliceShow(), "NeedDaemonReload=no", "NeedDaemonReload=yes")
	err := f.m.verifyStartedLimits(context.Background(), f.p)
	if !errors.Is(err, ErrLimitReadback) || DefiniteLimitFailure(err) || errors.Is(err, ErrLimitCheckFailed) || !strings.Contains(err.Error(), "daemon-reload") {
		t.Fatalf("check with a pending daemon-reload = %v", err)
	}
}

// Start owns the unit it started: it stops it after any failed check after
// start, a failure that proves nothing included, and it joins a failed stop.
func TestStartCleanupOutcomes(t *testing.T) {
	t.Run("failed stop is joined", func(t *testing.T) {
		f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
		f.cgroup(t, f.p.Cgroup, "memory.high", "8589934592\n")
		stopErr := errors.New("stop refused")
		f.r.errs = map[string]error{"/usr/bin/true --user stop -- " + f.p.Unit: stopErr}
		err := f.start(t)
		if !errors.Is(err, ErrLimitCheckFailed) || !errors.Is(err, ErrLimitDrift) || !strings.Contains(err.Error(), "systemctl stop") {
			t.Fatalf("start with a failed cleanup stop = %v", err)
		}
	})
	t.Run("failure that proves nothing", func(t *testing.T) {
		f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
		f.r.afterStart = func() { f.r.errs = map[string]error{f.sliceShow(): errors.New("bus")} }
		err := f.start(t)
		if err == nil || DefiniteLimitFailure(err) || !f.stopped() {
			t.Fatalf("start with a failed probe after start = %v: %v", err, f.r.calls)
		}
	})
}

// A failed GPU identity query proves nothing about the binding.
func TestGPUIdentityQueryFailureIsNotDefinite(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	f.p.NativeModel.GPUUUID = "GPU-01234567-89ab-cdef-0123-456789abcdef"
	f.r.errs = map[string]error{"/usr/bin/true --query-gpu=uuid --format=csv,noheader -i 0": errors.New("exec")}
	err := f.m.verifyProfileGPU(context.Background(), f.p)
	if !errors.Is(err, ErrLaunchUnsupported) || DefiniteBindingFailure(err) || !strings.Contains(err.Error(), "identity could not be queried (nvidia-smi failed); retry") ||
		strings.Contains(err.Error(), "launch binding") {
		t.Fatalf("GPU query failure = %v", err)
	}
	f.r.errs = nil
	f.r.outputs["/usr/bin/true --query-gpu=uuid --format=csv,noheader -i 0"] = []byte("GPU-11111111-2222-3333-4444-555555555555\n")
	if err := f.m.verifyProfileGPU(context.Background(), f.p); !DefiniteBindingFailure(err) {
		t.Fatalf("other GPU = %v", err)
	}
}

// A failed read of a host file that is not a limit file of the unit, such as
// the device scheduler, proves no limit, also for a running unit.
func TestHostFileReadFailureOfRunningUnitIsNotDefinite(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	f.r.outputs[f.unitShow()] = []byte("LoadState=loaded\nActiveState=active\nSubState=running\nControlGroup=" + f.p.Cgroup + "\n")
	remove(t, f.host, "/sys/dev/block/259:0/queue/scheduler")
	err := f.m.verifyStartedLimits(context.Background(), f.p)
	if !errors.Is(err, ErrLimitReadback) || errors.Is(err, ErrLimitCheckFailed) || DefiniteLimitFailure(err) {
		t.Fatalf("health with an unreadable scheduler = %v", err)
	}
}

// An I/O cap that the unit leaves unset while the slice caps it could use
// the reserve, so each capped I/O key must be set. requireCappedControls
// covers CPUQuota.
func TestReserveRequiresSliceCappedControls(t *testing.T) {
	c := hostCap{memoryHigh: 8 * gib, memoryMax: 8 * gib, tasks: 1000, cpu: 3000000, io: map[string]uint64{"rbps": 100 << 20, "wbps": 100 << 20}}
	l := control.ResourceLimits{Version: 1, MemoryMaxMiB: 1024, TasksMax: 64, CPUQuotaPercent: 100, IOReadBandwidthMax: 1 << 20}
	err := checkReserve(l, c)
	if part := "the slice caps io.max wbps; set ioWriteBandwidthMax to 94371840 bytes per second or less"; !errors.Is(err, ErrLimitReserve) || !strings.Contains(err.Error(), part) {
		t.Fatalf("reserve = %v, missing %q", err, part)
	}
	l.IOWriteBandwidthMax = 1 << 20
	if err := checkReserve(l, c); err != nil {
		t.Fatalf("reserve with every capped control set = %v", err)
	}
}

// The slice io.max is read for every unit with limits, through each entry
// point. A cap on a device that the unit does not limit fails the reserve,
// also for a unit with no I/O fields or with only IOWeight.
func TestSliceIOCapsNeedUnitCaps(t *testing.T) {
	for name, c := range map[string]struct {
		limits control.ResourceLimits
		loaded string
		want   string
	}{
		"no I/O fields": {control.ResourceLimits{Version: 1, MemoryMaxMiB: 8192, TasksMax: 4096, CPUQuotaPercent: 100},
			"OOMPolicy=stop\nOOMScoreAdjust=500\nMemoryHigh=infinity\nMemoryMax=8589934592\nMemorySwapMax=0\nTasksMax=4096\nCPUWeight=[not set]\nCPUQuotaPerSecUSec=1s\nIOWeight=[not set]\n",
			"the slice caps the I/O of device 259:0 (/dev/nvme0n1), but a unit with limits caps only its one ioDevice (none)"},
		"only IOWeight": {control.ResourceLimits{Version: 1, MemoryMaxMiB: 8192, TasksMax: 4096, CPUQuotaPercent: 100, IODevice: "/dev/nvme0n1", IODeviceNumber: "259:0", IOWeight: 500},
			"OOMPolicy=stop\nOOMScoreAdjust=500\nMemoryHigh=infinity\nMemoryMax=8589934592\nMemorySwapMax=0\nTasksMax=4096\nCPUWeight=[not set]\nCPUQuotaPerSecUSec=1s\nIOWeight=500\n",
			"the slice caps io.max rbps; set ioReadBandwidthMax"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newLimitFixture(t, c.limits, c.loaded)
			ctx := context.Background()
			f.r.outputs[f.unitShow()] = []byte("LoadState=loaded\nActiveState=active\nSubState=running\nControlGroup=" + f.p.Cgroup + "\n")
			for entry, check := range map[string]func() error{
				"before unload": func() error { return f.m.VerifyLaunchLimits(ctx, f.p.ID) },
				"retention":     func() error { return f.m.VerifyLimitProtection(ctx, f.p.ID) },
				"health":        func() error { return f.m.Healthy(ctx, f.p.ID) },
				"start":         func() error { return f.m.Start(ctx, f.p.ID) },
			} {
				if err := check(); !errors.Is(err, ErrLimitReserve) || !strings.Contains(err.Error(), c.want) || f.started() {
					t.Fatalf("%s with a slice I/O cap and no unit cap = %v", entry, err)
				}
			}
			f.r.outputs[f.unitShow()] = stoppedOutput()
			f.cgroup(t, limitSlice, "io.max", "")
			if err := f.m.VerifyLaunchLimits(ctx, f.p.ID); err != nil {
				t.Fatalf("slice without I/O caps = %v", err)
			}
			remove(t, f.cgroups, limitSlice+"/io.max")
			if err := f.m.VerifyLaunchLimits(ctx, f.p.ID); err != nil {
				t.Fatalf("slice without the io controller = %v", err)
			}
		})
	}
}

// A slice cap on a device other than the unit ioDevice cannot be covered by
// the unit, so the refusal names both devices and a remedy that works.
func TestOtherDeviceRefusalNamesARemedy(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	f.cgroup(t, limitSlice, "io.max", "259:0 rbps=1000000000 wbps=max riops=max wiops=max\n8:0 rbps=1048576 wbps=max riops=max wiops=max\n")
	err := f.m.VerifyLaunchLimits(context.Background(), f.p.ID)
	for _, part := range []string{"the slice caps the I/O of device 8:0", "its one ioDevice (/dev/nvme0n1)", "remove the cap on 8:0 from the slice drop-in, or set ioDevice to that device"} {
		if !errors.Is(err, ErrLimitReserve) || !strings.Contains(err.Error(), part) {
			t.Fatalf("other device refusal = %v, missing %q", err, part)
		}
	}
	if strings.Contains(err.Error(), "set the matching unit caps") {
		t.Fatalf("other device refusal gives a remedy that cannot work: %v", err)
	}
}

// Each host-unit error names the unit whose state or drop-in failed: the
// account slice or the user manager.
func TestHostUnitErrorsNameTheUnit(t *testing.T) {
	for name, c := range map[string]struct {
		edit func(*limitFixture)
		want []string
	}{
		"slice needs daemon-reload": {func(f *limitFixture) { f.edit(f.sliceShow(), "NeedDaemonReload=no", "NeedDaemonReload=yes") },
			[]string{"user-1000.slice needs daemon-reload"}},
		"slice not loaded": {func(f *limitFixture) { f.edit(f.sliceShow(), "LoadState=loaded", "LoadState=not-found") },
			[]string{"user-1000.slice is not loaded"}},
		"slice host cap not loaded": {func(f *limitFixture) {
			f.edit(f.sliceShow(), "/etc/systemd/system/user-1000.slice.d/50-gws-host-cap.conf", "")
		}, []string{"user-1000.slice does not load /etc/systemd/system/user-1000.slice.d/50-gws-host-cap.conf"}},
		"slice unknown drop-in": {func(f *limitFixture) {
			f.edit(f.sliceShow(), "DropInPaths=", "DropInPaths=/run/systemd/system/user-1000.slice.d/50-raise.conf ")
		}, []string{"drop-in of user-1000.slice", "50-raise.conf"}},
		"manager needs daemon-reload": {func(f *limitFixture) { f.edit(f.managerShow(), "NeedDaemonReload=no", "NeedDaemonReload=yes") },
			[]string{"user@1000.service needs daemon-reload"}},
		"manager not loaded": {func(f *limitFixture) { f.edit(f.managerShow(), "LoadState=loaded", "LoadState=not-found") },
			[]string{"user@1000.service is not loaded"}},
		"manager delegation not loaded": {func(f *limitFixture) {
			f.edit(f.managerShow(), "/etc/systemd/system/user@1000.service.d/50-gws-delegate-io.conf", "")
		}, []string{"user@1000.service does not load /etc/systemd/system/user@1000.service.d/50-gws-delegate-io.conf"}},
		"manager unknown drop-in": {func(f *limitFixture) {
			f.edit(f.managerShow(), "DropInPaths=", "DropInPaths=/run/systemd/system/user@1000.service.d/50-raise.conf ")
		}, []string{"drop-in of user@1000.service", "50-raise.conf"}},
	} {
		t.Run(name, func(t *testing.T) {
			f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
			c.edit(f)
			err := f.m.VerifyLaunchLimits(context.Background(), f.p.ID)
			// A pending daemon-reload is a readback failure, not a wrong cap.
			class := ErrHostCap
			if strings.Contains(name, "daemon-reload") {
				class = ErrLimitReadback
			}
			for _, part := range c.want {
				if !errors.Is(err, class) || !strings.Contains(err.Error(), part) {
					t.Fatalf("host unit error = %v, missing %q", err, part)
				}
			}
		})
	}
}

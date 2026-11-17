package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"golang.org/x/sys/unix"
)

const capDropInPath = "/etc/systemd/system/user-1000.slice.d/50-gws-host-cap.conf"

// fixtureHostCap holds the units of the fixture with their reserve.
func fixtureHostCap() HostCapValues {
	return HostCapValues{MemoryHighMiB: 58368, MemoryMaxMiB: 61440}
}

// The rendered slice drop-in of fixtureHostCap: the filled values are the
// largest unit limit plus the reserve.
const fixtureCapContent = "[Slice]\nMemoryHigh=58368M\nMemoryMax=61440M\nMemorySwapMax=0\nTasksMax=4608\nCPUQuota=500%\n" +
	"IOReadBandwidthMax=/dev/nvme0n1 596523235\n"

// Without a unit with limits, no root step is needed.
func TestHostCapStepNotNeeded(t *testing.T) {
	m := strictManager(t, &fakeRunner{})
	step, err := m.HostCapStep(context.Background(), &HostCapValues{MemoryHighMiB: 1})
	if err != nil || step.Status != HostCapNotNeeded || step.Reserve != DefaultLimitReserve || len(step.DropIns) != 0 {
		t.Fatalf("step = %+v, %v", step, err)
	}
}

// On the fixture host, the cap is present. Without requested values, the
// step gives the drop-in paths and the I/O weight option, and no command.
func TestHostCapStepPresent(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	step, err := f.m.HostCapStep(context.Background(), nil)
	if err != nil || step.Status != HostCapPresent || step.UID != 1000 || len(step.Commands) != 0 || len(step.Problems) != 0 ||
		len(step.DropIns) != 2 || step.DropIns[0].Path != capDropInPath || step.DropIns[0].Content != "" ||
		step.DropIns[1].Path != "/etc/systemd/system/user@1000.service.d/50-gws-delegate-io.conf" ||
		!slices.Equal(step.IOWeight, []IOWeightOption{{Workload: f.p.ID, Device: "/dev/nvme0n1", Provider: "iocost"}}) {
		t.Fatalf("step = %+v, %v", step, err)
	}
}

// Requested values that differ from the installed drop-in give the status
// different and the commands for that drop-in only.
func TestHostCapStepDifferentAndInstalled(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	want := fixtureHostCap()
	step, err := f.m.HostCapStep(context.Background(), &want)
	if err != nil || step.Status != HostCapDifferent || step.Values == nil || step.Values.TasksMax != 4608 ||
		step.DropIns[0].Content != fixtureCapContent || !step.DropIns[0].Install || step.DropIns[1].Install {
		t.Fatalf("step = %+v, %v", step, err)
	}
	wantCommands := []string{"sudo install -d -m 0755 /etc/systemd/system/user-1000.slice.d",
		"sudo install -m 0644 /dev/stdin " + capDropInPath + " <<'EOF'\n" + fixtureCapContent + "EOF", "sudo systemctl daemon-reload"}
	if !slices.Equal(step.Commands, wantCommands) {
		t.Fatalf("commands = %q", step.Commands)
	}
	f.file(t, capDropInPath, fixtureCapContent)
	if step, err := f.m.HostCapStep(context.Background(), &want); err != nil || step.Status != HostCapPresent || len(step.Commands) != 0 {
		t.Fatalf("step after the install = %+v, %v", step, err)
	}
}

// A missing drop-in makes the cap absent. Its commands install both files,
// and the delegation drop-in needs a restart of the user manager.
func TestHostCapStepAbsent(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	remove(t, f.host, capDropInPath)
	remove(t, f.host, "/etc/systemd/system/user@1000.service.d/50-gws-delegate-io.conf")
	want := fixtureHostCap()
	step, err := f.m.HostCapStep(context.Background(), &want)
	delegate := "/etc/systemd/system/user@1000.service.d/50-gws-delegate-io.conf"
	wantCommands := []string{"sudo install -d -m 0755 /etc/systemd/system/user-1000.slice.d",
		"sudo install -m 0644 /dev/stdin " + capDropInPath + " <<'EOF'\n" + fixtureCapContent + "EOF",
		"sudo install -d -m 0755 /etc/systemd/system/user@1000.service.d",
		"sudo install -m 0644 /dev/stdin " + delegate + " <<'EOF'\n[Service]\nDelegate=io\nEOF",
		"sudo systemctl daemon-reload", "sudo systemctl restart user@1000.service"}
	if err != nil || step.Status != HostCapAbsent || !strings.Contains(strings.Join(step.Problems, "\n"), capDropInPath+" is missing") ||
		!slices.Equal(step.Commands, wantCommands) {
		t.Fatalf("step = %+v, %v", step, err)
	}
}

// A check that proves nothing gives the status unknown. A definite check
// failure gives different.
func TestHostCapStepCheckResults(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	f.r.errs = map[string]error{f.sliceShow(): errors.New("bus")}
	if step, err := f.m.HostCapStep(context.Background(), nil); err != nil || step.Status != HostCapUnknown || len(step.Problems) != 1 {
		t.Fatalf("step with a failed show = %+v, %v", step, err)
	}
	f.r.errs = nil
	f.edit(f.sliceShow(), "TasksMax=16384", "TasksMax=9")
	if step, err := f.m.HostCapStep(context.Background(), nil); err != nil || step.Status != HostCapDifferent ||
		len(step.Problems) == 0 || !strings.HasPrefix(step.Problems[0], string(f.p.ID)+": ") {
		t.Fatalf("step with a mismatch = %+v, %v", step, err)
	}
	if _, err := f.m.HostCapStep(context.Background(), &HostCapValues{MemoryHighMiB: 4096, MemoryMaxMiB: 8192}); !errors.Is(err, ErrLimitReserve) {
		t.Fatalf("step with a small memory cap = %v", err)
	}
}

// The I/O weight option names BFQ, or none when the device supports no I/O
// weight. A device that cannot be checked gives unknown, and a device that
// fails the device check gives invalid.
func TestIOWeightOption(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	f.file(t, "/sys/dev/block/259:0/queue/scheduler", "mq-deadline [bfq] none\n")
	if got := f.m.ioWeightOption(fixtureLimits()); got != "bfq" {
		t.Fatalf("option with BFQ = %q", got)
	}
	f.file(t, "/sys/dev/block/259:0/queue/scheduler", "[none]\n")
	f.cgroup(t, "", "io.cost.qos", "259:0 enable=0 ctrl=auto\n")
	if got := f.m.ioWeightOption(fixtureLimits()); got != "none" {
		t.Fatalf("option without a provider = %q", got)
	}
	remove(t, f.host, "/sys/dev/block/259:0/queue/scheduler")
	if got := f.m.ioWeightOption(fixtureLimits()); got != "unknown" {
		t.Fatalf("option of an unreadable device = %q", got)
	}
	f.deviceErr = unix.ENOENT
	if got := f.m.ioWeightOption(fixtureLimits()); got != "invalid" {
		t.Fatalf("option of a missing device = %q", got)
	}
}

// A delegation drop-in matches when it delegates io, whatever its text.
// Without requested values, a missing drop-in gets a hint and no command.
func TestDelegationDropInMeaning(t *testing.T) {
	for text, want := range map[string]bool{
		"[Service]\nDelegate=io\n":                     true,
		"# gws\n[Service]\n; io\nDelegate=cpu io\n":    true,
		"[Service]\nDelegate=yes\n":                    true,
		"[Service]\nDelegate=io\nDelegate=\n":          false,
		"[Unit]\nDelegate=io\n":                        false,
		"[Service]\nDelegate=cpu\n":                    false,
		"[Service]\nDelegate=cpu\n[Unit]\nDelegate=io": false,
		"[Service]\nDelegate=io\nDelegate=no\n":        false,
		"[Service]\nDelegate=on\n":                     true,
		"[Service]\nDelegate = io\n":                   true,
		"[Service]\nDelegate=io\nDelegate=memory\n":    true,
		"[Service]\nDelegateSubgroup=io\n":             false,
	} {
		if got := delegatesIO(text); got != want {
			t.Errorf("delegatesIO(%q) = %v", text, got)
		}
	}
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	f.file(t, "/etc/systemd/system/user@1000.service.d/50-gws-delegate-io.conf", "# local\n[Service]\nDelegate=io cpu\n")
	f.file(t, capDropInPath, fixtureCapContent)
	want := fixtureHostCap()
	if step, err := f.m.HostCapStep(context.Background(), &want); err != nil || step.Status != HostCapPresent || step.DropIns[1].Install || len(step.Commands) != 0 {
		t.Fatalf("step with another delegation text = %+v, %v", step, err)
	}
	remove(t, f.host, capDropInPath)
	step, err := f.m.HostCapStep(context.Background(), nil)
	if err != nil || step.Status != HostCapAbsent || !step.DropIns[0].Install || len(step.Commands) != 0 ||
		!strings.Contains(strings.Join(step.Problems, "\n"), "set hostCap.memoryHighMiB and hostCap.memoryMaxMiB") {
		t.Fatalf("step without values = %+v, %v", step, err)
	}
}

// CheckLimitHost runs the host checks of a unit with limits, without the
// binding and the loaded properties. A unit without limits passes.
func TestCheckLimitHost(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	ctx := context.Background()
	// The unit is not written yet, so its binding and properties are
	// missing.
	f.r.errs = map[string]error{f.bindingShow(): errors.New("no unit"), f.limitShow(): errors.New("no unit")}
	if err := f.m.CheckLimitHost(ctx, f.p.ID); err != nil {
		t.Fatalf("host checks = %v", err)
	}
	if err := f.m.CheckLimitHost(ctx, control.WorkloadText); err != nil {
		t.Fatalf("host checks of an unlimited unit = %v", err)
	}
	if err := f.m.CheckLimitHost(ctx, "unknown"); !errors.Is(err, ErrLimitUnchecked) {
		t.Fatalf("host checks of an unknown unit = %v", err)
	}
	remove(t, f.host, capDropInPath)
	if err := f.m.CheckLimitHost(ctx, f.p.ID); !errors.Is(err, ErrHostCap) {
		t.Fatalf("host checks without the drop-in = %v", err)
	}
}

// A stored reserve replaces the default in the reserve and memory checks.
func TestStoredReserve(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	f.m.reserve = LimitReserve{MemoryMiB: 55000, Tasks: 512, CPUPercent: 100, IOPercent: 10}
	if err := f.m.CheckLimitHost(context.Background(), f.p.ID); !errors.Is(err, ErrLimitReserve) {
		t.Fatalf("host checks with a large reserve = %v", err)
	}
	// The start admission uses the stored memory reserve too.
	c := hostCap{slice: limitSlice, memoryHigh: 61203283968, memoryMax: 64424509440}
	if err := f.m.admitMemory(c, fixtureLimits()); !errors.Is(err, ErrLimitMemory) || !strings.Contains(err.Error(), "a 55000 MiB reserve") {
		t.Fatalf("admission with a large reserve = %v", err)
	}
	f.m.reserve = DefaultLimitReserve
	if err := f.m.admitMemory(c, fixtureLimits()); err != nil {
		t.Fatalf("admission with the default reserve = %v", err)
	}
	// A reserve that the caller cannot read fails each check without a
	// definite failure.
	f.m.config.LimitReserveErr = errors.New("profile unreadable")
	if err := f.m.CheckLimitHost(context.Background(), f.p.ID); !errors.Is(err, ErrLimitReadback) || DefiniteLimitFailure(err) ||
		!strings.Contains(err.Error(), "profile unreadable") {
		t.Fatalf("host checks without the stored reserve = %v", err)
	}
	config := testConfig()
	config.LimitReserve = &LimitReserve{MemoryMiB: 4096, Tasks: 1024, CPUPercent: 200, IOPercent: 20}
	m, err := newSystemdManager(config, &fakeRunner{}, http.DefaultClient)
	if err != nil || m.reserve != *config.LimitReserve {
		t.Fatalf("stored reserve = %+v, %v", m, err)
	}
	config.LimitReserve = &LimitReserve{MemoryMiB: 4096, Tasks: 1024, CPUPercent: 200, IOPercent: 51}
	if _, err := newSystemdManager(config, &fakeRunner{}, http.DefaultClient); err == nil || !strings.Contains(err.Error(), "ioPercent") {
		t.Fatalf("an out-of-range reserve = %v", err)
	}
}

func TestLimitReserveBounds(t *testing.T) {
	for name, r := range map[string]LimitReserve{
		"memory":       {MemoryMiB: 255, Tasks: 512, CPUPercent: 100, IOPercent: 10},
		"tasks":        {MemoryMiB: 2048, Tasks: 63, CPUPercent: 100, IOPercent: 10},
		"cpu":          {MemoryMiB: 2048, Tasks: 512, CPUPercent: 0, IOPercent: 10},
		"io":           {MemoryMiB: 2048, Tasks: 512, CPUPercent: 100, IOPercent: 0},
		"memory upper": {MemoryMiB: control.MaxLimitMemoryMiB + 1, Tasks: 512, CPUPercent: 100, IOPercent: 10},
		"tasks upper":  {MemoryMiB: 2048, Tasks: control.MaxLimitTasks + 1, CPUPercent: 100, IOPercent: 10},
		"cpu upper":    {MemoryMiB: 2048, Tasks: 512, CPUPercent: maxReserveCPUPercent + 1, IOPercent: 10},
		"io upper":     {MemoryMiB: 2048, Tasks: 512, CPUPercent: 100, IOPercent: maxReserveIOPercent + 1},
	} {
		if err := r.Validate(); err == nil || !strings.Contains(err.Error(), "limit reserve") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := DefaultLimitReserve.Validate(); err != nil {
		t.Fatal(err)
	}
}

// HostCapFor fills the unset caps with the largest unit limit plus the
// reserve, and refuses caps that leave too little room.
func TestHostCapFor(t *testing.T) {
	r := DefaultLimitReserve
	l := fixtureLimits()
	other := l
	other.TasksMax, other.CPUQuotaPercent, other.IOReadBandwidthMax = 8000, 1200, 1<<30
	values, device, err := r.HostCapFor(fixtureHostCap(), []control.ResourceLimits{l, other})
	if err != nil || device != "/dev/nvme0n1" || values.TasksMax != 8512 || values.CPUQuotaPercent != 1300 ||
		values.IOReadBandwidthMax != r.ioCeilingFor(1<<30) || values.IOReadBandwidthMax-r.ioReserve(values.IOReadBandwidthMax) < 1<<30 {
		t.Fatalf("filled = %+v, %q, %v", values, device, err)
	}
	if got := renderSliceCap(func() HostCapValues { v, _, _ := r.HostCapFor(fixtureHostCap(), []control.ResourceLimits{l}); return v }(), "/dev/nvme0n1"); got != fixtureCapContent {
		t.Fatalf("drop-in = %q", got)
	}
	noIO := control.ResourceLimits{Version: 1, MemoryMaxMiB: 1024, TasksMax: 64, CPUQuotaPercent: 100}
	iops := l
	iops.IOReadBandwidthMax, iops.IOReadIOPSMax = 0, 1000
	for name, c := range map[string]struct {
		want   HostCapValues
		limits []control.ResourceLimits
		part   string
	}{
		"no memory":     {HostCapValues{}, []control.ResourceLimits{l}, "needs memoryHighMiB and memoryMaxMiB"},
		"max not above": {HostCapValues{MemoryHighMiB: 8192, MemoryMaxMiB: 8192}, []control.ResourceLimits{l}, "memoryMaxMiB must be above"},
		"small memory":  {HostCapValues{MemoryHighMiB: 9000, MemoryMaxMiB: 9500}, []control.ResourceLimits{l}, "set it to at least 10240 MiB"},
		"small tasks":   {HostCapValues{MemoryHighMiB: 58368, MemoryMaxMiB: 61440, TasksMax: 4100}, []control.ResourceLimits{l}, "tasksMax 4100 leaves too little room"},
		"large cpu":     {HostCapValues{MemoryHighMiB: 58368, MemoryMaxMiB: 61440, CPUQuotaPercent: maxHostCapCPU + 1}, []control.ResourceLimits{l}, "cpuQuotaPercent must be"},
		"small io":      {HostCapValues{MemoryHighMiB: 58368, MemoryMaxMiB: 61440, IOReadBandwidthMax: 512 << 20}, []control.ResourceLimits{l}, "hostCap.ioReadBandwidthMax 536870912 leaves too little room"},
		"large bandwidth": {HostCapValues{MemoryHighMiB: 58368, MemoryMaxMiB: 61440, IOReadBandwidthMax: maxHostCapBandwidth + 1}, []control.ResourceLimits{l},
			"hostCap.ioReadBandwidthMax must be 2199023255552 bytes per second or less"},
		"large iops": {HostCapValues{MemoryHighMiB: 58368, MemoryMaxMiB: 61440, IOReadIOPSMax: maxHostCapIOPS + 1}, []control.ResourceLimits{iops},
			"hostCap.ioReadIOPSMax must be 20000000 operations per second or less"},
		"io without unit": {HostCapValues{MemoryHighMiB: 58368, MemoryMaxMiB: 61440, IOWriteIOPSMax: 1000}, []control.ResourceLimits{l}, "no unit sets ioWriteIOPSMax"},
		"unit without io": {fixtureHostCap(), []control.ResourceLimits{l, noIO}, "set ioReadBandwidthMax on each unit"},
		"two devices":     {fixtureHostCap(), []control.ResourceLimits{l, func() control.ResourceLimits { d := l; d.IODevice = "/dev/sda"; return d }()}, "one ioDevice"},
	} {
		if _, _, err := r.HostCapFor(c.want, c.limits); !errors.Is(err, ErrLimitReserve) || !strings.Contains(err.Error(), c.part) {
			t.Errorf("%s: %v, want %q", name, err, c.part)
		}
	}
	// The task cap cannot pass the kernel maximum of pids.max.
	large := l
	large.TasksMax = control.MaxLimitTasks
	if _, _, err := r.HostCapFor(fixtureHostCap(), []control.ResourceLimits{large}); !errors.Is(err, ErrLimitReserve) ||
		!strings.Contains(err.Error(), "the largest unit tasksMax plus the reserve is above the host maximum of 4194304 tasks") {
		t.Fatalf("a filled cap above the upper bound = %v", err)
	}
	set := fixtureHostCap()
	set.TasksMax = control.MaxLimitTasks + 1
	if _, _, err := r.HostCapFor(set, []control.ResourceLimits{l}); !strings.Contains(err.Error(), "hostCap.tasksMax must be 4194304 tasks or less") {
		t.Fatalf("a set cap above the upper bound = %v", err)
	}
	huge := l
	huge.MemoryMaxMiB = control.MaxLimitMemoryMiB
	if _, _, err := r.HostCapFor(fixtureHostCap(), []control.ResourceLimits{huge}); !errors.Is(err, ErrLimitReserve) || !strings.Contains(err.Error(), "no host cap fits") {
		t.Fatalf("a unit memory limit that no cap fits = %v", err)
	}
	// An I/O reserve of at most 50 percent always fits the largest unit
	// cap under the I/O upper bound.
	wide := l
	wide.IOReadBandwidthMax = control.MaxLimitIOBandwidth
	wideReserve := r
	wideReserve.IOPercent = maxReserveIOPercent
	if values, _, err := wideReserve.HostCapFor(fixtureHostCap(), []control.ResourceLimits{wide}); err != nil || values.IOReadBandwidthMax > maxHostCapBandwidth {
		t.Fatalf("the widest I/O cap = %+v, %v", values, err)
	}
}

// A drop-in that setup cannot read and a stored reserve that the caller
// cannot read make the status unknown. Matching drop-ins with failed
// checks get the daemon-reload hint.
func TestHostCapStepUnknownCauses(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	remove(t, f.host, capDropInPath)
	if err := os.Mkdir(filepath.Join(f.host, capDropInPath), 0o755); err != nil {
		t.Fatal(err)
	}
	f.r.errs = map[string]error{f.sliceShow(): errors.New("bus")}
	step, err := f.m.HostCapStep(context.Background(), nil)
	if err != nil || step.Status != HostCapUnknown || !strings.Contains(strings.Join(step.Problems, "\n"), capDropInPath+" cannot be read") {
		t.Fatalf("step with an unreadable drop-in = %+v, %v", step, err)
	}
	f = newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	f.file(t, capDropInPath, fixtureCapContent)
	f.m.config.LimitReserveErr = errors.New("profile unreadable")
	want := fixtureHostCap()
	step, err = f.m.HostCapStep(context.Background(), &want)
	text := strings.Join(step.Problems, "\n")
	if err != nil || step.Status != HostCapUnknown || !strings.Contains(text, "profile unreadable") || !strings.Contains(text, "systemctl daemon-reload") {
		t.Fatalf("step without the stored reserve = %+v, %v", step, err)
	}
}

// The JSON of the host cap step is a contract with the desktop page.
func TestHostCapStepJSON(t *testing.T) {
	step := HostCapStep{Status: HostCapAbsent, UID: 1000, Values: &HostCapValues{MemoryHighMiB: 2, MemoryMaxMiB: 3, TasksMax: 4},
		DropIns: []DropIn{{Path: "/p", Content: "c", Install: true}}, Commands: []string{"x"}, Problems: []string{"y"},
		IOWeight: []IOWeightOption{{Workload: "vision", Device: "/dev/d", Provider: "bfq"}}, Reserve: DefaultLimitReserve,
		Usage: &HostCapUsage{MemoryInUseMiB: 5, PageCacheMiB: 6, CeilingMiB: 7}}
	data, err := json.Marshal(step)
	want := `{"status":"absent","uid":1000,"values":{"memoryHighMiB":2,"memoryMaxMiB":3,"tasksMax":4},` +
		`"dropIns":[{"path":"/p","content":"c","install":true}],"commands":["x"],"problems":["y"],` +
		`"ioWeight":[{"workload":"vision","device":"/dev/d","provider":"bfq"}],"reserve":{"memoryMiB":2048,"tasks":512,"cpuPercent":100,"ioPercent":10},` +
		`"usage":{"memoryInUseMiB":5,"pageCacheMiB":6,"ceilingMiB":7}}`
	if err != nil || string(data) != want {
		t.Fatalf("JSON = %s, %v", data, err)
	}
	data, err = json.Marshal(HostCapStep{Status: HostCapNotNeeded, Reserve: DefaultLimitReserve})
	if err != nil || string(data) != `{"status":"notNeeded","reserve":{"memoryMiB":2048,"tasks":512,"cpuPercent":100,"ioPercent":10}}` {
		t.Fatalf("JSON = %s, %v", data, err)
	}
}

// The step reports the slice memory in use without the reclaimable page
// cache, beside the ceiling. A failed read gives a problem and no usage,
// and the status stays.
func TestHostCapStepUsage(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	f.cgroup(t, limitSlice, "memory.current", strconv.FormatUint(6<<30, 10)+"\n")
	f.cgroup(t, limitSlice, "memory.stat", "anon 1\nactive_file "+strconv.FormatUint(1<<30, 10)+"\ninactive_file "+strconv.FormatUint(1<<30, 10)+"\n")
	step, err := f.m.HostCapStep(context.Background(), nil)
	if err != nil || step.Status != HostCapPresent || step.Usage == nil ||
		*step.Usage != (HostCapUsage{MemoryInUseMiB: 4096, PageCacheMiB: 2048, CeilingMiB: 58368}) {
		t.Fatalf("step = %+v, usage %+v, %v", step, step.Usage, err)
	}
	remove(t, f.cgroups, limitSlice+"/memory.stat")
	step, err = f.m.HostCapStep(context.Background(), nil)
	if err != nil || step.Status != HostCapPresent || step.Usage != nil || len(step.Problems) != 1 ||
		!strings.Contains(step.Problems[0], "the memory in use cannot be read") {
		t.Fatalf("step without memory.stat = %+v, %v", step, err)
	}
}

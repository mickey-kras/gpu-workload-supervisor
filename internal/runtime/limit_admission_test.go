package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

// Admission leaves out the page cache that the kernel can reclaim: the
// active_file and inactive_file pages of the slice. Shmem and mlocked
// pages are not on these lists, so they stay in use.
func TestAdmitMemoryLeavesOutPageCache(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	c := hostCap{slice: limitSlice, memoryHigh: 16 * gib, memoryMax: 16 * gib}
	l := control.ResourceLimits{Version: 1, MemoryMaxMiB: 8192}
	stat := func(active, inactive, shmem uint64) string {
		return "anon " + strconv.FormatUint(2*gib, 10) + "\nactive_file " + strconv.FormatUint(active, 10) + "\ninactive_file " +
			strconv.FormatUint(inactive, 10) + "\nshmem " + strconv.FormatUint(shmem, 10) + "\nunevictable " + strconv.FormatUint(gib, 10) + "\n"
	}
	for name, c2 := range map[string]struct {
		current uint64
		stat    string
		inUse   string
	}{
		"reclaimable cache": {40 * gib, stat(20*gib, 17*gib, gib), ""},
		"too little cache":  {40 * gib, stat(4*gib, 4*gib, 30*gib), "32768 MiB is in use. The 8192 MiB of page cache that the kernel can reclaim does not count."},
		// The two files are read at different times, so the cache can
		// be above memory.current.
		"cache above current": {gib, stat(2*gib, 0, 0), ""},
		"shmem is no cache":   {15 * gib, stat(0, 0, 10*gib), "15360 MiB is in use. The 0 MiB"},
	} {
		f.cgroup(t, limitSlice, "memory.current", strconv.FormatUint(c2.current, 10)+"\n")
		f.cgroup(t, limitSlice, "memory.stat", c2.stat)
		err := f.m.admitMemory(c, l)
		if c2.inUse == "" && err != nil || c2.inUse != "" && (!errors.Is(err, ErrLimitMemory) || !strings.Contains(err.Error(), c2.inUse)) {
			t.Errorf("admission with %s = %v", name, err)
		}
	}
	for name, stat := range map[string]string{"missing": "", "malformed": "anon 1\nactive_file lots\ninactive_file 0\n",
		"no active_file": "anon 1\ninactive_file 0\n", "no inactive_file": "anon 1\nactive_file 0\n",
		"overflow": "anon 1\nactive_file 18446744073709551615\ninactive_file 1\n"} {
		if stat == "" {
			remove(t, f.cgroups, limitSlice+"/memory.stat")
		} else {
			f.cgroup(t, limitSlice, "memory.stat", stat)
		}
		if err := f.m.admitMemory(c, l); !errors.Is(err, ErrLimitReadback) || errors.Is(err, ErrLimitMemory) {
			t.Errorf("admission with a %s memory.stat = %v", name, err)
		}
	}
}

// A readback failure on a spent probe context proves nothing, also when
// the runner ignores the cancellation and reports the unit active.
func TestCancelledReadbackIsNotDefinite(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	f.r.ignoreCtx = true
	f.r.outputs[f.unitShow()] = []byte("LoadState=loaded\nActiveState=active\nSubState=running\nControlGroup=" + f.p.Cgroup + "\n")
	remove(t, f.cgroups, f.p.Cgroup+"/memory.high")
	if err := os.Mkdir(filepath.Join(f.cgroups, f.p.Cgroup, "memory.high"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := f.m.verifyStartedLimits(ctx, f.p)
	if !errors.Is(err, ErrLimitReadback) || errors.Is(err, ErrLimitCheckFailed) || DefiniteLimitFailure(err) {
		t.Fatalf("readback on a spent context = %v", err)
	}
}

// A slice whose path is only a string prefix of the manager cgroup does not
// contain it.
func TestSlicePrefixDoesNotContainTheManager(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	sibling := strings.TrimSuffix(limitSlice, "0.slice")
	for _, name := range []string{"memory.high", "memory.max", "memory.swap.max", "pids.max", "cpu.max", "io.max", "memory.current", "memory.stat"} {
		data, err := os.ReadFile(filepath.Join(f.cgroups, limitSlice, name))
		if err != nil {
			t.Fatal(err)
		}
		f.cgroup(t, sibling, name, string(data))
	}
	f.edit(f.sliceShow(), "ControlGroup="+limitSlice, "ControlGroup="+sibling)
	if err := f.m.VerifyLaunchLimits(context.Background(), f.p.ID); !errors.Is(err, ErrHostCap) || !strings.Contains(err.Error(), "is not inside") {
		t.Fatalf("launch checks with a slice that is a string prefix = %v", err)
	}
}

// memory.current and memory.stat get the trust checks of the other slice
// files: a forged count would admit memory that is in use. Any failure is a
// readback failure only.
func TestUntrustedMemoryFilesFailAdmission(t *testing.T) {
	for _, file := range []string{"memory.current", "memory.stat"} {
		for name, edit := range map[string]func(*limitFixture){
			"writable":    func(f *limitFixture) { chmod(t, f.cgroups, limitSlice+"/"+file, 0o666) },
			"other owner": func(f *limitFixture) { f.giveOtherOwner(t, filepath.Join(f.cgroups, limitSlice, file)) },
		} {
			f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
			edit(f)
			err := f.start(t)
			if !errors.Is(err, ErrLimitReadback) || errors.Is(err, ErrHostCap) || DefiniteLimitFailure(err) || !strings.Contains(err.Error(), file) || f.started() {
				t.Errorf("start with a %s %s = %v", name, file, err)
			}
		}
	}
}

// An inactive unit whose cgroup.events cannot be read has an unknown
// cgroup state, so a readback failure proves nothing: a later probe can
// decide.
func TestReadbackOfInactiveUnitWithUnreadableEvents(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	for _, name := range []string{"memory.high", "cgroup.events"} {
		remove(t, f.cgroups, f.p.Cgroup+"/"+name)
		if err := os.Mkdir(filepath.Join(f.cgroups, f.p.Cgroup, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	err := f.m.verifyStartedLimits(context.Background(), f.p)
	if !errors.Is(err, ErrLimitReadback) || errors.Is(err, ErrLimitCheckFailed) || DefiniteLimitFailure(err) {
		t.Fatalf("readback of an inactive unit with unreadable cgroup.events = %v", err)
	}
}

// Each readback failure of a slice property mismatch keeps the first
// mismatch, with one class prefix. A missing flag on the second read is
// named as an output problem.
func TestSliceCapChangeKeepsTheMismatch(t *testing.T) {
	mismatch := `TasksMax of user-1000.slice is "9", but its cgroup holds "16384"`
	for name, c := range map[string]struct {
		second func(string) string
		want   string
	}{
		"values changed": {func(show string) string { return strings.Replace(show, "MemorySwapMax=0", "MemorySwapMax=1", 1) },
			"the MemorySwapMax of user-1000.slice changed during the check"},
		"reload needed": {func(show string) string {
			return strings.Replace(show, "NeedDaemonReload=no", "NeedDaemonReload=yes", 1)
		},
			"user-1000.slice needs daemon-reload"},
		"flag missing": {func(show string) string { return strings.Replace(show, "NeedDaemonReload=no\n", "", 1) },
			"NeedDaemonReload is missing or ambiguous"},
		"second read fails": {nil, "and the properties of user-1000.slice cannot be read again"},
	} {
		f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
		f.edit(f.sliceShow(), "TasksMax=16384", "TasksMax=9")
		f.r.after = map[string]func(){f.sliceShow(): func() {
			if c.second == nil {
				f.r.errs = map[string]error{f.sliceShow(): errors.New("bus")}
				return
			}
			f.r.outputs[f.sliceShow()] = []byte(c.second(string(f.r.outputs[f.sliceShow()])))
		}}
		err := f.m.VerifyLaunchLimits(context.Background(), f.p.ID)
		text := err.Error()
		if !errors.Is(err, ErrLimitReadback) || strings.Count(text, ErrLimitReadback.Error()) != 1 || !strings.Contains(text, mismatch) || !strings.Contains(text, c.want) ||
			strings.Contains(text, ErrHostCap.Error()) || strings.Contains(text, "cap of user-1000.slice changed") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

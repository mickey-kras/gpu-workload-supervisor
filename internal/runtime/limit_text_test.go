package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A launch source read failure proves nothing, so its text claims no
// change and names the path one time.
func TestLaunchSourceReadFailureText(t *testing.T) {
	_, err := readLaunchSource(procMemory)
	text := err.Error()
	if !errors.Is(err, ErrLaunchChanged) || DefiniteBindingFailure(err) || strings.Contains(text, ErrLaunchChanged.Error()) ||
		!strings.HasPrefix(text, "cannot inspect the launch binding: ") || strings.Count(text, "/mem") != 1 {
		t.Fatalf("launch source read failure = %q", text)
	}
}

// A controller that the user manager root has but the unit cgroup lacks is
// not a delegation problem, so the remedy names the unit.
func TestUnitControllerRemedyNamesTheUnit(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	f.cgroup(t, f.p.Cgroup, "cgroup.controllers", "cpu io pids\n")
	err := f.m.readbackLimits(f.p.Cgroup, fixtureLimits(), "iocost")
	want := "memory is absent from " + f.p.Cgroup + "/cgroup.controllers; the user manager root has it, so systemd did not enable it for " + f.p.Unit +
		": run `systemctl --user daemon-reload`, then start the workload again through gpu-mode"
	if !errors.Is(err, ErrLimitController) || !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), "delegation") {
		t.Fatalf("unit controller missing = %v", err)
	}
}

// Device, controller and interface refusals end with a step the operator
// can take.
func TestLimitRefusalSteps(t *testing.T) {
	for name, c := range map[string]struct {
		edit func(*limitFixture)
		want string
	}{
		"no queue": {func(f *limitFixture) {
			remove(t, f.host, "/sys/dev/block/259:0/queue/scheduler")
			removeAll(t, f.host, "/sys/dev/block/259:0/queue")
		},
			"/dev/nvme0n1 has no request queue; " + wholeDiskRemedy},
		"stacked": {func(f *limitFixture) { f.file(t, "/sys/dev/block/259:0/slaves/sda", "") }, "/dev/nvme0n1 is a stacked device; " + wholeDiskRemedy},
		"pids not delegated": {func(f *limitFixture) { f.cgroup(t, limitManagerRoot, "cgroup.controllers", "cpu io memory\n") },
			"add pids to Delegate= in /etc/systemd/system/user@1000.service.d/50-gws-delegate-io.conf, run `sudo systemctl daemon-reload`, then restart user@1000.service from a verified Idle"},
		"no swap accounting": {func(f *limitFixture) { remove(t, f.cgroups, limitManagerRoot+"/memory.swap.max") },
			"memory.swap.max is missing (swap accounting is off); enable swap accounting: the kernel command line must not set `cgroup.memory=noswap` (older kernels need `swapaccount=1`), then reboot"},
		"no iocost": {func(f *limitFixture) { remove(t, f.cgroups, limitManagerRoot+"/io.weight") },
			"io.weight is missing (iocost is off); the kernel needs CONFIG_BLK_CGROUP_IOCOST, or remove ioWeight through setup"},
	} {
		f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
		c.edit(f)
		if err := f.m.VerifyLaunchLimits(context.Background(), f.p.ID); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s = %v", name, err)
		}
	}
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	f.cgroup(t, f.p.Cgroup, "cgroup.controllers", "cpu io pids\n")
	err := f.m.readbackLimits(f.p.Cgroup, fixtureLimits(), "iocost")
	if want := "run `systemctl --user daemon-reload`, then start the workload again through gpu-mode"; err == nil || !strings.Contains(err.Error(), want) ||
		strings.Contains(err.Error(), "then restart") {
		t.Errorf("unit controller missing = %v", err)
	}
}

func removeAll(t *testing.T, root, name string) {
	t.Helper()
	if err := os.RemoveAll(filepath.Join(root, name)); err != nil {
		t.Fatal(err)
	}
}

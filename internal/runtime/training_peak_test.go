package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

const peakInvocation = "1234567890abcdef1234567890abcdef"

func TestTrainingMemoryPeakReadsRecursiveHighWaterMark(t *testing.T) {
	r := stoppedRunner()
	m := strictManager(t, r)
	root := fixtureCgroups(t, m)
	group := filepath.Join(root, "trainer.service")
	if err := os.Mkdir(group, 0o700); err != nil {
		t.Fatal(err)
	}
	p := control.WorkloadProfile{ID: "trainer", Unit: "trainer.service", Cgroup: "/workloads/trainer.service", Trainer: &control.TrainerBinding{}}
	job := control.TrainingJob{InvocationID: peakInvocation}
	if err := os.WriteFile(filepath.Join(group, "memory.peak"), []byte("8589934592\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// While the group exists, the larger of the cgroup file and the systemd
	// property counts.
	r.outputs[trainerPeakCommand] = []byte("InvocationID=" + peakInvocation + "\nControlGroup=/workloads/trainer.service\nMemoryPeak=1\n")
	peak, err := m.TrainingMemoryPeak(context.Background(), p, job)
	if err != nil || peak != 8589934592 {
		t.Fatalf("peak = %d, %v", peak, err)
	}
	r.outputs[trainerPeakCommand] = []byte("InvocationID=" + peakInvocation + "\nControlGroup=/workloads/trainer.service\nMemoryPeak=99999999999\n")
	if peak, err := m.TrainingMemoryPeak(context.Background(), p, job); err != nil || peak != 99999999999 {
		t.Fatalf("larger systemd value: peak = %d, %v", peak, err)
	}
	// The confirming snapshot is newer: its larger MemoryPeak counts, not the first.
	r.outputs[trainerPeakCommand] = []byte("InvocationID=" + peakInvocation + "\nControlGroup=/workloads/trainer.service\nMemoryPeak=1\n")
	m.runner = &changingRunner{fakeRunner: r, after: "InvocationID=" + peakInvocation + "\nControlGroup=/workloads/trainer.service\nMemoryPeak=99999999999\n"}
	if peak, err := m.TrainingMemoryPeak(context.Background(), p, job); err != nil || peak != 99999999999 {
		t.Fatalf("newer confirming value: peak = %d, %v", peak, err)
	}
	m.runner = r
	// The live cgroup is the only source when systemd has no measured value:
	// systemd 255 prints [not set], and systemd 252 has no MemoryPeak line.
	for _, property := range []string{"MemoryPeak=[not set]\n", ""} {
		r.outputs[trainerPeakCommand] = []byte("InvocationID=" + peakInvocation + "\nControlGroup=/workloads/trainer.service\n" + property)
		if peak, err := m.TrainingMemoryPeak(context.Background(), p, job); err != nil || peak != 8589934592 {
			t.Fatalf("cgroup only (%q): peak = %d, %v", property, peak, err)
		}
	}
	if _, err := m.TrainingMemoryPeak(context.Background(), control.WorkloadProfile{Unit: p.Unit, Cgroup: p.Cgroup}, job); !errors.Is(err, ErrLaunchUnsupported) {
		t.Fatal("read a peak for a profile without a trainer", err)
	}
	if _, err := m.TrainingMemoryPeak(context.Background(), p, control.TrainingJob{}); !errors.Is(err, ErrLaunchUnsupported) {
		t.Fatal("read a peak for a job without an invocation", err)
	}
	// An unreadable memory.peak in the reported group falls back to systemd.
	for _, value := range []string{"", "max\n"} {
		if value == "" {
			if err := os.Remove(filepath.Join(group, "memory.peak")); err != nil {
				t.Fatal(err)
			}
		} else if err := os.WriteFile(filepath.Join(group, "memory.peak"), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
		r.outputs[trainerPeakCommand] = []byte("InvocationID=" + peakInvocation + "\nControlGroup=/workloads/trainer.service\nMemoryPeak=123\n")
		if peak, err := m.TrainingMemoryPeak(context.Background(), p, job); err != nil || peak != 123 {
			t.Fatalf("memory.peak %q: peak = %d, %v", value, peak, err)
		}
	}
	// A group that changes invocation during the read is not evidence. The first
	// snapshot was verified for this invocation, so its MemoryPeak counts.
	if err := os.WriteFile(filepath.Join(group, "memory.peak"), []byte("8589934592\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	inv := "InvocationID=" + peakInvocation + "\n"
	live := inv + "ControlGroup=/workloads/trainer.service\n"
	other := "InvocationID=ffffffffffffffffffffffffffffffff\nControlGroup=/workloads/trainer.service\n"
	confirmed := func(first, after string, afterErr error) (uint64, error) {
		t.Helper()
		r.outputs[trainerPeakCommand] = []byte(first)
		runner := &changingRunner{fakeRunner: r, after: after, afterErr: afterErr}
		m.runner = runner
		peak, err := m.TrainingMemoryPeak(context.Background(), p, job)
		if runner.calls != 2 {
			t.Fatalf("systemctl calls = %d, want the snapshot and the confirming query", runner.calls)
		}
		return peak, err
	}
	if peak, err := confirmed(live+"MemoryPeak=123\n", other, nil); err != nil || peak != 123 {
		t.Fatalf("invocation change during the read: peak = %d, %v", peak, err)
	}
	if _, err := confirmed(live+"MemoryPeak=[not set]\n", other, nil); err == nil {
		t.Fatal("accepted a peak read across an invocation change")
	}
	// A unit that exits during the read gives the final systemd value.
	if peak, err := confirmed(live+"MemoryPeak=123\n", inv+"ControlGroup=\nMemoryPeak=9663676416\n", nil); err != nil || peak != 9663676416 {
		t.Fatalf("peak after exit during the read = %d, %v", peak, err)
	}
	// A group that systemd no longer reports after the read is not trusted, even
	// for the same invocation. Without a kept systemd value there is no peak.
	if peak, err := confirmed(live+"MemoryPeak=123\n", inv+"ControlGroup=\nMemoryPeak=[not set]\n", nil); err == nil {
		t.Fatalf("trusted a group that systemd no longer reports: %d", peak)
	}
	// A failed confirming query leaves the read unverified: the first snapshot counts.
	if peak, err := confirmed(live+"MemoryPeak=123\n", "", errors.New("systemctl failed")); err != nil || peak != 123 {
		t.Fatalf("peak with a failed confirming query = %d, %v", peak, err)
	}
	// A failed live read uses the newer systemd value, not the first one.
	if err := os.Remove(filepath.Join(group, "memory.peak")); err != nil {
		t.Fatal(err)
	}
	if peak, err := confirmed(live+"MemoryPeak=1\n", inv+"ControlGroup=\nMemoryPeak=777\n", nil); err != nil || peak != 777 {
		t.Fatalf("peak after a failed live read = %d, %v", peak, err)
	}
	if peak, err := confirmed(live+"MemoryPeak=1\n", "", errors.New("systemctl failed")); err != nil || peak != 1 {
		t.Fatalf("peak with no reading and no confirming query = %d, %v", peak, err)
	}
	if _, err := confirmed(live+"MemoryPeak=[not set]\n", "", errors.New("systemctl failed")); err == nil {
		t.Fatal("accepted a peak with no reading, no confirming query and no systemd value")
	}
	m.runner = r
	// Another invocation of the unit is not evidence for this job.
	r.outputs[trainerPeakCommand] = []byte("InvocationID=ffffffffffffffffffffffffffffffff\nControlGroup=/workloads/trainer.service\nMemoryPeak=1\n")
	if _, err := m.TrainingMemoryPeak(context.Background(), p, job); !errors.Is(err, ErrLaunchChanged) {
		t.Fatal("accepted the peak of another invocation", err)
	}
}

func TestTrainingMemoryPeakIsUnmeasuredWhenEvidenceIsMissingOrMalformed(t *testing.T) {
	for name, value := range map[string]string{
		"empty": "", "negative": "-1\n", "text": "max\n", "padded": " 1\n", "leading zero": "01\n",
		"two lines": "1\n2\n", "overflow": "18446744073709551616\n", "oversized": string(make([]byte, 4097)),
	} {
		t.Run(name, func(t *testing.T) {
			m := strictManager(t, stoppedRunner())
			root := fixtureCgroups(t, m)
			group := filepath.Join(root, "trainer.service")
			if err := os.Mkdir(group, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(group, "memory.peak"), []byte(value), 0o600); err != nil {
				t.Fatal(err)
			}
			if peak, err := m.cgroups.memoryPeak("/workloads/trainer.service"); err == nil {
				t.Fatalf("accepted malformed peak %d", peak)
			}
		})
	}
	m := strictManager(t, stoppedRunner())
	root := fixtureCgroups(t, m)
	if _, err := m.cgroups.memoryPeak("/workloads/trainer.service"); !errors.Is(err, errCgroupRemoved) {
		t.Fatal("a removed group is not reported as removed", err)
	}
	if err := os.Mkdir(filepath.Join(root, "trainer.service"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := m.cgroups.memoryPeak("/workloads/trainer.service"); err == nil || errors.Is(err, errCgroupRemoved) {
		t.Fatal("a kernel without memory.peak gave a value or looked removed", err)
	}
	if err := os.Symlink("/dev/zero", filepath.Join(root, "trainer.service", "memory.peak")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.cgroups.memoryPeak("/workloads/trainer.service"); err == nil {
		t.Fatal("followed a symlinked memory.peak")
	}
}

const trainerPeakCommand = "/usr/bin/true --user show --property=InvocationID,ControlGroup,MemoryPeak -- trainer.service"

func TestTrainingMemoryPeakUsesSystemdValueAfterCgroupRemoval(t *testing.T) {
	r := stoppedRunner()
	m := strictManager(t, r)
	root := fixtureCgroups(t, m)
	// systemd already removed the trainer cgroup of the exited or failed unit.
	p := control.WorkloadProfile{ID: "trainer", Unit: "trainer.service", Cgroup: "/workloads/trainer.service", Trainer: &control.TrainerBinding{}}
	job := control.TrainingJob{InvocationID: peakInvocation}
	prefix := "InvocationID=" + peakInvocation + "\nControlGroup=\n"
	r.outputs[trainerPeakCommand] = []byte(prefix + "MemoryPeak=6442450944\n")
	peak, err := m.TrainingMemoryPeak(context.Background(), p, job)
	if err != nil || peak != 6442450944 {
		t.Fatalf("peak = %d, %v", peak, err)
	}
	// A stale group at the configured path is not read when systemd reports no
	// group, even when its value is larger.
	group := filepath.Join(root, "trainer.service")
	if err := os.Mkdir(group, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(group, "memory.peak"), []byte("99999999999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if peak, err := m.TrainingMemoryPeak(context.Background(), p, job); err != nil || peak != 6442450944 {
		t.Fatalf("peak = %d, %v", peak, err)
	}
	for _, output := range []string{"MemoryPeak=[not set]\n", "MemoryPeak=infinity\n", "MemoryPeak=18446744073709551615\n", "MemoryPeak=\n", "", "MemoryPeak=01\n", "MemoryPeak=1\nMemoryPeak=2\n"} {
		r.outputs[trainerPeakCommand] = []byte(prefix + output)
		if peak, err := m.TrainingMemoryPeak(context.Background(), p, job); err == nil {
			t.Fatalf("accepted unmeasured systemd peak %q as %d", output, peak)
		}
	}
	r.errs = map[string]error{trainerPeakCommand: errors.New("systemctl failed")}
	if _, err := m.TrainingMemoryPeak(context.Background(), p, job); err == nil {
		t.Fatal("accepted a failed systemd observation")
	}
}

// changingRunner answers the first peak observation from fakeRunner and later ones with after.
type changingRunner struct {
	*fakeRunner
	after    string
	afterErr error
	calls    int
}

func (r *changingRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	r.calls++
	if r.calls > 1 {
		return []byte(r.after), r.afterErr
	}
	return r.fakeRunner.Run(ctx, name, args...)
}

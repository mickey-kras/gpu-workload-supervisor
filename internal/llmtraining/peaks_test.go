package llmtraining

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/strictjson"
	"golang.org/x/sys/unix"
)

// testWalk walks with the current package hooks.
func testWalk(root string, remaining int, limit walkLimit) (uint64, int, bool) {
	return peakWalkHooks.allocatedBytes(root, remaining, limit)
}

// publishPeaks takes the final sample and writes the file, as RunJob does.
func publishPeaks(p *peakRecorder, work, outcome string) error {
	p.finalSample()
	return p.write(work, outcome)
}

var farDeadline = walkLimit{deadline: time.Now().Add(time.Hour)}

func readPeaks(t *testing.T, jobPath string) resourcePeaks {
	t.Helper()
	path := filepath.Join(filepath.Dir(jobPath), ".llm-"+executionInvocation, "peaks.json")
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal("peaks file is missing", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("peaks file mode = %v", info.Mode().Perm())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var peaks resourcePeaks
	if err := strictjson.DecodeLimited(bytes.NewReader(raw), 4096, &peaks); err != nil {
		t.Fatal(err)
	}
	return peaks
}

// progressScript writes the worker progress file ($8) with a per-phase VRAM sample.
func progressScript(train, reload string) string {
	return "if [ \"$5\" = train ]; then v=" + train + "; else v=" + reload + "; fi\n" +
		"printf '{\"version\":1,\"phase\":\"%s\",\"step\":2,\"vramPeakBytes\":%s}' \"$5\" \"$v\" > \"$8\"\nchmod 600 \"$8\"\n"
}

func TestRunJobRecordsPeaksBeforeCompletion(t *testing.T) {
	r, e, jobPath := executionFixture(t, executionScript)
	// The export is larger than all other files of the execution together.
	r.Budget.ExportBytes = 8 << 20
	script := executionScript + progressScript("7340032", "5242880") +
		"if [ \"$5\" = train ]; then head -c 4194304 /dev/urandom > '" + filepath.Join(r.OutputDirectory, "adapter_model.safetensors") + "'; fi\n"
	updateExecutionScript(t, r, &e, jobPath, script)
	if err := RunJob(context.Background(), jobPath, executionInvocation, executionGPU); err != nil {
		t.Fatal(err)
	}
	peaks := readPeaks(t, jobPath)
	if peaks.Version != 1 || peaks.Outcome != "completed" || peaks.Phase != "export" || peaks.SampleIntervalMS != peakSampleInterval.Milliseconds() {
		t.Fatalf("peaks = %+v", peaks)
	}
	// VRAM keeps the largest phase sample; storage includes the 4 MiB export.
	if peaks.VRAMPeakBytes == nil || *peaks.VRAMPeakBytes != 7340032 {
		t.Fatal("VRAM peak is not the largest phase sample")
	}
	if peaks.RAMPeakBytes == nil || *peaks.RAMPeakBytes == 0 || peaks.StoragePeakBytes == nil || *peaks.StoragePeakBytes < 4<<20 {
		t.Fatalf("RAM or storage peak is missing: %+v", peaks)
	}
}

// TestWorkerProgressContract decodes the progress record that worker.py writes.
// tests/training/test_worker.py asserts that the worker writes these exact bytes.
func TestWorkerProgressContract(t *testing.T) {
	golden, err := os.ReadFile(filepath.Join("testdata", "worker-progress.json"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "progress.json")
	if err := os.WriteFile(path, golden, 0600); err != nil {
		t.Fatal(err)
	}
	p := newPeakRecorder(t.TempDir(), t.TempDir(), nil)
	p.finishWorker(nil, path)
	if p.peaks.VRAMPeakBytes == nil || *p.peaks.VRAMPeakBytes != 9 {
		t.Fatalf("Go did not decode the worker progress record: %v", p.peaks.VRAMPeakBytes)
	}
}

func TestRunJobMeasuresRAMInBytes(t *testing.T) {
	r, e, jobPath := executionFixture(t, executionScript)
	// dd holds a 32 MiB block buffer in its resident set.
	script := executionScript + "if [ \"$5\" = train ]; then dd if=/dev/zero of=/dev/null bs=33554432 count=1 2>/dev/null; fi\n" +
		"printf x > '" + filepath.Join(r.OutputDirectory, "adapter_model.safetensors") + "'\n"
	updateExecutionScript(t, r, &e, jobPath, script)
	if err := RunJob(context.Background(), jobPath, executionInvocation, executionGPU); err != nil {
		t.Fatal(err)
	}
	peaks := readPeaks(t, jobPath)
	if peaks.RAMPeakBytes == nil || *peaks.RAMPeakBytes < 32<<20 || *peaks.RAMPeakBytes >= 1<<30 {
		t.Fatalf("RAM peak is not in bytes: %v", peaks.RAMPeakBytes)
	}
}

func TestRunJobSamplesNothingAfterTheFinalInputChecks(t *testing.T) {
	r, e, jobPath := executionFixture(t, executionScript)
	script := executionScript + "printf x > '" + filepath.Join(r.OutputDirectory, "adapter_model.safetensors") + "'\n"
	updateExecutionScript(t, r, &e, jobPath, script)
	previousStat, previousCheck := peakWalkHooks.stat, checkInputs
	t.Cleanup(func() { peakWalkHooks.stat, checkInputs = previousStat, previousCheck })
	var checked, late atomic.Bool
	checkInputs = func(jobPath string, e jobEnvelope, r Recipe) error {
		checked.Store(true)
		return previousCheck(jobPath, e, r)
	}
	// No storage walk may run once the final input checks start.
	peakWalkHooks.stat = func(dirfd int, name string, stat *unix.Stat_t) error {
		if checked.Load() {
			late.Store(true)
		}
		return previousStat(dirfd, name, stat)
	}
	if err := RunJob(context.Background(), jobPath, executionInvocation, executionGPU); err != nil {
		t.Fatal(err)
	}
	if late.Load() {
		t.Fatal("a storage walk ran after the final input checks")
	}
	if peaks := readPeaks(t, jobPath); peaks.Outcome != "completed" || peaks.StoragePeakBytes == nil {
		t.Fatalf("peaks = %+v", peaks)
	}
}

func TestRunJobWritesPeaksWhenPreparationFails(t *testing.T) {
	_, e, jobPath := executionFixture(t, executionScript)
	previous := prepareFiles
	t.Cleanup(func() { prepareFiles = previous })
	prepareFiles = func(string, Recipe) error { return errors.New("preparation failed") }
	if err := RunJob(context.Background(), jobPath, executionInvocation, executionGPU); err == nil {
		t.Fatal("a failed preparation completed the job")
	}
	if _, err := os.Lstat(e.TerminalFile); !os.IsNotExist(err) {
		t.Fatal("a failed preparation published completion", err)
	}
	if peaks := readPeaks(t, jobPath); peaks.Outcome != "failed" || peaks.Phase != "prepare" || peaks.RAMPeakBytes != nil {
		t.Fatalf("peaks = %+v", peaks)
	}
}

func TestRunJobStopsStorageWalksAtShutdown(t *testing.T) {
	for _, sampled := range []bool{true, false} {
		previous := peakSampleInterval
		t.Cleanup(func() { peakSampleInterval = previous })
		peakSampleInterval = time.Hour
		if sampled {
			peakSampleInterval = 10 * time.Millisecond
		}
		_, _, jobPath := executionFixture(t, "#!/bin/sh\nsleep 30\n")
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(300*time.Millisecond, cancel)
		started := time.Now()
		if err := RunJob(ctx, jobPath, executionInvocation, executionGPU); err == nil {
			t.Fatal("a cancelled job completed")
		}
		if time.Since(started) > 10*time.Second {
			t.Fatal("shutdown waited for the worker or the storage walk")
		}
		peaks := readPeaks(t, jobPath)
		// Complete samples before the signal stay; with none, storage is null.
		if peaks.Outcome != "failed" || (peaks.StoragePeakBytes != nil) != sampled {
			t.Fatalf("sampled=%v peaks = %+v", sampled, peaks)
		}
		peakSampleInterval = previous
	}
}

func TestRunJobRaisesVRAMFromAReloadSample(t *testing.T) {
	r, e, jobPath := executionFixture(t, executionScript)
	script := executionScript + progressScript("5000", "9000") +
		"printf x > '" + filepath.Join(r.OutputDirectory, "adapter_model.safetensors") + "'\n"
	updateExecutionScript(t, r, &e, jobPath, script)
	if err := RunJob(context.Background(), jobPath, executionInvocation, executionGPU); err != nil {
		t.Fatal(err)
	}
	if peaks := readPeaks(t, jobPath); peaks.VRAMPeakBytes == nil || *peaks.VRAMPeakBytes != 9000 {
		t.Fatalf("reload sample did not raise the VRAM peak: %+v", peaks)
	}
}

func TestRunJobRecordsPeaksOnFailure(t *testing.T) {
	for name, script := range map[string]string{
		"worker failure":   "#!/bin/sh\n" + progressScript("4096", "0") + "exit 1\n",
		"invalid progress": "#!/bin/sh\nprintf '{\"version\":1,\"vramPeakBytes\":-1}' > \"$8\"\nchmod 600 \"$8\"\nexit 1\n",
		"public progress":  "#!/bin/sh\n" + progressScript("4096", "0") + "chmod 644 \"$8\"\nexit 1\n",
		"no progress":      "#!/bin/sh\nexit 1\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, e, jobPath := executionFixture(t, script)
			if err := RunJob(context.Background(), jobPath, executionInvocation, executionGPU); err == nil {
				t.Fatal("failed worker completed the job")
			}
			if _, err := os.Lstat(e.TerminalFile); !os.IsNotExist(err) {
				t.Fatal("failed execution published terminal completion", err)
			}
			peaks := readPeaks(t, jobPath)
			if peaks.Outcome != "failed" || peaks.Phase != "train" || peaks.RAMPeakBytes == nil || peaks.StoragePeakBytes == nil {
				t.Fatalf("peaks = %+v", peaks)
			}
			if name == "worker failure" && (peaks.VRAMPeakBytes == nil || *peaks.VRAMPeakBytes != 4096) {
				t.Fatalf("VRAM peak = %v", peaks.VRAMPeakBytes)
			}
			if name != "worker failure" && peaks.VRAMPeakBytes != nil {
				t.Fatalf("VRAM peak from invalid progress = %v", *peaks.VRAMPeakBytes)
			}
		})
	}
}

func TestRunJobKeepsTrainPeaksWhenReloadFails(t *testing.T) {
	r, e, jobPath := executionFixture(t, executionScript)
	script := executionScript + progressScript("9000", "5000") +
		"printf x > '" + filepath.Join(r.OutputDirectory, "adapter_model.safetensors") + "'\n" +
		"if [ \"$5\" = reload ]; then exit 3; fi\n"
	updateExecutionScript(t, r, &e, jobPath, script)
	if err := RunJob(context.Background(), jobPath, executionInvocation, executionGPU); err == nil {
		t.Fatal("failed reload completed the job")
	}
	peaks := readPeaks(t, jobPath)
	if peaks.Outcome != "failed" || peaks.Phase != "reload" || peaks.VRAMPeakBytes == nil || *peaks.VRAMPeakBytes != 9000 {
		t.Fatalf("peaks = %+v", peaks)
	}
	if _, err := os.Lstat(filepath.Join(r.OutputDirectory, provenanceName)); !os.IsNotExist(err) {
		t.Fatal("failed reload published provenance", err)
	}
}

func TestRunJobWritesFailedPeaksWhenTheReceiptFails(t *testing.T) {
	r, e, jobPath := executionFixture(t, executionScript)
	// A directory at the terminal path makes the receipt write fail after the
	// provenance exists.
	script := executionScript + "printf x > '" + filepath.Join(r.OutputDirectory, "adapter_model.safetensors") + "'\n" +
		"if [ \"$5\" = reload ]; then mkdir -m 700 '" + e.TerminalFile + "'; fi\n"
	updateExecutionScript(t, r, &e, jobPath, script)
	if err := RunJob(context.Background(), jobPath, executionInvocation, executionGPU); err == nil {
		t.Fatal("completed without a terminal receipt")
	}
	peaks := readPeaks(t, jobPath)
	if peaks.Outcome != "failed" || peaks.Phase != "export" {
		t.Fatalf("peaks = %+v", peaks)
	}
}

func TestRunJobCompletesWhenPeaksCannotBePublished(t *testing.T) {
	r, e, jobPath := executionFixture(t, executionScript)
	work := filepath.Join(filepath.Dir(jobPath), ".llm-"+executionInvocation)
	// The worker blocks the peaks destination with a directory. The peaks file
	// is evidence, not a completion condition.
	script := executionScript + "mkdir -p '" + filepath.Join(work, "peaks.json", "blocked") + "'\n" +
		"printf x > '" + filepath.Join(r.OutputDirectory, "adapter_model.safetensors") + "'\n"
	updateExecutionScript(t, r, &e, jobPath, script)
	if err := RunJob(context.Background(), jobPath, executionInvocation, executionGPU); err != nil {
		t.Fatal("a peaks write failure failed the job", err)
	}
	if _, err := os.Lstat(e.TerminalFile); err != nil {
		t.Fatal("no completion receipt", err)
	}
	if info, err := os.Lstat(filepath.Join(work, "peaks.json")); err != nil || !info.IsDir() {
		t.Fatal("the blocked peaks destination changed", err)
	}
}

func TestPeakRecorderStagingFailure(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	if err := publishPeaks(newPeakRecorder(missing, missing, nil), missing, "failed"); err == nil || !strings.Contains(err.Error(), "staging") {
		t.Fatal("published peaks into a missing directory", err)
	}
}

func TestAllocatedBytesIsBoundedAndNeverFollowsLinks(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "large"), make([]byte, 1<<20), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "checkpoint"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "checkpoint", "weights"), make([]byte, 64<<10), 0600); err != nil {
		t.Fatal(err)
	}
	total, remaining, complete := testWalk(root, 100, farDeadline)
	if total < 64<<10 || total >= 1<<20 || remaining != 96 || !complete {
		t.Fatalf("total=%d remaining=%d complete=%v", total, remaining, complete)
	}
	// The limit stops the walk, and a truncated walk is not complete.
	for _, limit := range []int{1, 2, 3} {
		if _, _, complete := testWalk(root, limit, farDeadline); complete {
			t.Fatalf("limit %d gave a complete walk", limit)
		}
	}
	if _, _, complete := testWalk(root, 0, farDeadline); complete {
		t.Fatal("a walk with no entry budget was complete")
	}
	// A root that is a link is not followed.
	if _, _, complete := testWalk(filepath.Join(root, "link"), 100, farDeadline); complete {
		t.Fatal("followed a linked root")
	}
	if total, remaining, complete := testWalk(filepath.Join(root, "missing"), 5, farDeadline); total != 0 || remaining != 5 || !complete {
		t.Fatalf("missing root total=%d remaining=%d complete=%v", total, remaining, complete)
	}
	if _, _, complete := testWalk(root, 100, walkLimit{deadline: time.Now().Add(-time.Second)}); complete {
		t.Fatal("a walk past its deadline gave a complete result")
	}
	stopped := make(chan struct{})
	close(stopped)
	if _, _, complete := testWalk(root, 100, walkLimit{deadline: time.Now().Add(time.Hour), done: stopped}); complete {
		t.Fatal("a walk after shutdown gave a complete result")
	}
	previousStat, previousList := peakWalkHooks.stat, peakWalkHooks.list
	t.Cleanup(func() { peakWalkHooks.stat, peakWalkHooks.list = previousStat, previousList })
	peakWalkHooks.stat = func(dirfd int, name string, stat *unix.Stat_t) error {
		if name == "weights" {
			return unix.EACCES
		}
		return previousStat(dirfd, name, stat)
	}
	if _, _, complete := testWalk(root, 100, farDeadline); complete {
		t.Fatal("an unreadable entry gave a complete walk")
	}
	// An entry that a save removed is left out, and the walk stays complete.
	peakWalkHooks.stat = func(dirfd int, name string, stat *unix.Stat_t) error {
		if name == "weights" {
			return unix.ENOENT
		}
		return previousStat(dirfd, name, stat)
	}
	if total, _, complete := testWalk(root, 100, farDeadline); !complete || total >= 64<<10 {
		t.Fatalf("removed entry: total=%d complete=%v", total, complete)
	}
	// Another device under the root is not part of the total.
	peakWalkHooks.stat = func(dirfd int, name string, stat *unix.Stat_t) error {
		err := previousStat(dirfd, name, stat)
		if name == "checkpoint" {
			stat.Dev++
		}
		return err
	}
	if total, _, complete := testWalk(root, 100, farDeadline); !complete || total >= 64<<10 {
		t.Fatalf("crossed a mount: total=%d complete=%v", total, complete)
	}
	peakWalkHooks.stat = previousStat
	// Injected errors make the directory cases independent of the test uid.
	lists := 0
	peakWalkHooks.list = func(fd int, limit int) ([]string, error) {
		lists++
		if lists > 1 {
			return nil, errors.New("permission denied")
		}
		return previousList(fd, limit)
	}
	if _, _, complete := testWalk(root, 100, farDeadline); complete {
		t.Fatal("an unreadable directory gave a complete walk")
	}
	peakWalkHooks.list = previousList
	// A directory that a save removes between the stat and the open is skipped.
	peakWalkHooks.stat = func(dirfd int, name string, stat *unix.Stat_t) error {
		err := previousStat(dirfd, name, stat)
		if name == "checkpoint" {
			if removeErr := os.RemoveAll(filepath.Join(root, "checkpoint")); removeErr != nil {
				t.Fatal(removeErr)
			}
		}
		return err
	}
	if _, _, complete := testWalk(root, 100, farDeadline); !complete {
		t.Fatal("a removed directory made the walk incomplete")
	}
	peakWalkHooks.stat = previousStat
	// A directory replaced by a link during the walk is not followed.
	if err := os.Symlink(outside, filepath.Join(root, "checkpoint")); err != nil {
		t.Fatal(err)
	}
	if total, _, complete := testWalk(root, 100, farDeadline); !complete || total >= 1<<20 {
		t.Fatalf("followed a link: total=%d complete=%v", total, complete)
	}
}

func TestIncompleteStorageWalksGiveNoValue(t *testing.T) {
	work, output := t.TempDir(), t.TempDir()
	for index := 0; index < 3; index++ {
		for _, dir := range []string{work, output} {
			if err := os.WriteFile(filepath.Join(dir, strings.Repeat("f", index+1)), make([]byte, 8192), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	previous := peakStorageEntries
	t.Cleanup(func() { peakStorageEntries = previous })
	// The output root takes 4 entries; the shared budget runs out in the work root.
	peakStorageEntries = 6
	p := newPeakRecorder(work, output, nil)
	p.sampleStorage(nil)
	if p.peaks.StoragePeakBytes != nil {
		t.Fatalf("a truncated walk gave a value: %d", *p.peaks.StoragePeakBytes)
	}
	work2 := t.TempDir()
	if err := publishPeaks(p, work2, "failed"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(work2, "peaks.json"))
	if err != nil || !strings.Contains(string(raw), `"storagePeakBytes":null`) {
		t.Fatalf("truncated storage peak was published: %s %v", raw, err)
	}
	// A complete walk sets the value; a later incomplete walk does not erase it.
	p.entries = previous
	p.sampleStorage(nil)
	if p.peaks.StoragePeakBytes == nil || *p.peaks.StoragePeakBytes < 6*8192 {
		t.Fatalf("complete walk value = %v", p.peaks.StoragePeakBytes)
	}
	kept := *p.peaks.StoragePeakBytes
	stopped := make(chan struct{})
	close(stopped)
	p.sampleStorage(stopped)
	if p.peaks.StoragePeakBytes == nil || *p.peaks.StoragePeakBytes != kept {
		t.Fatal("an interrupted walk changed the value")
	}
}

func TestStopEndsAStorageWalkInProgress(t *testing.T) {
	root := t.TempDir()
	for index := 0; index < 200; index++ {
		if err := os.WriteFile(filepath.Join(root, strings.Repeat("f", index%50+1)+string(rune('a'+index/50))), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	previousInterval, previousStat := peakSampleInterval, peakWalkHooks.stat
	t.Cleanup(func() { peakSampleInterval, peakWalkHooks.stat = previousInterval, previousStat })
	peakSampleInterval = time.Millisecond
	walking := make(chan struct{}, 1)
	// Each entry takes 20 ms, so one walk takes about 4 seconds.
	peakWalkHooks.stat = func(dirfd int, name string, stat *unix.Stat_t) error {
		select {
		case walking <- struct{}{}:
		default:
		}
		time.Sleep(20 * time.Millisecond)
		return previousStat(dirfd, name, stat)
	}
	p := newPeakRecorder(t.TempDir(), root, nil)
	stop := p.watch()
	<-walking
	started := time.Now()
	stop()
	// The walk stops at its next entry, well before the stop wait bound.
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("stop waited %v for the walk", elapsed)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.peaks.StoragePeakBytes != nil {
		t.Fatal("an interrupted walk gave a value")
	}
}

func TestFinalSampleRunsOnce(t *testing.T) {
	p := newPeakRecorder(t.TempDir(), t.TempDir(), nil)
	list := p.hooks.list
	lists := 0
	p.hooks.list = func(fd int, limit int) ([]string, error) {
		lists++
		return list(fd, limit)
	}
	p.finalSample()
	once := lists
	p.finalSample()
	if once == 0 || lists != once {
		t.Fatalf("lists after one sample = %d, after two = %d", once, lists)
	}
}

func TestLateStorageWalkResultIsDropped(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "save"), make([]byte, 8192), 0600); err != nil {
		t.Fatal(err)
	}
	// The work root is walked last, so its last entry ends the whole sample.
	p := newPeakRecorder(root, t.TempDir(), nil)
	stat := p.hooks.stat
	entered, release, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
	p.hooks.stat = func(dirfd int, name string, st *unix.Stat_t) error {
		if name == "save" {
			close(entered)
			<-release
		}
		return stat(dirfd, name, st)
	}
	stop := make(chan struct{})
	go func() {
		defer close(returned)
		p.sampleStorage(stop)
	}()
	<-entered
	// The caller stops waiting; the walk then completes its last entry.
	close(stop)
	close(release)
	<-returned
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.peaks.StoragePeakBytes != nil {
		t.Fatalf("a late walk raised the peak to %d", *p.peaks.StoragePeakBytes)
	}
}

func TestFinalWalkAfterItsWaitCannotSetStorage(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "save"), make([]byte, 8192), 0600); err != nil {
		t.Fatal(err)
	}
	// The work root is walked last, so its last entry ends the whole sample.
	p := newPeakRecorder(root, t.TempDir(), nil)
	stat := p.hooks.stat
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	p.hooks.stat = func(dirfd int, name string, st *unix.Stat_t) error {
		if name == "save" {
			close(entered)
			<-release
			defer close(finished)
		}
		return stat(dirfd, name, st)
	}
	go func() {
		<-entered
		time.Sleep(100 * time.Millisecond)
		close(release)
	}()
	p.boundedSample(make(chan struct{}), 20*time.Millisecond)
	<-finished
	time.Sleep(100 * time.Millisecond)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.peaks.StoragePeakBytes != nil {
		t.Fatalf("a walk after its wait set the peak to %d", *p.peaks.StoragePeakBytes)
	}
}

func TestBlockedStorageWalkNeverHoldsTheJob(t *testing.T) {
	r, e, jobPath := executionFixture(t, executionScript)
	script := executionScript + "printf x > '" + filepath.Join(r.OutputDirectory, "adapter_model.safetensors") + "'\n"
	updateExecutionScript(t, r, &e, jobPath, script)
	previousStat, previousInterval, previousWait, previousTimeout := peakWalkHooks.stat, peakSampleInterval, peakStopWait, peakWalkTimeout
	release := make(chan struct{})
	t.Cleanup(func() {
		close(release)
		peakWalkHooks.stat, peakSampleInterval, peakStopWait, peakWalkTimeout = previousStat, previousInterval, previousWait, previousTimeout
	})
	peakSampleInterval, peakStopWait, peakWalkTimeout = time.Millisecond, 50*time.Millisecond, 50*time.Millisecond
	// Every filesystem call of the walk blocks, as on a hung mount.
	peakWalkHooks.stat = func(int, string, *unix.Stat_t) error {
		<-release
		return unix.ENOENT
	}
	started := time.Now()
	if err := RunJob(context.Background(), jobPath, executionInvocation, executionGPU); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("a blocked walk held the job for %v", elapsed)
	}
	if peaks := readPeaks(t, jobPath); peaks.Outcome != "completed" || peaks.StoragePeakBytes != nil {
		t.Fatalf("peaks = %+v", peaks)
	}
}

func TestStorageSamplerSeesATransientSaveInTheOutput(t *testing.T) {
	previous := peakSampleInterval
	peakSampleInterval = 10 * time.Millisecond
	t.Cleanup(func() { peakSampleInterval = previous })
	r, e, jobPath := executionFixture(t, executionScript)
	// A save in progress exists only while the worker runs.
	save := filepath.Join(r.OutputDirectory, "checkpoint-1.pending")
	script := executionScript + "if [ \"$5\" = train ]; then head -c 8388608 /dev/urandom > '" + save + "'; sleep 0.3; rm '" + save + "'; fi\n" +
		"printf x > '" + filepath.Join(r.OutputDirectory, "adapter_model.safetensors") + "'\n"
	updateExecutionScript(t, r, &e, jobPath, script)
	if err := RunJob(context.Background(), jobPath, executionInvocation, executionGPU); err != nil {
		t.Fatal(err)
	}
	peaks := readPeaks(t, jobPath)
	if peaks.StoragePeakBytes == nil || *peaks.StoragePeakBytes < 8<<20 || peaks.SampleIntervalMS != 10 {
		t.Fatalf("storage sampler missed the transient save: %+v", peaks)
	}
}

func TestCgroupMemoryPeakReadsTheRunnerCgroup(t *testing.T) {
	root := t.TempDir()
	previousFile, previousMount := selfCgroupFile, cgroupMount
	t.Cleanup(func() { selfCgroupFile, cgroupMount = previousFile, previousMount })
	selfCgroupFile, cgroupMount = filepath.Join(root, "cgroup"), filepath.Join(root, "mount")
	group := filepath.Join(cgroupMount, "user.slice", "trainer.service")
	if err := os.MkdirAll(group, 0700); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(group, "memory.peak"), "4294967296\n")
	writeFixture(t, selfCgroupFile, "0::/user.slice/trainer.service\n")
	if peak, err := cgroupMemoryPeak(selfCgroupFile, cgroupMount); err != nil || peak != 4294967296 {
		t.Fatalf("peak = %d, %v", peak, err)
	}
	for _, line := range []string{"", "1:memory:/user.slice/trainer.service\n", "0::relative\n", "0::/user.slice/../trainer.service\n", "0::/missing\n"} {
		writeFixture(t, selfCgroupFile, line)
		if _, err := cgroupMemoryPeak(selfCgroupFile, cgroupMount); err == nil {
			t.Fatalf("accepted cgroup line %q", line)
		}
	}
	writeFixture(t, selfCgroupFile, "0::/user.slice/trainer.service\n")
	for _, value := range []string{"max\n", "01\n", " 1\n", strings.Repeat("9", 65)} {
		writeFixture(t, filepath.Join(group, "memory.peak"), value)
		if _, err := cgroupMemoryPeak(selfCgroupFile, cgroupMount); err == nil {
			t.Fatalf("accepted memory.peak %q", value)
		}
	}
	work := t.TempDir()
	writeFixture(t, filepath.Join(group, "memory.peak"), "1024\n")
	if err := publishPeaks(newPeakRecorder(work, t.TempDir(), nil), work, "failed"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(work, "peaks.json"))
	if err != nil || !strings.Contains(string(raw), `"cgroupMemoryPeakBytes":1024`) {
		t.Fatalf("peaks file has no cgroup peak: %s %v", raw, err)
	}
}

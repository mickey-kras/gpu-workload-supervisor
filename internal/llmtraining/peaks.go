package llmtraining

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/strictjson"
)

// Tests replace these values. A recorder copies them when it is created.
var (
	// The storage walk stops at this many entries. A truncated walk gives no value.
	peakStorageEntries = 65536
	peakSampleInterval = time.Second
	// One storage walk stops at this deadline. A late walk gives no value.
	peakWalkTimeout = 10 * time.Second
	// The runner waits at most this long past a stop for a walk in progress.
	// A walk that ends later is detached, and its result is dropped.
	peakStopWait = time.Second
	// peakWalkHooks are the filesystem calls of the storage walk. The recorder
	// copies them; unit tests set the hooks of one recorder or walk instead.
	peakWalkHooks  = walkHooks{stat: statEntry, list: listNames}
	selfCgroupFile = "/proc/self/cgroup"
	cgroupMount    = "/sys/fs/cgroup"
)

// resourcePeaks is the bounded peaks file in the private execution directory.
// A null value is unmeasured. Sampled values are lower bounds, not exact peaks.
type resourcePeaks struct {
	Version          int     `json:"version"`
	Outcome          string  `json:"outcome"`
	Phase            string  `json:"phase"`
	RAMPeakBytes     *uint64 `json:"ramPeakBytes"`
	VRAMPeakBytes    *uint64 `json:"vramPeakBytes"`
	StoragePeakBytes *uint64 `json:"storagePeakBytes"`
	// CgroupMemoryPeakBytes is the memory.peak of the runner's own cgroup, the
	// trainer unit, read with the final sample.
	CgroupMemoryPeakBytes *uint64 `json:"cgroupMemoryPeakBytes"`
	SampleIntervalMS      int64   `json:"sampleIntervalMs"`
}

// workerProgress is the bounded progress file of the embedded worker.
type workerProgress struct {
	Version       int     `json:"version"`
	Phase         string  `json:"phase"`
	Step          int     `json:"step"`
	VRAMPeakBytes *uint64 `json:"vramPeakBytes"`
}

// peakRecorder keeps the high-water marks of one execution. RAM is the largest
// ru_maxrss of a worker and the descendants that it reaped; it is not a sum.
// VRAM comes from the worker progress samples. Storage is the allocated size of
// the output directory and the execution directory: retained checkpoints, a
// save in progress, the cache and the export.
type peakRecorder struct {
	mu    sync.Mutex
	roots []string
	peaks resourcePeaks
	// done stops storage walks at shutdown. A nil channel never stops them.
	done        <-chan struct{}
	entries     int
	interval    time.Duration
	walkTimeout time.Duration
	stopWait    time.Duration
	hooks       walkHooks
	selfCgroup  string
	cgroupMount string
	final       sync.Once
}

// walkLimit stops one storage walk at its deadline, at shutdown or when the
// phase sampler stops. A nil channel never fires.
type walkLimit struct {
	deadline time.Time
	done     <-chan struct{}
	stop     <-chan struct{}
}

// stopped is true after shutdown or after the caller stopped waiting.
func (l walkLimit) stopped() bool {
	select {
	case <-l.done:
		return true
	case <-l.stop:
		return true
	default:
		return false
	}
}

func (l walkLimit) reached() bool {
	return l.stopped() || time.Now().After(l.deadline)
}

func newPeakRecorder(work, output string, done <-chan struct{}) *peakRecorder {
	// The recorder copies the tunable values once, so walks that run in other
	// goroutines never read the package variables.
	return &peakRecorder{
		roots: []string{output, work}, done: done, peaks: resourcePeaks{Version: 1, Phase: "prepare", SampleIntervalMS: peakSampleInterval.Milliseconds()},
		entries: peakStorageEntries, interval: peakSampleInterval, walkTimeout: peakWalkTimeout, stopWait: peakStopWait,
		hooks: peakWalkHooks, selfCgroup: selfCgroupFile, cgroupMount: cgroupMount,
	}
}

func raisePeak(target **uint64, value uint64) {
	if *target == nil || **target < value {
		*target = &value
	}
}

func (p *peakRecorder) startPhase(phase string) {
	p.mu.Lock()
	p.peaks.Phase = phase
	p.mu.Unlock()
}

// sampleStorage raises the storage peak from one complete walk. An incomplete
// walk gives no value, because a partial total is not a usable capacity value.
func (p *peakRecorder) sampleStorage(stop <-chan struct{}) {
	total := uint64(0)
	remaining := p.entries
	limit := walkLimit{time.Now().Add(p.walkTimeout), p.done, stop}
	for _, root := range p.roots {
		size, left, complete := p.hooks.allocatedBytes(root, remaining, limit)
		if !complete {
			return
		}
		total += size
		remaining = left
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	// A walk that ends after its stop is late: the caller no longer waits.
	if limit.stopped() {
		return
	}
	raisePeak(&p.peaks.StoragePeakBytes, total)
}

// boundedSample runs one storage sample and waits at most wait for it. A
// sample that is still running after that is detached; stop drops its result.
func (p *peakRecorder) boundedSample(stop chan struct{}, wait time.Duration) {
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		p.sampleStorage(stop)
	}()
	select {
	case <-finished:
	case <-time.After(wait):
		close(stop)
	}
}

// cgroupMemoryPeak reads memory.peak of the cgroup that holds this process.
func cgroupMemoryPeak(selfFile, mount string) (uint64, error) {
	raw, err := readBounded(selfFile, 4096)
	if err != nil {
		return 0, err
	}
	group, ok := strings.CutPrefix(strings.TrimSuffix(string(raw), "\n"), "0::")
	if !ok || !strings.HasPrefix(group, "/") || filepath.Clean(group) != group || strings.ContainsAny(group, "\n\x00") {
		return 0, errors.New("training cgroup is not a unified cgroup")
	}
	raw, err = readBounded(filepath.Join(mount, group, "memory.peak"), 64)
	if err != nil {
		return 0, err
	}
	peak, ok := control.CanonicalUint(strings.TrimSuffix(string(raw), "\n"))
	if !ok {
		return 0, errors.New("training cgroup memory.peak is malformed")
	}
	return peak, nil
}

func readBounded(path string, limit int64) ([]byte, error) {
	f, err := openRegular(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(raw)) > limit {
		return nil, errors.New("training resource file is unreadable or too large")
	}
	return raw, nil
}

// watch samples storage until the returned function is called. That call also
// stops a walk in progress. finalSample takes the sample at the end of the job.
func (p *peakRecorder) watch() func() {
	done := make(chan struct{})
	var wait sync.WaitGroup
	wait.Add(1)
	go func() {
		defer wait.Done()
		ticker := time.NewTicker(p.interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				p.sampleStorage(done)
			}
		}
	}()
	return func() {
		close(done)
		finished := make(chan struct{})
		go func() {
			wait.Wait()
			close(finished)
		}()
		// A blocked filesystem call cannot delay the worker result for long.
		select {
		case <-finished:
		case <-time.After(p.stopWait):
		}
	}
}

// finishWorker records the peak resident set that wait4 reports for the worker
// and the last VRAM sample of its progress file. Both survive a failed worker.
func (p *peakRecorder) finishWorker(state *os.ProcessState, progressPath string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if state != nil {
		if usage, ok := state.SysUsage().(*syscall.Rusage); ok && usage.Maxrss > 0 {
			// Linux reports ru_maxrss in KiB.
			raisePeak(&p.peaks.RAMPeakBytes, uint64(usage.Maxrss)*1024)
		}
	}
	raw, err := readJobInput(progressPath, 4096, true)
	if err != nil {
		return
	}
	var progress workerProgress
	if strictjson.DecodeLimited(bytes.NewReader(raw), 4096, &progress) != nil || progress.Version != 1 || progress.VRAMPeakBytes == nil {
		return
	}
	raisePeak(&p.peaks.VRAMPeakBytes, *progress.VRAMPeakBytes)
}

// finalSample takes the last storage sample and reads the cgroup memory peak.
// A completed run calls it before its final input checks, so slow work never
// separates those checks from the receipt.
func (p *peakRecorder) finalSample() {
	// At most once: a run that fails after its final sample never walks again.
	p.final.Do(func() {
		p.boundedSample(make(chan struct{}), p.walkTimeout+p.stopWait)
		if peak, err := cgroupMemoryPeak(p.selfCgroup, p.cgroupMount); err == nil {
			p.mu.Lock()
			raisePeak(&p.peaks.CgroupMemoryPeakBytes, peak)
			p.mu.Unlock()
		}
	})
}

// write encodes the peaks with their outcome and writes the small file
// atomically with private permissions. It does no sampling.
func (p *peakRecorder) write(work, outcome string) error {
	p.mu.Lock()
	p.peaks.Outcome = outcome
	raw, err := json.Marshal(p.peaks)
	p.mu.Unlock()
	if err != nil {
		return errors.New("training resource peaks cannot be encoded")
	}
	return publishPrivateFile(filepath.Join(work, "peaks.json"), ".peaks-*", raw, "training resource peaks")
}

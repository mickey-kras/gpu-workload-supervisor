package modelseal

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/attestation"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/strictjson"
)

// maxScannerOutput bounds the combined standard output and error of one
// scan. The adapter discards that output: it can hold model text.
const maxScannerOutput = 64 << 20

// timeoutUnit scales TimeoutSeconds. Tests shorten it.
var timeoutUnit = time.Second

// killDelay bounds the wait for output pipes after the process group is killed.
const killDelay = 5 * time.Second

// runSteps follow the support checks in every report.
var runSteps = []string{"runtime-inventory", "execution", "artifact-unchanged", "verdict-format", "behavioral-probe"}

// assessment is the scan evidence and its protected report, if any.
type assessment struct {
	scan   attestation.Scan
	report []byte
}

// notScanned is the evidence of an intentionally skipped scan. It carries no
// scanner evidence, so it can never look like a pass.
func notScanned() assessment {
	return assessment{scan: attestation.Scan{Status: attestation.ScanNotScanned}}
}

// assess runs the configured scanner against one target in a private scratch
// directory below scratchParent and builds bounded evidence. Unsupported
// artifacts never run. Every failure gives error evidence, never a pass.
func assess(ctx context.Context, target Target, taken inventory, config ScannerConfig, scratchParent string) (assessment, error) {
	scanner, err := config.identity()
	if err != nil {
		return assessment{}, err
	}
	report := Report{Version: ReportVersion, Artifact: taken.digest, Scanner: scanner}
	report.Checks = supportChecks(target, taken)
	report.Status = statusOf(report.Checks)
	if report.Status == "" {
		runChecks(ctx, target, taken, config, scratchParent, &report)
	} else {
		report.Checks = append(report.Checks, skipped(runSteps...)...)
	}
	raw, err := marshalReport(report)
	if err != nil {
		return assessment{}, err
	}
	sum := sha256.Sum256(raw)
	scan := attestation.Scan{Status: report.Status, Scanner: &scanner, ReportSHA256: hex.EncodeToString(sum[:])}
	if report.Result != nil {
		scan.Coverage, scan.Verdict = report.Coverage, report.Result.Verdict
	}
	return assessment{scan: scan, report: raw}, nil
}

// runChecks verifies the runtime, runs the scanner and reads its verdict.
// The first failed step sets the error status and skips the rest.
func runChecks(ctx context.Context, target Target, taken inventory, config ScannerConfig, scratchParent string, report *Report) {
	steps := runSteps
	fail := func(step int, outcome string) {
		report.Status = attestation.ScanError
		report.Checks = append(report.Checks, Check{Name: steps[step], Outcome: outcome})
		report.Checks = append(report.Checks, skipped(steps[step+1:]...)...)
	}
	if config.Runtime.Inventory.Inspect() != nil {
		fail(0, OutcomeError)
		return
	}
	report.Checks = append(report.Checks, Check{Name: steps[0], Outcome: OutcomePassed})
	scratch, err := os.MkdirTemp(scratchParent, ".gpu-model-seal-scan-*")
	if err != nil {
		fail(1, OutcomeError)
		return
	}
	defer os.RemoveAll(scratch)
	if outcome := execute(ctx, config, target.path(), scratch); outcome != OutcomePassed {
		fail(1, outcome)
		return
	}
	report.Checks = append(report.Checks, Check{Name: steps[1], Outcome: OutcomePassed})
	if !taken.unchanged() {
		fail(2, OutcomeError)
		return
	}
	report.Checks = append(report.Checks, Check{Name: steps[2], Outcome: OutcomePassed})
	verdict, err := readVerdict(scratch)
	if err != nil || !verdict.consistent(config) {
		fail(3, OutcomeError)
		return
	}
	report.Checks = append(report.Checks, Check{Name: steps[3], Outcome: OutcomePassed})
	report.Result, report.Coverage, report.Status = &verdict, attestation.CoverageComplete, attestation.ScanPassed
	outcome := OutcomePassed
	if verdict.Verdict == "SUSPICIOUS" {
		report.Status, outcome = attestation.ScanFindings, OutcomeFindings
	}
	report.Checks = append(report.Checks, Check{Name: steps[4], Outcome: outcome})
}

func skipped(names ...string) []Check {
	checks := make([]Check, 0, len(names))
	for _, name := range names {
		checks = append(checks, Check{Name: name, Outcome: OutcomeSkipped})
	}
	return checks
}

// statusOf gives error before unsupported, so a broken input never reads
// as a format limit.
func statusOf(checks []Check) attestation.ScanStatus {
	status := attestation.ScanStatus("")
	for _, check := range checks {
		switch check.Outcome {
		case OutcomeError:
			return attestation.ScanError
		case OutcomeUnsupported:
			status = attestation.ScanUnsupported
		}
	}
	return status
}

// supportChecks declare what the adapter supports: the safetensors files
// form of one of the declared architectures, with the tokenizer in the same
// root, no adapter, no quantization and no remote code.
func supportChecks(target Target, taken inventory) []Check {
	files := taken.artifact.Files
	format, adapter, tokenizer := OutcomeUnsupported, OutcomePassed, OutcomeUnsupported
	if target.Root != "" && hasSafetensorsOnly(files) {
		format = OutcomePassed
	}
	for name := range files {
		if path.Base(name) == "adapter_config.json" {
			adapter = OutcomeUnsupported
		}
	}
	if files["tokenizer_config.json"] != "" {
		tokenizer = OutcomePassed
	}
	return []Check{
		{Name: "format", Outcome: format},
		{Name: "adapter", Outcome: adapter},
		{Name: "architecture", Outcome: architecture(target, files)},
		{Name: "tokenizer", Outcome: tokenizer},
	}
}

func hasSafetensorsOnly(files map[string]string) bool {
	weights := false
	for name := range files {
		ext := path.Ext(name)
		if otherWeights[ext] {
			return false
		}
		weights = weights || ext == ".safetensors"
	}
	return weights
}

// architecture reads config.json from the root. Its bytes must still match
// the inventory, so the declared support binds the sealed bytes.
func architecture(target Target, files map[string]string) string {
	if target.Root == "" || files["config.json"] == "" {
		return OutcomeUnsupported
	}
	f, err := openRegular(filepath.Join(target.Root, "config.json"))
	if err != nil {
		return OutcomeError
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	sum := sha256.Sum256(raw)
	if err != nil || hex.EncodeToString(sum[:]) != files["config.json"] {
		return OutcomeError
	}
	var config map[string]json.RawMessage
	if !utf8.Valid(raw) || strictjson.DecodeLimited(bytes.NewReader(raw), 1<<20, &config) != nil || config == nil {
		return OutcomeUnsupported
	}
	if supportedConfig(config) {
		return OutcomePassed
	}
	return OutcomeUnsupported
}

func supportedConfig(config map[string]json.RawMessage) bool {
	for _, key := range []string{"auto_map", "quantization_config"} {
		if _, exists := config[key]; exists {
			return false
		}
	}
	var architectures []string
	var modelType string
	if json.Unmarshal(config["architectures"], &architectures) != nil || len(architectures) != 1 || json.Unmarshal(config["model_type"], &modelType) != nil {
		return false
	}
	expected, known := supportedArchitectures[architectures[0]]
	return known && expected == modelType
}

// boundedSink counts output and cancels the scan when it exceeds the limit.
type boundedSink struct {
	written  atomic.Int64
	exceeded atomic.Bool
	cancel   context.CancelFunc
}

func (s *boundedSink) Write(p []byte) (int, error) {
	if s.written.Add(int64(len(p))) > maxScannerOutput && !s.exceeded.Swap(true) {
		s.cancel()
	}
	return len(p), nil
}

// execute runs the inventoried wrapper with fixed, validated arguments, a
// minimal offline environment and no standard input. The wrapper runs the
// pinned scanner stages and writes verdict.json into scratch. A timeout or
// output overrun kills the whole process group.
func execute(parent context.Context, config ScannerConfig, modelRoot, scratch string) string {
	ctx, cancel := context.WithTimeout(parent, time.Duration(config.TimeoutSeconds)*timeoutUnit)
	defer cancel()
	sink := &boundedSink{cancel: cancel}
	command := exec.CommandContext(ctx, filepath.Join(config.Runtime.Inventory.Root, config.Runtime.Entrypoint),
		"--model-dir", modelRoot, "--output-dir", scratch, "--experiment", config.Experiment,
		"--threshold", strconv.FormatFloat(config.Threshold, 'g', -1, 64))
	command.Dir = scratch
	command.Env = scannerEnvironment(config, scratch)
	command.Stdout, command.Stderr = sink, sink
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	command.Cancel = func() error { return syscall.Kill(-command.Process.Pid, syscall.SIGKILL) }
	command.WaitDelay = killDelay
	err := command.Run()
	if command.Process != nil {
		// Stop any process that the wrapper left in its group.
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	}
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return OutcomeTimeout
	case sink.exceeded.Load() || err != nil:
		return OutcomeError
	}
	return OutcomePassed
}

// scannerEnvironment passes no inherited variable, so no credential or
// signing material reaches the scanner. Model hubs stay offline.
func scannerEnvironment(config ScannerConfig, scratch string) []string {
	env := []string{
		"PATH=/usr/bin:/bin", "HOME=" + scratch, "TMPDIR=" + scratch, "LANG=C.UTF-8",
		"HF_HUB_OFFLINE=1", "HF_DATASETS_OFFLINE=1", "TRANSFORMERS_OFFLINE=1", "HF_HUB_DISABLE_TELEMETRY=1",
		"PYTHONDONTWRITEBYTECODE=1", "PYTHONNOUSERSITE=1",
	}
	if config.Devices != "" {
		env = append(env, "CUDA_VISIBLE_DEVICES="+config.Devices)
	}
	return env
}

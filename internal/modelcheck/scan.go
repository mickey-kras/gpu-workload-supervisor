package modelcheck

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/strictjson"
)

// This file is the behavioral scan of one exact build, as #585 stages it
// before the real-GPU spike: the support declaration, the scan job, the
// isolated unit that runs it and the bounded verdict that comes back. The
// unit runs a pinned third-party scanner; this repository pins its
// revision and its entrypoint path and maps its verdict to a check. What
// the spike validates on a real GPU is the declared support list, the
// pinned scanner environment and the scan duration; until setup installs
// that environment, no build ships a scanner and the behavior check stays
// not run.

// Reasons of the behavioral scan. They are fixed codes; they never hold a
// path, model content or scanner output.
const (
	// ReasonScanFindings tells that the scanner loaded the build and found
	// backdoor behavior. It is the only failing outcome of the scan.
	ReasonScanFindings = "findings"
	// ReasonScanFailed tells that the scan unit did not complete.
	ReasonScanFailed = "scanner-failed"
	// ReasonScanTimeout tells that the scan unit overran its time limit.
	ReasonScanTimeout = "scan-timeout"
	// ReasonScanVerdict tells that the unit wrote no usable verdict.
	ReasonScanVerdict = "verdict-invalid"
	// ReasonUnsupportedArch, ReasonUnsupportedFormat and
	// ReasonUnsupportedHardware tell that the declared support of the
	// scanner does not cover this exact build; the scanner does not run.
	ReasonUnsupportedArch     = "unsupported-architecture"
	ReasonUnsupportedFormat   = "unsupported-format"
	ReasonUnsupportedHardware = "unsupported-hardware"
)

// The supported weight formats, after a quantized build decompressed its
// weights to BF16 for the scan. The declared formats of the serving
// target: non-quantized BF16 safetensors, and the NVFP4, INT4 and FP8
// quantized serving builds.
const (
	scanFormatBF16  = "bf16"
	scanFormatNVFP4 = "nvfp4"
	scanFormatINT4  = "int4"
	scanFormatFP8   = "fp8"
)

// scanArchitectures declares the config.json architectures values that the
// scanner runs on. The real-GPU spike of #585 extends or corrects this
// list before the scan enforces at launch.
var scanArchitectures = map[string]bool{"Qwen3_5ForConditionalGeneration": true}

// scanQuantMethods maps a recognized quantization_config quant_method to
// the declared format name.
var scanQuantMethods = map[string]string{
	"nvfp4":      scanFormatNVFP4,
	"int4":       scanFormatINT4,
	"fp8":        scanFormatFP8,
	"fbgemm_fp8": scanFormatFP8,
}

// maxScanHeaderBytes bounds the safetensors header read for format
// detection. The header of a large mixture-of-experts model stays below
// this; a larger claim is not a weights header.
const maxScanHeaderBytes = 16 << 20

// MaxScanVerdictBytes bounds the verdict JSON that the scan unit writes
// (4 KiB). It is the only output of the unit that the supervisor reads.
const MaxScanVerdictBytes = 4 << 10

// ScannerSpec pins the scanner code that runs a scan unit: the scanner
// revision and the absolute path of its entrypoint, as installed by the
// setup installer.
type ScannerSpec struct {
	Revision string `json:"revision"`
	Command  string `json:"command"`
}

// ScanJob is the bounded input of one behavioral scan of one exact build.
// Files are the inventory SHA-256 values of the model files, so the unit
// runs on the exact bytes that the check bound. The supervisor runs the
// job as a finite job on one GPU: it waits for the GPU, runs the unit to
// completion and releases the GPU. The unit cannot write supervisor state;
// the supervisor writes the result itself from the unit's bounded verdict.
type ScanJob struct {
	Version int               `json:"version"`
	Model   string            `json:"model"`
	Root    string            `json:"root"`
	Files   map[string]string `json:"files"`
	Scanner ScannerSpec       `json:"scanner"`
	// GPU is the UUID of the one GPU assigned to the unit, as
	// CUDA_VISIBLE_DEVICES. An empty or invalid GPU is an unsupported
	// build, not an invalid job: the scanner does not run.
	GPU string `json:"gpu"`
}

// valid checks the structure of the job. It accepts an empty GPU.
func (j ScanJob) valid() bool {
	return j.Version == 1 && modelPattern.MatchString(j.Model) && cleanAbsolute(j.Root) &&
		validScanFiles(j.Files) && checkerPattern.MatchString(j.Scanner.Revision) &&
		cleanAbsolute(j.Scanner.Command) && (j.GPU == "" || control.ValidGPUUUID(j.GPU))
}

// validScanFiles needs a bounded digest set like a record does.
func validScanFiles(files map[string]string) bool {
	if len(files) == 0 || len(files) > maxRecordFiles {
		return false
	}
	for name, digest := range files {
		if !filepath.IsLocal(name) || !sha256Pattern.MatchString(digest) {
			return false
		}
	}
	return true
}

// Scanner runs the behavioral scan of one exact build. The host scanner is
// the pinned scanner of the setup-installed environment, running as an
// isolated unit on one GPU. Tests substitute their own scanner.
type Scanner interface {
	// Revision names the pinned scanner revision that runs.
	Revision() string
	// Scan classifies the build and, when the declaration supports it,
	// runs the scan unit. It gives the check of CheckBehavior. Its error
	// is only the stop of ctx; every scan failure is an outcome.
	Scan(ctx context.Context, model, root string, files map[string]string) (Check, error)
}

// hostScanner is the pinned scanner of one GPU host: the scanner spec and
// the UUID of the GPU assigned to this process.
type hostScanner struct {
	spec ScannerSpec
	gpu  string
}

// NewHostScanner returns the Scanner of one installed scanner
// environment: the pinned scanner spec and the GPU the scan builds on. The
// setup installer activates this path after it installs an environment and
// points current at it. An empty gpu reports unsupported-hardware.
func NewHostScanner(spec ScannerSpec, gpu string) Scanner {
	return hostScanner{spec: spec, gpu: gpu}
}

// Revision names the pinned scanner revision.
func (s hostScanner) Revision() string { return s.spec.Revision }

// Scan classifies the build and runs the scan unit on the assigned GPU.
func (s hostScanner) Scan(ctx context.Context, model, root string, files map[string]string) (Check, error) {
	return RunScanJob(ctx, ScanJob{Version: 1, Model: model, Root: root, Files: files, Scanner: s.spec, GPU: s.gpu})
}

// RunScanJob runs the behavioral scan of one exact build as an isolated
// unit and gives its check. The unit gets a private temporary directory,
// no network, no token or proxy variables, the read model files and the
// one assigned GPU; it writes a bounded strict verdict. The job re-
// inventories the model files after the unit, so a unit that wrote model
// files gives files-changed. A stop of ctx gives no check and the context
// error, except a time limit, which the scan reports as scan-timeout.
func RunScanJob(ctx context.Context, job ScanJob) (Check, error) {
	if !job.valid() {
		return Check{}, errors.New("scan job is not valid")
	}
	if err := Stopped(ctx); err != nil {
		return Check{}, err
	}
	_, _, reason, err := scanSupport(ctx, job.Root, job.Files)
	if err != nil {
		if stop := Stopped(ctx); stop != nil {
			return Check{}, stop
		}
		return scanReadOutcome(err), nil
	}
	switch {
	case reason != "":
		return Check{Name: CheckBehavior, Outcome: OutcomeUnsupported, Reason: reason}, nil
	case job.GPU == "" || !control.ValidGPUUUID(job.GPU):
		return Check{Name: CheckBehavior, Outcome: OutcomeUnsupported, Reason: ReasonUnsupportedHardware}, nil
	}
	check, err := runScanUnit(ctx, job)
	if err != nil {
		return Check{}, err
	}
	if check.Outcome == OutcomeNotRun {
		return Check{}, errors.New("scan unit gave no check")
	}
	return check, nil
}

// scanReadOutcome maps a classification read failure to an error check. A
// file that changed after the inventory beats an unreadable file, as in
// the other checks.
func scanReadOutcome(err error) Check {
	switch {
	case errors.Is(err, errChanged):
		return Check{Name: CheckBehavior, Outcome: OutcomeError, Reason: ReasonFilesChanged}
	case errors.Is(err, errUnreadable):
		return Check{Name: CheckBehavior, Outcome: OutcomeError, Reason: ReasonUnreadable}
	default:
		return Check{Name: CheckBehavior, Outcome: OutcomeError, Reason: ReasonUnsupportedFormat}
	}
}

// runScanUnit runs the pinned scanner in the isolated unit and maps its
// verdict. The model files are bound again after the unit.
func runScanUnit(ctx context.Context, job ScanJob) (Check, error) {
	dir, err := os.MkdirTemp("", "modelcheck-scan-")
	if err != nil {
		return Check{}, errors.New("scan unit directory cannot be created")
	}
	defer os.RemoveAll(dir)
	jobPath := filepath.Join(dir, "job.json")
	raw, err := json.Marshal(job)
	if err != nil {
		return Check{}, errors.New("scan job cannot be encoded")
	}
	if os.WriteFile(jobPath, raw, 0o600) != nil {
		return Check{}, errors.New("scan job cannot be written")
	}
	verdictPath := filepath.Join(dir, "verdict.json")
	cmd := exec.CommandContext(ctx, job.Scanner.Command, jobPath, verdictPath)
	cmd.Env = scanEnvironment(dir, job.GPU)
	cmd.Dir = dir
	// The unit output may hold model content. It is discarded: only the
	// bounded verdict crosses the supervisor boundary.
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	runErr := cmd.Run()
	if err := ctx.Err(); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return Check{Name: CheckBehavior, Outcome: OutcomeError, Reason: ReasonScanTimeout}, nil
		}
		return Check{}, Stopped(ctx)
	}
	if runErr != nil {
		return Check{Name: CheckBehavior, Outcome: OutcomeError, Reason: ReasonScanFailed}, nil
	}
	check, err := readScanVerdict(verdictPath, job.Scanner.Revision)
	if err != nil {
		return check, nil
	}
	after, reason, err := Inventory(ctx, job.Root)
	if err != nil && !errors.Is(err, errChanged) && !errors.Is(err, errUnreadable) {
		return Check{}, err
	}
	if err != nil || reason != "" || !maps.Equal(after, job.Files) {
		// A unit that wrote model files, or files that changed during the
		// scan, gives files-changed: the verdict is not usable.
		return Check{Name: CheckBehavior, Outcome: OutcomeError, Reason: ReasonFilesChanged}, nil
	}
	return check, nil
}

// scanEnvironment is the environment of the scan unit: a private home, no
// network, no token or proxy variables, and the one assigned GPU.
func scanEnvironment(dir, gpu string) []string {
	return []string{
		"PATH=/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=" + filepath.Join(dir, "home"),
		"TMPDIR=" + dir,
		"CUDA_VISIBLE_DEVICES=" + gpu,
		"HF_HUB_OFFLINE=1", "HF_DATASETS_OFFLINE=1", "TRANSFORMERS_OFFLINE=1",
		"HF_HUB_DISABLE_TELEMETRY=1", "DO_NOT_TRACK=1",
		"LANG=C.UTF-8",
	}
}

// scanVerdict is the bounded output of the scan unit. It is the only
// output of the unit that the supervisor reads.
type scanVerdict struct {
	Version int    `json:"version"`
	Scanner string `json:"scanner"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
}

// readScanVerdict reads and validates the unit verdict. It gives the
// mapped check, or an error-outcome check and no error.
func readScanVerdict(path, revision string) (Check, error) {
	raw, err := os.ReadFile(path)
	if err != nil || int64(len(raw)) > MaxScanVerdictBytes || !utf8.Valid(raw) {
		return Check{Name: CheckBehavior, Outcome: OutcomeError, Reason: ReasonScanVerdict}, nil
	}
	var verdict scanVerdict
	if strictjson.DecodeLimited(bytes.NewReader(raw), MaxScanVerdictBytes, &verdict) != nil {
		return Check{Name: CheckBehavior, Outcome: OutcomeError, Reason: ReasonScanVerdict}, nil
	}
	canonical, err := json.Marshal(verdict)
	if err != nil {
		return Check{Name: CheckBehavior, Outcome: OutcomeError, Reason: ReasonScanVerdict}, nil
	}
	var got, want any
	if json.Unmarshal(raw, &got) != nil || json.Unmarshal(canonical, &want) != nil || !reflect.DeepEqual(got, want) {
		return Check{Name: CheckBehavior, Outcome: OutcomeError, Reason: ReasonScanVerdict}, nil
	}
	check, ok := verdictCheck(verdict, revision)
	if !ok {
		return Check{Name: CheckBehavior, Outcome: OutcomeError, Reason: ReasonScanVerdict}, nil
	}
	return check, nil
}

// verdictCheck validates and maps one verdict. A passed verdict needs no
// reason; findings, error and unsupported verdicts carry a fixed code.
func verdictCheck(v scanVerdict, revision string) (Check, bool) {
	if v.Version != 1 || v.Scanner != revision || (v.Reason != "" && !reasonPattern.MatchString(v.Reason)) {
		return Check{}, false
	}
	switch v.Status {
	case "passed":
		if v.Reason != "" {
			return Check{}, false
		}
		return Check{Name: CheckBehavior, Outcome: OutcomePassed}, true
	case "findings":
		if v.Reason != "" {
			return Check{}, false
		}
		return Check{Name: CheckBehavior, Outcome: OutcomeFailed, Reason: ReasonScanFindings}, true
	case "error":
		if v.Reason == "" {
			return Check{}, false
		}
		return Check{Name: CheckBehavior, Outcome: OutcomeError, Reason: v.Reason}, true
	case "unsupported":
		if v.Reason == "" {
			return Check{}, false
		}
		return Check{Name: CheckBehavior, Outcome: OutcomeUnsupported, Reason: v.Reason}, true
	}
	return Check{}, false
}

// scanSupport classifies one exact build for the behavioral scan. It reads
// the model configuration and the safetensors headers of the inventoried
// weights. It gives the architecture and the format when the declared
// support covers the build, else a fixed reason; the scanner does not run
// then. A read error is errChanged or errUnreadable of the model files.
func scanSupport(ctx context.Context, root string, files map[string]string) (arch, format, reason string, err error) {
	config, err := scanConfigOf(ctx, root, files["config.json"])
	if err != nil {
		return "", "", "", err
	}
	if len(config.Architectures) == 0 || !scanArchitectures[config.Architectures[0]] {
		return "", "", ReasonUnsupportedArch, nil
	}
	arch = config.Architectures[0]
	if method := strings.ToLower(config.QuantMethod); method != "" {
		if format = scanQuantMethods[method]; format == "" {
			return arch, "", ReasonUnsupportedFormat, nil
		}
		return arch, format, "", nil
	}
	dtypes, err := weightDtypes(ctx, root, files)
	if err != nil {
		return "", "", "", err
	}
	format, reason = weightFormat(dtypes)
	return arch, format, reason, nil
}

// scanConfig is the model configuration that classifies the build.
type scanConfig struct {
	Architectures []string `json:"architectures"`
	QuantMethod   string   `json:"-"`
}

// scanConfigWire reads the quantization_config quant_method alongside the
// architectures without binding the whole configuration file.
type scanConfigWire struct {
	Architectures      []string `json:"architectures"`
	QuantizationConfig struct {
		QuantMethod string `json:"quant_method"`
	} `json:"quantization_config"`
}

// scanConfigOf reads and leniently decodes config.json against its digest.
func scanConfigOf(ctx context.Context, root, digest string) (scanConfig, error) {
	var config scanConfig
	if digest == "" {
		return config, errUnreadable
	}
	raw, reason, err := readBound(ctx, filepath.Join(root, "config.json"), digest, MaxJSONFileBytes, ReasonJSONTooLarge)
	if err != nil {
		return config, err
	}
	if reason != "" {
		return config, errUnreadable
	}
	if strictjson.Check(json.NewDecoder(bytes.NewReader(raw))) != nil {
		return config, errUnreadable
	}
	var wire scanConfigWire
	if json.Unmarshal(raw, &wire) != nil {
		return config, errUnreadable
	}
	config.Architectures, config.QuantMethod = wire.Architectures, wire.QuantizationConfig.QuantMethod
	return config, nil
}

// weightDtypes reads the dtype set of every safetensors shard. The header
// is the only part the classification reads; the scan result binds the
// whole files through the inventory.
func weightDtypes(ctx context.Context, root string, files map[string]string) (map[string]bool, error) {
	dtypes := map[string]bool{}
	for _, name := range slices.Sorted(maps.Keys(files)) {
		if !strings.HasSuffix(name, ".safetensors") {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		dtype, err := shardDtype(root, name)
		if err != nil {
			return nil, err
		}
		dtypes[dtype] = true
	}
	return dtypes, nil
}

// shardDtype reads the dtype of one safetensors shard header. A shard
// declares one dtype for all of its tensors.
func shardDtype(root, name string) (string, error) {
	file, err := openRegular(filepath.Join(root, name))
	if err != nil {
		return "", errUnreadable
	}
	defer file.Close()
	var size [8]byte
	if _, err := io.ReadFull(file, size[:]); err != nil {
		return "", errUnreadable
	}
	length := binary.LittleEndian.Uint64(size[:])
	if length > maxScanHeaderBytes {
		return "", errUnreadable
	}
	raw := make([]byte, length)
	if _, err := io.ReadFull(file, raw); err != nil {
		return "", errUnreadable
	}
	var header map[string]struct {
		Dtype string `json:"dtype"`
	}
	if strictjson.Check(json.NewDecoder(bytes.NewReader(raw))) != nil || json.Unmarshal(raw, &header) != nil {
		return "", errUnreadable
	}
	dtype := ""
	for name, tensor := range header {
		if name == "__metadata__" {
			continue
		}
		if dtype != "" && tensor.Dtype != dtype {
			return "", errUnreadable
		}
		dtype = tensor.Dtype
	}
	if dtype == "" {
		return "", errUnreadable
	}
	return dtype, nil
}

// weightFormat classifies the dtype set of the weights. Only a uniform
// BF16 build and a uniform FP8 build are supported without a
// quantization_config; anything mixed reads as unsupported.
func weightFormat(dtypes map[string]bool) (format, reason string) {
	switch {
	case len(dtypes) == 0:
		return "", ReasonUnsupportedFormat
	case len(dtypes) == 1 && dtypes["BF16"]:
		return scanFormatBF16, ""
	case len(dtypes) == 1 && strings.HasPrefix(slices.Collect(maps.Keys(dtypes))[0], "F8"):
		return scanFormatFP8, ""
	}
	return "", ReasonUnsupportedFormat
}

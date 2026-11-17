package modelcheck

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// scanTestGPU is the assigned GPU of the scan tests.
const scanTestGPU = "GPU-01234567-89ab-cdef-0123-456789abcdef"

// scanModel builds one model directory for the scan classification and
// gives its inventory.
func scanModel(t *testing.T, config string, shards map[string]string) (string, map[string]string) {
	t.Helper()
	files := map[string]string{"config.json": config, "tokenizer.json": `{"model":{"vocab":{}}}`}
	for name, content := range shards {
		files[name] = content
	}
	root := writeModel(t, files)
	inventory, reason, err := Inventory(context.Background(), root)
	if reason != "" || err != nil {
		t.Fatal("scan model files cannot be inventoried", reason, err)
	}
	return root, inventory
}

// safetensors builds one minimal safetensors file of one dtype.
func safetensors(dtype string) string {
	header := fmt.Sprintf(`{"weight":{"dtype":%q,"shape":[1],"data_offsets":[0,2]},"__metadata__":{"format":"pt"}}`, dtype)
	raw := make([]byte, 8+len(header)+2)
	binary.LittleEndian.PutUint64(raw[:8], uint64(len(header)))
	copy(raw[8:], header)
	return string(raw)
}

// scanScript installs one shell scanner. It receives the job path and the
// verdict path as arguments. Its directory holds the "ran" marker file
// that proves whether the unit executed.
func scanScript(t *testing.T, body string) (command, marker string) {
	t.Helper()
	dir := t.TempDir()
	command = filepath.Join(dir, "scanner")
	if err := os.WriteFile(command, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
	return command, filepath.Join(dir, "ran")
}

// passedScript writes one verdict.
func verdictScript(verdict string) string {
	return `printf '%s' '` + verdict + `' > "$2"`
}

// markerScript writes the marker file and one verdict.
func markerScript(verdict string) string {
	return `touch "$(dirname "$0")/ran"; ` + verdictScript(verdict)
}

func scanJob(root string, files map[string]string, command string) ScanJob {
	return ScanJob{Version: 1, Model: "chat", Root: root, Files: files,
		Scanner: ScannerSpec{Revision: "scan-1", Command: command}, GPU: scanTestGPU}
}

func TestRunScanJobPassesSupportedBF16Build(t *testing.T) {
	root, files := scanModel(t, `{"architectures":["Qwen3_5ForConditionalGeneration"],"model_type":"qwen3_5"}`,
		map[string]string{"model.safetensors": safetensors("BF16")})
	command, marker := scanScript(t, markerScript(`{"version":1,"scanner":"scan-1","status":"passed"}`))
	check, err := RunScanJob(context.Background(), scanJob(root, files, command))
	if err != nil || check != (Check{Name: CheckBehavior, Outcome: OutcomePassed}) {
		t.Fatal("a supported BF16 build must pass a passing scanner", check, err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("the scanner did not run for a supported build", err)
	}
}

func TestRunScanJobMapsTheVerdict(t *testing.T) {
	root, files := scanModel(t, `{"architectures":["Qwen3_5ForConditionalGeneration"]}`,
		map[string]string{"model.safetensors": safetensors("BF16")})
	for name, tc := range map[string]struct {
		verdict string
		want    Check
	}{
		"findings": {
			`{"version":1,"scanner":"scan-1","status":"findings"}`,
			Check{Name: CheckBehavior, Outcome: OutcomeFailed, Reason: ReasonScanFindings},
		},
		"error": {
			`{"version":1,"scanner":"scan-1","status":"error","reason":"load-failed"}`,
			Check{Name: CheckBehavior, Outcome: OutcomeError, Reason: "load-failed"},
		},
		"unsupported": {
			`{"version":1,"scanner":"scan-1","status":"unsupported","reason":"unsupported-architecture"}`,
			Check{Name: CheckBehavior, Outcome: OutcomeUnsupported, Reason: ReasonUnsupportedArch},
		},
	} {
		t.Run(name, func(t *testing.T) {
			command, _ := scanScript(t, verdictScript(tc.verdict))
			check, err := RunScanJob(context.Background(), scanJob(root, files, command))
			if err != nil || check != tc.want {
				t.Fatal("the verdict did not map to its check", check, err)
			}
		})
	}
}

func TestRunScanJobRejectsBadVerdicts(t *testing.T) {
	root, files := scanModel(t, `{"architectures":["Qwen3_5ForConditionalGeneration"]}`,
		map[string]string{"model.safetensors": safetensors("BF16")})
	want := Check{Name: CheckBehavior, Outcome: OutcomeError, Reason: ReasonScanVerdict}
	for name, body := range map[string]string{
		"missing":          `exit 0`,
		"malformed":        `printf '%s' '{bad' > "$2"`,
		"other scanner":    `printf '%s' '{"version":1,"scanner":"scan-2","status":"passed"}' > "$2"`,
		"other version":    `printf '%s' '{"version":2,"scanner":"scan-1","status":"passed"}' > "$2"`,
		"reason on passed": `printf '%s' '{"version":1,"scanner":"scan-1","status":"passed","reason":"x"}' > "$2"`,
		"oversized":        `head -c ` + fmt.Sprint(MaxScanVerdictBytes+1) + ` /dev/zero > "$2"`,
	} {
		t.Run(name, func(t *testing.T) {
			command, _ := scanScript(t, body)
			check, err := RunScanJob(context.Background(), scanJob(root, files, command))
			if err != nil || check != want {
				t.Fatal("a bad verdict must give verdict-invalid", check, err)
			}
		})
	}
}

func TestRunScanJobReportsScanFailures(t *testing.T) {
	root, files := scanModel(t, `{"architectures":["Qwen3_5ForConditionalGeneration"]}`,
		map[string]string{"model.safetensors": safetensors("BF16")})
	t.Run("scanner failed", func(t *testing.T) {
		command, _ := scanScript(t, `exit 3`)
		check, err := RunScanJob(context.Background(), scanJob(root, files, command))
		if err != nil || check != (Check{Name: CheckBehavior, Outcome: OutcomeError, Reason: ReasonScanFailed}) {
			t.Fatal("a failed unit must give scanner-failed", check, err)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		command, _ := scanScript(t, `sleep 30`)
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		check, err := RunScanJob(ctx, scanJob(root, files, command))
		if err != nil || check != (Check{Name: CheckBehavior, Outcome: OutcomeError, Reason: ReasonScanTimeout}) {
			t.Fatal("a timed-out unit must give scan-timeout", check, err)
		}
	})
	t.Run("canceled", func(t *testing.T) {
		command, _ := scanScript(t, `sleep 30`)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := RunScanJob(ctx, scanJob(root, files, command)); !errors.Is(err, ErrCheckCanceled) {
			t.Fatal("a canceled run gives no check", err)
		}
	})
}

func TestRunScanJobBindsTheExactFiles(t *testing.T) {
	root, files := scanModel(t, `{"architectures":["Qwen3_5ForConditionalGeneration"]}`,
		map[string]string{"model.safetensors": safetensors("BF16")})
	t.Run("unit wrote model files", func(t *testing.T) {
		body := verdictScript(`{"version":1,"scanner":"scan-1","status":"passed"}`) +
			fmt.Sprintf("\necho x >> %q", filepath.Join(root, "model.safetensors"))
		command, _ := scanScript(t, body)
		check, err := RunScanJob(context.Background(), scanJob(root, files, command))
		if err != nil || check != (Check{Name: CheckBehavior, Outcome: OutcomeError, Reason: ReasonFilesChanged}) {
			t.Fatal("a unit that wrote model files must give files-changed", check, err)
		}
	})
	t.Run("stale inventory", func(t *testing.T) {
		command, _ := scanScript(t, verdictScript(`{"version":1,"scanner":"scan-1","status":"passed"}`))
		job := scanJob(root, files, command)
		job.Files["config.json"] = strings.Repeat("f", 64)
		check, err := RunScanJob(context.Background(), job)
		if err != nil || check != (Check{Name: CheckBehavior, Outcome: OutcomeError, Reason: ReasonFilesChanged}) {
			t.Fatal("a stale inventory must give files-changed", check, err)
		}
	})
}

func TestRunScanJobUnsupportedBuildsDoNotRunTheScanner(t *testing.T) {
	run := func(t *testing.T, config string, shards map[string]string, gpu string) Check {
		t.Helper()
		root, files := scanModel(t, config, shards)
		command, marker := scanScript(t, markerScript(`{"version":1,"scanner":"scan-1","status":"passed"}`))
		job := scanJob(root, files, command)
		job.GPU = gpu
		check, err := RunScanJob(context.Background(), job)
		if err != nil {
			t.Fatal("an unsupported build gives a check, not an error", err)
		}
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatal("the scanner ran for an unsupported build")
		}
		return check
	}
	t.Run("architecture", func(t *testing.T) {
		check := run(t, `{"architectures":["LlamaForCausalLM"]}`, map[string]string{"model.safetensors": safetensors("BF16")}, scanTestGPU)
		if check.Outcome != OutcomeUnsupported || check.Reason != ReasonUnsupportedArch {
			t.Fatal("an undeclared architecture is unsupported", check)
		}
	})
	t.Run("no architecture", func(t *testing.T) {
		check := run(t, `{"model_type":"qwen3_5"}`, map[string]string{"model.safetensors": safetensors("BF16")}, scanTestGPU)
		if check.Outcome != OutcomeUnsupported || check.Reason != ReasonUnsupportedArch {
			t.Fatal("a build without architectures is unsupported", check)
		}
	})
	t.Run("quant method", func(t *testing.T) {
		check := run(t, `{"architectures":["Qwen3_5ForConditionalGeneration"],"quantization_config":{"quant_method":"gptq"}}`,
			map[string]string{"model.safetensors": safetensors("BF16")}, scanTestGPU)
		if check.Outcome != OutcomeUnsupported || check.Reason != ReasonUnsupportedFormat {
			t.Fatal("an undeclared quantization is unsupported", check)
		}
	})
	t.Run("mixed dtypes", func(t *testing.T) {
		check := run(t, `{"architectures":["Qwen3_5ForConditionalGeneration"]}`,
			map[string]string{"a.safetensors": safetensors("BF16"), "b.safetensors": safetensors("F16")}, scanTestGPU)
		if check.Outcome != OutcomeUnsupported || check.Reason != ReasonUnsupportedFormat {
			t.Fatal("mixed weight dtypes are unsupported", check)
		}
	})
	t.Run("no gpu", func(t *testing.T) {
		check := run(t, `{"architectures":["Qwen3_5ForConditionalGeneration"]}`, map[string]string{"model.safetensors": safetensors("BF16")}, "")
		if check.Outcome != OutcomeUnsupported || check.Reason != ReasonUnsupportedHardware {
			t.Fatal("a build without an assigned GPU is unsupported", check)
		}
	})
}

func TestRunScanJobSupportedFormatsRunTheScanner(t *testing.T) {
	for name, tc := range map[string]struct {
		config string
		shards map[string]string
	}{
		"bf16":  {`{"architectures":["Qwen3_5ForConditionalGeneration"]}`, map[string]string{"model.safetensors": safetensors("BF16")}},
		"fp8":   {`{"architectures":["Qwen3_5ForConditionalGeneration"]}`, map[string]string{"model.safetensors": safetensors("F8_E4M3")}},
		"nvfp4": {`{"architectures":["Qwen3_5ForConditionalGeneration"],"quantization_config":{"quant_method":"nvfp4"}}`, map[string]string{"model.safetensors": safetensors("BF16")}},
		"int4":  {`{"architectures":["Qwen3_5ForConditionalGeneration"],"quantization_config":{"quant_method":"int4"}}`, map[string]string{"model.safetensors": safetensors("BF16")}},
	} {
		t.Run(name, func(t *testing.T) {
			root, files := scanModel(t, tc.config, tc.shards)
			command, marker := scanScript(t, markerScript(`{"version":1,"scanner":"scan-1","status":"passed"}`))
			check, err := RunScanJob(context.Background(), scanJob(root, files, command))
			if err != nil || check.Outcome != OutcomePassed {
				t.Fatal("a supported build must run the unit", check, err)
			}
			if _, err := os.Stat(marker); err != nil {
				t.Fatal("the scanner did not run for a supported build", err)
			}
		})
	}
}

func TestRunScanJobIsolation(t *testing.T) {
	root, files := scanModel(t, `{"architectures":["Qwen3_5ForConditionalGeneration"]}`,
		map[string]string{"model.safetensors": safetensors("BF16")})
	// The unit proves its own isolation contract; a failed assertion writes
	// no passing verdict, so a passed check means every assertion held.
	body := `
set -eu
[ "$CUDA_VISIBLE_DEVICES" = "` + scanTestGPU + `" ]
[ "$(dirname "$1")" = "$(dirname "$2")" ]
[ "${1#` + root + `/}" = "$1" ]
[ "$(dirname "$HOME")" = "$(dirname "$1")" ]
[ "$TMPDIR" = "$(dirname "$1")" ]
[ -z "${HTTP_PROXY+x}" ] && [ -z "${HTTPS_PROXY+x}" ] && [ -z "${ALL_PROXY+x}" ] && [ -z "${HF_TOKEN+x}" ]
printf '%s' '{"version":1,"scanner":"scan-1","status":"passed"}' > "$2"
`
	command, _ := scanScript(t, body)
	check, err := RunScanJob(context.Background(), scanJob(root, files, command))
	if err != nil || check.Outcome != OutcomePassed {
		t.Fatal("the scan unit left its isolation contract", check, err)
	}
}

func TestRunScanJobRejectsInvalidJobs(t *testing.T) {
	root, files := scanModel(t, `{"architectures":["Qwen3_5ForConditionalGeneration"]}`,
		map[string]string{"model.safetensors": safetensors("BF16")})
	command, _ := scanScript(t, verdictScript(`{"version":1,"scanner":"scan-1","status":"passed"}`))
	job := scanJob(root, files, command)
	for name, broken := range map[string]func(ScanJob) ScanJob{
		"version":  func(j ScanJob) ScanJob { j.Version = 2; return j },
		"model":    func(j ScanJob) ScanJob { j.Model = "-bad"; return j },
		"root":     func(j ScanJob) ScanJob { j.Root = "relative"; return j },
		"files":    func(j ScanJob) ScanJob { j.Files = nil; return j },
		"digest":   func(j ScanJob) ScanJob { j.Files["config.json"] = "bad"; return j },
		"revision": func(j ScanJob) ScanJob { j.Scanner.Revision = ""; return j },
		"command":  func(j ScanJob) ScanJob { j.Scanner.Command = "scanner"; return j },
		"gpu":      func(j ScanJob) ScanJob { j.GPU = "not-a-uuid"; return j },
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := RunScanJob(context.Background(), broken(job)); err == nil {
				t.Fatal("an invalid job ran")
			}
		})
	}
}

// fakeScanner is a Scanner of tests.
type fakeScanner struct {
	revision string
	check    Check
	err      error
}

func (f fakeScanner) Revision() string { return f.revision }

func (f fakeScanner) Scan(context.Context, string, string, map[string]string) (Check, error) {
	return f.check, f.err
}

// withScanner sets the scanner of the checker.
func (c Checker) withScanner(s Scanner) Checker {
	c.Scanner = s
	return c
}

func TestRunWithScanner(t *testing.T) {
	files := modelFiles()
	hub := newHub(t, upstreamOf(files))
	record, err := hub.checker().withScanner(fakeScanner{revision: "scan-1", check: Check{Outcome: OutcomePassed}}).Run(context.Background(),
		Request{Model: "chat", Root: writeModel(t, files), Source: testSource})
	if err != nil {
		t.Fatal(err)
	}
	if record.Scanner != "scan-1" || record.Checks[3] != (Check{Name: CheckBehavior, Outcome: OutcomePassed}) ||
		record.Status != StatusPassed || record.Coverage != CoverageComplete {
		t.Fatalf("a scanned record = %+v", record)
	}
	if raw, err := MarshalRecord(record); err != nil {
		t.Fatal(err)
	} else if _, err := ReadRecord(raw); err != nil {
		t.Fatal("a scanned record must round trip", err)
	}
}

func TestRunWithScannerFindings(t *testing.T) {
	files := modelFiles()
	hub := newHub(t, upstreamOf(files))
	record, err := hub.checker().withScanner(fakeScanner{revision: "scan-1", check: Check{Outcome: OutcomeFailed, Reason: ReasonScanFindings}}).Run(context.Background(),
		Request{Model: "chat", Root: writeModel(t, files), Source: testSource})
	if err != nil {
		t.Fatal(err)
	}
	if record.Scanner != "scan-1" || record.Status != StatusFindings || record.Coverage != CoverageComplete {
		t.Fatalf("a finding record = %+v", record)
	}
}

func TestRunWithScannerUnsupported(t *testing.T) {
	files := modelFiles()
	hub := newHub(t, upstreamOf(files))
	record, err := hub.checker().withScanner(fakeScanner{revision: "scan-1", check: Check{Outcome: OutcomeUnsupported, Reason: ReasonUnsupportedArch}}).Run(context.Background(),
		Request{Model: "chat", Root: writeModel(t, files), Source: testSource})
	if err != nil {
		t.Fatal(err)
	}
	if record.Scanner != "scan-1" || record.Status != StatusUnsupported || record.Checks[3].Reason != ReasonUnsupportedArch {
		t.Fatalf("an unsupported record = %+v", record)
	}
}

func TestRunWithScannerStopGivesNoRecord(t *testing.T) {
	files := modelFiles()
	hub := newHub(t, upstreamOf(files))
	for name, stop := range map[string]error{
		"canceled": ErrCheckCanceled,
		"timeout":  ErrCheckTimedOut,
	} {
		t.Run(name, func(t *testing.T) {
			_, got := hub.checker().withScanner(fakeScanner{revision: "scan-1", err: stop}).Run(context.Background(),
				Request{Model: "chat", Root: writeModel(t, files), Source: testSource})
			if !errors.Is(got, stop) {
				t.Fatal("a scanner stop must give no record", got)
			}
		})
	}
}

func TestRunWithoutScannerStaysNotScanned(t *testing.T) {
	files := modelFiles()
	hub := newHub(t, upstreamOf(files))
	record, err := hub.checker().Run(context.Background(), Request{Model: "chat", Root: writeModel(t, files), Source: testSource})
	if err != nil {
		t.Fatal(err)
	}
	if record.Scanner != "" || record.Checks[3].Outcome != OutcomeNotRun || record.Status != StatusNotScanned {
		t.Fatalf("a record without a scanner = %+v", record)
	}
}

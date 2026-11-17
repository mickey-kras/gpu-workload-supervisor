package modelcheck

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/strictjson"
)

// RecordVersion identifies the safetensors + template-pin model check format.
const RecordVersion = 2

const (
	MaxRecordBytes = 4 << 20
	maxRecordFiles = 2 * MaxModelFiles
)

// ErrStaleRecord marks a result with another positive record version.
var ErrStaleRecord = errors.New("model check record has another version")

// Status is the overall check result. Only StatusPassed is clean.
type Status string

const (
	StatusPassed      Status = "passed"
	StatusFindings    Status = "findings"
	StatusError       Status = "error"
	StatusUnsupported Status = "unsupported"
	StatusNotScanned  Status = "not_scanned"
)

// Outcome is the result of one check stage.
type Outcome string

const (
	OutcomePassed      Outcome = "passed"
	OutcomeFailed      Outcome = "failed"
	OutcomeError       Outcome = "error"
	OutcomeUnsupported Outcome = "unsupported"
	OutcomeNotRun      Outcome = "not_run"
)

// Coverage reports whether all check stages executed.
type Coverage string

const (
	CoverageComplete Coverage = "complete"
	CoveragePartial  Coverage = "partial"
)

// Configuration records the bounds under which a check ran.
type Configuration struct {
	Hub              string `json:"hub"`
	MaxJSONFileBytes int64  `json:"maxJSONFileBytes"`
	MaxModelFiles    int    `json:"maxModelFiles"`
	MaxTemplateBytes int    `json:"maxTemplateBytes"`
}

func currentConfiguration(hub string) Configuration {
	return Configuration{
		Hub:              hub,
		MaxJSONFileBytes: MaxJSONFileBytes,
		MaxModelFiles:    MaxModelFiles,
		MaxTemplateBytes: MaxTemplateBytes,
	}
}

func (c Configuration) valid() bool {
	return strings.HasPrefix(c.Hub, "https://") && len(c.Hub) <= 256 &&
		c.MaxJSONFileBytes > 0 && c.MaxModelFiles > 0 && c.MaxTemplateBytes > 0
}

const (
	CheckAllowlist = "file-allowlist"
	CheckTemplate  = "chat-template"
)

var checkOrder = []string{CheckAllowlist, CheckTemplate}

// Check holds the outcome of one named check stage.
type Check struct {
	Name    string  `json:"name"`
	Outcome Outcome `json:"outcome"`
	Reason  string  `json:"reason,omitempty"`
}

// Record binds the check outcome of a local model directory to SHA-256 digests of every file.
type Record struct {
	Version            int               `json:"version"`
	Model              string            `json:"model"`
	Root               string            `json:"root"`
	Source             Source            `json:"source"`
	Files              map[string]string `json:"files"`
	Status             Status            `json:"status"`
	Coverage           Coverage          `json:"coverage"`
	Checks             []Check           `json:"checks"`
	ChatTemplateSHA256 string            `json:"chatTemplateSHA256,omitempty"`
	TemplateFile       string            `json:"templateFile,omitempty"`
	Checker            string            `json:"checker"`
	Configuration      Configuration     `json:"configuration"`
	CheckedAt          time.Time         `json:"checkedAt"`
}

var (
	modelPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	checkerPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.+_-]{0,127}$`)
	reasonPattern  = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)
	validOutcomes  = map[Outcome]bool{
		OutcomePassed: true, OutcomeFailed: true, OutcomeError: true,
		OutcomeUnsupported: true, OutcomeNotRun: true,
	}
)

// Validate verifies structural invariants of a stored model check record.
func (r Record) Validate() error {
	if !r.validIdentity() {
		return errors.New("model check record has an invalid identity")
	}
	if r.CheckedAt.IsZero() || r.CheckedAt.Location() != time.UTC {
		return errors.New("model check record needs a UTC check time")
	}
	if !validChecks(r.Checks) {
		return errors.New("model check record needs every check in order")
	}
	if !validFiles(r.Files, r.Checks[0].Outcome == OutcomePassed) {
		return fmt.Errorf("model check record needs valid file entries, at most %d", maxRecordFiles)
	}
	if !r.validTemplatePin() {
		return errors.New("model check record has invalid chat template pin fields")
	}
	if !r.Configuration.valid() {
		return errors.New("model check record has an invalid configuration")
	}
	if coverageOf(r.Checks) != r.Coverage || statusOf(r.Checks) != r.Status {
		return errors.New("model check record status or coverage does not follow from its checks")
	}
	return nil
}

func (r Record) validIdentity() bool {
	return r.Version == RecordVersion && modelPattern.MatchString(r.Model) &&
		cleanAbsolute(r.Root) && r.Source.Valid() && checkerPattern.MatchString(r.Checker)
}

func (r Record) validTemplatePin() bool {
	if r.ChatTemplateSHA256 == "" && r.TemplateFile == "" {
		return true
	}
	if r.Checks[1].Outcome != OutcomePassed {
		return false
	}
	return sha256Pattern.MatchString(r.ChatTemplateSHA256) && filepath.IsLocal(r.TemplateFile) && r.Files[r.TemplateFile] != ""
}

func validFiles(files map[string]string, complete bool) bool {
	if files == nil || (complete && len(files) == 0) || len(files) > maxRecordFiles {
		return false
	}
	for name, digest := range files {
		if !filepath.IsLocal(name) || !sha256Pattern.MatchString(digest) {
			return false
		}
	}
	return true
}

func validChecks(checks []Check) bool {
	if len(checks) != len(checkOrder) {
		return false
	}
	for i, check := range checks {
		if check.Name != checkOrder[i] || !validOutcomes[check.Outcome] || (check.Reason != "" && !reasonPattern.MatchString(check.Reason)) {
			return false
		}
	}
	return true
}

func statusOf(checks []Check) Status {
	for _, check := range checks {
		switch check.Outcome {
		case OutcomeFailed:
			return StatusFindings
		case OutcomeError:
			return StatusError
		case OutcomeUnsupported:
			return StatusUnsupported
		case OutcomeNotRun:
			return StatusNotScanned
		}
	}
	return StatusPassed
}

func coverageOf(checks []Check) Coverage {
	for _, check := range checks {
		if check.Outcome == OutcomeNotRun {
			return CoveragePartial
		}
	}
	return CoverageComplete
}

// MarshalRecord encodes a valid record within MaxRecordBytes.
func MarshalRecord(r Record) ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(r)
	if err != nil || len(raw) > MaxRecordBytes {
		return nil, errors.New("model check record exceeds its limit")
	}
	return raw, nil
}

// ReadRecord decodes one bounded strict JSON record.
func ReadRecord(raw []byte) (Record, error) {
	var version struct {
		Version int `json:"version"`
	}
	if len(raw) <= MaxRecordBytes && json.Unmarshal(raw, &version) == nil && version.Version != RecordVersion && version.Version > 0 {
		return Record{}, ErrStaleRecord
	}
	var record Record
	if len(raw) > MaxRecordBytes || !utf8.Valid(raw) || strictjson.DecodeLimited(bytes.NewReader(raw), MaxRecordBytes, &record) != nil {
		return Record{}, errors.New("model check record is not bounded strict JSON")
	}
	canonical, err := MarshalRecord(record)
	if err != nil || !bytes.Equal(compactJSON(raw), canonical) {
		return Record{}, errors.New("model check record is not canonical")
	}
	return record, nil
}

func compactJSON(raw []byte) []byte {
	var out bytes.Buffer
	if json.Compact(&out, raw) != nil {
		return nil
	}
	return out.Bytes()
}

func cleanAbsolute(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && path != "/"
}

func filesMatch(ctx context.Context, r Record, root string) (bool, error) {
	actual, reason, err := Inventory(ctx, root)
	if err != nil {
		return false, err
	}
	if reason != "" || len(actual) != len(r.Files) {
		return false, nil
	}
	for name, digest := range r.Files {
		if actual[name] != digest {
			return false, nil
		}
	}
	return true, nil
}

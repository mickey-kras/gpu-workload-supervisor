package modelcheck

import (
	"bytes"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"regexp"
	"time"
	"unicode/utf8"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/strictjson"
)

// RecordVersion is the only result format.
const RecordVersion = 1

// Bounds of one stored result.
const (
	MaxRecordBytes = 4 << 20
	MaxModelFiles  = 4096
)

// Status is the overall result. Only StatusPassed is clean.
type Status string

const (
	StatusPassed      Status = "passed"
	StatusFindings    Status = "findings"
	StatusError       Status = "error"
	StatusUnsupported Status = "unsupported"
	StatusNotScanned  Status = "not_scanned"
)

// Outcome is the result of one check.
type Outcome string

const (
	OutcomePassed      Outcome = "passed"
	OutcomeFailed      Outcome = "failed"
	OutcomeError       Outcome = "error"
	OutcomeUnsupported Outcome = "unsupported"
	OutcomeNotRun      Outcome = "not_run"
)

// Check names, in the order that they run. Each must pass.
const (
	CheckAllowlist = "file-allowlist"
	CheckIdentity  = "upstream-identity"
	CheckTemplate  = "chat-template"
	CheckBehavior  = "behavioral-scan"
)

var checkOrder = []string{CheckAllowlist, CheckIdentity, CheckTemplate, CheckBehavior}

// Check is one check result. Reason is a fixed code; it never holds a path
// or file content.
type Check struct {
	Name    string  `json:"name"`
	Outcome Outcome `json:"outcome"`
	Reason  string  `json:"reason,omitempty"`
}

// Record is one stored model check result. Files binds the result to the
// SHA-256 of every model file. A record for other files, another root or
// another source does not apply.
type Record struct {
	Version   int               `json:"version"`
	Model     string            `json:"model"`
	Root      string            `json:"root"`
	Source    Source            `json:"source"`
	Files     map[string]string `json:"files"`
	Status    Status            `json:"status"`
	Checks    []Check           `json:"checks"`
	Checker   string            `json:"checker"`
	CheckedAt time.Time         `json:"checkedAt"`
}

var (
	modelPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	checkerPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.+_-]{0,127}$`)
	reasonPattern  = regexp.MustCompile(`^[a-z][a-z-]{0,63}$`)
	validOutcomes  = map[Outcome]bool{OutcomePassed: true, OutcomeFailed: true, OutcomeError: true, OutcomeUnsupported: true, OutcomeNotRun: true}
)

// Validate needs a complete record whose status follows from its checks.
func (r Record) Validate() error {
	if r.Version != RecordVersion || !modelPattern.MatchString(r.Model) || !cleanAbsolute(r.Root) || !r.Source.valid() || !checkerPattern.MatchString(r.Checker) {
		return errors.New("model check record has an invalid identity")
	}
	if r.CheckedAt.IsZero() || r.CheckedAt.Location() != time.UTC {
		return errors.New("model check record needs a UTC check time")
	}
	if !validFiles(r.Files) {
		return errors.New("model check record needs 1 to 4096 valid file entries")
	}
	if !validChecks(r.Checks) {
		return errors.New("model check record needs every check in order")
	}
	if statusOf(r.Checks) != r.Status {
		return errors.New("model check record status does not follow from its checks")
	}
	return nil
}

func validFiles(files map[string]string) bool {
	if len(files) == 0 || len(files) > MaxModelFiles {
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

// statusOf gives the status of the first check that did not pass. A check
// that did not run after passing checks leaves the model not scanned.
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

// ReadRecord decodes one bounded strict record. Unknown, duplicate, null and
// differently spelled fields are rejected.
func ReadRecord(raw []byte) (Record, error) {
	var record Record
	if len(raw) > MaxRecordBytes || !utf8.Valid(raw) || strictjson.DecodeLimited(bytes.NewReader(raw), MaxRecordBytes, &record) != nil {
		return Record{}, errors.New("model check record is not bounded strict JSON")
	}
	canonical, err := MarshalRecord(record)
	if err != nil {
		return Record{}, err
	}
	var got, want any
	if json.Unmarshal(raw, &got) != nil || json.Unmarshal(canonical, &want) != nil || !reflect.DeepEqual(got, want) {
		return Record{}, errors.New("model check record fields differ from the canonical form")
	}
	return record, nil
}

// Current gives the status that applies to the model files on disk now. A
// record for other digests, another root or another source leaves the
// model not scanned: results never carry over to changed or other models.
func Current(r *Record, model, root string, source Source, files map[string]string) Status {
	if r == nil || r.Model != model || r.Root != root || r.Source != source || !reflect.DeepEqual(r.Files, files) {
		return StatusNotScanned
	}
	return r.Status
}

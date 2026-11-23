package modelcheck

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"time"
	"unicode/utf8"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/strictjson"
)

// RecordVersion is the only result format. Version 3 drops the upstream
// identity check, the source and the template review. A result of another
// version reads as stale.
const RecordVersion = 3

// MaxRecordBytes bounds one stored result. maxRecordFiles bounds its file
// entries; it is above MaxModelFiles, so a later limit change stays readable.
const (
	MaxRecordBytes = 4 << 20
	maxRecordFiles = 2 * MaxModelFiles
)

// ErrStaleRecord marks a result with another positive record version. Only
// the version is read. Its model is not scanned for this checker.
var ErrStaleRecord = errors.New("model check record has another version")

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

// Coverage records whether every check ran.
type Coverage string

const (
	CoverageComplete Coverage = "complete"
	CoveragePartial  Coverage = "partial"
)

// Configuration is the effective configuration of the checks.
type Configuration struct {
	MaxJSONFileBytes int64          `json:"maxJSONFileBytes"`
	MaxModelFiles    int            `json:"maxModelFiles"`
	Template         TemplateLimits `json:"template"`
}

// TemplateLimits are the limits of the chat template check.
type TemplateLimits struct {
	MaxBytes int `json:"maxBytes"`
}

// CurrentConfiguration is the configuration that this checker applies.
func CurrentConfiguration() Configuration {
	return Configuration{MaxJSONFileBytes: MaxJSONFileBytes, MaxModelFiles: MaxModelFiles, Template: TemplateLimits{MaxBytes: MaxTemplateBytes}}
}

// valid checks the structure only. Current compares the values, so a
// result from another configuration reads as not scanned, not as invalid.
func (c Configuration) valid() bool {
	return c.MaxJSONFileBytes > 0 && c.MaxModelFiles > 0 && c.Template.MaxBytes > 0
}

// Check names, in the order that they run. Each must pass.
const (
	CheckAllowlist = "file-allowlist"
	CheckTemplate  = "chat-template"
	CheckBehavior  = "behavioral-scan"
)

var checkOrder = []string{CheckAllowlist, CheckTemplate, CheckBehavior}

// Check is one check result. Reason is a fixed code; it never holds a path
// or file content.
type Check struct {
	Name    string  `json:"name"`
	Outcome Outcome `json:"outcome"`
	Reason  string  `json:"reason,omitempty"`
}

// Record is one stored model check result. Files binds the result to the
// SHA-256 of every model file. A record for other files or another root
// does not apply. ChatTemplateSHA256 is the SHA-256 of the chat template
// text, as UTF-8 bytes, that passed the template check. It equals the SHA-256 of a --chat-template file with the same
// text. The vLLM binding records that SHA-256, see
// runtime.ChatTemplateSHA256.
type Record struct {
	Version int               `json:"version"`
	Model   string            `json:"model"`
	Root    string            `json:"root"`
	Files   map[string]string `json:"files"`
	// FileStats holds the file identity of each file in Files. The launch
	// gate compares it instead of hashing the weights again. A record
	// without it cannot allow a launch.
	FileStats          map[string]FileStat `json:"fileStats,omitempty"`
	Status             Status              `json:"status"`
	Coverage           Coverage            `json:"coverage"`
	Checks             []Check             `json:"checks"`
	ChatTemplateSHA256 string              `json:"chatTemplateSHA256,omitempty"`
	// TemplateFile names the first file of the model that holds the
	// template, such as chat_template.jinja, when the record has a pin.
	TemplateFile  string        `json:"templateFile,omitempty"`
	Checker       string        `json:"checker"`
	Configuration Configuration `json:"configuration"`
	CheckedAt     time.Time     `json:"checkedAt"`
}

var (
	modelPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	checkerPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.+_-]{0,127}$`)
	reasonPattern  = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)
	sha256Pattern  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	validOutcomes  = map[Outcome]bool{OutcomePassed: true, OutcomeFailed: true, OutcomeError: true, OutcomeUnsupported: true, OutcomeNotRun: true}
)

// Validate needs a complete record whose status follows from its checks.
func (r Record) Validate() error {
	if r.Version != RecordVersion || !modelPattern.MatchString(r.Model) || !cleanAbsolute(r.Root) || !checkerPattern.MatchString(r.Checker) {
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
	if err := validFileStats(r.Files, r.FileStats); err != nil {
		return err
	}
	if !r.validTemplate() {
		return errors.New("model check record has template fields that do not follow from the template check")
	}
	if !r.Configuration.valid() {
		return errors.New("model check record has an invalid configuration")
	}
	if coverageOf(r.Checks) != r.Coverage {
		return errors.New("model check record coverage does not follow from its checks")
	}
	if statusOf(r.Checks) != r.Status {
		return errors.New("model check record status does not follow from its checks")
	}
	return nil
}

// validFiles needs at least one file once the allowlist passed. A failed
// inventory can stop before the first file.
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

// templateIndex is the position of the template check in checkOrder.
var templateIndex = slices.Index(checkOrder, CheckTemplate)

// validTemplate needs a pin and the name of a template file exactly when
// the template check passed. A passed check has no reason.
func (r Record) validTemplate() bool {
	check := r.Checks[templateIndex]
	if check.Outcome == OutcomePassed {
		return check.Reason == "" && sha256Pattern.MatchString(r.ChatTemplateSHA256) && slices.Contains(templateFiles, r.TemplateFile)
	}
	return r.ChatTemplateSHA256 == "" && r.TemplateFile == ""
}

// statusOf returns the status of the first check that did not pass. A check
// that did not run after passing checks leaves the model not_scanned.
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

// ReadRecord decodes one bounded strict record. Unknown, duplicate, null and
// differently spelled fields are rejected.
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
	if err != nil {
		return Record{}, err
	}
	var got, want any
	if json.Unmarshal(raw, &got) != nil || json.Unmarshal(canonical, &want) != nil || !reflect.DeepEqual(got, want) {
		return Record{}, errors.New("model check record fields differ from the canonical form")
	}
	return record, nil
}

// Current returns the status that applies to the model files on disk now,
// from a fresh inventory of root. An inventory error, an inventory problem
// the record does not hold, or a record for other digests, root, checker
// version or configuration leaves the model not_scanned: results never carry
// over to changed or other models, and a new checker checks again.
func Current(ctx context.Context, r *Record, model, root string, checker Checker) Status {
	if r == nil || r.Model != model || r.Root != root || r.Checker != checker.Version || r.Configuration != CurrentConfiguration() {
		return StatusNotScanned
	}
	if match, err := filesMatch(ctx, *r, root); err != nil || !match {
		return StatusNotScanned
	}
	return r.Status
}

// filesMatch reports whether a fresh inventory of root equals the files of
// r. An inventory error returns the error and no match.
func filesMatch(ctx context.Context, r Record, root string) (bool, error) {
	files, reason, err := Inventory(ctx, root)
	if err != nil {
		return false, err
	}
	if !reflect.DeepEqual(r.Files, files) {
		return false, nil
	}
	// An inventory with a problem stops early, so its digests can equal a
	// complete record. Only a record of the same problem still applies.
	return reason == "" || (len(r.Checks) != 0 && r.Checks[0].Reason == reason), nil
}

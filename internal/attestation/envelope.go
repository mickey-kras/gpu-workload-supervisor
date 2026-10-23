// Package attestation verifies signed model attestation envelopes. A valid
// signature binds a signer and a policy decision to exact artifact bytes; it
// never proves that a model is safe.
package attestation

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"reflect"
	"regexp"
	"time"
	"unicode/utf8"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/strictjson"
)

// FormatVersion is the only envelope format this package reads or signs.
const FormatVersion = 1

// Bounds apply before signature checks so hostile evidence stays cheap to reject.
const (
	MaxEnvelopeBytes  = 16 << 20
	MaxSignatureBytes = 1 << 10
	MaxFiles          = 32768
	MaxPathBytes      = 512
	maxTextBytes      = 256
	maxURIBytes       = 2048
)

// TimestampLayout is the only accepted timestamp form: RFC 3339 UTC seconds.
const TimestampLayout = "2006-01-02T15:04:05Z"

var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

type ScanStatus string

const (
	ScanPassed      ScanStatus = "passed"
	ScanFindings    ScanStatus = "findings"
	ScanNotScanned  ScanStatus = "not_scanned"
	ScanUnsupported ScanStatus = "unsupported"
	ScanError       ScanStatus = "error"
)

// Coverage reports whether the scanner examined every bound file. The signed
// report, bound by its digest, carries per-file detail.
type Coverage string

const (
	CoverageComplete Coverage = "complete"
	CoveragePartial  Coverage = "partial"
)

// Artifact binds either a complete directory inventory or one artifact file.
// Files maps clean relative paths to lowercase hex SHA-256 digests, the same
// shape as a training inventory. Shard index files are ordinary entries.
type Artifact struct {
	Files  map[string]string `json:"files,omitempty"`
	SHA256 string            `json:"sha256,omitempty"`
}

// Source records where the producer obtained the artifact. Producers omit
// unknown values instead of inventing them.
type Source struct {
	URI      string `json:"uri,omitempty"`
	Revision string `json:"revision,omitempty"`
}

// Scanner identifies the tool, its version and the digest of its effective
// configuration.
type Scanner struct {
	Tool         string `json:"tool"`
	Version      string `json:"version"`
	ConfigSHA256 string `json:"configSHA256"`
}

type Scan struct {
	Status       ScanStatus `json:"status"`
	Scanner      *Scanner   `json:"scanner,omitempty"`
	Coverage     Coverage   `json:"coverage,omitempty"`
	Verdict      string     `json:"verdict,omitempty"`
	ReportSHA256 string     `json:"reportSHA256,omitempty"`
}

type Envelope struct {
	Version       int      `json:"version"`
	Artifact      Artifact `json:"artifact"`
	Source        *Source  `json:"source,omitempty"`
	Scan          Scan     `json:"scan"`
	PolicyVersion int      `json:"policyVersion"`
	Timestamp     string   `json:"timestamp"`
}

type presence int

const (
	forbidden presence = iota
	optional
	required
)

// scanFields lists scanner, coverage, verdict and report presence per status.
// A skipped scan carries no scanner evidence, so it can never look like a pass.
var scanFields = map[ScanStatus][4]presence{
	ScanPassed:      {required, required, required, required},
	ScanFindings:    {required, required, required, required},
	ScanError:       {required, forbidden, forbidden, optional},
	ScanUnsupported: {required, forbidden, forbidden, optional},
	ScanNotScanned:  {forbidden, forbidden, forbidden, forbidden},
}

var errMalformedEnvelope = errors.New("invalid attestation envelope JSON")

// ReadEnvelope reads one bounded strict envelope. The input must decode to
// the same JSON value as its canonical form: no unknown, null, empty or
// differently cased fields can pass through.
func ReadEnvelope(r io.Reader) (Envelope, error) {
	var envelope Envelope
	raw, err := io.ReadAll(io.LimitReader(r, MaxEnvelopeBytes+1))
	if err != nil || !utf8.Valid(raw) || strictjson.DecodeLimited(bytes.NewReader(raw), MaxEnvelopeBytes, &envelope) != nil {
		return Envelope{}, errMalformedEnvelope
	}
	if err := envelope.Validate(); err != nil {
		return Envelope{}, err
	}
	canonical, err := canonicalJSON(envelope.object())
	if err != nil || !sameJSON(raw, canonical) {
		return Envelope{}, errMalformedEnvelope
	}
	return envelope, nil
}

func (e Envelope) Validate() error {
	if e.Version != FormatVersion {
		return errors.New("unsupported attestation format version")
	}
	if err := e.Artifact.Validate(); err != nil {
		return err
	}
	if e.Source != nil && !e.Source.valid() {
		return errors.New("invalid attestation source")
	}
	if err := e.Scan.validate(); err != nil {
		return err
	}
	if e.PolicyVersion < 1 {
		return errors.New("invalid attestation policy version")
	}
	if !validTimestamp(e.Timestamp) {
		return errors.New("attestation timestamp must be RFC 3339 UTC seconds")
	}
	return nil
}

// Validate accepts exactly one artifact form with bounded, clean entries.
func (a Artifact) Validate() error {
	if a.SHA256 != "" {
		if a.Files != nil || !digestPattern.MatchString(a.SHA256) {
			return errors.New("invalid single attestation artifact")
		}
		return nil
	}
	if len(a.Files) == 0 || len(a.Files) > MaxFiles {
		return errors.New("attestation artifact needs 1 to 32768 files")
	}
	for path, digest := range a.Files {
		if !validPath(path) || !digestPattern.MatchString(digest) {
			return errors.New("invalid attestation artifact entry")
		}
	}
	return nil
}

func (s Source) valid() bool {
	if s.URI == "" && s.Revision == "" {
		return false
	}
	return optionalText(s.URI, maxURIBytes) && optionalText(s.Revision, maxTextBytes)
}

func (s Scan) validate() error {
	fields, known := scanFields[s.Status]
	if !known {
		return errors.New("unsupported attestation scan status")
	}
	present := [4]bool{s.Scanner != nil, s.Coverage != "", s.Verdict != "", s.ReportSHA256 != ""}
	for i, rule := range fields {
		if (rule == required && !present[i]) || (rule == forbidden && present[i]) {
			return errors.New("attestation scan evidence does not match its status")
		}
	}
	if s.Scanner != nil && !s.Scanner.valid() {
		return errors.New("invalid attestation scanner identity")
	}
	if s.Coverage != "" && s.Coverage != CoverageComplete && s.Coverage != CoveragePartial {
		return errors.New("invalid attestation scan coverage")
	}
	if !optionalText(s.Verdict, maxTextBytes) || (s.ReportSHA256 != "" && !digestPattern.MatchString(s.ReportSHA256)) {
		return errors.New("invalid attestation scan verdict or report")
	}
	return nil
}

func (s Scanner) valid() bool {
	return text(s.Tool, maxTextBytes) && text(s.Version, maxTextBytes) && digestPattern.MatchString(s.ConfigSHA256)
}

func validTimestamp(value string) bool {
	parsed, err := time.Parse(TimestampLayout, value)
	return err == nil && parsed.Format(TimestampLayout) == value
}

func validPath(path string) bool {
	return text(path, MaxPathBytes) && filepath.IsLocal(path) && path != "." && filepath.Clean(path) == path
}

func optionalText(value string, max int) bool {
	return value == "" || text(value, max)
}

// text admits printable ASCII except quote and backslash, so canonical JSON
// strings never need escapes.
func text(value string, max int) bool {
	if value == "" || len(value) > max {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < 0x20 || value[i] > 0x7e || value[i] == '"' || value[i] == '\\' {
			return false
		}
	}
	return true
}

// sameJSON compares decoded values, so key order, whitespace and string
// escapes can differ while names, nulls and omitted fields cannot.
func sameJSON(supplied, expected []byte) bool {
	decode := func(raw []byte) (any, error) {
		var value any
		d := json.NewDecoder(bytes.NewReader(raw))
		d.UseNumber()
		err := d.Decode(&value)
		return value, err
	}
	got, err := decode(supplied)
	if err != nil {
		return false
	}
	want, err := decode(expected)
	return err == nil && reflect.DeepEqual(got, want)
}

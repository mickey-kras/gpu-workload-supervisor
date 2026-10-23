// Package attestation verifies signed model attestation envelopes. A valid
// signature binds a signer and a policy decision to exact artifact bytes; it
// never proves that a model is safe.
package attestation

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/strictjson"
)

// FormatVersion is the only envelope format this package reads or writes.
const FormatVersion = 1

// Bounds apply before signature checks so hostile evidence stays cheap to reject.
const (
	MaxEnvelopeBytes  = 16 << 20
	MaxSignatureBytes = 1 << 10
	MaxFiles          = 32768
	MaxPathBytes      = 512
	MaxTextBytes      = 256
	MaxURIBytes       = 2048
	// MaxPolicyVersion keeps every integer exact in any JSON number encoding.
	MaxPolicyVersion = 1<<31 - 1
)

// TimestampLayout is the only accepted timestamp form: RFC 3339 UTC seconds.
const TimestampLayout = "2006-01-02T15:04:05Z"

// Sentinel errors identify the failure class. Messages never echo evidence.
var (
	ErrUnreadable         = errors.New("attestation input is unreadable")
	ErrMalformedEnvelope  = errors.New("invalid attestation envelope")
	ErrMalformedSignature = errors.New("invalid attestation signature file")
	ErrUnsupportedVersion = errors.New("unsupported attestation format version")
	ErrInvalidInventory   = errors.New("invalid on-disk artifact inventory")
	ErrMissingInput       = errors.New("attestation verifier, envelope or signature is missing")
	ErrUnknownKey         = errors.New("attestation key is not pinned")
	ErrInvalidSignature   = errors.New("attestation signature is invalid")
	ErrIncompatiblePolicy = errors.New("attestation policy version is not supported")
	ErrArtifactMismatch   = errors.New("attestation does not bind the artifact on disk")
)

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
// Files maps relative paths to lowercase hex SHA-256 digests, the same shape
// as a training inventory. Shard index files are ordinary entries.
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

// clone copies every reference field, so the copy shares no state with e.
func (e Envelope) clone() Envelope {
	if e.Artifact.Files != nil {
		files := make(map[string]string, len(e.Artifact.Files))
		for path, digest := range e.Artifact.Files {
			files[path] = digest
		}
		e.Artifact.Files = files
	}
	if e.Source != nil {
		source := *e.Source
		e.Source = &source
	}
	if e.Scan.Scanner != nil {
		scanner := *e.Scan.Scanner
		e.Scan.Scanner = &scanner
	}
	return e
}

type presence int

const (
	forbidden presence = iota
	optional
	required
)

type scanEvidence struct {
	scanner, coverage, verdict, report presence
}

// scanFields lists evidence presence per status. A skipped scan carries no
// scanner evidence, so it can never look like a pass.
var scanFields = map[ScanStatus]scanEvidence{
	ScanPassed:      {scanner: required, coverage: required, verdict: required, report: required},
	ScanFindings:    {scanner: required, coverage: required, verdict: required, report: required},
	ScanError:       {scanner: required, coverage: forbidden, verdict: forbidden, report: optional},
	ScanUnsupported: {scanner: required, coverage: forbidden, verdict: forbidden, report: optional},
	ScanNotScanned:  {scanner: forbidden, coverage: forbidden, verdict: forbidden, report: forbidden},
}

func malformed(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrMalformedEnvelope}, args...)...)
}

// MarshalEnvelope returns the canonical JSON of a valid envelope without the
// signing domain prefix. Producers write these bytes as the envelope file.
func MarshalEnvelope(e Envelope) ([]byte, error) {
	if err := e.validate(); err != nil {
		return nil, err
	}
	return canonicalJSON(e.object())
}

// readEnvelope reads one bounded strict envelope. The input must decode to
// the same JSON value as its canonical form: no unknown, null, empty or
// differently cased fields can pass through.
func readEnvelope(r io.Reader) (Envelope, error) {
	raw, err := readLimited(r, MaxEnvelopeBytes, ErrMalformedEnvelope)
	if err != nil {
		return Envelope{}, err
	}
	if otherVersion(raw, FormatVersion) {
		return Envelope{}, ErrUnsupportedVersion
	}
	var envelope Envelope
	if err := strictjson.DecodeLimited(bytes.NewReader(raw), MaxEnvelopeBytes, &envelope); err != nil {
		return Envelope{}, malformed("strict JSON decoding failed")
	}
	if err := envelope.validate(); err != nil {
		return Envelope{}, err
	}
	canonical, err := canonicalJSON(envelope.object())
	if err != nil || !sameJSON(raw, canonical) {
		return Envelope{}, malformed("fields differ from the canonical form")
	}
	return envelope, nil
}

// readLimited separates reader failures from oversized or non-UTF-8 input.
func readLimited(r io.Reader, limit int64, invalid error) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, ErrUnreadable
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("%w: input exceeds %d bytes", invalid, limit)
	}
	if !utf8.Valid(raw) {
		return nil, fmt.Errorf("%w: input is not UTF-8", invalid)
	}
	return raw, nil
}

// otherVersion reports one well-formed JSON object whose exact "version"
// member is a positive integer other than current.
func otherVersion(raw []byte, current int) bool {
	if strictjson.Check(json.NewDecoder(bytes.NewReader(raw))) != nil {
		return false
	}
	var object map[string]json.RawMessage
	d := json.NewDecoder(bytes.NewReader(raw))
	if d.Decode(&object) != nil || d.Decode(&struct{}{}) != io.EOF {
		return false
	}
	version, err := strconv.ParseInt(string(object["version"]), 10, 64)
	return err == nil && version >= 1 && version != int64(current)
}

func (e Envelope) validate() error {
	if e.Version < 1 {
		return malformed("format version must be a positive integer")
	}
	if e.Version != FormatVersion {
		return ErrUnsupportedVersion
	}
	if err := e.Artifact.validate(); err != nil {
		return malformed("artifact: %v", err)
	}
	if e.Source != nil && !e.Source.valid() {
		return malformed("source needs a uri of at most %d bytes or a revision of at most %d bytes", MaxURIBytes, MaxTextBytes)
	}
	if err := e.Scan.validate(); err != nil {
		return err
	}
	if !validPolicyVersion(e.PolicyVersion) {
		return malformed("policy version must be 1 to %d", MaxPolicyVersion)
	}
	if !validTimestamp(e.Timestamp) {
		return malformed("timestamp must be RFC 3339 UTC seconds")
	}
	return nil
}

// validate accepts exactly one artifact form with bounded, clean entries. It
// checks envelope artifacts and the caller's on-disk inventory alike.
func (a Artifact) validate() error {
	if a.SHA256 != "" {
		if a.Files != nil || !digestPattern.MatchString(a.SHA256) {
			return errors.New("single form needs only a lowercase hex SHA-256")
		}
		return nil
	}
	if len(a.Files) == 0 || len(a.Files) > MaxFiles {
		return fmt.Errorf("needs 1 to %d files", MaxFiles)
	}
	for path, digest := range a.Files {
		if !validPath(path) || !digestPattern.MatchString(digest) {
			return errors.New("an entry has an unsupported path or digest")
		}
	}
	return nil
}

func (s Source) valid() bool {
	if s.URI == "" && s.Revision == "" {
		return false
	}
	return optionalText(s.URI, MaxURIBytes) && optionalText(s.Revision, MaxTextBytes)
}

func (s Scan) validate() error {
	fields, known := scanFields[s.Status]
	if !known {
		return malformed("unsupported scan status")
	}
	if !fields.scanner.allows(s.Scanner != nil) || !fields.coverage.allows(s.Coverage != "") || !fields.verdict.allows(s.Verdict != "") || !fields.report.allows(s.ReportSHA256 != "") {
		return malformed("scan evidence does not match its status")
	}
	if s.Scanner != nil && !s.Scanner.valid() {
		return malformed("scanner needs a tool and version of at most %d bytes and a configuration digest", MaxTextBytes)
	}
	if s.Coverage != "" && s.Coverage != CoverageComplete && s.Coverage != CoveragePartial {
		return malformed("unsupported scan coverage")
	}
	if !optionalText(s.Verdict, MaxTextBytes) || (s.ReportSHA256 != "" && !digestPattern.MatchString(s.ReportSHA256)) {
		return malformed("verdict must be at most %d bytes and the report a SHA-256 digest", MaxTextBytes)
	}
	return nil
}

func (p presence) allows(present bool) bool {
	return (p != required || present) && (p != forbidden || !present)
}

func (s Scanner) valid() bool {
	return safeText(s.Tool, MaxTextBytes) && safeText(s.Version, MaxTextBytes) && digestPattern.MatchString(s.ConfigSHA256)
}

func validPolicyVersion(version int) bool {
	return version >= 1 && version <= MaxPolicyVersion
}

func validTimestamp(value string) bool {
	parsed, err := time.Parse(TimestampLayout, value)
	return err == nil && parsed.Format(TimestampLayout) == value
}

// validPath accepts "/"-separated relative paths with no empty, "." or ".."
// segment. The alphabet is narrower than training inventories accept.
func validPath(path string) bool {
	if !safeText(path, MaxPathBytes) {
		return false
	}
	for _, segment := range strings.Split(path, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

func optionalText(value string, limit int) bool {
	return value == "" || safeText(value, limit)
}

// safeText admits printable ASCII except quote and backslash, so canonical
// JSON strings never need escapes.
func safeText(value string, limit int) bool {
	if value == "" || len(value) > limit {
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

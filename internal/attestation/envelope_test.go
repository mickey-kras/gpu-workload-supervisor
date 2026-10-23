package attestation

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("read failed at /private/path") }

func validEnvelope() Envelope {
	return fixtureEnvelopes()["passed"]
}

func TestValidateRejectsUnsafeArtifactPaths(t *testing.T) {
	for _, path := range []string{
		"", ".", "..", "../model.safetensors", "/abs/model.safetensors", "a/../b", "./a", "a/", "a//b", "a/./b", "a/..",
		`a\b`, `a"b`, "a\nb", "a\x00b", "modél.bin", strings.Repeat("a", MaxPathBytes+1),
	} {
		e := validEnvelope()
		e.Artifact = Artifact{Files: map[string]string{path: fixtureDigest(path)}}
		if !errors.Is(e.validate(), ErrMalformedEnvelope) {
			t.Fatalf("accepted path %q", path)
		}
	}
	for _, path := range []string{strings.Repeat("a", MaxPathBytes), "...", "a/.b", "a b/c-d_e+f@1.safetensors"} {
		e := validEnvelope()
		e.Artifact = Artifact{Files: map[string]string{path: fixtureDigest("x")}}
		if err := e.validate(); err != nil {
			t.Fatalf("rejected path %q: %v", path, err)
		}
	}
}

func TestValidateRejectsInvalidArtifactForms(t *testing.T) {
	tooMany := map[string]string{}
	for i := 0; i <= MaxFiles; i++ {
		tooMany["f"+strings.Repeat("x", i%7)+"-"+strings.Repeat("y", i/7%64)+"-"+string(rune('a'+i/448%26))+string(rune('a'+i/11648%26))] = fixtureDigest("x")
	}
	if len(tooMany) != MaxFiles+1 {
		t.Fatalf("fixture built %d files", len(tooMany))
	}
	for name, artifact := range map[string]Artifact{
		"empty":            {},
		"empty files":      {Files: map[string]string{}},
		"both forms":       {Files: map[string]string{"a": fixtureDigest("a")}, SHA256: fixtureDigest("a")},
		"single and empty": {Files: map[string]string{}, SHA256: fixtureDigest("a")},
		"uppercase digest": {Files: map[string]string{"a": strings.ToUpper(fixtureDigest("a"))}},
		"short digest":     {SHA256: "abc"},
		"too many files":   {Files: tooMany},
	} {
		if artifact.validate() == nil {
			t.Fatalf("accepted %s artifact", name)
		}
		if _, err := artifact.Digest(); !errors.Is(err, ErrMalformedEnvelope) {
			t.Fatalf("digested %s artifact: %v", name, err)
		}
	}
	for path := range tooMany {
		delete(tooMany, path)
		break
	}
	if err := (Artifact{Files: tooMany}).validate(); err != nil {
		t.Fatalf("rejected %d files: %v", len(tooMany), err)
	}
}

func TestValidateRejectsInvalidEnvelopeFields(t *testing.T) {
	for name, mutate := range map[string]func(*Envelope){
		"zero policy version":     func(e *Envelope) { e.PolicyVersion = 0 },
		"policy version over cap": func(e *Envelope) { e.PolicyVersion = MaxPolicyVersion + 1 },
		"empty source":            func(e *Envelope) { e.Source = &Source{} },
		"source escape":           func(e *Envelope) { e.Source = &Source{URI: `https://x/"`} },
		"offset timestamp":        func(e *Envelope) { e.Timestamp = "2026-10-01T12:00:00+00:00" },
		"fraction timestamp":      func(e *Envelope) { e.Timestamp = "2026-10-01T12:00:00.5Z" },
		"lowercase timestamp":     func(e *Envelope) { e.Timestamp = "2026-10-01t12:00:00z" },
		"date timestamp":          func(e *Envelope) { e.Timestamp = "2026-10-01" },
		"unknown status":          func(e *Envelope) { e.Scan.Status = "skipped" },
		"unknown coverage":        func(e *Envelope) { e.Scan.Coverage = "most" },
		"scanner config digest":   func(e *Envelope) { e.Scan.Scanner = &Scanner{Tool: "t", Version: "1", ConfigSHA256: "c"} },
		"verdict control byte":    func(e *Envelope) { e.Scan.Verdict = "no\tfindings" },
		"report digest":           func(e *Envelope) { e.Scan.ReportSHA256 = "r" },
	} {
		e := validEnvelope()
		mutate(&e)
		if err := e.validate(); !errors.Is(err, ErrMalformedEnvelope) {
			t.Fatalf("%s: %v", name, err)
		}
		if _, err := SigningBytes(e); err == nil {
			t.Fatalf("signed %s", name)
		}
	}
	e := validEnvelope()
	e.Version = 2
	if _, err := MarshalEnvelope(e); !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("version 2: %v", err)
	}
	e = validEnvelope()
	e.PolicyVersion = MaxPolicyVersion
	if err := e.validate(); err != nil {
		t.Fatalf("rejected maximum policy version: %v", err)
	}
}

func TestTextFieldLimits(t *testing.T) {
	for name, tc := range map[string]struct {
		limit int
		set   func(*Envelope, string)
	}{
		"uri":      {MaxURIBytes, func(e *Envelope, v string) { e.Source = &Source{URI: v} }},
		"revision": {MaxTextBytes, func(e *Envelope, v string) { e.Source = &Source{Revision: v} }},
		"verdict":  {MaxTextBytes, func(e *Envelope, v string) { e.Scan.Verdict = v }},
		"tool":     {MaxTextBytes, func(e *Envelope, v string) { e.Scan.Scanner.Tool = v }},
		"version":  {MaxTextBytes, func(e *Envelope, v string) { e.Scan.Scanner.Version = v }},
	} {
		for length, valid := range map[int]bool{1: true, tc.limit: true, tc.limit + 1: false} {
			e := validEnvelope()
			e.Scan.Scanner = fixtureScanner()
			tc.set(&e, strings.Repeat("v", length))
			if got := e.validate() == nil; got != valid {
				t.Fatalf("%s length %d: valid %v, want %v", name, length, got, valid)
			}
		}
	}
}

// TestScanEvidencePresenceTable restates the documented table independently
// of scanFields: N is necessary, F is not permitted and O is optional.
func TestScanEvidencePresenceTable(t *testing.T) {
	table := map[ScanStatus]string{
		ScanPassed:      "NNNN",
		ScanFindings:    "NNNN",
		ScanError:       "NFFO",
		ScanUnsupported: "NFFO",
		ScanNotScanned:  "FFFF",
	}
	for status, rules := range table {
		for mask := 0; mask < 16; mask++ {
			scan := Scan{Status: status}
			if mask&1 != 0 {
				scan.Scanner = fixtureScanner()
			}
			if mask&2 != 0 {
				scan.Coverage = CoverageComplete
			}
			if mask&4 != 0 {
				scan.Verdict = "verdict"
			}
			if mask&8 != 0 {
				scan.ReportSHA256 = fixtureDigest("report")
			}
			want := true
			for field, rule := range rules {
				present := mask&(1<<field) != 0
				if (rule == 'N' && !present) || (rule == 'F' && present) {
					want = false
				}
			}
			if got := scan.validate() == nil; got != want {
				t.Fatalf("%s mask %04b: valid %v, want %v", status, mask, got, want)
			}
		}
	}
}

func TestReadEnvelopeSeparatesReadFailures(t *testing.T) {
	if _, err := readEnvelope(failingReader{}); !errors.Is(err, ErrUnreadable) || strings.Contains(err.Error(), "/private") {
		t.Fatalf("read failure: %v", err)
	}
	raw := readFixture(t, "passed.json")
	for name, body := range map[string][]byte{
		"invalid UTF-8":          bytes.Replace(raw, []byte("no-findings"), []byte("no-\xfffindings"), 1),
		"non-integer policy":     bytes.Replace(raw, []byte(`"policyVersion": 1`), []byte(`"policyVersion": 1.0`), 1),
		"empty optional field":   bytes.Replace(raw, []byte(`"coverage": "complete",`), []byte(`"coverage": "",`), 1),
		"escaped quote":          bytes.Replace(raw, []byte("no-findings"), []byte(`no-\"findings`), 1),
		"version string":         bytes.Replace(raw, []byte(`"version": 1`), []byte(`"version": "2"`), 1),
		"version zero":           bytes.Replace(raw, []byte(`"version": 1`), []byte(`"version": 0`), 1),
		"other version trailing": append(bytes.Replace(raw, []byte(`"version": 1`), []byte(`"version": 2`), 1), []byte("{}")...),
		"other version array":    []byte(`[{"version":2}]`),
	} {
		if bytes.Equal(body, raw) {
			t.Fatalf("%s mutation did not apply", name)
		}
		_, err := readEnvelope(bytes.NewReader(body))
		if !errors.Is(err, ErrMalformedEnvelope) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestSigningBytesAreCanonicalAndDomainSeparated(t *testing.T) {
	e := fixtureEnvelopes()["single"]
	got, err := SigningBytes(e)
	if err != nil {
		t.Fatal(err)
	}
	want := SigningDomain + `{"artifact":{"sha256":"` + e.Artifact.SHA256 + `"},"policyVersion":1,"scan":{"coverage":"complete","reportSHA256":"` +
		e.Scan.ReportSHA256 + `","scanner":{"configSHA256":"` + e.Scan.Scanner.ConfigSHA256 + `","tool":"llm-backdoor-scanner","version":"0.0.0-fixture"},"status":"passed","verdict":"no-findings"},"source":{"revision":"v1.0.0"},"timestamp":"2026-10-01T12:00:00Z","version":1}`
	if string(got) != want {
		t.Fatalf("signing bytes:\n%s\nwant:\n%s", got, want)
	}
	marshaled, err := MarshalEnvelope(e)
	if err != nil || string(marshaled) != strings.TrimPrefix(want, SigningDomain) {
		t.Fatalf("marshal: %s %v", marshaled, err)
	}
	if parsed, err := readEnvelope(bytes.NewReader(marshaled)); err != nil || parsed.Timestamp != e.Timestamp {
		t.Fatalf("marshaled envelope does not read back: %v", err)
	}
}

func TestArtifactDigestMatchesIndependentGolden(t *testing.T) {
	var golden struct {
		Note   string `json:"note"`
		Files  string `json:"files"`
		Single string `json:"single"`
	}
	if err := json.Unmarshal(readFixture(t, "artifact-digests.json"), &golden); err != nil {
		t.Fatal(err)
	}
	if got := mustDigest(t, fixtureInventory(t)); got != golden.Files {
		t.Fatalf("files digest %s, golden %s", got, golden.Files)
	}
	if got := mustDigest(t, fixtureEnvelopes()["single"].Artifact); got != golden.Single {
		t.Fatalf("single digest %s, golden %s", got, golden.Single)
	}
}

func TestCanonicalEncoderRejectsUnsupportedValues(t *testing.T) {
	for _, value := range []any{1.5, true, nil, []any{}, "a\"b", map[string]any{"a": 1.5}, map[string]any{"a\nb": 1}} {
		if _, err := canonicalJSON(value); err == nil {
			t.Fatalf("encoded %#v", value)
		}
	}
}

func TestSameJSONRejectsInvalidInput(t *testing.T) {
	if sameJSON([]byte(`{`), []byte(`{}`)) || sameJSON([]byte(`{}`), []byte(`{`)) {
		t.Fatal("invalid JSON compared equal")
	}
}

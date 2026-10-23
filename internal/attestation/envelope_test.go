package attestation

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }

func validEnvelope() Envelope {
	return fixtureEnvelopes()["passed"]
}

func TestValidateRejectsUnsafeArtifactPaths(t *testing.T) {
	for _, path := range []string{
		"", ".", "..", "../model.safetensors", "/abs/model.safetensors", "a/../b", "./a", "a/", "a//b",
		`a\b`, `a"b`, "a\nb", "a\x00b", "modél.bin", strings.Repeat("a", MaxPathBytes+1),
	} {
		e := validEnvelope()
		e.Artifact = Artifact{Files: map[string]string{path: fixtureDigest(path)}}
		if e.Validate() == nil {
			t.Fatalf("accepted path %q", path)
		}
	}
	e := validEnvelope()
	e.Artifact = Artifact{Files: map[string]string{strings.Repeat("a", MaxPathBytes): fixtureDigest("long")}}
	if err := e.Validate(); err != nil {
		t.Fatalf("rejected maximum path: %v", err)
	}
}

func TestValidateRejectsInvalidArtifactForms(t *testing.T) {
	tooMany := map[string]string{}
	for i := 0; i <= MaxFiles; i++ {
		tooMany[strings.Repeat("d/", i%4)+"f"+strings.Repeat("x", i/4%64)+string(rune('a'+i%26))+"-"+string(rune('a'+i/26%26))+"-"+string(rune('a'+i/676%26))] = fixtureDigest("x")
	}
	if len(tooMany) <= MaxFiles {
		t.Fatalf("fixture built only %d files", len(tooMany))
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
		if artifact.Validate() == nil {
			t.Fatalf("accepted %s artifact", name)
		}
		if _, err := artifact.Digest(); err == nil {
			t.Fatalf("digested %s artifact", name)
		}
	}
}

func TestValidateRejectsInvalidEnvelopeFields(t *testing.T) {
	for name, mutate := range map[string]func(*Envelope){
		"format version":      func(e *Envelope) { e.Version = 2 },
		"policy version":      func(e *Envelope) { e.PolicyVersion = 0 },
		"empty source":        func(e *Envelope) { e.Source = &Source{} },
		"source escape":       func(e *Envelope) { e.Source = &Source{URI: `https://x/"`} },
		"long revision":       func(e *Envelope) { e.Source = &Source{Revision: strings.Repeat("a", maxTextBytes+1)} },
		"offset timestamp":    func(e *Envelope) { e.Timestamp = "2026-10-01T12:00:00+00:00" },
		"fraction timestamp":  func(e *Envelope) { e.Timestamp = "2026-10-01T12:00:00.5Z" },
		"lowercase timestamp": func(e *Envelope) { e.Timestamp = "2026-10-01t12:00:00z" },
		"date timestamp":      func(e *Envelope) { e.Timestamp = "2026-10-01" },
		"unknown status":      func(e *Envelope) { e.Scan.Status = "skipped" },
		"passed without report": func(e *Envelope) {
			e.Scan.ReportSHA256 = ""
		},
		"passed without scanner":  func(e *Envelope) { e.Scan.Scanner = nil },
		"passed without coverage": func(e *Envelope) { e.Scan.Coverage = "" },
		"passed without verdict":  func(e *Envelope) { e.Scan.Verdict = "" },
		"skipped with scanner":    func(e *Envelope) { e.Scan = Scan{Status: ScanNotScanned, Scanner: fixtureScanner()} },
		"skipped with report":     func(e *Envelope) { e.Scan = Scan{Status: ScanNotScanned, ReportSHA256: fixtureDigest("r")} },
		"error with verdict":      func(e *Envelope) { e.Scan = Scan{Status: ScanError, Scanner: fixtureScanner(), Verdict: "x"} },
		"unsupported coverage": func(e *Envelope) {
			e.Scan = Scan{Status: ScanUnsupported, Scanner: fixtureScanner(), Coverage: CoverageComplete}
		},
		"unknown coverage":        func(e *Envelope) { e.Scan.Coverage = "most" },
		"scanner without tool":    func(e *Envelope) { e.Scan.Scanner = &Scanner{Version: "1", ConfigSHA256: fixtureDigest("c")} },
		"scanner without version": func(e *Envelope) { e.Scan.Scanner = &Scanner{Tool: "t", ConfigSHA256: fixtureDigest("c")} },
		"scanner config digest":   func(e *Envelope) { e.Scan.Scanner = &Scanner{Tool: "t", Version: "1", ConfigSHA256: "c"} },
		"verdict control byte":    func(e *Envelope) { e.Scan.Verdict = "no\tfindings" },
		"report digest":           func(e *Envelope) { e.Scan.ReportSHA256 = "r" },
	} {
		e := validEnvelope()
		mutate(&e)
		if e.Validate() == nil {
			t.Fatalf("accepted %s", name)
		}
		if _, err := SigningBytes(e); err == nil {
			t.Fatalf("signed %s", name)
		}
	}
}

func TestReadEnvelopeRejectsUnreadableAndNonUTF8Input(t *testing.T) {
	if _, err := ReadEnvelope(failingReader{}); err == nil {
		t.Fatal("read failure ignored")
	}
	raw := readFixture(t, "passed.json")
	if _, err := ReadEnvelope(bytes.NewReader(bytes.Replace(raw, []byte("no-findings"), []byte("no-\xfffindings"), 1))); err == nil {
		t.Fatal("invalid UTF-8 accepted")
	}
	if _, err := ReadEnvelope(bytes.NewReader(bytes.Replace(raw, []byte(`"policyVersion": 1`), []byte(`"policyVersion": 1.0`), 1))); err == nil {
		t.Fatal("non-integer policy version accepted")
	}
	if _, err := ReadEnvelope(bytes.NewReader(bytes.Replace(raw, []byte(`"coverage": "complete",`), []byte(`"coverage": "",`), 1))); err == nil {
		t.Fatal("empty optional field accepted")
	}
	if _, err := ReadEnvelope(bytes.NewReader(bytes.Replace(raw, []byte("no-findings"), []byte(`no-\"findings`), 1))); err == nil {
		t.Fatal("escaped quote accepted")
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
	digest, err := e.Artifact.Digest()
	if err != nil || digest == e.Artifact.SHA256 || !digestPattern.MatchString(digest) {
		t.Fatalf("artifact digest %q %v", digest, err)
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

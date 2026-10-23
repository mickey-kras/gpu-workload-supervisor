package attestation

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func fixtureVerifier(t *testing.T, keys ...int) *Verifier {
	t.Helper()
	encoded := []string{}
	for _, key := range keys {
		encoded = append(encoded, testPublicKey(key))
	}
	v, err := NewVerifier(encoded, []int{1})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func forgedSignature(t *testing.T, raw []byte) []byte {
	t.Helper()
	var sig Signature
	if err := json.Unmarshal(raw, &sig); err != nil {
		t.Fatal(err)
	}
	value, err := base64.StdEncoding.DecodeString(sig.Value)
	if err != nil {
		t.Fatal(err)
	}
	value[0] ^= 0x01
	sig.Value = base64.StdEncoding.EncodeToString(value)
	return marshalFixture(t, sig)
}

func withFile(files map[string]string, path, digest string) Artifact {
	copied := map[string]string{}
	for name, value := range files {
		copied[name] = value
	}
	if digest == "" {
		delete(copied, path)
	} else {
		copied[path] = digest
	}
	return Artifact{Files: copied}
}

func TestVerificationFixtureMatrix(t *testing.T) {
	passed := readFixture(t, "passed.json")
	passedSig := readFixture(t, "passed.sig.json")
	inventory := fixtureInventory(t)
	files := inventory.Files
	compact := new(bytes.Buffer)
	if err := json.Compact(compact, passed); err != nil {
		t.Fatal(err)
	}
	reordered := `{"timestamp":"2026-10-01T12:00:00Z","policyVersion":1,` + strings.TrimPrefix(compact.String(), "{")
	reordered = strings.Replace(reordered, `,"policyVersion":1,"timestamp":"2026-10-01T12:00:00Z"}`, "}", 1)
	escaped := strings.Replace(string(passed), `"no-findings"`, `"no-findings"`, 1)
	oversized := append(append([]byte{}, passed...), bytes.Repeat([]byte(" "), MaxEnvelopeBytes)...)
	for _, tc := range []struct {
		name      string
		envelope  string
		signature []byte
		keys      []int
		onDisk    Artifact
		want      Result
	}{
		{"valid", string(passed), passedSig, []int{1}, inventory, Valid},
		{"whitespace and key order do not change signing bytes", reordered, passedSig, []int{1}, inventory, Valid},
		{"string escapes decode to the same value", escaped, passedSig, []int{1}, inventory, Valid},
		{"rotated key pinned beside old key", string(passed), readFixture(t, "passed.key-2.sig.json"), []int{1, 2}, inventory, Valid},
		{"old key removed after rotation", string(passed), passedSig, []int{2}, inventory, UnknownKey},
		{"new key not yet pinned", string(passed), readFixture(t, "passed.key-2.sig.json"), []int{1}, inventory, UnknownKey},
		{"forged signature", string(passed), forgedSignature(t, passedSig), []int{1}, inventory, InvalidSignature},
		{"envelope changed after signing", strings.Replace(string(passed), "no-findings", "no-finding", 1), passedSig, []int{1}, inventory, InvalidSignature},
		{"signature for other evidence", string(readFixture(t, "findings.json")), passedSig, []int{1}, inventory, InvalidSignature},
		{"wrong digest on disk", string(passed), passedSig, []int{1}, withFile(files, "model-00002-of-00002.safetensors", fixtureDigest("swapped shard")), ArtifactMismatch},
		{"extra file on disk", string(passed), passedSig, []int{1}, withFile(files, "adapter/extra.safetensors", fixtureDigest("extra")), ArtifactMismatch},
		{"missing shard index on disk", string(passed), passedSig, []int{1}, withFile(files, "model.safetensors.index.json", ""), ArtifactMismatch},
		{"renamed file on disk", string(passed), passedSig, []int{1}, withFile(withFile(files, "config.json", "").Files, "configuration.json", files["config.json"]), ArtifactMismatch},
		{"directory evidence for single artifact", string(passed), passedSig, []int{1}, Artifact{SHA256: fixtureDigest("model.gguf")}, ArtifactMismatch},
		{"single artifact valid", string(readFixture(t, "single.json")), readFixture(t, "single.sig.json"), []int{1}, Artifact{SHA256: fixtureDigest("model.gguf")}, Valid},
		{"single artifact wrong bytes", string(readFixture(t, "single.json")), readFixture(t, "single.sig.json"), []int{1}, Artifact{SHA256: fixtureDigest("other.gguf")}, ArtifactMismatch},
		{"single artifact against inventory", string(readFixture(t, "single.json")), readFixture(t, "single.sig.json"), []int{1}, inventory, ArtifactMismatch},
		{"incompatible policy version", string(readFixture(t, "policy-2.json")), readFixture(t, "policy-2.sig.json"), []int{1}, inventory, IncompatiblePolicy},
		{"truncated envelope", string(passed[:len(passed)/2]), passedSig, []int{1}, inventory, Malformed},
		{"truncated signature", string(passed), passedSig[:len(passedSig)/2], []int{1}, inventory, Malformed},
		{"oversized envelope", string(oversized), passedSig, []int{1}, inventory, Malformed},
		{"duplicate key", strings.Replace(string(passed), "{", `{"version":1,`, 1), passedSig, []int{1}, inventory, Malformed},
		{"duplicate file path", strings.Replace(string(passed), `"config.json":`, `"vocab.json":"`+files["vocab.json"]+`","config.json":`, 1), passedSig, []int{1}, inventory, Malformed},
		{"unknown field", strings.Replace(string(passed), "{", `{"approved":true,`, 1), passedSig, []int{1}, inventory, Malformed},
		{"differently cased field", strings.Replace(string(passed), `"version"`, `"Version"`, 1), passedSig, []int{1}, inventory, Malformed},
		{"null optional field", strings.Replace(string(readFixture(t, "not-scanned.json")), "{", `{"source":null,`, 1), readFixture(t, "not-scanned.sig.json"), []int{1}, inventory, Malformed},
		{"unscanned control", string(readFixture(t, "not-scanned.json")), readFixture(t, "not-scanned.sig.json"), []int{1}, inventory, Valid},
		{"trailing data", string(passed) + "{}", passedSig, []int{1}, inventory, Malformed},
		{"unsupported format version", strings.Replace(string(passed), `"version": 1`, `"version": 2`, 1), passedSig, []int{1}, inventory, Malformed},
		{"signature unknown field", string(passed), bytes.Replace(passedSig, []byte("{"), []byte(`{"trusted":true,`), 1), []int{1}, inventory, Malformed},
		{"signature other algorithm", string(passed), bytes.Replace(passedSig, []byte(`"ed25519"`), []byte(`"ed448"`), 1), []int{1}, inventory, Malformed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := fixtureVerifier(t, tc.keys...).Verify(strings.NewReader(tc.envelope), bytes.NewReader(tc.signature), tc.onDisk)
			if got.Result != tc.want {
				t.Fatalf("result %q, want %q: %v", got.Result, tc.want, got.Err)
			}
			if (got.Result == Valid) != (got.Err == nil) || (got.Result == Valid) != (got.Envelope.Version == FormatVersion) {
				t.Fatalf("valid result must carry only an envelope: %#v", got)
			}
			// Integrity failures reject in every mode, even with a matching exemption.
			if tc.want != Valid {
				for _, mode := range []Mode{Advisory, Enforce} {
					decision := Decide(Policy{Mode: mode, Exemptions: []Exemption{{Artifact: mustDigest(t, inventory), PolicyVersion: 1}}}, &got)
					if decision != (Decision{Status: IntegrityFailure, Reason: string(tc.want)}) {
						t.Fatalf("%s: %#v", mode, decision)
					}
				}
			}
		})
	}
}

func mustDigest(t *testing.T, a Artifact) string {
	t.Helper()
	digest, err := a.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func TestDecisionFixtureMatrix(t *testing.T) {
	inventory := fixtureInventory(t)
	digest := mustDigest(t, inventory)
	allow := func(status Status, reason string) Decision {
		return Decision{Allowed: true, Status: status, Reason: reason}
	}
	deny := func(status Status, reason string) Decision { return Decision{Status: status, Reason: reason} }
	exact := []Exemption{{Artifact: digest, PolicyVersion: 1}}
	otherArtifact := []Exemption{{Artifact: fixtureDigest("other artifact"), PolicyVersion: 1}}
	otherPolicy := []Exemption{{Artifact: digest, PolicyVersion: 2}}
	for _, tc := range []struct {
		fixture    string
		exemptions []Exemption
		advisory   Decision
		enforce    Decision
	}{
		{"passed", nil, allow(Verified, ReasonPassed), allow(Verified, ReasonPassed)},
		{"findings", nil, deny(FindingsDetected, ReasonFindings), deny(FindingsDetected, ReasonFindings)},
		{"findings", exact, deny(FindingsDetected, ReasonFindings), deny(FindingsDetected, ReasonFindings)},
		{"error", nil, allow(NotVerified, ReasonScanError), deny(NotVerified, ReasonScanError)},
		{"error", exact, allow(NotVerified, ReasonScanError), deny(NotVerified, ReasonScanError)},
		{"not-scanned", nil, allow(NotVerified, ReasonNotScanned), deny(NotVerified, ReasonNotScanned)},
		{"not-scanned", exact, allow(Exempted, ReasonNotScanned), allow(Exempted, ReasonNotScanned)},
		{"not-scanned", otherArtifact, allow(NotVerified, ReasonNotScanned), deny(NotVerified, ReasonNotScanned)},
		{"not-scanned", otherPolicy, allow(NotVerified, ReasonNotScanned), deny(NotVerified, ReasonNotScanned)},
		{"unsupported", nil, allow(NotVerified, ReasonUnsupported), deny(NotVerified, ReasonUnsupported)},
		{"unsupported", exact, allow(Exempted, ReasonUnsupported), allow(Exempted, ReasonUnsupported)},
		{"unsupported", otherArtifact, allow(NotVerified, ReasonUnsupported), deny(NotVerified, ReasonUnsupported)},
		{"partial", nil, allow(NotVerified, ReasonPartialCoverage), deny(NotVerified, ReasonPartialCoverage)},
		{"partial", exact, allow(Exempted, ReasonPartialCoverage), allow(Exempted, ReasonPartialCoverage)},
	} {
		verification := fixtureVerifier(t, 1).Verify(bytes.NewReader(readFixture(t, tc.fixture+".json")), bytes.NewReader(readFixture(t, tc.fixture+".sig.json")), inventory)
		if verification.Result != Valid {
			t.Fatalf("%s: %q %v", tc.fixture, verification.Result, verification.Err)
		}
		for mode, want := range map[Mode]Decision{"": tc.advisory, Advisory: tc.advisory, Enforce: tc.enforce, "strict": tc.enforce} {
			if got := Decide(Policy{Mode: mode, Exemptions: tc.exemptions}, &verification); got != want {
				t.Fatalf("%s %q %v: %#v, want %#v", tc.fixture, mode, tc.exemptions, got, want)
			}
		}
	}
}

func TestMissingAttestationIsUnverifiedNeverPassed(t *testing.T) {
	exempt := []Exemption{{Artifact: mustDigest(t, fixtureInventory(t)), PolicyVersion: 1}}
	if got := Decide(Policy{Exemptions: exempt}, nil); got != (Decision{Allowed: true, Status: NotVerified, Reason: ReasonMissing}) {
		t.Fatalf("advisory: %#v", got)
	}
	if got := Decide(Policy{Mode: Enforce, Exemptions: exempt}, nil); got != (Decision{Status: NotVerified, Reason: ReasonMissing}) {
		t.Fatalf("enforce: %#v", got)
	}
}

func TestDecideFailsClosedOnUnvalidatedEnvelope(t *testing.T) {
	got := Decide(Policy{}, &Verification{Result: Valid})
	if got != (Decision{Status: IntegrityFailure, Reason: string(Malformed)}) {
		t.Fatalf("%#v", got)
	}
	// A hand-built envelope without a valid artifact can never match an exemption.
	unexemptable := Verification{Result: Valid, Envelope: Envelope{Scan: Scan{Status: ScanNotScanned}}}
	if got := Decide(Policy{Mode: Enforce, Exemptions: []Exemption{{Artifact: fixtureDigest("x"), PolicyVersion: 0}}}, &unexemptable); got.Allowed {
		t.Fatalf("%#v", got)
	}
}

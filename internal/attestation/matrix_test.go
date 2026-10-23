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
	return fixtureVerifierWithPolicies(t, []int{1}, keys...)
}

func fixtureVerifierWithPolicies(t *testing.T, policies []int, keys ...int) *Verifier {
	t.Helper()
	encoded := []string{}
	for _, key := range keys {
		encoded = append(encoded, testPublicKey(key))
	}
	v, err := newVerifier(encoded, policies, true)
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

// replaced fails the test when a mutation does not change its input.
func replaced(t *testing.T, s, old, replacement string) string {
	t.Helper()
	out := strings.Replace(s, old, replacement, 1)
	if out == s {
		t.Fatalf("mutation %q did not apply", old)
	}
	return out
}

type verifyCase struct {
	name      string
	envelope  string
	signature []byte
	keys      []int
	onDisk    Artifact
	want      Result
}

func verificationCases(t *testing.T) []verifyCase {
	passed := string(readFixture(t, "passed.json"))
	passedSig := readFixture(t, "passed.sig.json")
	key2Sig := readFixture(t, "passed.key-2.sig.json")
	single := string(readFixture(t, "single.json"))
	singleSig := readFixture(t, "single.sig.json")
	notScanned := string(readFixture(t, "not-scanned.json"))
	notScannedSig := readFixture(t, "not-scanned.sig.json")
	inventory := fixtureInventory(t)
	files := inventory.Files
	singleDisk := Artifact{SHA256: fixtureDigest("model.gguf")}
	compact := new(bytes.Buffer)
	if err := json.Compact(compact, []byte(passed)); err != nil {
		t.Fatal(err)
	}
	reordered := `{"timestamp":"2026-10-01T12:00:00Z","policyVersion":1,` + strings.TrimPrefix(compact.String(), "{")
	reordered = replaced(t, reordered, `,"policyVersion":1,"timestamp":"2026-10-01T12:00:00Z"}`, "}")
	atLimit := passed + strings.Repeat(" ", MaxEnvelopeBytes-len(passed))
	return []verifyCase{
		{"valid", passed, passedSig, []int{1}, inventory, Valid},
		{"whitespace and key order do not change signing bytes", reordered, passedSig, []int{1}, inventory, Valid},
		{"unicode escape decodes to the same value", replaced(t, passed, `"status": "passed"`, `"status": "p\u0061ssed"`), passedSig, []int{1}, inventory, Valid},
		{"envelope at the size limit", atLimit, passedSig, []int{1}, inventory, Valid},
		{"envelope one byte over the size limit", atLimit + " ", passedSig, []int{1}, inventory, Malformed},
		{"rotated key pinned beside old key", passed, key2Sig, []int{1, 2}, inventory, Valid},
		{"old key removed after rotation", passed, passedSig, []int{2}, inventory, UnknownKey},
		{"new key not yet pinned", passed, key2Sig, []int{1}, inventory, UnknownKey},
		{"forged signature", passed, forgedSignature(t, passedSig), []int{1}, inventory, InvalidSignature},
		{"envelope changed after signing", replaced(t, passed, "no-findings", "no-finding"), passedSig, []int{1}, inventory, InvalidSignature},
		{"signature for other evidence", string(readFixture(t, "findings.json")), passedSig, []int{1}, inventory, InvalidSignature},
		{"wrong digest on disk", passed, passedSig, []int{1}, withFile(files, "model-00002-of-00002.safetensors", fixtureDigest("swapped shard")), ArtifactMismatch},
		{"extra file on disk", passed, passedSig, []int{1}, withFile(files, "adapter/extra.safetensors", fixtureDigest("extra")), ArtifactMismatch},
		{"missing shard index on disk", passed, passedSig, []int{1}, withFile(files, "model.safetensors.index.json", ""), ArtifactMismatch},
		{"renamed file on disk", passed, passedSig, []int{1}, withFile(withFile(files, "config.json", "").Files, "configuration.json", files["config.json"]), ArtifactMismatch},
		{"directory evidence for single artifact", passed, passedSig, []int{1}, singleDisk, ArtifactMismatch},
		{"single artifact valid", single, singleSig, []int{1}, singleDisk, Valid},
		{"single artifact wrong bytes", single, singleSig, []int{1}, Artifact{SHA256: fixtureDigest("other.gguf")}, ArtifactMismatch},
		{"single artifact against inventory", single, singleSig, []int{1}, inventory, ArtifactMismatch},
		{"inventory with both forms", single, singleSig, []int{1}, Artifact{SHA256: singleDisk.SHA256, Files: files}, InvalidInventory},
		{"empty inventory", passed, passedSig, []int{1}, Artifact{}, InvalidInventory},
		{"inventory with unsupported path", passed, passedSig, []int{1}, withFile(files, "../escape", fixtureDigest("x")), InvalidInventory},
		{"incompatible policy version", string(readFixture(t, "policy-2.json")), readFixture(t, "policy-2.sig.json"), []int{1}, inventory, IncompatiblePolicy},
		{"truncated envelope", passed[:len(passed)/2], passedSig, []int{1}, inventory, Malformed},
		{"truncated signature", passed, passedSig[:len(passedSig)/2], []int{1}, inventory, Malformed},
		{"duplicate key", replaced(t, passed, "{", `{"version":1,`), passedSig, []int{1}, inventory, Malformed},
		{"duplicate file path", replaced(t, passed, `"config.json":`, `"vocab.json":"`+files["vocab.json"]+`","config.json":`), passedSig, []int{1}, inventory, Malformed},
		{"unknown field", replaced(t, passed, "{", `{"approved":true,`), passedSig, []int{1}, inventory, Malformed},
		{"differently cased field", replaced(t, passed, `"version"`, `"Version"`), passedSig, []int{1}, inventory, Malformed},
		{"null optional field", replaced(t, notScanned, "{", `{"source":null,`), notScannedSig, []int{1}, inventory, Malformed},
		{"unscanned control", notScanned, notScannedSig, []int{1}, inventory, Valid},
		{"trailing data", passed + "{}", passedSig, []int{1}, inventory, Malformed},
		{"unsupported envelope version", replaced(t, passed, `"version": 1`, `"version": 2`), passedSig, []int{1}, inventory, UnsupportedVersion},
		{"envelope version not an integer", replaced(t, passed, `"version": 1`, `"version": 2.0`), passedSig, []int{1}, inventory, Malformed},
		{"unsupported signature version", passed, bytes.Replace(passedSig, []byte(`"version": 1`), []byte(`"version": 3`), 1), []int{1}, inventory, UnsupportedVersion},
		{"signature unknown field", passed, bytes.Replace(passedSig, []byte("{"), []byte(`{"trusted":true,`), 1), []int{1}, inventory, Malformed},
		{"signature other algorithm", passed, bytes.Replace(passedSig, []byte(`"ed25519"`), []byte(`"ed448"`), 1), []int{1}, inventory, Malformed},
		// Precedence: the signature file and key ID come before the envelope.
		{"unknown key before malformed envelope", "{", passedSig, []int{2}, inventory, UnknownKey},
		{"malformed signature before unknown key", passed, []byte("{"), []int{2}, inventory, Malformed},
		{"inventory before malformed signature", "{", []byte("{"), []int{1}, Artifact{}, InvalidInventory},
		{"signature before policy version", string(readFixture(t, "policy-2.json")), passedSig, []int{1}, inventory, InvalidSignature},
		{"policy version before artifact", string(readFixture(t, "policy-2.json")), readFixture(t, "policy-2.sig.json"), []int{1}, Artifact{SHA256: fixtureDigest("x")}, IncompatiblePolicy},
	}
}

func TestVerificationFixtureMatrix(t *testing.T) {
	inventory := fixtureInventory(t)
	for _, tc := range verificationCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			got := fixtureVerifier(t, tc.keys...).Verify(strings.NewReader(tc.envelope), bytes.NewReader(tc.signature), tc.onDisk)
			if got.Result() != tc.want {
				t.Fatalf("result %q, want %q: %v", got.Result(), tc.want, got.Err())
			}
			if (got.Result() == Valid) != (got.Err() == nil) || (got.Result() == Valid) != (got.Envelope().Version == FormatVersion) {
				t.Fatalf("valid result must carry only an envelope: %#v", got)
			}
			if tc.want == Valid {
				return
			}
			want := Decision{Status: IntegrityFailure, Reason: Reason(tc.want)}
			if tc.want == UnsupportedVersion {
				want = Decision{Status: NotVerified, Reason: ReasonUnsupportedVersion}
			}
			// No exemption admits failed verification, in any mode or kind.
			exemptions := []Exemption{}
			for _, kind := range []ExemptionKind{ExemptNotScanned, ExemptUnsupported, ExemptPartialCoverage, ExemptFindings} {
				exemptions = append(exemptions, Exemption{Artifact: mustDigest(t, inventory), PolicyVersion: 1, Kind: kind, Reason: "fixture"})
			}
			for _, mode := range []Mode{Advisory, Enforce} {
				want.Allowed = mode == Advisory && want.Status == NotVerified
				if decision := Decide(Policy{Mode: mode, Exemptions: exemptions}, &got); decision != want {
					t.Fatalf("%s: %#v, want %#v", mode, decision, want)
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

func allow(status Status, reason Reason) Decision {
	return Decision{Allowed: true, Status: status, Reason: reason}
}

func deny(status Status, reason Reason) Decision {
	return Decision{Status: status, Reason: reason}
}

func exemption(artifact string, policyVersion int, kind ExemptionKind) []Exemption {
	return []Exemption{{Artifact: artifact, PolicyVersion: policyVersion, Kind: kind, Reason: "operator accepted fixture risk"}}
}

func TestDecisionFixtureMatrix(t *testing.T) {
	inventory := fixtureInventory(t)
	single := Artifact{SHA256: fixtureDigest("model.gguf")}
	digest := mustDigest(t, inventory)
	other := fixtureDigest("other artifact")
	for _, tc := range []struct {
		fixture    string
		onDisk     Artifact
		exemptions []Exemption
		advisory   Decision
		enforce    Decision
	}{
		{"passed", inventory, nil, allow(Verified, ReasonPassed), allow(Verified, ReasonPassed)},
		{"single", single, nil, allow(Verified, ReasonPassed), allow(Verified, ReasonPassed)},
		{"policy-2", inventory, nil, allow(Verified, ReasonPassed), allow(Verified, ReasonPassed)},
		{"findings", inventory, nil, deny(FindingsDetected, ReasonFindings), deny(FindingsDetected, ReasonFindings)},
		{"findings", inventory, exemption(digest, 1, ExemptFindings), allow(Exempted, ReasonFindings), allow(Exempted, ReasonFindings)},
		{"findings", inventory, exemption(digest, 1, ExemptNotScanned), deny(FindingsDetected, ReasonFindings), deny(FindingsDetected, ReasonFindings)},
		{"findings", inventory, exemption(other, 1, ExemptFindings), deny(FindingsDetected, ReasonFindings), deny(FindingsDetected, ReasonFindings)},
		{"error", inventory, nil, allow(NotVerified, ReasonScanError), deny(NotVerified, ReasonScanError)},
		{"not-scanned", inventory, nil, allow(NotVerified, ReasonNotScanned), deny(NotVerified, ReasonNotScanned)},
		{"not-scanned", inventory, exemption(digest, 1, ExemptNotScanned), allow(Exempted, ReasonNotScanned), allow(Exempted, ReasonNotScanned)},
		{"not-scanned", inventory, exemption(digest, 1, ExemptUnsupported), allow(NotVerified, ReasonNotScanned), deny(NotVerified, ReasonNotScanned)},
		{"not-scanned", inventory, exemption(other, 1, ExemptNotScanned), allow(NotVerified, ReasonNotScanned), deny(NotVerified, ReasonNotScanned)},
		{"not-scanned", inventory, exemption(digest, 2, ExemptNotScanned), allow(NotVerified, ReasonNotScanned), deny(NotVerified, ReasonNotScanned)},
		{"unsupported", inventory, nil, allow(NotVerified, ReasonUnsupported), deny(NotVerified, ReasonUnsupported)},
		{"unsupported", inventory, exemption(digest, 1, ExemptUnsupported), allow(Exempted, ReasonUnsupported), allow(Exempted, ReasonUnsupported)},
		{"unsupported", inventory, exemption(other, 1, ExemptUnsupported), allow(NotVerified, ReasonUnsupported), deny(NotVerified, ReasonUnsupported)},
		{"unsupported", inventory, exemption(digest, 2, ExemptUnsupported), allow(NotVerified, ReasonUnsupported), deny(NotVerified, ReasonUnsupported)},
		{"partial", inventory, nil, allow(NotVerified, ReasonPartialCoverage), deny(NotVerified, ReasonPartialCoverage)},
		{"partial", inventory, exemption(digest, 1, ExemptPartialCoverage), allow(Exempted, ReasonPartialCoverage), allow(Exempted, ReasonPartialCoverage)},
		{"partial", inventory, exemption(other, 1, ExemptPartialCoverage), allow(NotVerified, ReasonPartialCoverage), deny(NotVerified, ReasonPartialCoverage)},
		{"partial", inventory, exemption(digest, 2, ExemptPartialCoverage), allow(NotVerified, ReasonPartialCoverage), deny(NotVerified, ReasonPartialCoverage)},
	} {
		verification := fixtureVerifierWithPolicies(t, []int{1, 2}, 1).Verify(bytes.NewReader(readFixture(t, tc.fixture+".json")), bytes.NewReader(readFixture(t, tc.fixture+".sig.json")), tc.onDisk)
		if verification.Result() != Valid {
			t.Fatalf("%s: %q %v", tc.fixture, verification.Result(), verification.Err())
		}
		for mode, want := range map[Mode]Decision{Advisory: tc.advisory, Enforce: tc.enforce, "": tc.enforce, "strict": tc.enforce, "Advisory": tc.enforce} {
			if got := Decide(Policy{Mode: mode, Exemptions: tc.exemptions}, &verification); got != want {
				t.Fatalf("%s %q %v: %#v, want %#v", tc.fixture, mode, tc.exemptions, got, want)
			}
		}
	}
}

func TestSingleArtifactExemptionUsesItsOwnDigest(t *testing.T) {
	single := Artifact{SHA256: fixtureDigest("model.gguf")}
	e := fixtureEnvelopes()["single"]
	e.Scan = Scan{Status: ScanNotScanned}
	verification := Verification{result: Valid, envelope: e, sealed: true}
	if got := Decide(Policy{Mode: Enforce, Exemptions: exemption(mustDigest(t, single), 1, ExemptNotScanned)}, &verification); got != allow(Exempted, ReasonNotScanned) {
		t.Fatalf("%#v", got)
	}
	if got := Decide(Policy{Mode: Enforce, Exemptions: exemption(single.SHA256, 1, ExemptNotScanned)}, &verification); got.Allowed {
		t.Fatalf("raw file digest must not match the domain-separated artifact digest: %#v", got)
	}
}

func TestDenyListRejectsValidEvidence(t *testing.T) {
	inventory := fixtureInventory(t)
	verification := fixtureVerifier(t, 1).Verify(bytes.NewReader(readFixture(t, "passed.json")), bytes.NewReader(readFixture(t, "passed.sig.json")), inventory)
	for _, mode := range []Mode{Advisory, Enforce} {
		got := Decide(Policy{Mode: mode, Deny: []string{fixtureDigest("x"), mustDigest(t, inventory)}}, &verification)
		if got != deny(IntegrityFailure, ReasonArtifactDenied) {
			t.Fatalf("%s: %#v", mode, got)
		}
	}
}

func TestMissingAttestationIsUnverifiedNeverPassed(t *testing.T) {
	exempt := exemption(mustDigest(t, fixtureInventory(t)), 1, ExemptNotScanned)
	if got := Decide(Policy{Mode: Advisory, Exemptions: exempt}, nil); got != allow(NotVerified, ReasonMissing) {
		t.Fatalf("advisory: %#v", got)
	}
	for _, mode := range []Mode{Enforce, ""} {
		if got := Decide(Policy{Mode: mode, Exemptions: exempt}, nil); got != deny(NotVerified, ReasonMissing) {
			t.Fatalf("%q: %#v", mode, got)
		}
	}
}

func TestDecideFailsClosedOnUnsealedOrUnknownVerification(t *testing.T) {
	envelope := fixtureEnvelopes()["passed"]
	for name, tc := range map[string]struct {
		verification Verification
		want         Decision
	}{
		"zero":                {Verification{}, deny(IntegrityFailure, ReasonUnsealed)},
		"unsealed valid":      {Verification{result: Valid, envelope: envelope}, deny(IntegrityFailure, ReasonUnsealed)},
		"unknown result":      {Verification{result: "trusted", envelope: envelope, sealed: true}, deny(IntegrityFailure, ReasonUnknownResult)},
		"zero result":         {Verification{envelope: envelope, sealed: true}, deny(IntegrityFailure, ReasonUnknownResult)},
		"invalid artifact":    {Verification{result: Valid, sealed: true}, deny(IntegrityFailure, ReasonMalformed)},
		"unknown scan status": {Verification{result: Valid, envelope: Envelope{Artifact: envelope.Artifact, Scan: Scan{Status: "skipped"}}, sealed: true}, deny(IntegrityFailure, ReasonMalformed)},
	} {
		if got := Decide(Policy{Mode: Advisory}, &tc.verification); got != tc.want {
			t.Fatalf("%s: %#v", name, got)
		}
	}
}

func TestDecideAllUsesMostSevereDecision(t *testing.T) {
	inventory := fixtureInventory(t)
	verify := func(name string) *Verification {
		v := fixtureVerifier(t, 1).Verify(bytes.NewReader(readFixture(t, name+".json")), bytes.NewReader(readFixture(t, name+".sig.json")), inventory)
		return &v
	}
	passed, findings, notScanned := verify("passed"), verify("findings"), verify("not-scanned")
	broken := &Verification{result: InvalidSignature, sealed: true}
	exempt := Policy{Mode: Enforce, Exemptions: exemption(mustDigest(t, inventory), 1, ExemptNotScanned)}
	for name, tc := range map[string]struct {
		policy Policy
		set    []*Verification
		want   Decision
	}{
		"empty advisory":                {Policy{Mode: Advisory}, nil, allow(NotVerified, ReasonMissing)},
		"empty enforce":                 {Policy{Mode: Enforce}, []*Verification{}, deny(NotVerified, ReasonMissing)},
		"all verified":                  {Policy{Mode: Enforce}, []*Verification{passed, passed}, allow(Verified, ReasonPassed)},
		"exempted over verified":        {exempt, []*Verification{passed, notScanned}, allow(Exempted, ReasonNotScanned)},
		"not verified over exempted":    {exempt, []*Verification{notScanned, passed, nil}, deny(NotVerified, ReasonMissing)},
		"advisory missing member":       {Policy{Mode: Advisory}, []*Verification{passed, nil}, allow(NotVerified, ReasonMissing)},
		"findings over not verified":    {Policy{Mode: Advisory}, []*Verification{nil, findings}, deny(FindingsDetected, ReasonFindings)},
		"integrity failure over others": {Policy{Mode: Advisory}, []*Verification{findings, broken, passed}, deny(IntegrityFailure, ReasonInvalidSignature)},
	} {
		if got := DecideAll(tc.policy, tc.set); got != tc.want {
			t.Fatalf("%s: %#v, want %#v", name, got, tc.want)
		}
	}
}

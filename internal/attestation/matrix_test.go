package attestation

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
)

func fixtureVerifier(t *testing.T, keys ...int) *Verifier {
	t.Helper()
	return fixtureVerifierWithPolicies(t, []int{1}, keys...)
}

func trustedKeys(keys ...int) []TrustedKey {
	trusted := []TrustedKey{}
	for _, key := range keys {
		trusted = append(trusted, TrustedKey{KeyID: KeyID(testKey(key).Public().(ed25519.PublicKey)), PublicKey: testPublicKey(key)})
	}
	return trusted
}

// fixtureVerifiers caches one verifier per trust set, as a gate holds one
// verifier until its trust configuration changes.
var fixtureVerifiers = map[string]*Verifier{}

func fixtureVerifierWithPolicies(t *testing.T, policies []int, keys ...int) *Verifier {
	t.Helper()
	id := fmt.Sprint(policies, keys)
	if v := fixtureVerifiers[id]; v != nil {
		return v
	}
	v, err := newVerifier(TrustConfig{Version: TrustConfigVersion, Keys: trustedKeys(keys...), PolicyVersions: policies}, true)
	if err != nil {
		t.Fatal(err)
	}
	fixtureVerifiers[id] = v
	return v
}

// decideTest decides with the verifier that sealed the verification, so
// these tests cover policy rules. TestStaleVerification covers the binding.
func decideTest(policy Policy, onDisk Artifact, verification *Verification) Decision {
	verifier := fixtureVerifiers[fmt.Sprint([]int{1, 2}, []int{1})]
	if verification != nil && verification.verifier != nil {
		verifier = verification.verifier
	}
	return Decide(policy, verifier, onDisk, verification)
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
		{"unsupported envelope version with unknown field", replaced(t, replaced(t, passed, `"version": 1`, `"version": 2`), "{", `{"future":true,`), passedSig, []int{1}, inventory, UnsupportedVersion},
		{"envelope version not an integer", replaced(t, passed, `"version": 1`, `"version": 2.0`), passedSig, []int{1}, inventory, Malformed},
		{"unsupported signature version", passed, bytes.Replace(passedSig, []byte(`"version": 1`), []byte(`"version": 3`), 1), []int{1}, inventory, UnsupportedVersion},
		{"unsupported signature version with unknown field", passed, bytes.Replace(bytes.Replace(passedSig, []byte(`"version": 1`), []byte(`"version": 3`), 1), []byte("{"), []byte(`{"future":true,`), 1), []int{1}, inventory, UnsupportedVersion},
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
	report := fixtureDigest("scanner report")
	withKeyID := map[Result]bool{UnknownKey: true, InvalidSignature: true, IncompatiblePolicy: true, ArtifactMismatch: true}
	withoutKeyID := map[string]bool{"inventory with both forms": true, "empty inventory": true, "truncated signature": true, "malformed signature before unknown key": true, "signature unknown field": true}
	for _, tc := range verificationCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			got := fixtureVerifier(t, tc.keys...).Verify(strings.NewReader(tc.envelope), bytes.NewReader(tc.signature), tc.onDisk)
			if got.Result() != tc.want {
				t.Fatalf("result %q, want %q: %v", got.Result(), tc.want, got.Err())
			}
			if (got.Result() == Valid) != (got.Err() == nil) || (got.Result() == Valid) != (got.Envelope().Version == FormatVersion) || (got.Result() == Valid) != (got.ArtifactDigest() != "") {
				t.Fatalf("valid result must carry only an envelope and digest: %#v", got)
			}
			if withKeyID[tc.want] && got.KeyID() == "" {
				t.Fatalf("%q result lost the parsed key ID", tc.want)
			}
			if withoutKeyID[tc.name] && got.KeyID() != "" {
				t.Fatalf("key ID %q reported before the signature file parsed", got.KeyID())
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
				e := Exemption{Artifact: mustDigest(t, inventory), PolicyVersion: 1, Kind: kind, Reason: "fixture"}
				if kind == ExemptFindings || kind == ExemptPartialCoverage {
					e.ReportSHA256 = report
				}
				exemptions = append(exemptions, e)
			}
			for _, mode := range []Mode{Advisory, Enforce} {
				want.Allowed = mode == Advisory && want.Status == NotVerified
				if decision := decideTest(Policy{Version: 1, Mode: mode, Exemptions: exemptions}, tc.onDisk, &got); decision != want {
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
	e := Exemption{Artifact: artifact, PolicyVersion: policyVersion, Kind: kind, Reason: "operator accepted fixture risk"}
	if kind == ExemptFindings || kind == ExemptPartialCoverage {
		e.ReportSHA256 = fixtureDigest("scanner report")
	}
	return []Exemption{e}
}

func withReport(exemptions []Exemption, report string) []Exemption {
	exemptions[0].ReportSHA256 = report
	return exemptions
}

func policy(mode Mode, exemptions []Exemption, deny ...string) Policy {
	return Policy{Version: PolicyFileVersion, Mode: mode, Exemptions: exemptions, Deny: deny}
}

func verifyFixture(t *testing.T, name string, onDisk Artifact) *Verification {
	t.Helper()
	v := fixtureVerifierWithPolicies(t, []int{1, 2}, 1).Verify(bytes.NewReader(readFixture(t, name+".json")), bytes.NewReader(readFixture(t, name+".sig.json")), onDisk)
	if v.Result() != Valid {
		t.Fatalf("%s: %q %v", name, v.Result(), v.Err())
	}
	return &v
}

func TestDecisionFixtureMatrix(t *testing.T) {
	inventory := fixtureInventory(t)
	single := Artifact{SHA256: fixtureDigest("model.gguf")}
	digest := mustDigest(t, inventory)
	other := fixtureDigest("other artifact")
	newReport := fixtureDigest("new scanner report")
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
		{"findings", inventory, withReport(exemption(digest, 1, ExemptFindings), newReport), deny(FindingsDetected, ReasonFindings), deny(FindingsDetected, ReasonFindings)},
		{"findings", inventory, exemption(digest, 1, ExemptNotScanned), deny(FindingsDetected, ReasonFindings), deny(FindingsDetected, ReasonFindings)},
		{"findings", inventory, exemption(other, 1, ExemptFindings), deny(FindingsDetected, ReasonFindings), deny(FindingsDetected, ReasonFindings)},
		{"error", inventory, nil, allow(NotVerified, ReasonScanError), deny(NotVerified, ReasonScanError)},
		{"error", inventory, exemption(digest, 1, ExemptNotScanned), allow(NotVerified, ReasonScanError), deny(NotVerified, ReasonScanError)},
		{"error", inventory, exemption(digest, 1, ExemptUnsupported), allow(NotVerified, ReasonScanError), deny(NotVerified, ReasonScanError)},
		{"error", inventory, exemption(digest, 1, ExemptFindings), allow(NotVerified, ReasonScanError), deny(NotVerified, ReasonScanError)},
		{"error", inventory, exemption(digest, 1, ExemptPartialCoverage), allow(NotVerified, ReasonScanError), deny(NotVerified, ReasonScanError)},
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
		{"partial", inventory, withReport(exemption(digest, 1, ExemptPartialCoverage), newReport), allow(NotVerified, ReasonPartialCoverage), deny(NotVerified, ReasonPartialCoverage)},
		{"partial", inventory, exemption(other, 1, ExemptPartialCoverage), allow(NotVerified, ReasonPartialCoverage), deny(NotVerified, ReasonPartialCoverage)},
		{"partial", inventory, exemption(digest, 2, ExemptPartialCoverage), allow(NotVerified, ReasonPartialCoverage), deny(NotVerified, ReasonPartialCoverage)},
	} {
		verification := verifyFixture(t, tc.fixture, tc.onDisk)
		for mode, want := range map[Mode]Decision{Advisory: tc.advisory, Enforce: tc.enforce, "": deny(IntegrityFailure, ReasonInvalidPolicy), "strict": deny(IntegrityFailure, ReasonInvalidPolicy), "Advisory": deny(IntegrityFailure, ReasonInvalidPolicy)} {
			if got := decideTest(policy(mode, tc.exemptions), tc.onDisk, verification); got != want {
				t.Fatalf("%s %q %v: %#v, want %#v", tc.fixture, mode, tc.exemptions, got, want)
			}
		}
	}
}

func TestSingleArtifactExemptionUsesItsOwnDigest(t *testing.T) {
	single := Artifact{SHA256: fixtureDigest("model.gguf")}
	e := fixtureEnvelopes()["single"]
	e.Scan = Scan{Status: ScanNotScanned}
	verification := Verification{result: Valid, envelope: e, digest: mustDigest(t, e.Artifact), verifier: fixtureVerifierWithPolicies(t, []int{1, 2}, 1)}
	if got := decideTest(policy(Enforce, exemption(mustDigest(t, single), 1, ExemptNotScanned)), single, &verification); got != allow(Exempted, ReasonNotScanned) {
		t.Fatalf("%#v", got)
	}
	if got := decideTest(policy(Enforce, exemption(single.SHA256, 1, ExemptNotScanned)), single, &verification); got.Allowed {
		t.Fatalf("raw file digest must not match the domain-separated artifact digest: %#v", got)
	}
}

// Denial follows the gate's own inventory, so changing or removing the
// evidence never admits a denied artifact.
func TestDenialHoldsForEveryEvidenceState(t *testing.T) {
	inventory := fixtureInventory(t)
	passed := string(readFixture(t, "passed.json"))
	passedSig := readFixture(t, "passed.sig.json")
	v := fixtureVerifier(t, 1)
	states := map[string]*Verification{"missing": nil}
	for name, tc := range map[string]struct {
		envelope  io.Reader
		signature []byte
	}{
		"valid":               {strings.NewReader(passed), passedSig},
		"unreadable":          {failingReader{}, passedSig},
		"unsupported-version": {strings.NewReader(replaced(t, passed, `"version": 1`, `"version": 2`)), passedSig},
		"forged":              {strings.NewReader(passed), forgedSignature(t, passedSig)},
	} {
		got := v.Verify(tc.envelope, bytes.NewReader(tc.signature), inventory)
		states[name] = &got
	}
	for name, verification := range states {
		for _, mode := range []Mode{Advisory, Enforce} {
			got := decideTest(policy(mode, nil, fixtureDigest("x"), mustDigest(t, inventory)), inventory, verification)
			if got != deny(IntegrityFailure, ReasonArtifactDenied) {
				t.Fatalf("%s %s: %#v", name, mode, got)
			}
		}
	}
}

func TestDenialMatchesFileDigestsInRearrangedCopies(t *testing.T) {
	inventory := fixtureInventory(t)
	weights := inventory.Files["model-00001-of-00002.safetensors"]
	repackaged := withFile(inventory.Files, "README.md", fixtureDigest("harmless extra file"))
	renamed := withFile(withFile(inventory.Files, "model-00001-of-00002.safetensors", "").Files, "renamed/weights.bin", weights)
	for name, onDisk := range map[string]Artifact{
		"extra harmless file": repackaged,
		"renamed weights":     renamed,
		"single weights file": {SHA256: weights},
		"unsupported path":    {Files: map[string]string{`bad"path`: weights}},
	} {
		for _, mode := range []Mode{Advisory, Enforce} {
			if got := decideTest(policy(mode, nil, weights), onDisk, nil); got != deny(IntegrityFailure, ReasonArtifactDenied) {
				t.Fatalf("%s %s: %#v", name, mode, got)
			}
		}
	}
	if got := decideTest(policy(Advisory, nil, mustDigest(t, inventory)), repackaged, nil); got != allow(NotVerified, ReasonMissing) {
		t.Fatalf("an inventory digest denial covers only those exact bytes: %#v", got)
	}
}

func TestDenialMatchesSingleFormDigestOfAFile(t *testing.T) {
	inventory := fixtureInventory(t)
	weights := inventory.Files["model-00001-of-00002.safetensors"]
	shown := mustDigest(t, Artifact{SHA256: weights})
	if got := decideTest(policy(Advisory, nil, shown), inventory, nil); got != deny(IntegrityFailure, ReasonArtifactDenied) {
		t.Fatalf("directory copy of a denied single-file model: %#v", got)
	}
	if got := decideTest(policy(Advisory, nil, shown), Artifact{SHA256: weights}, nil); got != deny(IntegrityFailure, ReasonArtifactDenied) {
		t.Fatalf("denied single-file model: %#v", got)
	}
}

func TestDefaultPolicyIsValidAdvisory(t *testing.T) {
	p := DefaultPolicy()
	if err := p.Validate(); err != nil || p.Mode != Advisory || p.Version != PolicyFileVersion || p.Exemptions != nil || p.Deny != nil {
		t.Fatalf("%#v %v", p, err)
	}
	if got := decideTest(p, fixtureInventory(t), nil); got != allow(NotVerified, ReasonMissing) {
		t.Fatalf("%#v", got)
	}
}

// A verification sealed under old trust must not outlive a trust reload,
// even after the reload removed its key or policy version.
func TestStaleVerification(t *testing.T) {
	inventory := fixtureInventory(t)
	trust := func(keys []int, policies []int) *Verifier {
		v, err := newVerifier(trustConfig(policies, trustedKeys(keys...)...), true)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	verifierA := trust([]int{1, 2}, []int{1, 2})
	verify := func(name string) *Verification {
		v := verifierA.Verify(bytes.NewReader(readFixture(t, name+".json")), bytes.NewReader(readFixture(t, name+".sig.json")), inventory)
		return &v
	}
	passed, findings := verify("passed"), verify("findings")
	forged := verifierA.Verify(bytes.NewReader(readFixture(t, "passed.json")), bytes.NewReader(forgedSignature(t, readFixture(t, "passed.sig.json"))), inventory)
	// fresh returns evidence that verifierB itself seals as valid.
	fresh := func(t *testing.T, verifierB *Verifier) *Verification {
		t.Helper()
		for _, pair := range [][2]string{{"passed.json", "passed.key-2.sig.json"}, {"policy-2.json", "policy-2.sig.json"}, {"passed.json", "passed.sig.json"}} {
			v := verifierB.Verify(bytes.NewReader(readFixture(t, pair[0])), bytes.NewReader(readFixture(t, pair[1])), inventory)
			if v.Result() == Valid {
				return &v
			}
		}
		t.Fatal("no fixture verifies under verifierB")
		return nil
	}
	for name, verifierB := range map[string]*Verifier{
		"key removed":            trust([]int{2}, []int{1, 2}),
		"policy version removed": trust([]int{1, 2}, []int{2}),
		"same trust rebuilt":     trust([]int{1, 2}, []int{1, 2}),
	} {
		current := fresh(t, verifierB)
		for _, mode := range []Mode{Advisory, Enforce} {
			// A stale non-first root must not borrow its own sealing verifier.
			roots := []Root{{Artifact: inventory, Verification: current}, {Artifact: inventory, Verification: passed}}
			if got := DecideAll(policy(mode, nil), verifierB, roots); got != deny(IntegrityFailure, ReasonStale) {
				t.Fatalf("%s %s stale second root: %#v", name, mode, got)
			}
			if got := DecideAll(policy(mode, nil), verifierB, roots[:1]); got != allow(Verified, ReasonPassed) {
				t.Fatalf("%s %s fresh root: %#v", name, mode, got)
			}
			for _, v := range []*Verification{passed, findings, &forged} {
				if got := Decide(policy(mode, nil), verifierB, inventory, v); got != deny(IntegrityFailure, ReasonStale) {
					t.Fatalf("%s %s: %#v", name, mode, got)
				}
			}
			if got := DecideAll(policy(mode, nil), verifierB, []Root{{Artifact: inventory, Verification: passed}}); got != deny(IntegrityFailure, ReasonStale) {
				t.Fatalf("%s %s all: %#v", name, mode, got)
			}
			// Missing evidence needs no verifier, and denial still comes first.
			if got := Decide(policy(mode, nil), verifierB, inventory, nil); got != missingDecision(policy(mode, nil), ReasonMissing) {
				t.Fatalf("%s %s missing: %#v", name, mode, got)
			}
			if got := Decide(policy(mode, nil, mustDigest(t, inventory)), verifierB, inventory, passed); got != deny(IntegrityFailure, ReasonArtifactDenied) {
				t.Fatalf("%s %s denied: %#v", name, mode, got)
			}
		}
	}
	for mode, want := range map[Mode]Decision{Advisory: allow(Verified, ReasonPassed), Enforce: allow(Verified, ReasonPassed)} {
		if got := Decide(policy(mode, nil), verifierA, inventory, passed); got != want {
			t.Fatalf("same verifier %s: %#v", mode, got)
		}
		if got := Decide(policy(mode, nil), verifierA, inventory, findings); got != deny(FindingsDetected, ReasonFindings) {
			t.Fatalf("same verifier findings %s: %#v", mode, got)
		}
		if got := Decide(policy(mode, nil), verifierA, inventory, &forged); got != deny(IntegrityFailure, ReasonInvalidSignature) {
			t.Fatalf("same verifier forged %s: %#v", mode, got)
		}
	}
}

// Evidence needs a loaded verifier; an unsealed value never reaches the
// verifier check. Missing evidence keeps the missing path for an install
// without a trust configuration.
func TestSealAndVerifierChecks(t *testing.T) {
	inventory := fixtureInventory(t)
	verifier := fixtureVerifierWithPolicies(t, []int{1, 2}, 1)
	passed := verifyFixture(t, "passed", inventory)
	var nilVerifier *Verifier
	nilReceiver := nilVerifier.Verify(bytes.NewReader(readFixture(t, "passed.json")), bytes.NewReader(readFixture(t, "passed.sig.json")), inventory)
	for _, mode := range []Mode{Advisory, Enforce} {
		p := policy(mode, nil)
		for name, tc := range map[string]struct {
			verifier     *Verifier
			verification *Verification
			want         Decision
		}{
			"zero verification":          {verifier, &Verification{}, deny(IntegrityFailure, ReasonUnsealed)},
			"nil receiver result":        {verifier, &nilReceiver, deny(IntegrityFailure, ReasonUnsealed)},
			"zero verification, nil arg": {nil, &Verification{}, deny(IntegrityFailure, ReasonUnsealed)},
			"nil verifier argument":      {nil, passed, deny(IntegrityFailure, ReasonMissingVerifier)},
			"nil verifier, no evidence":  {nil, nil, missingDecision(p, ReasonMissing)},
			"same verifier":              {verifier, passed, allow(Verified, ReasonPassed)},
		} {
			if got := Decide(p, tc.verifier, inventory, tc.verification); got != tc.want {
				t.Fatalf("%s %s: %#v, want %#v", name, mode, got, tc.want)
			}
		}
		if got := DecideAll(p, nil, []Root{{Artifact: inventory, Verification: passed}}); got != deny(IntegrityFailure, ReasonMissingVerifier) {
			t.Fatalf("DecideAll nil verifier %s: %#v", mode, got)
		}
	}
}

func TestInvalidPolicyFailsClosed(t *testing.T) {
	inventory := fixtureInventory(t)
	verification := verifyFixture(t, "passed", inventory)
	for name, p := range map[string]Policy{
		"zero":                           {},
		"no version":                     {Mode: Advisory},
		"bad denial":                     policy(Advisory, nil, "abc"),
		"bad exemption":                  policy(Advisory, []Exemption{{Kind: ExemptNotScanned}}),
		"padded mode":                    policy("advisory ", nil),
		"version 0 with matching denial": {Version: 0, Mode: Advisory, Deny: []string{mustDigest(t, inventory)}},
	} {
		for _, v := range []*Verification{nil, verification} {
			if got := decideTest(p, inventory, v); got != deny(IntegrityFailure, ReasonInvalidPolicy) {
				t.Fatalf("%s: %#v", name, got)
			}
		}
	}
}

func TestDecideUsesTheGateInventory(t *testing.T) {
	inventory := fixtureInventory(t)
	verification := verifyFixture(t, "passed", inventory)
	other := withFile(inventory.Files, "config.json", fixtureDigest("changed"))
	if got := decideTest(policy(Advisory, nil), other, verification); got != deny(IntegrityFailure, ReasonArtifactMismatch) {
		t.Fatalf("evidence for other bytes: %#v", got)
	}
	if got := decideTest(policy(Advisory, nil), Artifact{}, verification); got != deny(IntegrityFailure, ReasonInvalidInventory) {
		t.Fatalf("invalid inventory with evidence: %#v", got)
	}
	// An unsupported path has no digest, so no denial can name it. Without
	// evidence the root is missing, not tampered.
	unsupportedPath := Artifact{Files: map[string]string{`model"name.bin`: fixtureDigest("x")}}
	if got := decideTest(policy(Advisory, nil), unsupportedPath, nil); got != allow(NotVerified, ReasonInvalidInventory) {
		t.Fatalf("advisory unsupported path: %#v", got)
	}
	if got := decideTest(policy(Enforce, nil), unsupportedPath, nil); got != deny(NotVerified, ReasonInvalidInventory) {
		t.Fatalf("enforce unsupported path: %#v", got)
	}
}

func TestEnvelopeCopyCannotChangeTheDecision(t *testing.T) {
	inventory := fixtureInventory(t)
	verification := verifyFixture(t, "passed", inventory)
	saved := verification.Envelope()
	copied := verification.Envelope()
	for path := range copied.Artifact.Files {
		copied.Artifact.Files[path] = fixtureDigest("changed")
	}
	copied.Scan.Scanner.Tool = "changed"
	copied.Source.URI = "changed"
	if !reflect.DeepEqual(verification.Envelope(), saved) {
		t.Fatal("Envelope must return a copy")
	}
	if got := decideTest(policy(Enforce, nil, mustDigest(t, inventory)), inventory, verification); got != deny(IntegrityFailure, ReasonArtifactDenied) {
		t.Fatalf("a changed copy must not bypass the denial: %#v", got)
	}
}

func TestMissingAttestationIsUnverifiedNeverPassed(t *testing.T) {
	inventory := fixtureInventory(t)
	exempt := exemption(mustDigest(t, inventory), 1, ExemptNotScanned)
	if got := decideTest(policy(Advisory, exempt), inventory, nil); got != allow(NotVerified, ReasonMissing) {
		t.Fatalf("advisory: %#v", got)
	}
	if got := decideTest(policy(Enforce, exempt), inventory, nil); got != deny(NotVerified, ReasonMissing) {
		t.Fatalf("enforce: %#v", got)
	}
}

func TestDecideFailsClosedOnImpossibleVerification(t *testing.T) {
	inventory := fixtureInventory(t)
	digest := mustDigest(t, inventory)
	envelope := fixtureEnvelopes()["passed"]
	unknownStatus := envelope
	unknownStatus.Scan = Scan{Status: "skipped"}
	v := fixtureVerifierWithPolicies(t, []int{1, 2}, 1)
	for name, verification := range map[string]Verification{
		"zero":                {},
		"unsealed valid":      {result: Valid, envelope: envelope, digest: digest},
		"unsealed failure":    {result: InvalidSignature},
		"unknown result":      {result: "trusted", envelope: envelope, digest: digest, verifier: v},
		"zero result":         {envelope: envelope, digest: digest, verifier: v},
		"no sealed digest":    {result: Valid, envelope: envelope, verifier: v},
		"unknown scan status": {result: Valid, envelope: unknownStatus, digest: digest, verifier: v},
	} {
		if got := decideTest(policy(Advisory, nil), inventory, &verification); got != deny(IntegrityFailure, ReasonUnsealed) {
			t.Fatalf("%s: %#v", name, got)
		}
	}
}

func TestDecideAllUsesFirstMostSevereDecision(t *testing.T) {
	inventory := fixtureInventory(t)
	root := func(name string) Root {
		return Root{Artifact: inventory, Verification: verifyFixture(t, name, inventory)}
	}
	passed, findings, notScanned := root("passed"), root("findings"), root("not-scanned")
	missing := Root{Artifact: inventory}
	single := Artifact{SHA256: fixtureDigest("model.gguf")}
	broken := Root{Artifact: inventory, Verification: &Verification{result: InvalidSignature, verifier: fixtureVerifierWithPolicies(t, []int{1, 2}, 1)}}
	exempt := policy(Enforce, exemption(mustDigest(t, inventory), 1, ExemptNotScanned))
	for name, tc := range map[string]struct {
		policy Policy
		roots  []Root
		want   Decision
	}{
		"empty advisory":         {policy(Advisory, nil), nil, allow(NotVerified, ReasonMissing)},
		"empty enforce":          {policy(Enforce, nil), []Root{}, deny(NotVerified, ReasonMissing)},
		"empty empty mode":       {policy("", nil), nil, deny(IntegrityFailure, ReasonInvalidPolicy)},
		"empty unknown mode":     {policy("strict", nil), nil, deny(IntegrityFailure, ReasonInvalidPolicy)},
		"empty invalid advisory": {policy(Advisory, nil, "abc"), nil, deny(IntegrityFailure, ReasonInvalidPolicy)},
		"empty zero policy":      {Policy{}, []Root{}, deny(IntegrityFailure, ReasonInvalidPolicy)},
		"empty default policy":   {DefaultPolicy(), nil, allow(NotVerified, ReasonMissing)},

		"invalid policy with roots":          {policy(Advisory, nil, "abc"), []Root{passed}, deny(IntegrityFailure, ReasonInvalidPolicy)},
		"invalid inventory without evidence": {policy(Advisory, nil), []Root{passed, {Artifact: Artifact{}}}, allow(NotVerified, ReasonInvalidInventory)},
		"invalid inventory with evidence":    {policy(Advisory, nil), []Root{{Artifact: Artifact{}, Verification: passed.Verification}}, deny(IntegrityFailure, ReasonInvalidInventory)},
		"denied single root, advisory":       {policy(Advisory, nil, mustDigest(t, single)), []Root{passed, {Artifact: single}}, deny(IntegrityFailure, ReasonArtifactDenied)},
		"two different inventories":          {policy(Enforce, nil), []Root{passed, {Artifact: single, Verification: verifyFixture(t, "single", single)}}, allow(Verified, ReasonPassed)},
		"all verified":                       {policy(Enforce, nil), []Root{passed, passed}, allow(Verified, ReasonPassed)},
		"only first member rejected":         {policy(Enforce, nil), []Root{findings, passed}, deny(FindingsDetected, ReasonFindings)},
		"missing first member in enforce":    {policy(Enforce, nil), []Root{missing, passed}, deny(NotVerified, ReasonMissing)},
		"exempted over verified":             {exempt, []Root{passed, notScanned}, allow(Exempted, ReasonNotScanned)},
		"not verified over exempted":         {exempt, []Root{notScanned, passed, missing}, deny(NotVerified, ReasonMissing)},
		"first most severe wins a tie":       {policy(Advisory, nil), []Root{notScanned, missing}, allow(NotVerified, ReasonNotScanned)},
		"findings over not verified":         {policy(Advisory, nil), []Root{missing, findings}, deny(FindingsDetected, ReasonFindings)},
		"integrity failure over others":      {policy(Advisory, nil), []Root{findings, broken, passed}, deny(IntegrityFailure, ReasonInvalidSignature)},
	} {
		if got := DecideAll(tc.policy, fixtureVerifierWithPolicies(t, []int{1, 2}, 1), tc.roots); got != tc.want {
			t.Fatalf("%s: %#v, want %#v", name, got, tc.want)
		}
	}
}

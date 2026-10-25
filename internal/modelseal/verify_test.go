package modelseal

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/attestation"
)

// signedCase is one sealed and signed not-scanned model.
type signedCase struct {
	w                          workspace
	key                        signer
	signature, trust, envelope string
}

func newSignedCase(t *testing.T) signedCase {
	t.Helper()
	w := newWorkspace(t)
	if _, err := sealWith(t, w, ""); err != nil {
		t.Fatal(err)
	}
	c := signedCase{w: w, key: newSigner(t), signature: filepath.Join(w.dir, "signature.json"), trust: filepath.Join(w.dir, "trust.json"), envelope: filepath.Join(w.output, EnvelopeFile)}
	c.key.sign(t, w.output, c.signature)
	c.key.trust(t, c.trust, 1)
	return c
}

func (c signedCase) request() VerifyRequest {
	return VerifyRequest{Target: Target{Root: c.w.model}, Envelope: c.envelope, Signature: c.signature, Trust: c.trust}
}

// direct runs the shared verifier itself on the same inputs, so the CLI path
// is shown to be the attestation.Verify path.
func (c signedCase) direct(t *testing.T) attestation.Result {
	t.Helper()
	raw, err := os.ReadFile(c.trust)
	if err != nil {
		t.Fatal(err)
	}
	config, err := attestation.ReadTrustConfig(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := attestation.NewVerifier(config)
	if err != nil {
		t.Fatal(err)
	}
	taken, err := Target{Root: c.w.model}.take()
	if err != nil {
		t.Fatal(err)
	}
	envelope, signature := evidence(c.envelope), evidence(c.signature)
	defer envelope.Close()
	defer signature.Close()
	return verifier.Verify(envelope, signature, taken.artifact).Result()
}

func replace(t *testing.T, path, content string) {
	t.Helper()
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	writeFile(t, path, content, 0o600)
}

func TestVerifyMatchesTheSharedVerificationMatrix(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(t *testing.T, c signedCase)
		want   attestation.Result
	}{
		"valid": {func(*testing.T, signedCase) {}, attestation.Valid},
		"tampered weights after the seal": {func(t *testing.T, c signedCase) {
			replace(t, filepath.Join(c.w.model, "model-00001-of-00002.safetensors"), "changed")
		}, attestation.ArtifactMismatch},
		"added file after the seal": {func(t *testing.T, c signedCase) {
			writeFile(t, filepath.Join(c.w.model, "added.json"), "{}", 0o600)
		}, attestation.ArtifactMismatch},
		"removed file after the seal": {func(t *testing.T, c signedCase) {
			if err := os.Remove(filepath.Join(c.w.model, "tokenizer.json")); err != nil {
				t.Fatal(err)
			}
		}, attestation.ArtifactMismatch},
		"unknown key": {func(t *testing.T, c signedCase) {
			newSigner(t).sign(t, c.w.output, c.signature)
		}, attestation.UnknownKey},
		"signature by another key under the pinned key ID": {func(t *testing.T, c signedCase) {
			other := newSigner(t)
			other.public = c.key.public
			other.sign(t, c.w.output, c.signature)
		}, attestation.InvalidSignature},
		"tampered envelope": {func(t *testing.T, c signedCase) {
			raw, _ := os.ReadFile(c.envelope)
			replace(t, c.envelope, strings.Replace(string(raw), `"policyVersion":1`, `"policyVersion":2`, 1))
		}, attestation.InvalidSignature},
		"incompatible policy version": {func(t *testing.T, c signedCase) {
			replace(t, c.trust, "")
			c.key.trust(t, c.trust, 2)
		}, attestation.IncompatiblePolicy},
		"unknown envelope field": {func(t *testing.T, c signedCase) {
			raw, _ := os.ReadFile(c.envelope)
			replace(t, c.envelope, strings.Replace(string(raw), "{", `{"extra":1,`, 1))
		}, attestation.Malformed},
		"duplicate envelope key": {func(t *testing.T, c signedCase) {
			raw, _ := os.ReadFile(c.envelope)
			replace(t, c.envelope, strings.Replace(string(raw), "{", `{"version":1,`, 1))
		}, attestation.Malformed},
		"oversized envelope": {func(t *testing.T, c signedCase) {
			replace(t, c.envelope, strings.Repeat(" ", attestation.MaxEnvelopeBytes+1))
		}, attestation.Malformed},
		"other envelope version": {func(t *testing.T, c signedCase) {
			replace(t, c.envelope, `{"version":2}`)
		}, attestation.UnsupportedVersion},
		"malformed signature": {func(t *testing.T, c signedCase) {
			replace(t, c.signature, `{"version":1,"algorithm":"rsa","keyId":"00","signature":""}`)
		}, attestation.Malformed},
		"missing signature": {func(t *testing.T, c signedCase) {
			if err := os.Remove(c.signature); err != nil {
				t.Fatal(err)
			}
		}, attestation.Unreadable},
		"missing envelope": {func(t *testing.T, c signedCase) {
			if err := os.Remove(c.envelope); err != nil {
				t.Fatal(err)
			}
		}, attestation.Unreadable},
	} {
		t.Run(name, func(t *testing.T) {
			c := newSignedCase(t)
			tc.change(t, c)
			outcome, err := Verify(c.request())
			if err != nil {
				t.Fatal(err)
			}
			if outcome.Result != tc.want || c.direct(t) != tc.want {
				t.Fatalf("result = %s, want %s", outcome.Result, tc.want)
			}
			if outcome.Accepted() != (tc.want == attestation.Valid) {
				t.Fatal("only valid evidence is accepted without a policy")
			}
			if tc.want == attestation.Valid && (outcome.Scan != attestation.ScanNotScanned || outcome.KeyID != attestation.KeyID(c.key.public)) {
				t.Fatalf("valid outcome must report scan status and key ID: %+v", outcome)
			}
		})
	}
}

func TestVerifyRejectsUntrustedTrustConfiguration(t *testing.T) {
	for name, change := range map[string]func(t *testing.T, c signedCase){
		"missing": func(t *testing.T, c signedCase) {
			if err := os.Remove(c.trust); err != nil {
				t.Fatal(err)
			}
		},
		"writable by others": func(t *testing.T, c signedCase) {
			if err := os.Chmod(c.trust, 0o666); err != nil {
				t.Fatal(err)
			}
		},
		"unknown field": func(t *testing.T, c signedCase) {
			replace(t, c.trust, `{"version":1,"keys":[],"policyVersions":[1],"extra":true}`)
		},
		"no keys": func(t *testing.T, c signedCase) { replace(t, c.trust, `{"version":1,"keys":[],"policyVersions":[1]}`) },
		"small-order key": func(t *testing.T, c signedCase) {
			replace(t, c.trust, `{"version":1,"keys":[{"keyId":"66687aadf862bd776c8fc18b8e9f8e20089714856ee233b3902a591d0d5f2925","publicKey":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}],"policyVersions":[1]}`)
		},
		"model inventory link": func(t *testing.T, c signedCase) {
			if err := os.Symlink("config.json", filepath.Join(c.w.model, "linked.json")); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := newSignedCase(t)
			change(t, c)
			if outcome, err := Verify(c.request()); err == nil || outcome.Accepted() {
				t.Fatal("untrusted inputs were accepted")
			}
		})
	}
}

func TestVerifyAppliesPolicyToSkippedScans(t *testing.T) {
	c := newSignedCase(t)
	taken, err := Target{Root: c.w.model}.take()
	if err != nil {
		t.Fatal(err)
	}
	policy := filepath.Join(c.w.dir, "policy.json")
	for name, tc := range map[string]struct {
		policy  string
		allowed bool
		status  attestation.Status
		reason  attestation.Reason
	}{
		"advisory warns":           {`{"version":1,"mode":"advisory"}`, true, attestation.NotVerified, attestation.ReasonNotScanned},
		"enforce rejects":          {`{"version":1,"mode":"enforce"}`, false, attestation.NotVerified, attestation.ReasonNotScanned},
		"scoped exemption":         {`{"version":1,"mode":"enforce","exemptions":[{"artifact":"` + taken.digest + `","policyVersion":1,"kind":"not_scanned","reason":"format has no scanner"}]}`, true, attestation.Exempted, attestation.ReasonNotScanned},
		"other artifact exemption": {`{"version":1,"mode":"enforce","exemptions":[{"artifact":"` + strings.Repeat("0", 64) + `","policyVersion":1,"kind":"not_scanned","reason":"other"}]}`, false, attestation.NotVerified, attestation.ReasonNotScanned},
		"denied artifact":          {`{"version":1,"mode":"advisory","deny":["` + taken.digest + `"]}`, false, attestation.IntegrityFailure, attestation.ReasonArtifactDenied},
		"invalid policy":           {`{"version":1,"mode":"warn"}`, false, attestation.IntegrityFailure, attestation.ReasonInvalidPolicy},
		"missing policy":           {"", false, attestation.IntegrityFailure, attestation.ReasonInvalidPolicy},
	} {
		t.Run(name, func(t *testing.T) {
			replace(t, policy, tc.policy)
			if tc.policy == "" {
				if err := os.Remove(policy); err != nil {
					t.Fatal(err)
				}
			}
			request := c.request()
			request.Policy = policy
			outcome, err := Verify(request)
			if err != nil {
				t.Fatal(err)
			}
			d := outcome.Decision
			if d == nil || d.Allowed != tc.allowed || d.Status != tc.status || d.Reason != tc.reason || outcome.Accepted() != tc.allowed {
				t.Fatalf("decision = %+v", d)
			}
		})
	}
}

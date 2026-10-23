package attestation

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func trustConfigJSON(t *testing.T, keys ...int) string {
	t.Helper()
	raw, err := json.Marshal(trustConfig([]int{1}, trustedKeys(keys...)...))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestReadTrustConfigBuildsVerifier(t *testing.T) {
	config, err := ReadTrustConfig(strings.NewReader(trustConfigJSON(t, 3, 4)))
	if err != nil {
		t.Fatal(err)
	}
	v, err := NewVerifier(config)
	if err != nil || len(v.keys) != 2 || !v.policies[1] {
		t.Fatalf("%v %#v", err, v)
	}
}

// ReadTrustConfig and NewVerifier share one validation, so the reader
// rejects every configuration the verifier rejects.
func TestReadTrustConfigRejectsInvalidInput(t *testing.T) {
	valid := trustConfigJSON(t, 3)
	key3 := trustedKeys(3)[0]
	for name, body := range map[string]string{
		"unknown field":    strings.Replace(valid, "{", `{"allowAll":true,`, 1),
		"duplicate key":    strings.Replace(valid, "{", `{"version":1,`, 1),
		"cased field":      strings.Replace(valid, `"policyVersions"`, `"PolicyVersions"`, 1),
		"key ID mismatch":  strings.Replace(valid, key3.KeyID, trustedKeys(4)[0].KeyID, 1),
		"uppercase key ID": strings.Replace(valid, key3.KeyID, strings.ToUpper(key3.KeyID), 1),
		"bad public key":   strings.Replace(valid, key3.PublicKey, "!", 1),
		"null keys":        strings.Replace(valid, `"keys":[`+mustJSON(t, key3)+`]`, `"keys":null`, 1),
		"null versions":    strings.Replace(valid, `"policyVersions":[1]`, `"policyVersions":null`, 1),
		"duplicate pins":   strings.Replace(valid, mustJSON(t, key3), mustJSON(t, key3)+","+mustJSON(t, key3), 1),
		"test-only key":    trustConfigJSON(t, 1),
		"small-order key":  strings.Replace(strings.Replace(valid, key3.PublicKey, encodedKey(make([]byte, 32)), 1), key3.KeyID, KeyID(make([]byte, 32)), 1),
		"truncated":        valid[:len(valid)/2],
		"oversized":        valid + strings.Repeat(" ", MaxTrustConfigBytes),
	} {
		if body == valid {
			t.Fatalf("%s mutation did not apply", name)
		}
		_, err := ReadTrustConfig(strings.NewReader(body))
		if !errors.Is(err, ErrInvalidTrust) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	for _, body := range []string{strings.Replace(valid, `"version":1`, `"version":2`, 1), strings.Replace(valid, `"version":1`, `"version":2,"future":true`, 1)} {
		if _, err := ReadTrustConfig(strings.NewReader(body)); !errors.Is(err, ErrUnsupportedVersion) || !strings.Contains(err.Error(), "trust config") {
			t.Fatalf("other version: %v", err)
		}
	}
	if _, err := ReadTrustConfig(failingReader{}); !errors.Is(err, ErrUnreadable) {
		t.Fatalf("read failure: %v", err)
	}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestReadPolicy(t *testing.T) {
	digest := fixtureDigest("artifact")
	valid := mustJSON(t, policy(Enforce, exemption(digest, 1, ExemptFindings), fixtureDigest("denied")))
	got, err := ReadPolicy(strings.NewReader(valid))
	if err != nil || got.Mode != Enforce || len(got.Exemptions) != 1 || len(got.Deny) != 1 {
		t.Fatalf("%#v %v", got, err)
	}
	for name, body := range map[string]string{
		"unknown field":    strings.Replace(valid, "{", `{"allowAll":true,`, 1),
		"empty exemptions": mustJSON(t, map[string]any{"version": 1, "mode": "enforce", "exemptions": []any{}}),
		"implicit mode":    mustJSON(t, map[string]any{"version": 1}),
		"empty mode":       strings.Replace(valid, `"mode":"enforce"`, `"mode":""`, 1),
		"truncated":        valid[:len(valid)/2],
	} {
		if body == valid {
			t.Fatalf("%s mutation did not apply", name)
		}
		if _, err := ReadPolicy(strings.NewReader(body)); !errors.Is(err, ErrInvalidPolicy) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, err := ReadPolicy(strings.NewReader(strings.Replace(valid, `"version":1`, `"version":2`, 1))); !errors.Is(err, ErrUnsupportedVersion) || !strings.Contains(err.Error(), "policy") {
		t.Fatalf("other version: %v", err)
	}
	if _, err := ReadPolicy(strings.NewReader(strings.Replace(valid, `"version":1`, `"version":0`, 1))); !errors.Is(err, ErrInvalidPolicy) {
		t.Fatalf("version 0: %v", err)
	}
	atLimit := valid + strings.Repeat(" ", MaxPolicyBytes-len(valid))
	if _, err := ReadPolicy(strings.NewReader(atLimit)); err != nil {
		t.Fatalf("policy at the size limit: %v", err)
	}
	if _, err := ReadPolicy(strings.NewReader(atLimit + " ")); !errors.Is(err, ErrInvalidPolicy) {
		t.Fatalf("policy over the size limit: %v", err)
	}
	if _, err := ReadTrustConfig(strings.NewReader(strings.Replace(trustConfigJSON(t, 3), `"version":1`, `"version":0`, 1))); !errors.Is(err, ErrInvalidTrust) {
		t.Fatalf("trust version 0: %v", err)
	}
}

func TestPolicyValidate(t *testing.T) {
	digest := fixtureDigest("artifact")
	report := fixtureDigest("report")
	scoped := func(kind ExemptionKind, report, reason string) Exemption {
		return Exemption{Artifact: digest, PolicyVersion: 1, Kind: kind, ReportSHA256: report, Reason: reason}
	}
	for _, valid := range []Policy{
		policy(Advisory, nil),
		policy(Enforce, []Exemption{scoped(ExemptNotScanned, "", "r"), scoped(ExemptFindings, report, "r"), scoped(ExemptUnsupported, "", "r"), scoped(ExemptPartialCoverage, report, strings.Repeat("r", MaxTextBytes))}),
		policy(Enforce, []Exemption{scoped(ExemptFindings, report, "r"), scoped(ExemptFindings, fixtureDigest("new report"), "r")}),
		policy(Enforce, []Exemption{{Artifact: digest, PolicyVersion: 2147483647, Kind: ExemptNotScanned, Reason: "r"}}),
		policy(Enforce, nil, digest, fixtureDigest("other")),
	} {
		if err := valid.Validate(); err != nil {
			t.Fatalf("%#v: %v", valid, err)
		}
	}
	for name, invalid := range map[string]Policy{
		"empty mode":              policy("", nil),
		"unknown mode":            policy("strict", nil),
		"short digest":            policy(Enforce, []Exemption{{Artifact: "abc", PolicyVersion: 1, Kind: ExemptNotScanned, Reason: "r"}}),
		"zero policy":             policy(Enforce, []Exemption{{Artifact: digest, Kind: ExemptNotScanned, Reason: "r"}}),
		"policy over cap":         policy(Enforce, []Exemption{{Artifact: digest, PolicyVersion: 2147483648, Kind: ExemptNotScanned, Reason: "r"}}),
		"error kind":              policy(Enforce, []Exemption{scoped("error", "", "r")}),
		"hyphenated kind":         policy(Enforce, []Exemption{scoped("not-scanned", "", "r")}),
		"findings without report": policy(Enforce, []Exemption{scoped(ExemptFindings, "", "r")}),
		"partial without report":  policy(Enforce, []Exemption{scoped(ExemptPartialCoverage, "", "r")}),
		"unscanned with report":   policy(Enforce, []Exemption{scoped(ExemptNotScanned, report, "r")}),
		"uppercase report":        policy(Enforce, []Exemption{scoped(ExemptFindings, strings.ToUpper(report), "r")}),
		"missing reason":          policy(Enforce, []Exemption{scoped(ExemptNotScanned, "", "")}),
		"long reason":             policy(Enforce, []Exemption{scoped(ExemptNotScanned, "", strings.Repeat("r", MaxTextBytes+1))}),
		"duplicate scope":         policy(Enforce, []Exemption{scoped(ExemptNotScanned, "", "a"), scoped(ExemptNotScanned, "", "b")}),
		"duplicate denial":        policy(Enforce, nil, digest, digest),
		"uppercase denial":        policy(Enforce, nil, strings.ToUpper(digest)),
		"unscanned non-digest":    policy(Enforce, []Exemption{scoped(ExemptNotScanned, "abc", "r")}),
		"invalid denial":          policy(Enforce, nil, "abc"),
	} {
		if err := invalid.Validate(); !errors.Is(err, ErrInvalidPolicy) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	other := policy(Enforce, nil)
	other.Version = 2
	if err := other.Validate(); !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("other version: %v", err)
	}
	for _, version := range []int{0, -1} {
		other.Version = version
		if err := other.Validate(); !errors.Is(err, ErrInvalidPolicy) {
			t.Fatalf("version %d: %v", version, err)
		}
	}
}

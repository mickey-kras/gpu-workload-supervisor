package attestation

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"io"
	"strings"
	"testing"
)

func encodedKey(raw []byte) string {
	return base64.StdEncoding.EncodeToString(raw)
}

func TestNewVerifierRejectsInvalidTrustConfiguration(t *testing.T) {
	seventeen := []string{}
	for i := 3; i < maxTrustedKeys+4; i++ {
		seventeen = append(seventeen, testPublicKey(i))
	}
	for name, tc := range map[string]struct {
		keys     []string
		policies []int
	}{
		"no keys":                 {nil, []int{1}},
		"too many keys":           {seventeen, []int{1}},
		"not base64":              {[]string{"not base64!"}, []int{1}},
		"unpadded base64":         {[]string{strings.TrimRight(testPublicKey(3), "=")}, []int{1}},
		"short key":               {[]string{encodedKey(make([]byte, 31))}, []int{1}},
		"private key length":      {[]string{encodedKey(testKey(3))}, []int{1}},
		"duplicate key":           {[]string{testPublicKey(3), testPublicKey(3)}, []int{1}},
		"no policy versions":      {[]string{testPublicKey(3)}, nil},
		"zero policy version":     {[]string{testPublicKey(3)}, []int{0}},
		"policy version over cap": {[]string{testPublicKey(3)}, []int{MaxPolicyVersion + 1}},
		"duplicate policy":        {[]string{testPublicKey(3)}, []int{1, 1}},
	} {
		if _, err := NewVerifier(tc.keys, tc.policies); err == nil {
			t.Fatalf("accepted %s", name)
		}
	}
	if _, err := NewVerifier(seventeen[:maxTrustedKeys], []int{1, MaxPolicyVersion}); err != nil {
		t.Fatalf("rejected maximum trust set: %v", err)
	}
	_, err := NewVerifier([]string{testPublicKey(3), "bad"}, []int{1})
	if err == nil || !strings.Contains(err.Error(), "key 1") || strings.Contains(err.Error(), "bad") {
		t.Fatalf("error must locate the key without echoing it: %v", err)
	}
}

func TestNewVerifierRejectsTestOnlyFixtureKeys(t *testing.T) {
	for _, key := range []int{1, 2} {
		if _, err := NewVerifier([]string{testPublicKey(key)}, []int{1}); err == nil {
			t.Fatalf("pinned test-only key %d", key)
		}
		if _, err := newVerifier([]string{testPublicKey(key)}, []int{1}, true); err != nil {
			t.Fatalf("test constructor rejected key %d: %v", key, err)
		}
	}
	if KeyID(testKey(1).Public().(ed25519.PublicKey)) != testOnlyKeyID1 || KeyID(testKey(2).Public().(ed25519.PublicKey)) != testOnlyKeyID2 {
		t.Fatal("test-only key IDs do not match the fixture keys")
	}
}

func TestNewVerifierRejectsSmallOrderAndNonCanonicalKeys(t *testing.T) {
	keys := map[string][]byte{}
	for i, blocked := range smallOrderKeys {
		keys["small-order "+string(rune('0'+i))] = append([]byte{}, blocked[:]...)
		flipped := append([]byte{}, blocked[:]...)
		flipped[31] |= 0x80
		keys["small-order sign bit "+string(rune('0'+i))] = flipped
	}
	beyond := bytes.Repeat([]byte{0xff}, ed25519.PublicKeySize)
	beyond[31] = 0x7f
	keys["y = 2^255 - 1"] = beyond
	pPlus2 := append([]byte{0xef}, bytes.Repeat([]byte{0xff}, 30)...)
	keys["y = p + 2"] = append(pPlus2, 0x7f)
	for name, raw := range keys {
		if _, err := newVerifier([]string{encodedKey(raw)}, []int{1}, true); err == nil {
			t.Fatalf("accepted %s key", name)
		}
	}
	almost := append([]byte{0xec}, bytes.Repeat([]byte{0xff}, 30)...)
	if !weakPublicKey(append(almost, 0x7f)) || weakPublicKey(append(append([]byte{0xeb}, bytes.Repeat([]byte{0xff}, 30)...), 0x7f)) {
		t.Fatal("canonical boundary below p - 1 misclassified")
	}
	lowMiddle := append([]byte{0xed}, make([]byte, 30)...)
	if weakPublicKey(append(lowMiddle, 0x7f)) {
		t.Fatal("canonical y with a high top byte misclassified")
	}
}

// The identity key verifies an all-identity forgery for any message under
// cofactorless Ed25519. Pinning that key must fail closed.
func TestIdentityKeyForgeryFailsClosed(t *testing.T) {
	identity := make(ed25519.PublicKey, ed25519.PublicKeySize)
	identity[0] = 0x01
	forged := make([]byte, ed25519.SignatureSize)
	forged[0] = 0x01
	envelope := fixtureEnvelopes()["findings"]
	message, err := SigningBytes(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if !ed25519.Verify(identity, message, forged) {
		t.Fatal("expected the standard library to accept the identity-key forgery")
	}
	if _, err := NewVerifier([]string{encodedKey(identity)}, []int{1}); err == nil {
		t.Fatal("pinned the identity key")
	}
	if _, err := NewVerifier([]string{encodedKey(make([]byte, ed25519.PublicKeySize))}, []int{1}); err == nil {
		t.Fatal("pinned the all-zero key")
	}
}

func TestVerifyFailsClosedOnMissingInput(t *testing.T) {
	inventory := fixtureInventory(t)
	envelope := func() io.Reader { return bytes.NewReader(readFixture(t, "passed.json")) }
	signature := func() io.Reader { return bytes.NewReader(readFixture(t, "passed.sig.json")) }
	var nilVerifier *Verifier
	for name, got := range map[string]Verification{
		"nil verifier":  nilVerifier.Verify(envelope(), signature(), inventory),
		"nil envelope":  fixtureVerifier(t, 1).Verify(nil, signature(), inventory),
		"nil signature": fixtureVerifier(t, 1).Verify(envelope(), nil, inventory),
	} {
		if got.Result() != Malformed || !errors.Is(got.Err(), ErrMissingInput) {
			t.Fatalf("%s: %q %v", name, got.Result(), got.Err())
		}
		if Decide(Policy{Mode: Advisory}, &got).Allowed {
			t.Fatalf("%s allowed", name)
		}
	}
}

func TestVerifyReportsUnreadableInputSeparately(t *testing.T) {
	inventory := fixtureInventory(t)
	v := fixtureVerifier(t, 1)
	for name, got := range map[string]Verification{
		"envelope":  v.Verify(failingReader{}, bytes.NewReader(readFixture(t, "passed.sig.json")), inventory),
		"signature": v.Verify(bytes.NewReader(readFixture(t, "passed.json")), failingReader{}, inventory),
	} {
		if got.Result() != Unreadable || !errors.Is(got.Err(), ErrUnreadable) || strings.Contains(got.Err().Error(), "/private") {
			t.Fatalf("%s: %q %v", name, got.Result(), got.Err())
		}
		if d := Decide(Policy{Mode: Advisory}, &got); d != allow(NotVerified, ReasonUnreadable) {
			t.Fatalf("%s advisory: %#v", name, d)
		}
		if d := Decide(Policy{Mode: Enforce}, &got); d != deny(NotVerified, ReasonUnreadable) {
			t.Fatalf("%s enforce: %#v", name, d)
		}
	}
}

func TestVerifyErrorsAreSentinelsWithoutEvidence(t *testing.T) {
	inventory := fixtureInventory(t)
	wrong := withFile(inventory.Files, "config.json", fixtureDigest("changed"))
	got := fixtureVerifier(t, 1).Verify(bytes.NewReader(readFixture(t, "passed.json")), bytes.NewReader(readFixture(t, "passed.sig.json")), wrong)
	if !errors.Is(got.Err(), ErrArtifactMismatch) || !strings.Contains(got.Err().Error(), "digest differs") {
		t.Fatalf("%v", got.Err())
	}
	if strings.Contains(got.Err().Error(), "config.json") || strings.Contains(got.Err().Error(), inventory.Files["config.json"]) {
		t.Fatalf("mismatch error echoes evidence: %v", got.Err())
	}
	for kind, onDisk := range map[string]Artifact{
		"file count differs":      withFile(inventory.Files, "extra", fixtureDigest("x")),
		"a bound file is missing": withFile(withFile(inventory.Files, "config.json", "").Files, "other.json", fixtureDigest("x")),
		"artifact form differs":   {SHA256: fixtureDigest("x")},
	} {
		got := fixtureVerifier(t, 1).Verify(bytes.NewReader(readFixture(t, "passed.json")), bytes.NewReader(readFixture(t, "passed.sig.json")), onDisk)
		if !strings.Contains(got.Err().Error(), kind) {
			t.Fatalf("%s: %v", kind, got.Err())
		}
	}
	single := fixtureVerifier(t, 1).Verify(bytes.NewReader(readFixture(t, "single.json")), bytes.NewReader(readFixture(t, "single.sig.json")), Artifact{SHA256: fixtureDigest("x")})
	if !strings.Contains(single.Err().Error(), "single artifact digest differs") {
		t.Fatalf("%v", single.Err())
	}
}

func TestVerifierAcceptsOnlyConfiguredPolicyVersions(t *testing.T) {
	got := fixtureVerifierWithPolicies(t, []int{1, 2}, 1).Verify(bytes.NewReader(readFixture(t, "policy-2.json")), bytes.NewReader(readFixture(t, "policy-2.sig.json")), fixtureInventory(t))
	if got.Result() != Valid || got.Envelope().PolicyVersion != 2 || got.KeyID() != testOnlyKeyID1 {
		t.Fatalf("%#v", got)
	}
}

func TestKeyIDIsSHA256OfRawPublicKey(t *testing.T) {
	// SHA-256 of 32 zero bytes, a fixed known answer independent of this package.
	if got := KeyID(make(ed25519.PublicKey, ed25519.PublicKeySize)); got != "66687aadf862bd776c8fc18b8e9f8e20089714856ee233b3902a591d0d5f2925" {
		t.Fatal(got)
	}
}

func TestReadSignatureRejectsMalformedFiles(t *testing.T) {
	raw := readFixture(t, "passed.sig.json")
	sig, _, err := readSignature(bytes.NewReader(raw))
	if err != nil || sig.Algorithm != SignatureAlgorithm {
		t.Fatalf("%#v %v", sig, err)
	}
	for name, body := range map[string][]byte{
		"uppercase key id": bytes.Replace(raw, []byte(sig.KeyID), []byte(strings.ToUpper(sig.KeyID)), 1),
		"short signature":  bytes.Replace(raw, []byte(sig.Value), []byte(base64.StdEncoding.EncodeToString(make([]byte, 63))), 1),
		"bad base64":       bytes.Replace(raw, []byte(sig.Value), []byte("!"+sig.Value[1:]), 1),
		"missing field":    bytes.Replace(raw, []byte(`"version": 1,`), nil, 1),
		"cased field":      bytes.Replace(raw, []byte(`"keyId"`), []byte(`"keyid"`), 1),
		"invalid UTF-8":    bytes.Replace(raw, []byte("ed25519"), []byte("ed25519\xff"), 1),
		"oversized":        append(append([]byte{}, raw...), bytes.Repeat([]byte(" "), MaxSignatureBytes)...),
	} {
		if bytes.Equal(body, raw) {
			t.Fatalf("%s mutation did not apply", name)
		}
		if _, _, err := readSignature(bytes.NewReader(body)); !errors.Is(err, ErrMalformedSignature) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestPolicyValidate(t *testing.T) {
	digest := fixtureDigest("artifact")
	scoped := func(kind ExemptionKind, reason string) Exemption {
		return Exemption{Artifact: digest, PolicyVersion: 1, Kind: kind, Reason: reason}
	}
	for _, valid := range []Policy{
		{Mode: Advisory},
		{Mode: Enforce, Exemptions: []Exemption{scoped(ExemptNotScanned, "r"), scoped(ExemptFindings, "r"), scoped(ExemptUnsupported, "r"), scoped(ExemptPartialCoverage, strings.Repeat("r", MaxTextBytes))}},
		{Mode: Enforce, Deny: []string{digest, fixtureDigest("other")}},
	} {
		if err := valid.Validate(); err != nil {
			t.Fatalf("%#v: %v", valid, err)
		}
	}
	for name, invalid := range map[string]Policy{
		"empty mode":       {},
		"unknown mode":     {Mode: "strict"},
		"short digest":     {Mode: Enforce, Exemptions: []Exemption{{Artifact: "abc", PolicyVersion: 1, Kind: ExemptNotScanned, Reason: "r"}}},
		"zero policy":      {Mode: Enforce, Exemptions: []Exemption{{Artifact: digest, Kind: ExemptNotScanned, Reason: "r"}}},
		"policy over cap":  {Mode: Enforce, Exemptions: []Exemption{{Artifact: digest, PolicyVersion: MaxPolicyVersion + 1, Kind: ExemptNotScanned, Reason: "r"}}},
		"error kind":       {Mode: Enforce, Exemptions: []Exemption{scoped("error", "r")}},
		"missing reason":   {Mode: Enforce, Exemptions: []Exemption{scoped(ExemptNotScanned, "")}},
		"long reason":      {Mode: Enforce, Exemptions: []Exemption{scoped(ExemptNotScanned, strings.Repeat("r", MaxTextBytes+1))}},
		"duplicate scope":  {Mode: Enforce, Exemptions: []Exemption{scoped(ExemptNotScanned, "a"), scoped(ExemptNotScanned, "b")}},
		"duplicate denial": {Mode: Enforce, Deny: []string{digest, digest}},
		"invalid denial":   {Mode: Enforce, Deny: []string{"abc"}},
	} {
		if invalid.Validate() == nil {
			t.Fatalf("accepted %s", name)
		}
	}
}

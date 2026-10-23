package attestation

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"strings"
	"testing"
)

func TestNewVerifierRejectsInvalidTrustConfiguration(t *testing.T) {
	sixteen := []string{}
	for i := 0; i < maxTrustedKeys+1; i++ {
		sixteen = append(sixteen, testPublicKey(i))
	}
	for name, tc := range map[string]struct {
		keys     []string
		policies []int
	}{
		"no keys":             {nil, []int{1}},
		"too many keys":       {sixteen, []int{1}},
		"not base64":          {[]string{"not base64!"}, []int{1}},
		"unpadded base64":     {[]string{strings.TrimRight(testPublicKey(1), "=")}, []int{1}},
		"short key":           {[]string{base64.StdEncoding.EncodeToString(make([]byte, 31))}, []int{1}},
		"private seed length": {[]string{base64.StdEncoding.EncodeToString(testKey(1))}, []int{1}},
		"duplicate key":       {[]string{testPublicKey(1), testPublicKey(1)}, []int{1}},
		"no policy versions":  {[]string{testPublicKey(1)}, nil},
		"zero policy version": {[]string{testPublicKey(1)}, []int{0}},
		"duplicate policy":    {[]string{testPublicKey(1)}, []int{1, 1}},
	} {
		if _, err := NewVerifier(tc.keys, tc.policies); err == nil {
			t.Fatalf("accepted %s", name)
		}
	}
	if _, err := NewVerifier(sixteen[:maxTrustedKeys], []int{1, 2}); err != nil {
		t.Fatalf("rejected maximum trust set: %v", err)
	}
}

func TestVerifierAcceptsOnlyConfiguredPolicyVersions(t *testing.T) {
	v, err := NewVerifier([]string{testPublicKey(1)}, []int{1, 2})
	if err != nil {
		t.Fatal(err)
	}
	got := v.Verify(bytes.NewReader(readFixture(t, "policy-2.json")), bytes.NewReader(readFixture(t, "policy-2.sig.json")), fixtureInventory(t))
	if got.Result != Valid || got.Envelope.PolicyVersion != 2 {
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
	sig, err := ReadSignature(bytes.NewReader(raw))
	if err != nil || sig.Algorithm != SignatureAlgorithm {
		t.Fatalf("%#v %v", sig, err)
	}
	for name, body := range map[string][]byte{
		"read failure":     nil,
		"version":          bytes.Replace(raw, []byte(`"version": 1`), []byte(`"version": 2`), 1),
		"uppercase key id": bytes.Replace(raw, []byte(sig.KeyID), []byte(strings.ToUpper(sig.KeyID)), 1),
		"short signature":  bytes.Replace(raw, []byte(sig.Value), []byte(base64.StdEncoding.EncodeToString(make([]byte, 63))), 1),
		"bad base64":       bytes.Replace(raw, []byte(sig.Value), []byte("!"+sig.Value[1:]), 1),
		"missing field":    bytes.Replace(raw, []byte(`"version": 1,`), nil, 1),
		"cased field":      bytes.Replace(raw, []byte(`"keyId"`), []byte(`"keyid"`), 1),
		"invalid UTF-8":    bytes.Replace(raw, []byte("ed25519"), []byte("ed25519\xff"), 1),
		"oversized":        append(append([]byte{}, raw...), bytes.Repeat([]byte(" "), MaxSignatureBytes)...),
	} {
		var err error
		if body == nil {
			_, err = ReadSignature(failingReader{})
		} else {
			_, err = ReadSignature(bytes.NewReader(body))
		}
		if err == nil {
			t.Fatalf("accepted %s", name)
		}
	}
}

func TestPolicyValidate(t *testing.T) {
	digest := fixtureDigest("artifact")
	for _, valid := range []Policy{{}, {Mode: Advisory}, {Mode: Enforce, Exemptions: []Exemption{{Artifact: digest, PolicyVersion: 1}}}} {
		if err := valid.Validate(); err != nil {
			t.Fatalf("%#v: %v", valid, err)
		}
	}
	for _, invalid := range []Policy{
		{Mode: "strict"},
		{Exemptions: []Exemption{{Artifact: "abc", PolicyVersion: 1}}},
		{Exemptions: []Exemption{{Artifact: digest}}},
	} {
		if invalid.Validate() == nil {
			t.Fatalf("accepted %#v", invalid)
		}
	}
}

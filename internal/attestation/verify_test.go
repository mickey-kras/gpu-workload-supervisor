package attestation

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"testing"
)

func encodedKey(raw []byte) string {
	return base64.StdEncoding.EncodeToString(raw)
}

func trustConfig(policies []int, keys ...TrustedKey) TrustConfig {
	return TrustConfig{Version: TrustConfigVersion, Keys: keys, PolicyVersions: policies}
}

func rawTrustedKey(raw []byte) TrustedKey {
	return TrustedKey{KeyID: KeyID(raw), PublicKey: encodedKey(raw)}
}

func TestNewVerifierRejectsInvalidTrustConfiguration(t *testing.T) {
	seventeen := trustedKeys()
	for i := 3; i < maxTrustedKeys+4; i++ {
		seventeen = append(seventeen, trustedKeys(i)...)
	}
	key3 := trustedKeys(3)[0]
	unpadded := TrustedKey{KeyID: key3.KeyID, PublicKey: strings.TrimRight(key3.PublicKey, "=")}
	uppercase := TrustedKey{KeyID: strings.ToUpper(key3.KeyID), PublicKey: key3.PublicKey}
	for name, config := range map[string]TrustConfig{
		"no keys":                 trustConfig([]int{1}),
		"too many keys":           trustConfig([]int{1}, seventeen...),
		"not base64":              trustConfig([]int{1}, TrustedKey{KeyID: key3.KeyID, PublicKey: "not base64!"}),
		"unpadded base64":         trustConfig([]int{1}, unpadded),
		"short key":               trustConfig([]int{1}, rawTrustedKey(make([]byte, 31))),
		"private key length":      trustConfig([]int{1}, rawTrustedKey(testKey(3))),
		"duplicate key":           trustConfig([]int{1}, key3, key3),
		"key ID mismatch":         trustConfig([]int{1}, TrustedKey{KeyID: trustedKeys(4)[0].KeyID, PublicKey: key3.PublicKey}),
		"uppercase key ID":        trustConfig([]int{1}, uppercase),
		"no policy versions":      trustConfig(nil, key3),
		"zero policy version":     trustConfig([]int{0}, key3),
		"policy version over cap": trustConfig([]int{2147483648}, key3),
		"duplicate policy":        trustConfig([]int{1, 1}, key3),
	} {
		if _, err := NewVerifier(config); !errors.Is(err, ErrInvalidTrust) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	other := trustConfig([]int{1}, key3)
	other.Version = 2
	if _, err := NewVerifier(other); !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("other version: %v", err)
	}
	if _, err := NewVerifier(trustConfig([]int{1, 2147483647}, seventeen[:maxTrustedKeys]...)); err != nil {
		t.Fatalf("rejected maximum trust set: %v", err)
	}
	_, err := NewVerifier(trustConfig([]int{1}, key3, TrustedKey{KeyID: key3.KeyID, PublicKey: "bad"}))
	if err == nil || !strings.Contains(err.Error(), "key 1") || strings.Contains(err.Error(), "bad") {
		t.Fatalf("error must locate the key without echoing it: %v", err)
	}
}

func TestNewVerifierRejectsTestOnlyFixtureKeys(t *testing.T) {
	for _, key := range []int{1, 2} {
		raw := []byte(testKey(key).Public().(ed25519.PublicKey))
		flipped := append([]byte{}, raw...)
		flipped[31] ^= 0x80
		for _, candidate := range [][]byte{raw, flipped} {
			if _, err := NewVerifier(trustConfig([]int{1}, rawTrustedKey(candidate))); !errors.Is(err, ErrInvalidTrust) {
				t.Fatalf("pinned test-only key %d: %v", key, err)
			}
		}
		if _, err := newVerifier(trustConfig([]int{1}, trustedKeys(key)...), true); err != nil {
			t.Fatalf("test constructor rejected key %d: %v", key, err)
		}
		if testOnlyPublicKeys[key-1] != testPublicKey(key) {
			t.Fatalf("test-only key %d does not match the fixture key", key)
		}
	}
}

// libsodium ge25519_has_small_order blocklist, restated here as literals so
// the test does not trust the production table.
var smallOrderHex = []string{
	"0000000000000000000000000000000000000000000000000000000000000000",
	"0100000000000000000000000000000000000000000000000000000000000000",
	"26e8958fc2b227b045c3f489f2ef98f0d5dfac05d3c63339b13802886d53fc05",
	"c7176a703d4dd84fba3c0b760d10670f2a2053fa2c39ccc64ec7fd7792ac037a",
	"ecffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f",
	"edffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f",
	"eeffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f",
}

// torsionHex lists the canonical encodings of the eight small-order points.
var torsionHex = []string{
	"0100000000000000000000000000000000000000000000000000000000000000",
	"ecffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f",
	"0000000000000000000000000000000000000000000000000000000000000000",
	"0000000000000000000000000000000000000000000000000000000000000080",
	"26e8958fc2b227b045c3f489f2ef98f0d5dfac05d3c63339b13802886d53fc05",
	"26e8958fc2b227b045c3f489f2ef98f0d5dfac05d3c63339b13802886d53fc85",
	"c7176a703d4dd84fba3c0b760d10670f2a2053fa2c39ccc64ec7fd7792ac037a",
	"c7176a703d4dd84fba3c0b760d10670f2a2053fa2c39ccc64ec7fd7792ac03fa",
}

func mustHex(t *testing.T, value string) []byte {
	t.Helper()
	raw, err := hex.DecodeString(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// forges reports whether a signature with S = 0 and a small-order R verifies
// under key for some message. Cofactorless Ed25519 then checks R = -[k]A.
func forges(t *testing.T, key []byte) bool {
	t.Helper()
	for i := 0; i < 64; i++ {
		message := []byte("forged message " + strconv.Itoa(i))
		for _, r := range torsionHex {
			signature := append(mustHex(t, r), make([]byte, 32)...)
			if ed25519.Verify(ed25519.PublicKey(key), message, signature) {
				return true
			}
		}
	}
	return false
}

func TestSmallOrderKeysForgeAndAreRejected(t *testing.T) {
	for i, value := range smallOrderHex {
		raw := mustHex(t, value)
		if !forges(t, raw) {
			t.Fatalf("entry %d: expected the standard library to accept a forgery", i)
		}
		flipped := append([]byte{}, raw...)
		flipped[31] |= 0x80
		for _, candidate := range [][]byte{raw, flipped} {
			if _, err := newVerifier(trustConfig([]int{1}, rawTrustedKey(candidate)), true); !errors.Is(err, ErrInvalidTrust) {
				t.Fatalf("entry %d accepted: %v", i, err)
			}
		}
	}
	if forges(t, []byte(testKey(3).Public().(ed25519.PublicKey))) {
		t.Fatal("an ordinary key accepted a small-order forgery")
	}
}

func TestNonCanonicalKeysAreRejected(t *testing.T) {
	for name, value := range map[string]string{
		"y = p + 2":     "efffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f",
		"y = 2^255 - 1": "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f",
	} {
		if _, err := newVerifier(trustConfig([]int{1}, rawTrustedKey(mustHex(t, value))), true); !errors.Is(err, ErrInvalidTrust) {
			t.Fatalf("accepted %s: %v", name, err)
		}
	}
	for name, value := range map[string]string{
		"y = p - 2":            "ebffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f",
		"high top byte only":   "ed00000000000000000000000000000000000000000000000000000000000000",
		"high low byte, mixed": "edffffffffffffffffffffffffffffffffffffffffffffffffffffffff00ff7f",
	} {
		if weakPublicKey(mustHex(t, value)) {
			t.Fatalf("%s misclassified as weak", name)
		}
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
		if Decide(policy(Advisory, nil), inventory, &got).Allowed {
			t.Fatalf("%s allowed", name)
		}
	}
	for name, read := range map[string]func() error{
		"envelope":     func() error { _, err := readEnvelope(nil); return err },
		"trust config": func() error { _, err := ReadTrustConfig(nil); return err },
		"policy":       func() error { _, err := ReadPolicy(nil); return err },
	} {
		if err := read(); !errors.Is(err, ErrMissingInput) || !strings.Contains(err.Error(), name) {
			t.Fatalf("nil %s reader: %v", name, err)
		}
	}
}

type errorReader struct{ err error }

func (r errorReader) Read([]byte) (int, error) { return 0, r.err }

func TestUnreadableInputNamesInputAndClassOnly(t *testing.T) {
	inventory := fixtureInventory(t)
	v := fixtureVerifier(t, 1)
	for class, err := range map[string]error{
		"permission": &fs.PathError{Op: "open", Path: "/private/secret", Err: fs.ErrPermission},
		"timeout":    fmt.Errorf("wrapped /private/secret: %w", os.ErrDeadlineExceeded),
		"other":      errors.New("failed at /private/secret"),
	} {
		for name, got := range map[string]Verification{
			"envelope":       v.Verify(errorReader{err}, bytes.NewReader(readFixture(t, "passed.sig.json")), inventory),
			"signature file": v.Verify(bytes.NewReader(readFixture(t, "passed.json")), errorReader{err}, inventory),
		} {
			message := got.Err().Error()
			if got.Result() != Unreadable || !errors.Is(got.Err(), ErrUnreadable) || !strings.Contains(message, name+": "+class) || strings.Contains(message, "/private") {
				t.Fatalf("%s %s: %q %v", name, class, got.Result(), got.Err())
			}
			if d := Decide(policy(Advisory, nil), inventory, &got); d != allow(NotVerified, ReasonUnreadable) {
				t.Fatalf("%s advisory: %#v", name, d)
			}
			if d := Decide(policy(Enforce, nil), inventory, &got); d != deny(NotVerified, ReasonUnreadable) {
				t.Fatalf("%s enforce: %#v", name, d)
			}
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
	versioned := fixtureVerifier(t, 1).Verify(bytes.NewReader(bytes.Replace(readFixture(t, "passed.json"), []byte(`"version": 1`), []byte(`"version": 2`), 1)), bytes.NewReader(readFixture(t, "passed.sig.json")), inventory)
	if !errors.Is(versioned.Err(), ErrUnsupportedVersion) || !strings.Contains(versioned.Err().Error(), "envelope") {
		t.Fatalf("%v", versioned.Err())
	}
}

func TestVerifierAcceptsOnlyConfiguredPolicyVersions(t *testing.T) {
	got := fixtureVerifierWithPolicies(t, []int{1, 2}, 1).Verify(bytes.NewReader(readFixture(t, "policy-2.json")), bytes.NewReader(readFixture(t, "policy-2.sig.json")), fixtureInventory(t))
	if got.Result() != Valid || got.Envelope().PolicyVersion != 2 || got.KeyID() != trustedKeys(1)[0].KeyID {
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

package attestation

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// SignatureFormatVersion is the only detached signature format this package reads.
const SignatureFormatVersion = 1

// SignatureAlgorithm is fixed by the format version; the signature file names
// it so a different algorithm is rejected instead of negotiated.
const SignatureAlgorithm = "ed25519"

const maxTrustedKeys = 16

// testOnlyKeys are the raw fixture public keys with the sign bit cleared.
// Production trust never pins them, with or without the sign bit.
var testOnlyKeys = [][ed25519.PublicKeySize]byte{
	{0xf2, 0x05, 0xfb, 0x11, 0x94, 0x76, 0x5f, 0x76, 0x36, 0x78, 0x43, 0x22, 0x22, 0xae, 0x4b, 0x1f, 0xa4, 0xbf, 0xa9, 0x7c, 0x4b, 0x97, 0xc3, 0x67, 0x6f, 0xad, 0xae, 0xcf, 0x02, 0x47, 0x63, 0x00},
	{0x48, 0x48, 0x35, 0xf3, 0x57, 0x2c, 0xf6, 0xbc, 0x6e, 0x11, 0x5d, 0x87, 0x93, 0xaf, 0xdc, 0xf1, 0x01, 0xe7, 0xd1, 0xfe, 0xe6, 0x36, 0x6d, 0xc0, 0x23, 0x99, 0x8f, 0x67, 0xd5, 0xeb, 0x79, 0x6f},
}

// smallOrderKeys are the blocklist of ge25519_has_small_order in libsodium
// 1.0.20-RELEASE, src/libsodium/crypto_core/ed25519/ref10/ref10.c, with the
// sign bit cleared. Each value was checked independently by decoding the
// point and multiplying it to the identity. A small-order key verifies forged
// signatures for any message.
var smallOrderKeys = [][ed25519.PublicKeySize]byte{
	{},
	{0x01},
	{0x26, 0xe8, 0x95, 0x8f, 0xc2, 0xb2, 0x27, 0xb0, 0x45, 0xc3, 0xf4, 0x89, 0xf2, 0xef, 0x98, 0xf0, 0xd5, 0xdf, 0xac, 0x05, 0xd3, 0xc6, 0x33, 0x39, 0xb1, 0x38, 0x02, 0x88, 0x6d, 0x53, 0xfc, 0x05},
	{0xc7, 0x17, 0x6a, 0x70, 0x3d, 0x4d, 0xd8, 0x4f, 0xba, 0x3c, 0x0b, 0x76, 0x0d, 0x10, 0x67, 0x0f, 0x2a, 0x20, 0x53, 0xfa, 0x2c, 0x39, 0xcc, 0xc6, 0x4e, 0xc7, 0xfd, 0x77, 0x92, 0xac, 0x03, 0x7a},
	{0xec, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f},
	{0xed, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f},
	{0xee, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f},
}

// Signature is the detached signature file for one envelope.
type Signature struct {
	Version   int    `json:"version"`
	Algorithm string `json:"algorithm"`
	KeyID     string `json:"keyId"`
	Value     string `json:"signature"`
}

type Result string

const (
	Valid              Result = "valid"
	Malformed          Result = "malformed"
	Unreadable         Result = "unreadable"
	UnsupportedVersion Result = "unsupported-version"
	InvalidInventory   Result = "invalid-inventory"
	InvalidSignature   Result = "invalid-signature"
	UnknownKey         Result = "unknown-key"
	ArtifactMismatch   Result = "artifact-mismatch"
	IncompatiblePolicy Result = "incompatible-policy"
)

// Verification is sealed: only Verify sets its fields, and Decide treats any
// other value as an integrity failure.
type Verification struct {
	result   Result
	keyID    string
	envelope Envelope
	digest   string
	err      error
	sealed   bool
}

func (v Verification) Result() Result { return v.result }

// KeyID is empty until the signature file parses.
func (v Verification) KeyID() string { return v.keyID }

// Envelope is the zero value unless Result is Valid. It returns a copy, so a
// caller cannot change the evidence that Decide uses.
func (v Verification) Envelope() Envelope { return v.envelope.clone() }

// ArtifactDigest is the sealed artifact digest, empty unless Result is Valid.
// The producer CLI prints it and the UI shows it to scope exemptions and denials.
func (v Verification) ArtifactDigest() string { return v.digest }

// Err wraps one of the package sentinels and is nil only for Valid.
func (v Verification) Err() error { return v.err }

// Verifier holds public keys and policy versions from trusted configuration.
// It has no mutable allowlist; rotation replaces the configuration.
type Verifier struct {
	keys     map[string]ed25519.PublicKey
	policies map[int]bool
}

// KeyID is the lowercase hex SHA-256 of a raw 32-byte Ed25519 public key.
func KeyID(key ed25519.PublicKey) string {
	sum := sha256.Sum256(key)
	return hex.EncodeToString(sum[:])
}

// NewVerifier accepts exactly the trust configurations ReadTrustConfig
// accepts. Pin the old and new keys together during rotation, then remove the
// old key.
func NewVerifier(config TrustConfig) (*Verifier, error) {
	return newVerifier(config, false)
}

// newVerifier is the one trust validation. Only this package's tests admit
// the fixture keys.
func newVerifier(config TrustConfig, allowTestKeys bool) (*Verifier, error) {
	if config.Version < 1 {
		return nil, fmt.Errorf("%w: version must be a positive integer", ErrInvalidTrust)
	}
	if config.Version != TrustConfigVersion {
		return nil, fmt.Errorf("%w: trust config", ErrUnsupportedVersion)
	}
	keys, err := pinKeys(config.Keys, allowTestKeys)
	if err != nil {
		return nil, err
	}
	policies, err := policySet(config.PolicyVersions)
	if err != nil {
		return nil, err
	}
	return &Verifier{keys: keys, policies: policies}, nil
}

func pinKeys(trusted []TrustedKey, allowTestKeys bool) (map[string]ed25519.PublicKey, error) {
	if len(trusted) == 0 || len(trusted) > maxTrustedKeys {
		return nil, fmt.Errorf("%w: needs 1 to %d pinned public keys", ErrInvalidTrust, maxTrustedKeys)
	}
	keys := map[string]ed25519.PublicKey{}
	for index, entry := range trusted {
		key, err := parsePinnedKey(entry.PublicKey, allowTestKeys)
		if err != nil {
			return nil, fmt.Errorf("%w: key %d: %v", ErrInvalidTrust, index, err)
		}
		id := KeyID(key)
		if entry.KeyID != id {
			return nil, fmt.Errorf("%w: key %d ID does not match its public key", ErrInvalidTrust, index)
		}
		if keys[id] != nil {
			return nil, fmt.Errorf("%w: key %d duplicates an earlier key", ErrInvalidTrust, index)
		}
		keys[id] = key
	}
	return keys, nil
}

func policySet(versions []int) (map[int]bool, error) {
	if len(versions) == 0 {
		return nil, fmt.Errorf("%w: needs supported policy versions", ErrInvalidTrust)
	}
	policies := map[int]bool{}
	for index, version := range versions {
		if !validPolicyVersion(version) || policies[version] {
			return nil, fmt.Errorf("%w: policy version %d must be unique and 1 to %d", ErrInvalidTrust, index, MaxPolicyVersion)
		}
		policies[version] = true
	}
	return policies, nil
}

func parsePinnedKey(encoded string, allowTestKeys bool) (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, errors.New("must be standard base64 of 32 raw bytes")
	}
	if weakPublicKey(raw) {
		return nil, errors.New("small-order or non-canonical Ed25519 key")
	}
	if !allowTestKeys && testOnlyKey(raw) {
		return nil, errors.New("test-only fixture key")
	}
	return ed25519.PublicKey(raw), nil
}

func clearSignBit(raw []byte) [ed25519.PublicKeySize]byte {
	var masked [ed25519.PublicKeySize]byte
	copy(masked[:], raw)
	masked[31] &= 0x7f
	return masked
}

func testOnlyKey(raw []byte) bool {
	masked := clearSignBit(raw)
	for _, fixture := range testOnlyKeys {
		if masked == fixture {
			return true
		}
	}
	return false
}

// weakPublicKey rejects small-order points, ignoring the sign bit, and any
// encoding whose y coordinate is not below p = 2^255 - 19.
func weakPublicKey(raw []byte) bool {
	masked := clearSignBit(raw)
	for _, blocked := range smallOrderKeys {
		if masked == blocked {
			return true
		}
	}
	if masked[31] != 0x7f || masked[0] < 0xed {
		return false
	}
	for _, b := range masked[1:31] {
		if b != 0xff {
			return false
		}
	}
	return true
}

func failed(result Result, keyID string, err error) Verification {
	return Verification{result: result, keyID: keyID, err: err, sealed: true}
}

// classify maps a read error to its result. Anything unrecognized is malformed.
func classify(err error) Result {
	switch {
	case errors.Is(err, ErrUnreadable):
		return Unreadable
	case errors.Is(err, ErrUnsupportedVersion):
		return UnsupportedVersion
	}
	return Malformed
}

// Verify is the single check for producers and the supervisor gate. onDisk is
// the effective artifact the caller inventoried; copied evidence for other
// bytes reports ArtifactMismatch even when its signature is valid. The small
// signature file and its key ID are checked before the envelope is read.
func (v *Verifier) Verify(envelope, signature io.Reader, onDisk Artifact) Verification {
	if missing := missingInput(v, envelope, signature); missing != "" {
		return failed(Malformed, "", fmt.Errorf("%w: %s", ErrMissingInput, missing))
	}
	if err := onDisk.validate(); err != nil {
		return failed(InvalidInventory, "", fmt.Errorf("%w: %v", ErrInvalidInventory, err))
	}
	sig, value, err := readSignature(signature)
	if err != nil {
		return failed(classify(err), "", err)
	}
	key := v.keys[sig.KeyID]
	if key == nil {
		return failed(UnknownKey, sig.KeyID, ErrUnknownKey)
	}
	parsed, err := readEnvelope(envelope)
	if err != nil {
		return failed(classify(err), sig.KeyID, err)
	}
	message, err := SigningBytes(parsed)
	if err != nil {
		return failed(Malformed, sig.KeyID, err)
	}
	if !ed25519.Verify(key, message, value) {
		return failed(InvalidSignature, sig.KeyID, ErrInvalidSignature)
	}
	if !v.policies[parsed.PolicyVersion] {
		return failed(IncompatiblePolicy, sig.KeyID, ErrIncompatiblePolicy)
	}
	if err := parsed.Artifact.match(onDisk); err != nil {
		return failed(ArtifactMismatch, sig.KeyID, err)
	}
	digest, err := parsed.Artifact.Digest()
	if err != nil {
		return failed(Malformed, sig.KeyID, err)
	}
	return Verification{result: Valid, keyID: sig.KeyID, envelope: parsed, digest: digest, sealed: true}
}

func missingInput(v *Verifier, envelope, signature io.Reader) string {
	switch {
	case v == nil:
		return "verifier"
	case envelope == nil:
		return "envelope"
	case signature == nil:
		return "signature file"
	}
	return ""
}

func readSignature(r io.Reader) (Signature, []byte, error) {
	var sig Signature
	if err := signatureDocument.read(r, &sig, func() ([]byte, error) { return json.Marshal(sig) }); err != nil {
		return Signature{}, nil, err
	}
	if sig.Version != SignatureFormatVersion || sig.Algorithm != SignatureAlgorithm || !digestPattern.MatchString(sig.KeyID) {
		return Signature{}, nil, fmt.Errorf("%w: needs version %d, algorithm %s and a lowercase hex key ID", ErrMalformedSignature, SignatureFormatVersion, SignatureAlgorithm)
	}
	value, err := base64.StdEncoding.Strict().DecodeString(sig.Value)
	if err != nil || len(value) != ed25519.SignatureSize {
		return Signature{}, nil, fmt.Errorf("%w: signature must be standard base64 of %d bytes", ErrMalformedSignature, ed25519.SignatureSize)
	}
	return sig, value, nil
}

// match names only the mismatch kind, never a path or digest.
func (a Artifact) match(onDisk Artifact) error {
	if (a.SHA256 == "") != (onDisk.SHA256 == "") {
		return fmt.Errorf("%w: artifact form differs", ErrArtifactMismatch)
	}
	if a.SHA256 != "" {
		if a.SHA256 != onDisk.SHA256 {
			return fmt.Errorf("%w: single artifact digest differs", ErrArtifactMismatch)
		}
		return nil
	}
	if len(a.Files) != len(onDisk.Files) {
		return fmt.Errorf("%w: file count differs", ErrArtifactMismatch)
	}
	for path, digest := range a.Files {
		found, ok := onDisk.Files[path]
		if !ok {
			return fmt.Errorf("%w: a bound file is missing on disk", ErrArtifactMismatch)
		}
		if found != digest {
			return fmt.Errorf("%w: a file digest differs", ErrArtifactMismatch)
		}
	}
	return nil
}

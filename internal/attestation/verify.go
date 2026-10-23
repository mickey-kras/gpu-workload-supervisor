package attestation

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/strictjson"
)

// SignatureFormatVersion is the only detached signature format this package reads.
const SignatureFormatVersion = 1

// SignatureAlgorithm is fixed by the format version; the signature file names
// it so a different algorithm is rejected instead of negotiated.
const SignatureAlgorithm = "ed25519"

const maxTrustedKeys = 16

// Fixture key IDs from this package's tests. Production trust never pins them.
const (
	testOnlyKeyID1 = "16e9ace517de63926f01a91b90340dc8eff8e21e3786dbbd63dff31baf066d1c"
	testOnlyKeyID2 = "3db860a7e4791c1f25c3834cc6444f99515e7ed8fdf92e2b78cbe016a4eca529"
)

// smallOrderKeys are the libsodium has_small_order encodings with the sign
// bit cleared. A small-order key verifies forged signatures for any message.
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

// NewVerifier pins each standard base64 raw Ed25519 public key. Pin the old
// and new keys together during rotation, then remove the old key.
func NewVerifier(publicKeys []string, policyVersions []int) (*Verifier, error) {
	return newVerifier(publicKeys, policyVersions, false)
}

// newVerifier admits the fixture keys only for this package's tests.
func newVerifier(publicKeys []string, policyVersions []int, allowTestKeys bool) (*Verifier, error) {
	if len(publicKeys) == 0 || len(publicKeys) > maxTrustedKeys {
		return nil, fmt.Errorf("attestation verifier needs 1 to %d pinned public keys", maxTrustedKeys)
	}
	v := &Verifier{keys: map[string]ed25519.PublicKey{}, policies: map[int]bool{}}
	for index, encoded := range publicKeys {
		key, err := parsePinnedKey(encoded, allowTestKeys)
		if err != nil {
			return nil, fmt.Errorf("pinned attestation key %d: %w", index, err)
		}
		id := KeyID(key)
		if v.keys[id] != nil {
			return nil, fmt.Errorf("pinned attestation key %d: duplicate key", index)
		}
		v.keys[id] = key
	}
	if len(policyVersions) == 0 {
		return nil, errors.New("attestation verifier needs supported policy versions")
	}
	for index, version := range policyVersions {
		if !validPolicyVersion(version) || v.policies[version] {
			return nil, fmt.Errorf("supported policy version %d must be unique and 1 to %d", index, MaxPolicyVersion)
		}
		v.policies[version] = true
	}
	return v, nil
}

func parsePinnedKey(encoded string, allowTestKeys bool) (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, errors.New("must be standard base64 of 32 raw bytes")
	}
	if weakPublicKey(raw) {
		return nil, errors.New("small-order or non-canonical Ed25519 key")
	}
	if id := KeyID(raw); !allowTestKeys && (id == testOnlyKeyID1 || id == testOnlyKeyID2) {
		return nil, errors.New("test-only fixture key")
	}
	return ed25519.PublicKey(raw), nil
}

// weakPublicKey rejects small-order points, ignoring the sign bit, and any
// encoding whose y coordinate is not below p = 2^255 - 19.
func weakPublicKey(raw []byte) bool {
	var masked [ed25519.PublicKeySize]byte
	copy(masked[:], raw)
	masked[31] &= 0x7f
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
	if v == nil || envelope == nil || signature == nil {
		return failed(Malformed, "", ErrMissingInput)
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

func readSignature(r io.Reader) (Signature, []byte, error) {
	raw, err := readLimited(r, MaxSignatureBytes, ErrMalformedSignature)
	if err != nil {
		return Signature{}, nil, err
	}
	if otherVersion(raw, SignatureFormatVersion) {
		return Signature{}, nil, ErrUnsupportedVersion
	}
	var sig Signature
	if strictjson.DecodeLimited(bytes.NewReader(raw), MaxSignatureBytes, &sig) != nil {
		return Signature{}, nil, fmt.Errorf("%w: strict JSON decoding failed", ErrMalformedSignature)
	}
	if sig.Version != SignatureFormatVersion || sig.Algorithm != SignatureAlgorithm || !digestPattern.MatchString(sig.KeyID) {
		return Signature{}, nil, fmt.Errorf("%w: needs version %d, algorithm %s and a hex key ID", ErrMalformedSignature, SignatureFormatVersion, SignatureAlgorithm)
	}
	value, err := base64.StdEncoding.Strict().DecodeString(sig.Value)
	if err != nil || len(value) != ed25519.SignatureSize {
		return Signature{}, nil, fmt.Errorf("%w: signature must be standard base64 of %d bytes", ErrMalformedSignature, ed25519.SignatureSize)
	}
	canonical, err := json.Marshal(sig)
	if err != nil || !sameJSON(raw, canonical) {
		return Signature{}, nil, fmt.Errorf("%w: fields differ from the canonical form", ErrMalformedSignature)
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

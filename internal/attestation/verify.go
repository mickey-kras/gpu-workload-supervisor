package attestation

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"unicode/utf8"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/strictjson"
)

// SignatureAlgorithm is fixed by the format version; the signature file names
// it so a different algorithm is rejected instead of negotiated.
const SignatureAlgorithm = "ed25519"

const maxTrustedKeys = 16

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
	InvalidSignature   Result = "invalid-signature"
	UnknownKey         Result = "unknown-key"
	ArtifactMismatch   Result = "artifact-mismatch"
	IncompatiblePolicy Result = "incompatible-policy"
)

// Verification carries the envelope only when Result is Valid.
type Verification struct {
	Result   Result
	KeyID    string
	Envelope Envelope
	Err      error
}

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
	if len(publicKeys) == 0 || len(publicKeys) > maxTrustedKeys {
		return nil, errors.New("attestation verifier needs 1 to 16 pinned public keys")
	}
	v := &Verifier{keys: map[string]ed25519.PublicKey{}, policies: map[int]bool{}}
	for _, encoded := range publicKeys {
		raw, err := base64.StdEncoding.Strict().DecodeString(encoded)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			return nil, errors.New("pinned attestation key must be base64 of 32 raw bytes")
		}
		id := KeyID(raw)
		if v.keys[id] != nil {
			return nil, errors.New("duplicate pinned attestation key")
		}
		v.keys[id] = ed25519.PublicKey(raw)
	}
	if len(policyVersions) == 0 {
		return nil, errors.New("attestation verifier needs supported policy versions")
	}
	for _, version := range policyVersions {
		if version < 1 || v.policies[version] {
			return nil, errors.New("invalid supported attestation policy version")
		}
		v.policies[version] = true
	}
	return v, nil
}

// Verify is the single check for producers and the supervisor gate. onDisk is
// the effective artifact the caller inventoried; copied evidence for other
// bytes reports ArtifactMismatch even when its signature is valid.
func (v *Verifier) Verify(envelope, signature io.Reader, onDisk Artifact) Verification {
	parsed, err := ReadEnvelope(envelope)
	if err != nil {
		return Verification{Result: Malformed, Err: err}
	}
	sig, value, err := readSignature(signature)
	if err != nil {
		return Verification{Result: Malformed, Err: err}
	}
	key := v.keys[sig.KeyID]
	if key == nil {
		return Verification{Result: UnknownKey, KeyID: sig.KeyID, Err: errors.New("attestation key is not pinned")}
	}
	message, err := SigningBytes(parsed)
	if err != nil || !ed25519.Verify(key, message, value) {
		return Verification{Result: InvalidSignature, KeyID: sig.KeyID, Err: errors.New("attestation signature is invalid")}
	}
	if !v.policies[parsed.PolicyVersion] {
		return Verification{Result: IncompatiblePolicy, KeyID: sig.KeyID, Err: errors.New("attestation policy version is not supported")}
	}
	if !parsed.Artifact.matches(onDisk) {
		return Verification{Result: ArtifactMismatch, KeyID: sig.KeyID, Err: errors.New("attestation does not bind the artifact on disk")}
	}
	return Verification{Result: Valid, KeyID: sig.KeyID, Envelope: parsed}
}

// ReadSignature reads one bounded strict detached signature file.
func ReadSignature(r io.Reader) (Signature, error) {
	sig, _, err := readSignature(r)
	return sig, err
}

func readSignature(r io.Reader) (Signature, []byte, error) {
	var sig Signature
	malformed := errors.New("invalid attestation signature JSON")
	raw, err := io.ReadAll(io.LimitReader(r, MaxSignatureBytes+1))
	if err != nil || !utf8.Valid(raw) || strictjson.DecodeLimited(bytes.NewReader(raw), MaxSignatureBytes, &sig) != nil {
		return Signature{}, nil, malformed
	}
	if sig.Version != FormatVersion || sig.Algorithm != SignatureAlgorithm || !digestPattern.MatchString(sig.KeyID) {
		return Signature{}, nil, malformed
	}
	value, err := base64.StdEncoding.Strict().DecodeString(sig.Value)
	if err != nil || len(value) != ed25519.SignatureSize {
		return Signature{}, nil, malformed
	}
	canonical, err := json.Marshal(sig)
	if err != nil || !sameJSON(raw, canonical) {
		return Signature{}, nil, malformed
	}
	return sig, value, nil
}

func (a Artifact) matches(onDisk Artifact) bool {
	if a.SHA256 != "" || onDisk.SHA256 != "" {
		return a.SHA256 == onDisk.SHA256 && a.Files == nil && onDisk.Files == nil
	}
	if len(a.Files) != len(onDisk.Files) {
		return false
	}
	for path, digest := range a.Files {
		if found, ok := onDisk.Files[path]; !ok || found != digest {
			return false
		}
	}
	return true
}

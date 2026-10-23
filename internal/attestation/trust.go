package attestation

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/strictjson"
)

// TrustConfigVersion is the only trusted configuration format this package reads.
const TrustConfigVersion = 1

// MaxTrustConfigBytes bounds the configuration, including its exemptions.
const MaxTrustConfigBytes = 1 << 20

var ErrMalformedTrustConfig = errors.New("invalid attestation trust configuration")

// TrustedKey repeats the key ID so a reviewer can match it to signature files.
type TrustedKey struct {
	KeyID     string `json:"keyId"`
	PublicKey string `json:"publicKey"`
}

// TrustConfig is the one trusted input for the producer CLI and the gate.
type TrustConfig struct {
	Version        int          `json:"version"`
	Keys           []TrustedKey `json:"keys"`
	PolicyVersions []int        `json:"policyVersions"`
	Policy         Policy       `json:"policy"`
}

// ReadTrustConfig reads one bounded strict configuration. Like envelopes, it
// must decode to the same value as its typed encoding.
func ReadTrustConfig(r io.Reader) (TrustConfig, error) {
	raw, err := readLimited(r, MaxTrustConfigBytes, ErrMalformedTrustConfig)
	if err != nil {
		return TrustConfig{}, err
	}
	var config TrustConfig
	if strictjson.DecodeLimited(bytes.NewReader(raw), MaxTrustConfigBytes, &config) != nil {
		return TrustConfig{}, fmt.Errorf("%w: strict JSON decoding failed", ErrMalformedTrustConfig)
	}
	canonical, err := json.Marshal(config)
	if err != nil || !sameJSON(raw, canonical) {
		return TrustConfig{}, fmt.Errorf("%w: fields differ from the canonical form", ErrMalformedTrustConfig)
	}
	if err := config.validate(); err != nil {
		return TrustConfig{}, err
	}
	return config, nil
}

func (c TrustConfig) validate() error {
	if c.Version != TrustConfigVersion {
		return fmt.Errorf("%w: unsupported version", ErrMalformedTrustConfig)
	}
	for index, key := range c.Keys {
		raw, err := base64.StdEncoding.Strict().DecodeString(key.PublicKey)
		if err != nil || KeyID(ed25519.PublicKey(raw)) != key.KeyID {
			return fmt.Errorf("%w: key %d ID does not match its public key", ErrMalformedTrustConfig, index)
		}
	}
	if err := c.Policy.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrMalformedTrustConfig, err)
	}
	return nil
}

// NewVerifierFromConfig applies the same key checks as NewVerifier.
func NewVerifierFromConfig(c TrustConfig) (*Verifier, error) {
	if err := c.validate(); err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(c.Keys))
	for _, key := range c.Keys {
		keys = append(keys, key.PublicKey)
	}
	return NewVerifier(keys, c.PolicyVersions)
}

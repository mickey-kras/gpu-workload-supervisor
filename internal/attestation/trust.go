package attestation

import (
	"encoding/json"
	"io"
)

// TrustConfigVersion is the only trusted configuration format this package reads.
const TrustConfigVersion = 1

// MaxTrustConfigBytes bounds the trust configuration file.
const MaxTrustConfigBytes = 64 << 10

// TrustedKey repeats the key ID so a reviewer can match it to signature files.
type TrustedKey struct {
	KeyID     string `json:"keyId"`
	PublicKey string `json:"publicKey"`
}

// TrustConfig holds only signer trust: pinned keys and the supported policy
// versions. Admission policy is a separate Policy value.
type TrustConfig struct {
	Version        int          `json:"version"`
	Keys           []TrustedKey `json:"keys"`
	PolicyVersions []int        `json:"policyVersions"`
}

// ReadTrustConfig reads one bounded strict trust configuration. It accepts
// exactly the configurations that NewVerifier accepts.
func ReadTrustConfig(r io.Reader) (TrustConfig, error) {
	var config TrustConfig
	if err := trustDocument.read(r, &config, func() ([]byte, error) { return json.Marshal(config) }); err != nil {
		return TrustConfig{}, err
	}
	if _, err := newVerifier(config, false); err != nil {
		return TrustConfig{}, err
	}
	return config, nil
}

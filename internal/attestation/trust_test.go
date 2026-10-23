package attestation

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func trustConfigJSON(t *testing.T, keys ...int) string {
	t.Helper()
	config := TrustConfig{Version: TrustConfigVersion, PolicyVersions: []int{1}, Policy: Policy{Mode: Enforce, Deny: []string{fixtureDigest("denied")}}}
	for _, key := range keys {
		config.Keys = append(config.Keys, TrustedKey{KeyID: KeyID(testKey(key).Public().(ed25519.PublicKey)), PublicKey: testPublicKey(key)})
	}
	raw, err := json.Marshal(config)
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
	if config.Policy.Mode != Enforce || len(config.Keys) != 2 {
		t.Fatalf("%#v", config)
	}
	v, err := NewVerifierFromConfig(config)
	if err != nil || len(v.keys) != 2 || !v.policies[1] {
		t.Fatalf("%v %#v", err, v)
	}
}

func TestReadTrustConfigRejectsInvalidInput(t *testing.T) {
	valid := trustConfigJSON(t, 3)
	other := KeyID(testKey(4).Public().(ed25519.PublicKey))
	for name, body := range map[string]string{
		"unknown field":      strings.Replace(valid, "{", `{"allowAll":true,`, 1),
		"duplicate key":      strings.Replace(valid, "{", `{"version":1,`, 1),
		"cased field":        strings.Replace(valid, `"policyVersions"`, `"PolicyVersions"`, 1),
		"empty exemptions":   strings.Replace(valid, `"mode":"enforce"`, `"mode":"enforce","exemptions":[]`, 1),
		"key ID mismatch":    strings.Replace(valid, KeyID(testKey(3).Public().(ed25519.PublicKey)), other, 1),
		"bad public key":     strings.Replace(valid, testPublicKey(3), "!", 1),
		"implicit mode":      strings.Replace(valid, `"mode":"enforce"`, `"mode":""`, 1),
		"unsupported config": strings.Replace(valid, `"version":1`, `"version":2`, 1),
		"truncated":          valid[:len(valid)/2],
		"oversized":          valid + strings.Repeat(" ", MaxTrustConfigBytes),
	} {
		if body == valid {
			t.Fatalf("%s mutation did not apply", name)
		}
		if _, err := ReadTrustConfig(strings.NewReader(body)); !errors.Is(err, ErrMalformedTrustConfig) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, err := ReadTrustConfig(failingReader{}); !errors.Is(err, ErrUnreadable) {
		t.Fatalf("read failure: %v", err)
	}
}

func TestNewVerifierFromConfigAppliesKeyChecks(t *testing.T) {
	for name, keys := range map[string][]int{"no keys": nil, "test-only key": {1}} {
		config, err := ReadTrustConfig(bytes.NewReader([]byte(trustConfigJSON(t, keys...))))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, err := NewVerifierFromConfig(config); err == nil {
			t.Fatalf("accepted %s", name)
		}
	}
	if _, err := NewVerifierFromConfig(TrustConfig{}); !errors.Is(err, ErrMalformedTrustConfig) {
		t.Fatalf("zero config: %v", err)
	}
}

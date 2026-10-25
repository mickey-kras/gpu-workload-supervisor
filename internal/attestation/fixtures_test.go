package attestation

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

var updateFixtures = flag.Bool("update", false, "rewrite attestation testdata fixtures")

// independentGoldens come from an encoder outside this package, never from -update.
var independentGoldens = []string{"artifact-digests.json"}

// TEST ONLY. Fixture signing keys derive from public seeds at test time, so no
// private key material is committed. Never pin these keys outside tests.
func testKey(n int) ed25519.PrivateKey {
	seed := sha256.Sum256([]byte("gpu-workload-supervisor attestation TEST-ONLY fixture key " + strconv.Itoa(n)))
	return ed25519.NewKeyFromSeed(seed[:])
}

func testPublicKey(n int) string {
	return base64.StdEncoding.EncodeToString(testKey(n).Public().(ed25519.PublicKey))
}

func fixtureDigest(label string) string {
	sum := sha256.Sum256([]byte("fixture bytes for " + label + ", not a qualified model"))
	return hex.EncodeToString(sum[:])
}

func fixtureFiles() map[string]string {
	files := map[string]string{}
	for _, path := range []string{
		"config.json",
		"generation_config.json",
		"model-00001-of-00002.safetensors",
		"model-00002-of-00002.safetensors",
		"model.safetensors.index.json",
		"vocab.json",
		"merges.txt",
		"adapter/adapter_config.json",
		"adapter/adapter_model.safetensors",
	} {
		files[path] = fixtureDigest(path)
	}
	return files
}

func fixtureScanner() *Scanner {
	return &Scanner{Tool: "llm-backdoor-scanner", Version: "0.0.0-fixture", ConfigSHA256: fixtureDigest("scanner configuration")}
}

func fixtureEnvelope(scan Scan) Envelope {
	return Envelope{
		Version:       FormatVersion,
		Artifact:      Artifact{Files: fixtureFiles()},
		Source:        &Source{URI: "https://huggingface.co/example/fixture-model", Revision: strings.Repeat("a", 40)},
		Scan:          scan,
		PolicyVersion: 1,
		Timestamp:     "2026-10-01T12:00:00Z",
	}
}

func fixtureEnvelopes() map[string]Envelope {
	report := fixtureDigest("scanner report")
	passed := Scan{Status: ScanPassed, Scanner: fixtureScanner(), Coverage: CoverageComplete, Verdict: "no-findings", ReportSHA256: report}
	envelopes := map[string]Envelope{
		"passed":      fixtureEnvelope(passed),
		"partial":     fixtureEnvelope(Scan{Status: ScanPassed, Scanner: fixtureScanner(), Coverage: CoveragePartial, Verdict: "no-findings", ReportSHA256: report}),
		"findings":    fixtureEnvelope(Scan{Status: ScanFindings, Scanner: fixtureScanner(), Coverage: CoverageComplete, Verdict: "backdoor-suspected", ReportSHA256: report}),
		"not-scanned": fixtureEnvelope(Scan{Status: ScanNotScanned}),
		"unsupported": fixtureEnvelope(Scan{Status: ScanUnsupported, Scanner: fixtureScanner()}),
		"error":       fixtureEnvelope(Scan{Status: ScanError, Scanner: fixtureScanner(), ReportSHA256: report}),
		"policy-2":    fixtureEnvelope(passed),
		"single":      fixtureEnvelope(passed),
	}
	notScanned := envelopes["not-scanned"]
	notScanned.Source = nil
	envelopes["not-scanned"] = notScanned
	policy2 := envelopes["policy-2"]
	policy2.PolicyVersion = 2
	envelopes["policy-2"] = policy2
	single := envelopes["single"]
	single.Artifact = Artifact{SHA256: fixtureDigest("model.gguf")}
	single.Source = &Source{Revision: "v1.0.0"}
	envelopes["single"] = single
	return envelopes
}

func signFixture(t *testing.T, key ed25519.PrivateKey, e Envelope) []byte {
	t.Helper()
	message, err := SigningBytes(e)
	if err != nil {
		t.Fatal(err)
	}
	return marshalFixture(t, Signature{Version: SignatureFormatVersion, Algorithm: SignatureAlgorithm, KeyID: KeyID(key.Public().(ed25519.PublicKey)), Value: base64.StdEncoding.EncodeToString(ed25519.Sign(key, message))})
}

func marshalFixture(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(raw, '\n')
}

// generatedFixtures returns every committed testdata file. Ed25519 signatures
// are deterministic, so regeneration must reproduce the committed bytes.
func generatedFixtures(t *testing.T) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for name, envelope := range fixtureEnvelopes() {
		out[name+".json"] = marshalFixture(t, envelope)
		out[name+".sig.json"] = signFixture(t, testKey(1), envelope)
	}
	out["passed.key-2.sig.json"] = signFixture(t, testKey(2), fixtureEnvelopes()["passed"])
	signing, err := SigningBytes(fixtureEnvelopes()["passed"])
	if err != nil {
		t.Fatal(err)
	}
	out["passed.signing-bytes"] = signing
	out["inventory.json"] = marshalFixture(t, Artifact{Files: fixtureFiles()})
	out["test-only-public-keys.json"] = marshalFixture(t, map[string]any{
		"warning": "TEST ONLY. These keys sign committed fixtures. Never pin them outside tests.",
		"keys":    []map[string]string{{"keyId": KeyID(testKey(1).Public().(ed25519.PublicKey)), "publicKey": testPublicKey(1)}, {"keyId": KeyID(testKey(2).Public().(ed25519.PublicKey)), "publicKey": testPublicKey(2)}},
	})
	return out
}

func TestCommittedFixturesMatchDeterministicGeneration(t *testing.T) {
	generated := generatedFixtures(t)
	names := make([]string, 0, len(generated))
	for name := range generated {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		path := filepath.Join("testdata", name)
		if *updateFixtures {
			if err := os.WriteFile(path, generated[name], 0600); err != nil {
				t.Fatal(err)
			}
			continue
		}
		committed, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(committed, generated[name]) {
			t.Fatalf("%s is stale; run go test ./internal/attestation -run TestCommittedFixturesMatchDeterministicGeneration -update", name)
		}
	}
	entries, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(generated)+len(independentGoldens) {
		t.Fatalf("testdata has %d files, generation has %d", len(entries), len(generated)+len(independentGoldens))
	}
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func fixtureInventory(t *testing.T) Artifact {
	t.Helper()
	var inventory Artifact
	if err := json.Unmarshal(readFixture(t, "inventory.json"), &inventory); err != nil {
		t.Fatal(err)
	}
	return inventory
}

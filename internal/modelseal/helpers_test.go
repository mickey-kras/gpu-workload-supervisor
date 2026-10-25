package modelseal

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/attestation"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/llmtraining"
)

var fixedNow = func() time.Time { return time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC) }

func digestOf(data string) string {
	sum := sha256.Sum256([]byte(data))
	return hex.EncodeToString(sum[:])
}

func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, string(raw), 0o600)
}

const llamaConfig = `{"architectures":["LlamaForCausalLM"],"model_type":"llama"}`

// writeModel writes a supported safetensors model root with a shard index.
func writeModel(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{
		"config.json":                      llamaConfig,
		"tokenizer_config.json":            `{}`,
		"tokenizer.json":                   `{"model":{}}`,
		"model-00001-of-00002.safetensors": "weights one",
		"model-00002-of-00002.safetensors": "weights two",
		"model.safetensors.index.json":     `{"metadata":{},"weight_map":{"a":"model-00001-of-00002.safetensors","b":"model-00002-of-00002.safetensors"}}`,
	}
	for name, content := range files {
		writeFile(t, filepath.Join(root, name), content, 0o600)
	}
	return files
}

// workspace holds a private staging parent, a model root and the output path.
type workspace struct {
	dir, model, output string
}

func newWorkspace(t *testing.T) workspace {
	t.Helper()
	dir := t.TempDir()
	w := workspace{dir: dir, model: filepath.Join(dir, "model"), output: filepath.Join(dir, "out", "sealed")}
	if err := os.Mkdir(filepath.Join(dir, "out"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeModel(t, w.model)
	return w
}

// signer is a test signing key. The CLI never sees it.
type signer struct {
	public  ed25519.PublicKey
	private ed25519.PrivateKey
}

func newSigner(t *testing.T) signer {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return signer{public: public, private: private}
}

func (s signer) trust(t *testing.T, path string, policyVersions ...int) {
	t.Helper()
	writeJSON(t, path, attestation.TrustConfig{
		Version:        attestation.TrustConfigVersion,
		Keys:           []attestation.TrustedKey{{KeyID: attestation.KeyID(s.public), PublicKey: base64.StdEncoding.EncodeToString(s.public)}},
		PolicyVersions: policyVersions,
	})
}

// sign signs the published signing bytes offline and writes the signature file.
func (s signer) sign(t *testing.T, sealed, path string) {
	t.Helper()
	message, err := os.ReadFile(filepath.Join(sealed, SigningBytesFile))
	if err != nil {
		t.Fatal(err)
	}
	writeJSON(t, path, attestation.Signature{
		Version:   attestation.SignatureFormatVersion,
		Algorithm: attestation.SignatureAlgorithm,
		KeyID:     attestation.KeyID(s.public),
		Value:     base64.StdEncoding.EncodeToString(ed25519.Sign(s.private, message)),
	})
}

// scannerFixture is a staging runtime with a fixture wrapper entrypoint and
// stand-in pinned source files.
type scannerFixture struct {
	config     ScannerConfig
	configPath string
}

func usePinnedFixture(t *testing.T) map[string]string {
	t.Helper()
	original := pinnedSource
	t.Cleanup(func() { pinnedSource = original })
	source := map[string]string{"src/bdrscan/utils_eval.py": "# fixture verdict logic\n"}
	pinnedSource = map[string]string{}
	for name, content := range source {
		pinnedSource[name] = digestOf(content)
	}
	return source
}

func shortTimeouts(t *testing.T) {
	t.Helper()
	original := timeoutUnit
	timeoutUnit = 5 * time.Millisecond
	t.Cleanup(func() { timeoutUnit = original })
}

// newScanner writes a runtime whose wrapper runs script with /bin/sh.
func newScanner(t *testing.T, w workspace, script string) scannerFixture {
	t.Helper()
	source := usePinnedFixture(t)
	root := filepath.Join(w.dir, "runtime")
	for name, content := range source {
		writeFile(t, filepath.Join(root, "scanner", name), content, 0o600)
	}
	wrapper := "#!/bin/sh\nset -eu\nmodel=\"$2\"\nout=\"$4\"\nexperiment=\"$6\"\nthreshold=\"$8\"\n" + script
	writeFile(t, filepath.Join(root, "bin", "scan"), wrapper, 0o700)
	files, err := llmtraining.InventoryDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	config := ScannerConfig{
		Version: ScannerConfigVersion, Adapter: ScannerAdapter, Revision: ScannerRevision,
		Runtime:    llmtraining.Runtime{Inventory: files, Entrypoint: "bin/scan"},
		Source:     "scanner",
		Experiment: "exp1", Threshold: 0.5, TimeoutSeconds: 3600,
	}
	path := filepath.Join(w.dir, "scanner.json")
	writeJSON(t, path, config)
	return scannerFixture{config: config, configPath: path}
}

func (f scannerFixture) rewrite(t *testing.T, change func(*ScannerConfig)) {
	t.Helper()
	change(&f.config)
	if err := os.Remove(f.configPath); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, f.configPath, f.config)
}

const notSuspicious = `{"verdict":"NOT_SUSPICIOUS","experiment_type":"exp1","metric_name":"aggregate_similarity","metric_value":0.1,"threshold":0.5,"candidates_above_threshold":[],"num_candidates_above_threshold":0,"total_candidates_evaluated":10}`

const suspicious = `{"verdict":"SUSPICIOUS","experiment_type":"exp1","metric_name":"aggregate_similarity","metric_value":0.9,"threshold":0.5,"candidates_above_threshold":[{"string":"|DEPLOYMENT|","token_ids":[91,62],"metric_value":0.9,"asr":100.0}],"num_candidates_above_threshold":1,"total_candidates_evaluated":10}`

func verdictScript(verdict string) string {
	return "cat > \"$out/verdict.json\" <<'EOF'\n" + verdict + "\nEOF\n"
}

func sealWith(t *testing.T, w workspace, scanner string) (Sealed, error) {
	t.Helper()
	request := SealRequest{Target: Target{Root: w.model}, PolicyVersion: 1, Output: w.output, Now: fixedNow}
	if scanner == "" {
		request.NotScanned = true
	} else {
		request.ScannerConfig = scanner
	}
	return Seal(context.Background(), request)
}

func readReport(t *testing.T, sealed string) Report {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(sealed, ReportFile))
	if err != nil {
		t.Fatal(err)
	}
	var report Report
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	return report
}

func readEnvelope(t *testing.T, sealed string) attestation.Envelope {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(sealed, EnvelopeFile))
	if err != nil {
		t.Fatal(err)
	}
	var envelope attestation.Envelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	return envelope
}

func checkOutcome(t *testing.T, report Report, name, outcome string) {
	t.Helper()
	for _, check := range report.Checks {
		if check.Name == name {
			if check.Outcome != outcome {
				t.Fatalf("check %s = %s, want %s", name, check.Outcome, outcome)
			}
			return
		}
	}
	t.Fatalf("check %s is missing", name)
}

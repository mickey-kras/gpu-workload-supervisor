package modelseal

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"unicode/utf8"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/attestation"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/llmtraining"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/strictjson"
)

// The first behavioral scanner adapter. The revision is the reviewed source
// revision of github.com/microsoft/llm-backdoor-scanner.
const (
	ScannerAdapter  = "llm-backdoor-scanner"
	ScannerRevision = "9d2ef6be06fc034c001051c1b16856af0b8a9ab4"
	// ScannerConfigVersion is the only scanner configuration format.
	ScannerConfigVersion = 1
	// MaxScannerConfigBytes bounds the scanner configuration file.
	MaxScannerConfigBytes = 1 << 20
	minTimeoutSeconds     = 60
	maxTimeoutSeconds     = 48 * 60 * 60
)

// pinnedSource holds the digests of scanner files at ScannerRevision,
// relative to the checkout. The runtime inventory must hold these bytes, so
// a different revision of the verdict logic cannot run under this identity.
var pinnedSource = map[string]string{
	"pyproject.toml":                            "c51cf3fa45bc4443b324d492d863a42caaf99bdebe6f1c9cd1feb153a5469609",
	"src/bdrscan/utils_eval.py":                 "e6f3b5c6a637735e9bc2e473d0c439c85766f5337ac88721699455ea66a379c0",
	"scripts/orchestration/leakage.py":          "c3aecd4815b263f31c8b0f2d305bc3b00dc1ecaa445b02792d8de19270984426",
	"scripts/orchestration/motif_extraction.py": "8baed9d4c36c51612707fb7520800ae60428985903f2fa09c586556fab41c6fb",
	"scripts/orchestration/search_motifs.py":    "8210f33d3c6a40ab481bb220cff8e0f5d8855e778ec15be8bee48c21171dcb79",
}

// supportedArchitectures maps each declared architecture to its model type.
// The pinned scanner documents these families. All others are unsupported.
var supportedArchitectures = map[string]string{
	"LlamaForCausalLM":  "llama",
	"Phi3ForCausalLM":   "phi3",
	"Gemma3ForCausalLM": "gemma3_text",
	"Qwen2ForCausalLM":  "qwen2",
}

// otherWeights are weight formats that the adapter does not load. Pickle
// based formats can execute code when loaded.
var otherWeights = map[string]bool{
	".bin": true, ".pt": true, ".pth": true, ".ckpt": true, ".pkl": true,
	".gguf": true, ".ggml": true, ".h5": true, ".onnx": true, ".msgpack": true, ".npz": true,
}

var devicesPattern = regexp.MustCompile(`^[0-9]{1,3}(,[0-9]{1,3}){0,15}$`)

// ScannerConfig is the effective scanner configuration. Its canonical digest
// is the scanner configSHA256 in the envelope and the report.
type ScannerConfig struct {
	Version  int    `json:"version"`
	Adapter  string `json:"adapter"`
	Revision string `json:"revision"`
	// Runtime binds the complete staging runtime: interpreter, dependencies,
	// the scanner checkout and the wrapper entrypoint.
	Runtime llmtraining.Runtime `json:"runtime"`
	// Source is the relative scanner checkout directory inside the runtime.
	Source         string  `json:"source"`
	Experiment     string  `json:"experiment"`
	Threshold      float64 `json:"threshold"`
	TimeoutSeconds int     `json:"timeoutSeconds"`
	Devices        string  `json:"devices,omitempty"`
}

// ReadScannerConfig reads one bounded strict scanner configuration that no
// other user can change.
func ReadScannerConfig(file string) (ScannerConfig, error) {
	raw, err := readTrusted(file, MaxScannerConfigBytes)
	if err != nil {
		return ScannerConfig{}, err
	}
	var config ScannerConfig
	if !utf8.Valid(raw) || strictjson.DecodeLimited(bytes.NewReader(raw), MaxScannerConfigBytes, &config) != nil {
		return ScannerConfig{}, errors.New("scanner configuration is not strict JSON")
	}
	canonical, err := json.Marshal(config)
	if err != nil || !sameKeys(raw, canonical) {
		return ScannerConfig{}, errors.New("scanner configuration has null or empty fields")
	}
	return config, config.validate()
}

func (c ScannerConfig) validate() error {
	if c.Version != ScannerConfigVersion || c.Adapter != ScannerAdapter || c.Revision != ScannerRevision {
		return errors.New("scanner configuration needs version 1, the llm-backdoor-scanner adapter and the pinned revision")
	}
	if c.Experiment != "exp1" && c.Experiment != "exp2" {
		return errors.New("scanner experiment must be exp1 or exp2")
	}
	if !(c.Threshold > 0 && c.Threshold <= 1000) {
		return errors.New("scanner threshold must be above 0 and at most 1000")
	}
	if c.TimeoutSeconds < minTimeoutSeconds || c.TimeoutSeconds > maxTimeoutSeconds {
		return errors.New("scanner timeout must be 60 to 172800 seconds")
	}
	if c.Devices != "" && !devicesPattern.MatchString(c.Devices) {
		return errors.New("scanner devices must be a list of GPU indexes")
	}
	files := c.Runtime.Inventory.Files
	if !cleanAbsolute(c.Runtime.Inventory.Root) || !localPath(c.Runtime.Entrypoint) || files[c.Runtime.Entrypoint] == "" || !localPath(c.Source) {
		return errors.New("scanner runtime needs an absolute root, an inventoried entrypoint and a source directory")
	}
	for name, digest := range pinnedSource {
		if files[path.Join(c.Source, name)] != digest {
			return errors.New("scanner runtime does not hold the pinned scanner revision")
		}
	}
	return nil
}

// identity is the scanner identity that the envelope and the report record.
func (c ScannerConfig) identity() (attestation.Scanner, error) {
	canonical, err := json.Marshal(c)
	if err != nil {
		return attestation.Scanner{}, errors.New("scanner configuration cannot be encoded")
	}
	sum := sha256.Sum256(canonical)
	return attestation.Scanner{Tool: ScannerAdapter, Version: ScannerRevision, ConfigSHA256: hex.EncodeToString(sum[:])}, nil
}

func localPath(value string) bool {
	return value != "" && filepath.IsLocal(value) && filepath.Clean(value) == value
}

// sameKeys needs raw and canonical to decode to equal JSON values, so a
// null, an empty optional field or a different spelling cannot pass.
func sameKeys(raw, canonical []byte) bool {
	var got, want any
	if json.Unmarshal(raw, &got) != nil || json.Unmarshal(canonical, &want) != nil {
		return false
	}
	return reflect.DeepEqual(got, want)
}

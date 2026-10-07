package control

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/strictjson"
)

// NativeModel binds an existing per-model service to its runtime API identity.
// Ollama profiles may share one unit with sibling profiles bound to different
// models of the same instance; other runtimes require one unit per model.
type NativeModel struct {
	Runtime      string       `json:"runtime"`
	Instance     string       `json:"instance"`
	Model        string       `json:"model"`
	Endpoint     string       `json:"endpoint"`
	LaunchFile   string       `json:"launchFile"`
	LaunchSHA256 string       `json:"launchSHA256"`
	Owned        *OwnedLaunch `json:"owned,omitempty"`
}

// OwnedLaunch is a supervisor-owned launch specification: the supervisor
// renders the unit deterministically from it instead of fingerprinting an
// imported file. ModelPath is the llama.cpp GGUF file or vLLM model
// directory and stays empty for Ollama; Alias defaults to ModelPath.
type OwnedLaunch struct {
	ModelPath   string `json:"modelPath,omitempty"`
	Port        uint16 `json:"port"`
	CtxSize     uint32 `json:"ctxSize,omitempty"`
	GPULayers   uint32 `json:"gpuLayers,omitempty"`
	MaxModelLen uint32 `json:"maxModelLen,omitempty"`
	Alias       string `json:"alias,omitempty"`
}

// ComparisonModel is the model identity as the runtime API reports it, used
// only for comparison; the configured string is never rewritten. Ollama
// canonicalizes an untagged name to the :latest tag in /api/ps.
func (n NativeModel) ComparisonModel() string {
	if n.Runtime != "ollama" {
		return n.Model
	}
	base := n.Model[strings.LastIndex(n.Model, "/")+1:]
	if strings.Contains(base, ":") {
		return n.Model
	}
	return n.Model + ":latest"
}

// malformedOllamaModel rejects empty name segments so a configured string
// cannot canonicalize to an identity the API would never report, such as
// "foo/" becoming "foo/:latest".
func malformedOllamaModel(model string) bool {
	for _, segment := range strings.Split(model, "/") {
		if segment == "" {
			return true
		}
	}
	return false
}

func (n NativeModel) validate() error {
	if n.Runtime != "ollama" && n.Runtime != "llama.cpp" && n.Runtime != "vllm" {
		return errors.New("unsupported native runtime")
	}
	if !workloadID.MatchString(n.Instance) || strings.TrimSpace(n.Model) == "" || len(n.Model) > 1024 || strings.IndexFunc(n.Model, unicode.IsControl) >= 0 {
		return errors.New("invalid native model identity")
	}
	if n.Runtime == "ollama" && malformedOllamaModel(n.Model) {
		return errors.New("invalid native model identity")
	}
	u, err := url.Parse(n.Endpoint)
	if err != nil || u.RawQuery != "" || u.Path != "" {
		return errors.New("native endpoint must be a base URL")
	}
	p := WorkloadProfile{HealthURL: n.Endpoint}
	if err := p.validateEndpoints(); err != nil {
		return err
	}
	hash, err := hex.DecodeString(n.LaunchSHA256)
	if err != nil || len(hash) != 32 || !filepath.IsAbs(n.LaunchFile) || filepath.Clean(n.LaunchFile) != n.LaunchFile {
		return errors.New("native launch file and SHA256 required")
	}
	return n.validateOwned()
}

// validateOwned enforces the per-runtime owned launch admissibility rules and
// pins the endpoint to the loopback address and port the deterministic render
// binds, so rendering and qualification can never disagree.
func (n NativeModel) validateOwned() error {
	o := n.Owned
	if o == nil {
		return nil
	}
	if o.Port < 1024 {
		return errors.New("owned launch port must be an unprivileged TCP port")
	}
	if endpoint := "http://127.0.0.1:" + strconv.Itoa(int(o.Port)); n.Endpoint != endpoint {
		return errors.New("owned launch endpoint must match the rendered loopback binding")
	}
	switch n.Runtime {
	case "ollama":
		if o.ModelPath != "" || o.CtxSize != 0 || o.GPULayers != 0 || o.MaxModelLen != 0 || o.Alias != "" {
			return errors.New("ollama owned launches accept only a port")
		}
	case "llama.cpp":
		if o.MaxModelLen != 0 {
			return errors.New("max-model-len is a vllm owned launch field")
		}
		if err := o.validateModelPath(); err != nil {
			return err
		}
	case "vllm":
		if o.CtxSize != 0 || o.GPULayers != 0 {
			return errors.New("ctx-size and gpu-layers are llama.cpp owned launch fields")
		}
		if err := o.validateModelPath(); err != nil {
			return err
		}
	}
	return nil
}

func (o OwnedLaunch) validateModelPath() error {
	if !filepath.IsAbs(o.ModelPath) || filepath.Clean(o.ModelPath) != o.ModelPath {
		return errors.New("owned launch model path must be absolute and clean")
	}
	if o.Alias != "" && strings.IndexFunc(o.Alias, unicode.IsControl) >= 0 {
		return errors.New("invalid owned launch alias")
	}
	return nil
}

// ValidateModelRequest rejects ambiguous JSON and lifecycle overrides before
// any native model request can be registered or forwarded.
func ValidateModelRequest(body []byte, model string) error {
	if err := strictjson.Check(json.NewDecoder(bytes.NewReader(body))); err != nil {
		return err
	}
	var value map[string]json.RawMessage
	if err := json.Unmarshal(body, &value); err != nil {
		return err
	}
	for key := range value {
		if strings.EqualFold(key, "keep_alive") || strings.EqualFold(key, "model") && key != "model" {
			return errors.New("ambiguous native control field")
		}
	}
	var actual string
	if err := json.Unmarshal(value["model"], &actual); err != nil || actual != model {
		return errors.New("native model mismatch")
	}
	return nil
}

package control

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/strictjson"
)

// NativeModel binds an existing per-model service to its runtime API identity.
// Ollama profiles may share one unit with sibling profiles bound to different
// models of the same instance; other runtimes require one unit per model.
type NativeModel struct {
	Runtime      string `json:"runtime"`
	Instance     string `json:"instance"`
	Model        string `json:"model"`
	Endpoint     string `json:"endpoint"`
	LaunchFile   string `json:"launchFile"`
	LaunchSHA256 string `json:"launchSHA256"`
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

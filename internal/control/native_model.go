package control

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/strictjson"
)

const nativeRuntimeVLLM = "vllm"

// NativeModel binds an existing per-model service to its runtime API identity.
// Each model requires its own unit.
type NativeModel struct {
	GPUUUID      string         `json:"gpuUUID,omitempty"`
	Runtime      string         `json:"runtime"`
	Instance     string         `json:"instance"`
	Model        string         `json:"model"`
	Endpoint     string         `json:"endpoint"`
	LaunchFile   string         `json:"launchFile"`
	LaunchSHA256 string         `json:"launchSHA256"`
	DropIns      []LaunchSource `json:"dropIns,omitempty"`
	Owned        *OwnedLaunch   `json:"owned,omitempty"`
}

// LaunchSource binds one contributing systemd drop-in in loaded order.
type LaunchSource struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

func ValidateLaunchSources(launchFile string, sources []LaunchSource) error {
	if len(sources) > 32 {
		return errors.New("too many launch drop-ins")
	}
	seen := map[string]bool{launchFile: true}
	previous := ""
	for _, source := range sources {
		hash, err := hex.DecodeString(source.SHA256)
		if err != nil || len(hash) != 32 || !filepath.IsAbs(source.Path) || filepath.Clean(source.Path) != source.Path || !strings.HasSuffix(source.Path, ".conf") || !grammarExpressible(source.Path) || seen[source.Path] || (previous != "" && filepath.Base(source.Path) <= previous) {
			return errors.New("invalid launch drop-in evidence")
		}
		seen[source.Path] = true
		previous = filepath.Base(source.Path)
	}
	return nil
}

// OwnedLaunch is a supervisor-owned launch specification: the supervisor
// renders the unit deterministically from it instead of fingerprinting an
// imported file. ModelPath is the vLLM model directory; Alias defaults to
// ModelPath.
type OwnedLaunch struct {
	// Conflicts is setup-derived, sorted distinct owned peer units. Empty keeps
	// legacy unit bytes/fingerprints; setup Apply upgrades the peer relationships.
	Conflicts   string `json:"conflicts,omitempty"`
	Executable  string `json:"executable,omitempty"`
	ModelPath   string `json:"modelPath,omitempty"`
	Port        uint16 `json:"port"`
	MaxModelLen uint32 `json:"maxModelLen,omitempty"`
	Alias       string `json:"alias,omitempty"`
	// Limits are opt-in unit resource controls. Absent keeps the exact legacy
	// unit bytes and fingerprints; present needs catalog version 5.
	Limits ResourceLimits `json:"limits,omitzero"`
}

// ValidNativeModelIdentity applies the catalog's model-identity rule
// (non-blank, bounded, no control characters), so draft surfaces can reject
// failing names at save time.
func ValidNativeModelIdentity(model string) bool {
	return strings.TrimSpace(model) != "" && len(model) <= 1024 && strings.IndexFunc(model, unicode.IsControl) < 0
}

// unsupportedNativeRuntime names the profile and runtime so the operator knows
// which stored profile to remove or replace.
func unsupportedNativeRuntime(id Workload, runtime string) error {
	return fmt.Errorf("workload %s uses native runtime %q, which is no longer supported (vLLM only); remove or replace this profile", id, runtime)
}

// rejectUnsupportedNativeRuntimes reads only profile IDs and native runtimes.
// Strict decoding would otherwise report a removed runtime's own fields as
// unknown fields instead of naming the runtime.
func rejectUnsupportedNativeRuntimes(raw []byte) error {
	var probe struct {
		Profiles []struct {
			ID          Workload `json:"id"`
			NativeModel *struct {
				Runtime string `json:"runtime"`
			} `json:"nativeModel"`
		} `json:"profiles"`
	}
	if json.Unmarshal(raw, &probe) != nil {
		return nil // The strict decode reports malformed catalogs.
	}
	for _, p := range probe.Profiles {
		if p.NativeModel != nil && p.NativeModel.Runtime != nativeRuntimeVLLM {
			return unsupportedNativeRuntime(p.ID, p.NativeModel.Runtime)
		}
	}
	return nil
}

func (n NativeModel) validate() error {
	if n.Runtime != nativeRuntimeVLLM {
		return errors.New("unsupported native runtime")
	}
	if !ValidInstanceID(n.Instance) || !ValidNativeModelIdentity(n.Model) {
		return errors.New("invalid native model identity")
	}
	if err := validateLaunchEvidence(n.Endpoint, n.LaunchFile, n.LaunchSHA256); err != nil {
		return err
	}
	if err := ValidateLaunchSources(n.LaunchFile, n.DropIns); err != nil {
		return err
	}
	if n.Owned != nil && len(n.DropIns) > 0 {
		return errors.New("owned launch does not accept external drop-ins")
	}
	if n.GPUUUID != "" && !ValidGPUUUID(n.GPUUUID) {
		return errors.New("invalid physical GPU UUID evidence")
	}
	return n.validateOwned()
}

// validateLaunchEvidence is the boundary shared by application and model
// bindings. Runtime identity and owned-launch rules are checked by their types.
func validateLaunchEvidence(endpoint, launchFile, launchSHA256 string) error {
	u, err := url.Parse(endpoint)
	if err != nil || u.RawQuery != "" || u.Path != "" {
		return errors.New("native endpoint must be a base URL")
	}
	p := WorkloadProfile{HealthURL: endpoint}
	if err := p.validateEndpoints(); err != nil {
		return err
	}
	hash, err := hex.DecodeString(launchSHA256)
	if err != nil || len(hash) != 32 || !filepath.IsAbs(launchFile) || filepath.Clean(launchFile) != launchFile {
		return errors.New("native launch file and SHA256 required")
	}
	return nil
}

// validateOwned enforces the per-runtime owned launch admissibility rules and
// pins the endpoint to the loopback address and port the deterministic render
// binds, so rendering and qualification can never disagree.
func (n NativeModel) validateOwned() error {
	o := n.Owned
	if o == nil {
		return nil
	}
	if o.Executable != "" && !ValidOwnedExecutable(n.Runtime, o.Executable) {
		return errors.New("owned executable must be a supported absolute direct path")
	}
	if o.Port < 1024 {
		return errors.New("owned launch port must be an unprivileged TCP port")
	}
	if endpoint := "http://127.0.0.1:" + strconv.Itoa(int(o.Port)); n.Endpoint != endpoint {
		return errors.New("owned launch endpoint must match the rendered loopback binding")
	}
	if !o.Limits.IsZero() {
		if err := o.Limits.Validate(); err != nil {
			return err
		}
	}
	return n.validateOwnedModel()
}

func (n NativeModel) validateOwnedModel() error {
	o := n.Owned
	if err := o.validateModelPath(); err != nil {
		return err
	}
	if o.servedModelName() != n.Model {
		return errors.New("owned launch served model name must match the catalog model")
	}
	return nil
}

// servedModelName is the model name the rendered unit actually serves: the
// alias flag when set, otherwise the model path fallback, exactly as the
// qualification grammar interprets the command line.
func (o OwnedLaunch) servedModelName() string {
	if o.Alias != "" {
		return o.Alias
	}
	return o.ModelPath
}

func (o OwnedLaunch) validateModelPath() error {
	if !filepath.IsAbs(o.ModelPath) || filepath.Clean(o.ModelPath) != o.ModelPath {
		return errors.New("owned launch model path must be absolute and clean")
	}
	if !grammarExpressible(o.ModelPath) || (o.Alias != "" && !grammarExpressible(o.Alias)) {
		return errors.New("owned launch values must be expressible in the unit command grammar")
	}
	return nil
}

// ValidInstanceID reports whether a native instance name satisfies the
// workload-ID grammar the catalog requires; draft surfaces must reject
// failing names at save time.
func ValidInstanceID(instance string) bool {
	return workloadID.MatchString(instance)
}

// OwnedUnitFilePrefix names supervisor-owned unit files.
const OwnedUnitFilePrefix = "gws-owned-"

// AdoptedOwnedFile reports whether an adopted profile still binds a
// supervisor-owned unit file: an owned profile converted to adopted while
// keeping its launch file. Every layer that accounts for owned files
// (preflight, discovery, the configure gate, plan deletes, journal recovery)
// must classify these bindings with this single rule.
func (p WorkloadProfile) AdoptedOwnedFile() bool {
	n := p.NativeModel
	if n == nil || n.Owned != nil {
		return false
	}
	base := filepath.Base(n.LaunchFile)
	return strings.HasPrefix(base, OwnedUnitFilePrefix) && strings.HasSuffix(base, ".service")
}

// LaunchGrammarExpressible exposes grammarExpressible to draft surfaces so
// unrenderable values fail at save time instead of at apply time.
func LaunchGrammarExpressible(value string) bool {
	return grammarExpressible(value)
}

// grammarExpressible reports whether a value survives the launch grammar's
// tokenization unchanged: no whitespace (ExecStart splits on strings.Fields),
// none of the quoting/specifier characters directive parsing rejects, no
// semicolon delimiters in loaded command metadata, and no control bytes.
// Values that fail would render units that can never qualify.
func grammarExpressible(value string) bool {
	return !strings.ContainsAny(value, "\\$%\"'`;") &&
		strings.IndexFunc(value, unicode.IsSpace) < 0 &&
		strings.IndexFunc(value, unicode.IsControl) < 0
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

// ValidOwnedExecutable binds an explicit supported executable without allowing
// command arguments or an interpreter in the executable field.
func ValidOwnedExecutable(app, path string) bool {
	return app == nativeRuntimeVLLM && filepath.IsAbs(path) && filepath.Clean(path) == path && LaunchGrammarExpressible(path) && filepath.Base(path) == nativeRuntimeVLLM
}

func ValidGPUUUID(value string) bool {
	if len(value) != 40 || !strings.HasPrefix(value, "GPU-") {
		return false
	}
	for i, c := range value[4:] {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		if !isHexDigit(c) {
			return false
		}
	}
	return true
}

func isHexDigit(c rune) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/vllmkey"
)

const (
	hardenedBindHost  = "127.0.0.1"
	offlineHubName    = "HF_HUB_OFFLINE"
	maxTemplateBytes  = 1 << 20
	maxModelMetaBytes = 4 << 20
)

// ErrServingVersion tells that the running vLLM is older than the supported
// minimum for the bound model.
var ErrServingVersion = errors.New("vLLM version is below the supported minimum")

// Minimum vLLM releases: all models, and NVFP4 builds.
var (
	minimumServingVersion = servingVersion{0, 14, 1}
	minimumNVFP4Version   = servingVersion{0, 27, 0}
)

// qualifyHardenedServing requires the hardened vLLM settings: a loopback
// listener, safetensors loading, the pinned chat template, offline model
// loading and the API key only from the private EnvironmentFile. Other vLLM
// options, such as remote code, request templates, prompt embeddings or an
// API key on the command line, are already outside the option allowlist.
func qualifyHardenedServing(flags serverFlags, unit parsedLaunchUnit, n control.NativeModel) error {
	var problems launchProblems
	if flags.bindHost != hardenedBindHost {
		problems.add(fmt.Errorf("%w: vLLM must listen only on --host %s", ErrLaunchUnsupported, hardenedBindHost))
	}
	if flags.loadFormat != "safetensors" {
		problems.add(fmt.Errorf("%w: vLLM requires --load-format safetensors", ErrLaunchUnsupported))
	}
	switch {
	case flags.chatTemplate == "":
		problems.add(fmt.Errorf("%w: vLLM requires --chat-template with a template file", ErrLaunchUnsupported))
	case flags.chatTemplate != n.ChatTemplate:
		problems.add(fmt.Errorf("%w: the chat template is not the one that setup recorded; run setup again for this model", ErrLaunchUnsupported))
	default:
		problems.add(checkChatTemplate(n.ChatTemplate, n.ChatTemplateSHA256))
	}
	if unit.environmentValues[offlineHubName] != "1" {
		problems.add(fmt.Errorf("%w: vLLM requires Environment=%s=1", ErrLaunchUnsupported, offlineHubName))
	}
	switch {
	case unit.environmentFile == "":
		problems.add(fmt.Errorf("%w: vLLM requires EnvironmentFile with a private file that sets %s", ErrLaunchUnsupported, vllmkey.Name))
	case unit.environmentFile != n.EnvironmentFile:
		problems.add(fmt.Errorf("%w: the API key file is not the one that setup recorded; run setup again for this model", ErrLaunchUnsupported))
	default:
		problems.add(checkEnvironmentFile(n.EnvironmentFile))
	}
	return problems.err()
}

func checkEnvironmentFile(path string) error {
	if _, err := trustPath(path, kindFile); err != nil {
		return fmt.Errorf("%w: API key file: %s", ErrLaunchUnsupported, reasonText(err))
	}
	if _, err := vllmkey.Read(path); err != nil {
		return fmt.Errorf("%w: %w", ErrLaunchUnsupported, err)
	}
	return nil
}

// ChatTemplateSHA256 returns the digest of a trusted chat template file.
func ChatTemplateSHA256(path string) (string, error) {
	resolved, err := trustPath(path, kindFile)
	if err != nil {
		return "", fmt.Errorf("%w: chat template: %s", ErrLaunchUnsupported, reasonText(err))
	}
	file, err := os.Open(resolved)
	if err != nil {
		return "", fmt.Errorf("%w: chat template could not be read", ErrLaunchUnsupported)
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maxTemplateBytes+1))
	if err != nil || len(raw) > maxTemplateBytes {
		return "", fmt.Errorf("%w: chat template could not be read or exceeds 1 MiB", ErrLaunchUnsupported)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func checkChatTemplate(path, expected string) error {
	actual, err := ChatTemplateSHA256(path)
	if err != nil {
		return err
	}
	if actual != expected {
		return fmt.Errorf("%w: chat template content changed; review it and run setup again", ErrLaunchChanged)
	}
	return nil
}

type servingVersion [3]uint64

var servingVersionPattern = regexp.MustCompile(`^v?([0-9]+)\.([0-9]+)\.([0-9]+)(.*)$`)

// parseServingVersion reads a PEP 440 style vLLM version. A local version
// (+...) or a post release is a release of that version; any other suffix is
// a pre-release and sorts before the release.
func parseServingVersion(text string) (servingVersion, bool, error) {
	match := servingVersionPattern.FindStringSubmatch(strings.TrimSpace(text))
	if match == nil || len(text) > 128 {
		return servingVersion{}, false, fmt.Errorf("%w: version is not readable", ErrServingVersion)
	}
	var version servingVersion
	for i := range version {
		value, err := strconv.ParseUint(match[i+1], 10, 32)
		if err != nil {
			return servingVersion{}, false, fmt.Errorf("%w: version is not readable", ErrServingVersion)
		}
		version[i] = value
	}
	suffix := match[4]
	release := suffix == "" || strings.HasPrefix(suffix, "+") || strings.HasPrefix(suffix, ".post") || strings.HasPrefix(suffix, "post")
	return version, release, nil
}

func (v servingVersion) less(other servingVersion) bool {
	for i := range v {
		if v[i] != other[i] {
			return v[i] < other[i]
		}
	}
	return false
}

func (v servingVersion) String() string {
	return fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2])
}

// checkServingVersion compares the reported vLLM version with the minimum for
// the model: 0.14.1, or 0.27.0 for an NVFP4 build.
func checkServingVersion(reported string, nvfp4 bool) error {
	version, release, err := parseServingVersion(reported)
	if err != nil {
		return err
	}
	minimum := minimumServingVersion
	if nvfp4 {
		minimum = minimumNVFP4Version
	}
	if version.less(minimum) || (version == minimum && !release) {
		return fmt.Errorf("%w: install vLLM %s or later", ErrServingVersion, minimum)
	}
	return nil
}

// modelUsesNVFP4 reads the quantization settings of a model directory. A
// model whose settings name an FP4 scheme needs the NVFP4 minimum version.
func modelUsesNVFP4(dir string) (bool, error) {
	config, err := readModelMetadata(filepath.Join(dir, "config.json"))
	if err != nil {
		return false, err
	}
	var fields struct {
		Quantization json.RawMessage `json:"quantization_config"`
	}
	if config != nil && json.Unmarshal(config, &fields) != nil {
		return false, fmt.Errorf("%w: model config.json is not valid JSON", ErrModelUnavailable)
	}
	quant, err := readModelMetadata(filepath.Join(dir, "hf_quant_config.json"))
	if err != nil {
		return false, err
	}
	for _, raw := range [][]byte{fields.Quantization, quant} {
		if bytes.Contains(bytes.ToLower(raw), []byte("fp4")) {
			return true, nil
		}
	}
	return false, nil
}

func readModelMetadata(path string) ([]byte, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: model metadata could not be read", ErrModelUnavailable)
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maxModelMetaBytes+1))
	if err != nil || len(raw) > maxModelMetaBytes {
		return nil, fmt.Errorf("%w: model metadata could not be read or exceeds 4 MiB", ErrModelUnavailable)
	}
	return raw, nil
}

// checkServingVersion reads /version from the running vLLM and compares it
// with the minimum for the bound model. /version needs no API key.
func (m *SystemdManager) checkServingVersion(ctx context.Context, n control.NativeModel) error {
	dir, err := servingModelDirectory(n)
	if err != nil {
		return err
	}
	nvfp4, err := modelUsesNVFP4(dir)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, n.Endpoint+"/version", nil)
	if err != nil {
		return err
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return &safeHTTPRequestError{cause: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode >= http.StatusInternalServerError {
			return fmt.Errorf("version status %d", resp.StatusCode)
		}
		return fmt.Errorf("%w: version status %d", ErrServingVersion, resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4097))
	if err != nil || len(raw) > 4096 {
		return fmt.Errorf("%w: version response could not be read", ErrServingVersion)
	}
	var body struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(raw, &body) != nil {
		return fmt.Errorf("%w: version response is not valid JSON", ErrServingVersion)
	}
	return checkServingVersion(body.Version, nvfp4)
}

// servingModelDirectory returns the model directory of a bound launch: the
// owned model path, or the model operand of the adopted launch.
func servingModelDirectory(n control.NativeModel) (string, error) {
	if n.Owned != nil {
		return n.Owned.ModelPath, nil
	}
	sources, err := readLaunchSources(n.LaunchFile, n.DropIns)
	if err != nil {
		return "", err
	}
	unit, err := parseExternalLaunchSources(sources)
	if err != nil {
		return "", err
	}
	args := splitExecWords(unit.execStart)
	if len(args) < 3 {
		return "", ErrLaunchUnsupported
	}
	flags, err := parseServerCommand(args, n.Runtime)
	if err != nil {
		return "", err
	}
	return flags.model, nil
}

// BindHardenedNativeLaunch reads the hardened vLLM settings of an adopted
// launch into the binding: the --chat-template path and its current digest,
// and the EnvironmentFile path. It then qualifies the launch with them. An
// owned binding keeps the settings that setup recorded.
func BindHardenedNativeLaunch(path string, binding control.NativeModel) (control.NativeModel, string, error) {
	if binding.Owned == nil {
		binding.ChatTemplate, binding.ChatTemplateSHA256, binding.EnvironmentFile = "", "", ""
		if sources, err := readLaunchSources(path, binding.DropIns); err == nil {
			readHardenedSettings(sources, &binding)
		}
	}
	hash, err := InspectQualifiedNativeLaunch(path, binding)
	return binding, hash, err
}

// readHardenedSettings fills what it can read; the qualification reports
// every missing or unreadable setting.
func readHardenedSettings(sources [][]byte, binding *control.NativeModel) {
	unit, err := parseExternalLaunchSources(sources)
	if err != nil {
		return
	}
	binding.EnvironmentFile = unit.environmentFile
	args := splitExecWords(unit.execStart)
	if len(args) < 3 {
		return
	}
	flags, err := parseServerCommand(args, binding.Runtime)
	if err != nil || flags.chatTemplate == "" {
		return
	}
	binding.ChatTemplate = flags.chatTemplate
	if digest, err := ChatTemplateSHA256(flags.chatTemplate); err == nil {
		binding.ChatTemplateSHA256 = digest
	}
}

package setup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/httptransport"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/strictjson"
)

// ProbeRequest contains explicit references only; it is never an execution plan.
type ProbeRequest struct {
	App           string `json:"app"`
	Endpoint      string `json:"endpoint,omitempty"`
	Reference     string `json:"reference,omitempty"`
	ReferenceKind string `json:"referenceKind,omitempty"`
}
type ApplicationCandidate struct {
	SourceKind          string           `json:"sourceKind"`
	Recognized          bool             `json:"recognized"`
	Location            string           `json:"location,omitempty"`
	ConfigurationStatus string           `json:"configurationStatus,omitempty"`
	Binding             *DraftBinding    `json:"binding,omitempty"`
	App                 string           `json:"app"`
	ID                  string           `json:"id"`
	Label               string           `json:"label"`
	Endpoint            string           `json:"endpoint,omitempty"`
	Reference           string           `json:"reference,omitempty"`
	ReferenceKind       string           `json:"referenceKind,omitempty"`
	Unit                string           `json:"unit,omitempty"`
	Cgroup              string           `json:"cgroup,omitempty"`
	InstanceStatus      string           `json:"instanceStatus"`
	InventoryStatus     string           `json:"inventoryStatus"`
	Version             string           `json:"version,omitempty"`
	Models              []ModelCandidate `json:"models"`
	LifecycleControl    string           `json:"lifecycleControl"`
	NextStep            string           `json:"nextStep,omitempty"`
}
type ModelCandidate struct {
	ID       string   `json:"id"`
	Label    string   `json:"label"`
	Source   string   `json:"source"`
	Loaded   string   `json:"loaded"`
	Locality string   `json:"locality"`
	Aliases  []string `json:"aliases,omitempty"`
}

const (
	appLlamaCPP             = "llama.cpp"
	referenceModelDirectory = "model-directory"
)

func appLabel(app string) string {
	switch app {
	case "comfyui":
		return "ComfyUI"
	case "ollama":
		return "Ollama"
	case appLlamaCPP:
		return appLlamaCPP
	case "vllm":
		return "vLLM"
	}
	return ""
}
func (r ProbeRequest) validate() error {
	if appLabel(r.App) == "" {
		return errors.New("unsupported application")
	}
	if (r.Endpoint == "") == (r.Reference == "") {
		return errors.New("select exactly one endpoint or file/directory reference")
	}
	if r.Endpoint != "" {
		return r.validateEndpoint()
	}
	if !filepath.IsAbs(r.Reference) || filepath.Clean(r.Reference) != r.Reference || strings.ContainsRune(r.Reference, 0) || len(r.Reference) > 4096 {
		return errors.New("reference must be an absolute clean path")
	}
	switch r.ReferenceKind {
	case "application", "application-directory", "configuration", "model-file", referenceModelDirectory:
		return nil
	}
	return errors.New("unsupported reference kind")
}
func (r ProbeRequest) validateEndpoint() error {
	u, err := url.Parse(r.Endpoint)
	if err != nil || u.User != nil || strings.ContainsAny(r.Endpoint, "?#") || (u.Path != "" && u.Path != "/") || (u.Scheme != "http" && u.Scheme != "https") || r.ReferenceKind != "" {
		return errors.New("endpoint must be a loopback HTTP origin")
	}
	ip := net.ParseIP(u.Hostname())
	if u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return errors.New("endpoint must be loopback")
	}
	return nil
}
func DecodeProbe(reader io.Reader) (ProbeRequest, error) {
	var request ProbeRequest
	if err := strictjson.DecodeLimited(reader, 16384, &request); err != nil {
		return request, err
	}
	return request, request.validate()
}
func candidate(r ProbeRequest) ApplicationCandidate {
	sum := sha256.Sum256([]byte(r.App + "\x00" + r.Endpoint + "\x00" + r.Reference + "\x00" + r.ReferenceKind))
	sourceKind := "endpoint"
	if r.Reference != "" {
		sourceKind = "reference"
		if r.ReferenceKind == "configuration" {
			sourceKind = "configuration"
		}
	}
	return ApplicationCandidate{SourceKind: sourceKind, App: r.App, ID: hex.EncodeToString(sum[:12]), Label: appLabel(r.App), Endpoint: r.Endpoint, Reference: r.Reference, ReferenceKind: r.ReferenceKind, InstanceStatus: "candidate", InventoryStatus: "unknown", Models: []ModelCandidate{}, LifecycleControl: "unverified", NextStep: "Verify lifecycle control before using this workload."}
}

// Probe never starts a process or reads model contents. HTTP routes are fixed
// read-only routes, including no llama.cpp reload or routed autoload requests.
func Probe(ctx context.Context, r ProbeRequest) (ApplicationCandidate, error) {
	result := candidate(r)
	if err := r.validate(); err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if r.Reference != "" {
		return probeReference(ctx, r, result)
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	transport := httptransport.NewDirect()
	defer transport.CloseIdleConnections()
	// Pin localhost to a loopback address rather than trusting name resolution.
	endpoint := strings.TrimSuffix(r.Endpoint, "/")
	parsed, _ := url.Parse(endpoint)
	if parsed.Hostname() == "localhost" {
		parsed.Host = "127.0.0.1"
		if port := mustPort(r.Endpoint); port != "" {
			parsed.Host = net.JoinHostPort("127.0.0.1", port)
		}
		endpoint = parsed.String()
	}
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	probe := applicationHTTP{client: client, endpoint: endpoint}
	var err error
	switch r.App {
	case "comfyui":
		err = probe.comfy(ctx, &result)
	case "ollama":
		err = probe.ollama(ctx, &result)
	case appLlamaCPP:
		err = probe.llama(ctx, &result)
	case "vllm":
		err = probe.vllm(ctx, &result)
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	if err != nil {
		result = probeFailure(result, err)
	}
	return result, nil
}
func probeFailure(result ApplicationCandidate, err error) ApplicationCandidate {
	result.Models = []ModelCandidate{}
	result.InventoryStatus = "invalid"
	result.NextStep = "The application returned an invalid response; check its endpoint and version."
	var status *probeHTTPError
	if errors.As(err, &status) {
		if status.code == 404 || status.code == 405 || status.code == 501 {
			result.InstanceStatus = "unsupported"
			result.InventoryStatus = "unsupported"
			result.NextStep = "This API is unavailable; select an existing configuration or model reference."
		} else {
			result.InstanceStatus = "unreachable"
			result.InventoryStatus = "unknown"
			result.NextStep = "Check the application endpoint and access settings, then retry."
		}
	}
	var network *url.Error
	if errors.As(err, &network) {
		result.InstanceStatus = "unreachable"
		result.InventoryStatus = "unknown"
		result.NextStep = "The application is unreachable. Start it separately or select an existing configuration; saved references are unchanged."
	}
	return result
}
func mustPort(endpoint string) string { u, _ := url.Parse(endpoint); return u.Port() }
func probeReference(ctx context.Context, r ProbeRequest, result ApplicationCandidate) (ApplicationCandidate, error) {
	return probeReferenceWithExecutableValidator(ctx, r, result, gpuruntime.ValidateSelectedNativeExecutable)
}

func probeReferenceWithExecutableValidator(ctx context.Context, r ProbeRequest, result ApplicationCandidate, validateExecutable func(string, string) error) (ApplicationCandidate, error) {
	if err := ctx.Err(); err != nil {
		return result, err
	}
	info, err := os.Lstat(r.Reference)
	if err != nil {
		return missingReference(err, result), nil
	}
	// Native application selections delegate all alias and target checks to the
	// executable trust validator, which qualifies every resolution hop. Generic
	// references retain the no-symlink rule and never open their contents.
	if r.ReferenceKind == "application" && r.App != "comfyui" {
		return probeSelectedExecutable(r, result, validateExecutable)
	}
	// Do not resolve links or open devices/FIFOs. A selection is only a candidate.
	directory := r.ReferenceKind == referenceModelDirectory || r.ReferenceKind == "application-directory"
	if (directory && !info.IsDir()) || (!directory && !info.Mode().IsRegular()) {
		result.InstanceStatus = "invalid"
		result.NextStep = "Select a regular file or a model directory; symbolic links and special files are not inspected."
		return result, nil
	}
	if r.App == "comfyui" {
		result.InventoryStatus = "not-applicable"
		result.NextStep = "ComfyUI workflows select models. Select its application or configuration."
		return result, nil
	}
	return referencedCandidate(r, directory, result), nil
}
func probeSelectedExecutable(r ProbeRequest, result ApplicationCandidate, validate func(string, string) error) (ApplicationCandidate, error) {
	if err := validate(r.App, r.Reference); err != nil {
		result.InstanceStatus = inspectionFailedStatus
		result.ConfigurationStatus = inspectionFailedStatus
		result.NextStep = err.Error()
		return result, nil
	}
	ports := map[string]uint16{"ollama": 11434, appLlamaCPP: 8080, "vllm": 8000}
	result.SourceKind = "owned"
	result.Recognized = true
	result.InstanceStatus = "installed"
	result.ConfigurationStatus = "model-required"
	result.Binding = &DraftBinding{Instance: "instance-" + result.ID, Owned: &DraftOwnedLaunch{Executable: r.Reference, Port: ports[r.App]}}
	result.NextStep = "Supported executable installed. Select an existing local model, then review creation of a supervisor-owned launch. Nothing has been started."
	return result, nil
}

func missingReference(err error, result ApplicationCandidate) ApplicationCandidate {
	result.InstanceStatus = "missing"
	result.NextStep = "Select an existing file or directory."
	if !errors.Is(err, os.ErrNotExist) {
		result.InstanceStatus = inspectionFailedStatus
		result.NextStep = "Check access to the selected reference."
	}
	return result
}
func referencedCandidate(r ProbeRequest, directory bool, result ApplicationCandidate) ApplicationCandidate {
	if r.ReferenceKind == "model-file" || (directory && r.ReferenceKind == referenceModelDirectory) {
		source := "file"
		if directory {
			source = "directory"
		}
		result.Models = []ModelCandidate{{ID: r.Reference, Label: filepath.Base(r.Reference), Source: source, Loaded: "unknown", Locality: "local"}}
	}
	result.NextStep = "Reference saved as a candidate only. Verify application compatibility and lifecycle control."
	return result
}

type applicationHTTP struct {
	client   *http.Client
	endpoint string
}
type probeHTTPError struct{ code int }

func (e *probeHTTPError) Error() string { return fmt.Sprintf("application HTTP status %d", e.code) }
func (p applicationHTTP) get(ctx context.Context, path string, destination interface{}) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, p.endpoint+path, nil)
	if err != nil {
		return err
	}
	response, err := p.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return &probeHTTPError{response.StatusCode}
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 1048577))
	if err != nil {
		return err
	}
	if len(data) > 1048576 {
		return errors.New("discovery response exceeds 1 MiB")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := json.Unmarshal(data, destination); err != nil {
		return err
	}
	return ctx.Err()
}

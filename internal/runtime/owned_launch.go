package runtime

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

var ErrOwnedRender = errors.New("owned launch cannot be rendered to the supported unit subset")

// Owned binaries are packaged executables; the deterministic render names the
// packaged path and the round-trip qualification proves it is trusted. It is a
// variable only so tests can render against fixture executables.
var ownedBinaryDirectory = "/usr/bin"

// RenderOwnedUnit deterministically renders the static systemd user unit the
// supervisor owns for an owned launch profile. The output deliberately uses
// only the adopt grammar subset so one verifier covers both provenances, and
// carries no [Install] section so nothing auto-starts at login.
func RenderOwnedUnit(p control.WorkloadProfile) ([]byte, error) {
	n := p.NativeModel
	if n == nil || n.Owned == nil {
		return nil, fmt.Errorf("%w: profile %s has no owned launch spec", ErrOwnedRender, p.ID)
	}
	// Ollama units are per instance so owned models can share them; the
	// description must not embed a single workload's identity.
	subject := string(p.ID)
	if n.Runtime == "ollama" {
		subject = "instance " + n.Instance
	}
	var b strings.Builder
	b.WriteString("[Unit]\nDescription=Supervisor-owned " + n.Runtime + " launch for " + subject + "\n\n[Service]\nType=simple\nRestart=no\n")
	var execStart string
	switch n.Runtime {
	case "ollama":
		b.WriteString("Environment=OLLAMA_NO_CLOUD=1\nEnvironment=OLLAMA_HOST=127.0.0.1:" + strconv.Itoa(int(n.Owned.Port)) + "\nEnvironment=OLLAMA_MAX_LOADED_MODELS=1\n")
		execStart = ownedBinaryDirectory + "/ollama serve"
	case "llama.cpp":
		execStart = ownedBinaryDirectory + "/llama-server -m " + n.Owned.ModelPath + " --host 127.0.0.1 --port " + strconv.Itoa(int(n.Owned.Port))
		if n.Owned.CtxSize != 0 {
			execStart += " --ctx-size " + strconv.FormatUint(uint64(n.Owned.CtxSize), 10)
		}
		if n.Owned.GPULayers != 0 {
			execStart += " --n-gpu-layers " + strconv.FormatUint(uint64(n.Owned.GPULayers), 10)
		}
		if n.Owned.Alias != "" {
			execStart += " --alias " + n.Owned.Alias
		}
	case "vllm":
		execStart = ownedBinaryDirectory + "/vllm serve " + n.Owned.ModelPath + " --host 127.0.0.1 --port " + strconv.Itoa(int(n.Owned.Port))
		if n.Owned.MaxModelLen != 0 {
			execStart += " --max-model-len " + strconv.FormatUint(uint64(n.Owned.MaxModelLen), 10)
		}
		if n.Owned.Alias != "" {
			execStart += " --served-model-name " + n.Owned.Alias
		}
	default:
		return nil, fmt.Errorf("%w: runtime %s", ErrOwnedRender, n.Runtime)
	}
	b.WriteString("ExecStart=" + execStart + "\n")
	return []byte(b.String()), nil
}

// OwnedCgroup derives the workload cgroup from the systemd manager root.
func OwnedCgroup(managerRoot string, p control.WorkloadProfile) string {
	return strings.TrimSuffix(managerRoot, "/") + "/app.slice/" + p.Unit
}

// renderMustQualify is the render⇄verify round-trip invariant: rendered output
// must pass the same qualification as an adopted unit on this host.
func renderMustQualify(raw []byte, n control.NativeModel) error {
	if err := qualifyNativeLaunchWithValidator(raw, n, validateNativeExecutable); err != nil {
		return fmt.Errorf("%w: %v", ErrOwnedRender, err)
	}
	return nil
}

// QualifyOwnedUnit renders an owned profile's unit and qualifies it against
// this host, proving the packaged executable is present and trusted and the
// model path exists with the right type before anything becomes durable.
func QualifyOwnedUnit(p control.WorkloadProfile) error {
	if p.NativeModel == nil || p.NativeModel.Owned == nil {
		return nil
	}
	raw, err := RenderOwnedUnit(p)
	if err != nil {
		return err
	}
	return renderMustQualify(raw, *p.NativeModel)
}

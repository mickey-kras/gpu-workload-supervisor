package setup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/deployment"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/lock"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
)

const gpuCorrectionName = "zz-gpu-workload-supervisor.conf"

type GPUCorrectionRequest struct {
	App         string `json:"app"`
	Unit        string `json:"unit"`
	Fingerprint string `json:"fingerprint,omitempty"`
	Confirm     bool   `json:"confirm,omitempty"`
}

type GPUCorrection struct {
	Fingerprint string `json:"fingerprint"`
	Path        string `json:"path"`
	Content     string `json:"content"`
	Applied     bool   `json:"applied"`
}

func CorrectGPU(ctx context.Context, home string, request GPUCorrectionRequest) (GPUCorrection, error) {
	return SystemBackend().CorrectGPU(ctx, home, request)
}

func (b Backend) CorrectGPU(ctx context.Context, home string, request GPUCorrectionRequest) (GPUCorrection, error) {
	if !selectedUnitName.MatchString(request.Unit) || !supportedCorrectionApp(request.App) {
		return GPUCorrection{}, errors.New("choose a supported application service")
	}
	// Existing catalog bindings must not be invalidated by a setup repair.
	discovery := Discovery{Request: Request{Profile: Profile{StatePath: deployment.DefaultStatePath()}}}
	root := filepath.Join(home, ".config/gpu-workload-supervisor")
	if err := discoverCurrent(ctx, root, &discovery); err != nil {
		return GPUCorrection{}, err
	}
	gate, err := lock.TryAcquire(discovery.Request.Profile.StatePath + ".lock")
	if err != nil {
		return GPUCorrection{}, err
	}
	defer gate.Close()
	statePath := discovery.Request.Profile.StatePath
	if err := discoverCurrent(ctx, root, &discovery); err != nil {
		return GPUCorrection{}, err
	}
	if discovery.Request.Profile.StatePath != statePath {
		return GPUCorrection{}, errors.New("supervisor state location changed; retry")
	}
	if _, err := os.Lstat(statePath); err == nil {
		if err := checkNoTemporaryDiscovery(ctx, statePath); err != nil {
			return GPUCorrection{}, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return GPUCorrection{}, err
	}
	for _, profile := range discovery.Request.Catalog.Profiles {
		if profile.Unit == request.Unit {
			return GPUCorrection{}, errors.New("this service is already managed; change it through workload configuration")
		}
	}
	if _, err := os.Lstat(filepath.Join(root, "activation.json")); !errors.Is(err, os.ErrNotExist) {
		return GPUCorrection{}, errors.New("finish pending activation before correcting a service")
	}
	plan, err := b.previewGPUCorrection(ctx, home, request)
	if err != nil || !request.Confirm {
		return plan, err
	}
	if request.Fingerprint == "" || request.Fingerprint != plan.Fingerprint {
		return GPUCorrection{}, errors.New("service or GPU inventory changed; review the correction again")
	}
	directory := filepath.Dir(plan.Path)
	if err := os.Mkdir(directory, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return plan, err
	}
	if err := TrustedDirectory(directory); err != nil {
		return plan, err
	}
	if err := writeGPUCorrection(plan); err != nil {
		return plan, err
	}
	if _, err := b.runCommand(ctx, automaticSystemctlPath, "--user", "daemon-reload"); err != nil {
		return plan, fmt.Errorf("GPU correction saved; systemd reload failed. Retry Check again after reloading: %w", err)
	}
	model := ""
	if request.App == "ollama" {
		model = "selection-pending"
	}
	if _, err := b.automaticConfiguration(ctx, request.App, request.Unit, model); err != nil {
		return plan, fmt.Errorf("GPU correction saved; configuration still needs attention: %w", err)
	}
	plan.Applied = true
	return plan, nil
}

func writeGPUCorrection(plan GPUCorrection) error {
	directory := filepath.Dir(plan.Path)
	file, err := os.CreateTemp(directory, ".gpu-correction-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, writeErr := file.WriteString(plan.Content)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	// Publish the complete file without replacing another writer's override.
	if err := os.Link(file.Name(), plan.Path); err != nil {
		return err
	}
	parent, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer parent.Close()
	return parent.Sync()
}

func supportedCorrectionApp(app string) bool {
	return app == "comfyui" || app == "ollama" || app == appLlamaCPP || app == "vllm"
}

func (b Backend) previewGPUCorrection(ctx context.Context, home string, request GPUCorrectionRequest) (GPUCorrection, error) {
	values, err := b.showAutomatic(ctx, request.Unit)
	if err != nil {
		return GPUCorrection{}, err
	}
	if values["Id"] != request.Unit || values["LoadState"] != "loaded" || values["ActiveState"] != "inactive" || values["SubState"] != "dead" || values["NeedDaemonReload"] != "no" || appFromUnit(execStartPropertyPrefix+values["ExecStart"]) != request.App {
		return GPUCorrection{}, errors.New("correction requires an unchanged, loaded, stopped application service")
	}
	unitRoot := filepath.Join(home, ".config/systemd/user")
	if err := TrustedDirectory(unitRoot); err != nil {
		return GPUCorrection{}, err
	}
	if values["FragmentPath"] != filepath.Join(unitRoot, request.Unit) {
		return GPUCorrection{}, errors.New("automatic correction supports services in your user configuration directory")
	}
	inventory := Discovery{}
	b.discoverGPUInventory(ctx, &inventory)
	uuids := inventory.GPUUUIDs
	if inventory.GPUInventoryStatus != "available" || len(uuids) != 1 {
		return GPUCorrection{}, errors.New("automatic correction requires one supported physical GPU; configure the supervisor GPU explicitly on multi-GPU hosts")
	}
	dropIns := strings.Fields(values["DropInPaths"])
	for _, path := range dropIns {
		if filepath.Base(path) >= gpuCorrectionName {
			return GPUCorrection{}, errors.New("a later service override needs manual review")
		}
	}
	launch, err := gpuruntime.PreviewAutomaticGPUCorrection(values["FragmentPath"], request.App, dropIns, uuids[0])
	if err != nil {
		return GPUCorrection{}, err
	}
	path := filepath.Join(unitRoot, request.Unit+".d", gpuCorrectionName)
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return GPUCorrection{}, errors.New("GPU correction file already exists or could not be inspected; reload and check again")
	}
	content := "[Service]\nEnvironment=\"CUDA_VISIBLE_DEVICES=" + uuids[0] + "\"\n"
	evidence, err := json.Marshal(struct {
		Values  map[string]string
		Launch  gpuruntime.AutomaticLaunch
		Content string
	}{values, launch, content})
	if err != nil {
		return GPUCorrection{}, err
	}
	return GPUCorrection{Fingerprint: digest(evidence), Path: path, Content: content}, nil
}

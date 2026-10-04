package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/deployment"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/lock"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/supervisor"
)

type Profile struct {
	Version             int    `json:"version"`
	StatePath           string `json:"statePath"`
	ActivatedRelease    string `json:"activatedRelease"`
	SystemctlPath       string `json:"systemctlPath"`
	NvidiaSMIPath       string `json:"nvidiaSMIPath"`
	GPUIndex            int    `json:"gpuIndex"`
	CapacityHeadroomMiB uint64 `json:"capacityHeadroomMiB"`
}
type Request struct {
	Version          int             `json:"version"`
	Profile          Profile         `json:"profile"`
	Catalog          control.Catalog `json:"catalog"`
	ExpectedRevision string          `json:"expectedRevision"`
	ConfirmQuiesced  bool            `json:"confirmQuiesced"`
}
type Preview struct {
	Release string          `json:"release"`
	Profile Profile         `json:"profile"`
	Catalog control.Catalog `json:"catalog"`
	Changes []string        `json:"changes"`
}

func Decode(reader io.Reader) (Request, error) {
	var request Request
	data, err := io.ReadAll(io.LimitReader(reader, 262145))
	if err != nil {
		return request, err
	}
	if len(data) > 262144 {
		return request, errors.New("setup request exceeds 256 KiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return request, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return request, errors.New("trailing setup request")
	}
	return request, Validate(request)
}
func Validate(request Request) error {
	if request.Version != 1 || request.Profile.Version != 1 {
		return errors.New("unsupported setup/profile version")
	}
	if request.Profile.GPUIndex < 0 {
		return errors.New("invalid GPU index")
	}
	for _, path := range []string{request.Profile.StatePath, request.Profile.SystemctlPath, request.Profile.NvidiaSMIPath} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return errors.New("trusted paths must be absolute and clean")
		}
	}
	return request.Catalog.Validate()
}
func Home() (string, error) {
	account, err := user.LookupId(strconv.Itoa(os.Geteuid()))
	if err != nil {
		return "", err
	}
	if err := TrustedDirectory(account.HomeDir); err != nil {
		return "", err
	}
	return account.HomeDir, nil
}
func Plan(home string, request Request) (Preview, error) {
	if err := Validate(request); err != nil {
		return Preview{}, err
	}
	profile := request.Profile
	profile.ActivatedRelease = deployment.Release
	return Preview{Release: deployment.Release, Profile: profile, Catalog: request.Catalog, Changes: []string{filepath.Join(home, ".config/gpu-workload-supervisor/operator.json"), "Commit validated workload catalog to " + profile.StatePath, "Enable packaged user reconciliation for future logins (no workload is started now)", "Retain verified binary/configuration/state backups; user units and models are unchanged"}}, nil
}

var makeRuntime = runtimeFor
var runCommand = func(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

func runtimeFor(request Request) (gpuruntime.Manager, error) {
	return gpuruntime.NewSystemdManager(gpuruntime.SystemdConfig{Catalog: &request.Catalog, SystemctlPath: request.Profile.SystemctlPath, NvidiaSMIPath: request.Profile.NvidiaSMIPath, GPUIndex: request.Profile.GPUIndex, CapacityHeadroomMiB: request.Profile.CapacityHeadroomMiB, HealthTimeout: 10 * time.Second})
}
func mkdirTrusted(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	return TrustedDirectory(path)
}

// Apply never stops workloads. A caller must explicitly bring the existing
// deployment to closed, stable Idle before activation or adoption.
func Apply(ctx context.Context, home string, request Request) error {
	if err := Validate(request); err != nil {
		return err
	}
	if !request.ConfirmQuiesced {
		return errors.New("review preview and explicitly confirm quiescent activation")
	}
	root := filepath.Join(home, ".config/gpu-workload-supervisor")
	if err := mkdirTrusted(root); err != nil {
		return err
	}
	if err := mkdirTrusted(filepath.Dir(request.Profile.StatePath)); err != nil {
		return err
	}
	oldProfileData, err := privateRead(filepath.Join(root, "operator.json"))
	var old Profile
	if err == nil {
		if err := json.Unmarshal(oldProfileData, &old); err != nil {
			return err
		}
		if old.StatePath != request.Profile.StatePath {
			return errors.New("state relocation requires explicit maintenance migration")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	gate, err := lock.TryAcquire(request.Profile.StatePath + ".lock")
	if err != nil {
		return err
	}
	defer gate.Close()
	proxy, err := lock.TryAcquire(request.Profile.StatePath + ".proxy.lock")
	if err != nil {
		return err
	}
	defer proxy.Close()
	marker, err := deployment.Read(request.Profile.StatePath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if old.StatePath != "" && marker.Release != old.ActivatedRelease && !(marker.Maintenance && old.ActivatedRelease == deployment.Release) {
		return errors.New("activated profile and marker disagree")
	}
	if marker.Release != "" && marker.Release != deployment.Release && !newer(deployment.Release, marker.Release) {
		return errors.New("unsafe downgrade refused; use documented compatible backup restoration")
	}
	_, stateErr := os.Lstat(request.Profile.StatePath)
	existing := stateErr == nil
	if stateErr != nil && !errors.Is(stateErr, os.ErrNotExist) {
		return stateErr
	}
	if existing && !marker.Maintenance {
		if err := Inspect(ctx, request.Profile.StatePath); err != nil {
			return err
		}
	}
	manager, err := makeRuntime(request)
	if err != nil {
		return err
	}
	if err := manager.Released(ctx); err != nil {
		return fmt.Errorf("configured workloads have not released the GPU: %w", err)
	}
	// Existing committed mappings are also verified; removing a stopped profile
	// from the new catalog must not hide an old runtime still holding GPU memory.
	if existing && old.StatePath != "" {
		previous, err := privateRead(filepath.Join(root, "catalog.json"))
		if err != nil {
			return err
		}
		var catalog control.Catalog
		if err := json.Unmarshal(previous, &catalog); err != nil {
			return err
		}
		oldRequest := request
		oldRequest.Profile = old
		oldRequest.Catalog = catalog
		oldManager, err := makeRuntime(oldRequest)
		if err != nil {
			return err
		}
		if err := oldManager.Released(ctx); err != nil {
			return err
		}
	}
	if existing && !marker.Maintenance {
		backupDir := filepath.Join(root, "backups", digest([]byte(marker.Release+request.ExpectedRevision)))
		if err := mkdirTrusted(backupDir); err != nil {
			return err
		}
		if err := Backup(ctx, request.Profile.StatePath, filepath.Join(backupDir, "state.db")); err != nil {
			return err
		}
		if old.StatePath != "" {
			if err := copyActivation(root, backupDir, oldProfileData); err != nil {
				return err
			}
		}
	}
	activationPath := filepath.Join(root, "activation.json")
	requestData, _ := json.Marshal(request)
	if marker.Maintenance {
		saved, err := privateRead(activationPath)
		if err != nil {
			return err
		}
		if digest(saved) != digest(requestData) {
			return errors.New("interrupted activation must resume its original request")
		}
	} else if err := deployment.AtomicWrite(activationPath, requestData); err != nil {
		return err
	}
	marker.Version = 1
	if marker.Release == "" {
		marker.Release = deployment.Release
	}
	marker.Maintenance = true
	if err := deployment.Write(request.Profile.StatePath, marker); err != nil {
		return err
	}
	// A crash from this point intentionally leaves the maintenance fence in place.
	stateStore, err := store.Open(ctx, request.Profile.StatePath)
	if err != nil {
		return err
	}
	defer stateStore.Close()
	profile := request.Profile
	profile.ActivatedRelease = deployment.Release
	profileData, _ := json.Marshal(profile)
	catalogData, _ := json.Marshal(request.Catalog)
	tx := Transaction{Root: root, Changes: map[string][]byte{"operator.json": profileData, "catalog.json": catalogData}}
	committed := func() (bool, error) {
		snapshot, err := stateStore.Catalog(ctx)
		return reflect.DeepEqual(snapshot.Catalog, request.Catalog), err
	}
	err = tx.Apply(Hooks{Quiescent: func() error { return manager.Released(ctx) }, Committed: committed, Commit: func() error {
		same, err := committed()
		if err != nil {
			return err
		}
		if same {
			return nil
		}
		_, err = stateStore.ReplaceCatalog(ctx, request.ExpectedRevision, request.Catalog)
		return err
	}})
	if err != nil {
		return err
	}
	if !existing {
		state, err := stateStore.State(ctx)
		if err != nil {
			return err
		}
		state.ActiveWorkload = control.WorkloadIdle
		state.DesiredWorkload = control.WorkloadIdle
		state.Phase = control.PhaseStable
		state.Admission = control.AdmissionClosed
		if _, err := stateStore.UpdateState(ctx, state.Version, state); err != nil {
			return err
		}
	}
	if err := retainBinaries(root); err != nil {
		return err
	}
	// systemctl enable installs only the package-owned unit link, without --now.
	// Reject preexisting user overrides before invoking the standard unit manager.
	if err := enableReconciliation(ctx, home, profile.SystemctlPath); err != nil {
		return err
	}
	return deployment.Write(profile.StatePath, deployment.Marker{Version: 1, Release: deployment.Release})
}
func newer(next, previous string) bool {
	parse := func(value string) ([3]int, bool) {
		var out [3]int
		parts := strings.Split(strings.TrimPrefix(value, "v"), ".")
		if len(parts) != 3 {
			return out, false
		}
		for i, p := range parts {
			n, err := strconv.Atoi(p)
			if err != nil || n < 0 {
				return out, false
			}
			out[i] = n
		}
		return out, true
	}
	a, ok := parse(next)
	if !ok {
		return false
	}
	b, ok := parse(previous)
	if !ok {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}
func enableReconciliation(ctx context.Context, home, systemctl string) error {
	unit := "gpu-workload-supervisor-reconcile.service"
	userUnit := filepath.Join(home, ".config/systemd/user", unit)
	if _, err := os.Lstat(userUnit); err == nil {
		return errors.New("user reconciliation unit exists; refusing override")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	link := filepath.Join(home, ".config/systemd/user/default.target.wants", unit)
	if info, err := os.Lstat(link); err == nil {
		target, err := os.Readlink(link)
		if err != nil || info.Mode()&os.ModeSymlink == 0 || target != "/usr/lib/systemd/user/"+unit {
			return errors.New("unowned reconciliation enablement exists")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	output, err := runCommand(ctx, systemctl, "--user", "enable", unit)
	if err != nil {
		return fmt.Errorf("enable reconciliation: %w: %.4096s", err, output)
	}
	return nil
}
func Reconcile(ctx context.Context, home string) error {
	data, err := privateRead(filepath.Join(home, ".config/gpu-workload-supervisor/operator.json"))
	if err != nil {
		return err
	}
	var profile Profile
	if err := json.Unmarshal(data, &profile); err != nil {
		return err
	}
	gate, err := lock.TryAcquire(profile.StatePath + ".lock")
	if err != nil {
		return err
	}
	defer gate.Close()
	if err := deployment.Check(profile.StatePath, profile.ActivatedRelease); err != nil {
		return err
	}
	stateStore, err := store.Open(ctx, profile.StatePath)
	if err != nil {
		return err
	}
	defer stateStore.Close()
	snapshot, err := stateStore.Catalog(ctx)
	if err != nil {
		return err
	}
	manager, err := makeRuntime(Request{Profile: profile, Catalog: snapshot.Catalog})
	if err != nil {
		return err
	}
	controller, err := supervisor.New(stateStore, manager, supervisor.Config{Catalog: &snapshot, DrainTimeout: 2 * time.Minute, VerifyTimeout: time.Minute, ActionTimeout: time.Minute, CleanupTimeout: time.Minute, FinalizeTimeout: 10 * time.Second, PollInterval: 250 * time.Millisecond})
	if err != nil {
		return err
	}
	_, err = controller.Reconcile(ctx)
	return err
}

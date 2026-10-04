package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
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
	Version                 int    `json:"version"`
	StatePath               string `json:"statePath"`
	ActivatedRelease        string `json:"activatedRelease"`
	SystemctlPath           string `json:"systemctlPath"`
	NvidiaSMIPath           string `json:"nvidiaSMIPath"`
	GPUIndex                int    `json:"gpuIndex"`
	CapacityHeadroomMiB     uint64 `json:"capacityHeadroomMiB"`
	StatusTimeoutSeconds    int    `json:"statusTimeoutSeconds,omitempty"`
	OperationTimeoutSeconds int    `json:"operationTimeoutSeconds,omitempty"`
}
type Request struct {
	Version          int             `json:"version"`
	Profile          Profile         `json:"profile"`
	Catalog          control.Catalog `json:"catalog"`
	ExpectedRevision string          `json:"expectedRevision"`
	ConfirmQuiesced  bool            `json:"confirmQuiesced"`
}
type activation struct {
	Request Request `json:"request"`
	Fresh   bool    `json:"fresh"`
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
	if request.Profile.GPUIndex < 0 || request.Profile.StatusTimeoutSeconds < 0 || request.Profile.StatusTimeoutSeconds > 60 || request.Profile.OperationTimeoutSeconds < 0 || request.Profile.OperationTimeoutSeconds > 1800 {
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
	if _, err := makeRuntime(request); err != nil {
		return Preview{}, err
	}
	profile := request.Profile
	profile.ActivatedRelease = deployment.Release
	return Preview{Release: deployment.Release, Profile: profile, Catalog: request.Catalog, Changes: []string{filepath.Join(home, ".config/gpu-workload-supervisor/operator.json"), "Commit validated workload catalog to " + profile.StatePath, "Enable packaged user reconciliation for future logins (no workload is started now)", "Retain verified binary/configuration/state backups; user units and models are unchanged"}}, nil
}

var makeRuntime = runtimeFor
var runCommand = boundedCommand

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
type activationWork struct {
	home, root     string
	request        Request
	old            Profile
	oldProfileData []byte
	marker         deployment.Marker
	existing       bool
	accepted       control.CatalogSnapshot
}

func Apply(ctx context.Context, home string, request Request) error {
	if err := Validate(request); err != nil {
		return err
	}
	if !request.ConfirmQuiesced {
		return errors.New("review preview and explicitly confirm quiescent activation")
	}
	work := activationWork{home: home, root: filepath.Join(home, ".config/gpu-workload-supervisor"), request: request}
	if err := mkdirTrusted(work.root); err != nil {
		return err
	}
	if err := mkdirTrusted(filepath.Dir(request.Profile.StatePath)); err != nil {
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
	if err := work.inspect(ctx); err != nil {
		return err
	}
	manager, err := work.verifyRuntimes(ctx)
	if err != nil {
		return err
	}
	if err := work.backup(ctx); err != nil {
		return err
	}
	progress, err := work.enterMaintenance()
	if err != nil {
		return err
	}
	return commitConfiguration(ctx, home, work.root, request, manager, progress.Fresh)
}

func (work *activationWork) inspect(ctx context.Context) error {
	request := work.request
	if err := work.readPreviousProfile(); err != nil {
		return err
	}
	var err error
	work.marker, err = deployment.Read(request.Profile.StatePath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	marker := work.marker
	if work.old.StatePath != "" && marker.Release != work.old.ActivatedRelease && !(marker.Maintenance && work.old.ActivatedRelease == deployment.Release) {
		return errors.New("activated profile and marker disagree")
	}
	if marker.Release != "" && marker.Release != deployment.Release && !newer(deployment.Release, marker.Release) {
		return errors.New("unsafe downgrade refused; use documented compatible backup restoration")
	}
	if err := work.inspectExistingCatalog(ctx); err != nil {
		return err
	}
	if !marker.Maintenance && work.accepted.Revision != request.ExpectedRevision {
		return errors.New("configuration revision changed; refresh setup before activation")
	}
	return nil
}

func (work activationWork) verifyRuntimes(ctx context.Context) (gpuruntime.Manager, error) {
	manager, err := makeRuntime(work.request)
	if err != nil {
		return nil, err
	}
	if err := manager.Released(ctx); err != nil {
		return nil, fmt.Errorf("configured workloads have not released the GPU: %w", err)
	}
	if work.existing && work.old.StatePath != "" {
		previous := work.request
		previous.Profile = work.old
		previous.Catalog = work.accepted.Catalog
		// Before the first commit, only the recorded setup plan has a mapping.
		if work.accepted.Revision == "" {
			previous.Catalog = work.request.Catalog
		}
		oldManager, err := makeRuntime(previous)
		if err != nil {
			return nil, err
		}
		if err := oldManager.Released(ctx); err != nil {
			return nil, err
		}
	}
	return manager, nil
}

func (work activationWork) backup(ctx context.Context) error {
	if !work.existing || work.marker.Maintenance {
		return nil
	}
	root := filepath.Join(work.root, "backups")
	if err := mkdirTrusted(root); err != nil {
		return err
	}
	directory, err := os.MkdirTemp(root, "activation-")
	if err != nil {
		return err
	}
	if err := syncDirectory(root); err != nil {
		return err
	}
	if err := Backup(ctx, work.request.Profile.StatePath, filepath.Join(directory, "state.db")); err != nil {
		return err
	}
	if work.old.StatePath != "" {
		return copyActivation(work.root, directory, work.oldProfileData)
	}
	return nil
}

func (work activationWork) enterMaintenance() (activation, error) {
	path := filepath.Join(work.root, "activation.json")
	progress := activation{Request: work.request, Fresh: !work.existing}
	if work.marker.Maintenance {
		saved, err := privateRead(path)
		if err != nil {
			return progress, err
		}
		if err := json.Unmarshal(saved, &progress); err != nil {
			return progress, err
		}
		original, _ := json.Marshal(progress.Request)
		requested, _ := json.Marshal(work.request)
		if digest(original) != digest(requested) {
			return progress, errors.New("interrupted activation must resume its original request")
		}
	} else if err := writeJSON(path, progress); err != nil {
		return progress, err
	}
	marker := work.marker
	marker.Version = 1
	if marker.Release == "" {
		marker.Release = deployment.Release
	}
	marker.Maintenance = true
	return progress, deployment.Write(work.request.Profile.StatePath, marker)
}

func commitConfiguration(ctx context.Context, home, root string, request Request, manager gpuruntime.Manager, fresh bool) error {
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
	if fresh {
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

type integration struct {
	Version int    `json:"version"`
	Unit    string `json:"unit"`
	Target  string `json:"target"`
}

const reconcileUnit = "gpu-workload-supervisor-reconcile.service"

func enableReconciliation(ctx context.Context, home, systemctl string) error {
	userDir := filepath.Join(home, ".config/systemd/user")
	if err := mkdirTrusted(userDir); err != nil {
		return err
	}
	userUnit := filepath.Join(userDir, reconcileUnit)
	if _, err := os.Lstat(userUnit); err == nil {
		return errors.New("user reconciliation unit exists; refusing override")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	wants := filepath.Join(userDir, "default.target.wants")
	if err := mkdirTrusted(wants); err != nil {
		return err
	}
	target := "/usr/lib/systemd/user/" + reconcileUnit
	link := filepath.Join(wants, reconcileUnit)
	if info, err := os.Lstat(link); err == nil {
		destination, err := os.Readlink(link)
		if err != nil || info.Mode()&os.ModeSymlink == 0 || destination != target {
			return errors.New("unowned reconciliation enablement exists")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// Record planned ownership before the standard unit manager creates the link.
	if err := writeJSON(filepath.Join(home, ".config/gpu-workload-supervisor/integration.json"), integration{1, reconcileUnit, target}); err != nil {
		return err
	}
	output, err := runCommand(ctx, systemctl, "--user", "enable", reconcileUnit)
	if err != nil {
		return fmt.Errorf("enable reconciliation: %w: %.4096s", err, output)
	}
	return nil
}

// RemoveIntegration removes only the recorded enablement link. It does not stop
// any service and preserves profiles, state, audit, models and user workload units.
func RemoveIntegration(home string) error {
	root := filepath.Join(home, ".config/gpu-workload-supervisor")
	if err := TrustedDirectory(root); err != nil {
		return err
	}
	data, err := privateRead(filepath.Join(root, "integration.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var owned integration
	if err := json.Unmarshal(data, &owned); err != nil {
		return err
	}
	if owned.Version != 1 || owned.Unit != reconcileUnit || owned.Target != "/usr/lib/systemd/user/"+reconcileUnit {
		return errors.New("invalid integration ownership record")
	}
	directory := filepath.Join(home, ".config/systemd/user/default.target.wants")
	if err := TrustedDirectory(directory); err != nil {
		return err
	}
	link := filepath.Join(directory, reconcileUnit)
	target, err := os.Readlink(link)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil {
		if target != owned.Target {
			return errors.New("modified integration link is preserved")
		}
		if err := os.Remove(link); err != nil {
			return err
		}
	}
	return os.Remove(filepath.Join(root, "integration.json"))
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
	if profile.Version != 1 || profile.ActivatedRelease == "" {
		return errors.New("reconciliation requires an activated managed profile")
	}
	gate, err := lock.TryAcquire(profile.StatePath + ".lock")
	if err != nil {
		return err
	}
	defer gate.Close()
	if err := deployment.Check(profile.StatePath, profile.ActivatedRelease); err != nil {
		return err
	}
	file, err := deployment.OpenPrivate(profile.StatePath)
	if err != nil {
		return err
	}
	file.Close()
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

func (work *activationWork) readPreviousProfile() error {
	request := work.request
	data, err := privateRead(filepath.Join(work.root, "operator.json"))
	if err == nil {
		if err := json.Unmarshal(data, &work.old); err != nil {
			return err
		}
		if work.old.StatePath != request.Profile.StatePath {
			return errors.New("state relocation requires explicit maintenance migration")
		}
		work.oldProfileData = data
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (work *activationWork) inspectExistingCatalog(ctx context.Context) error {
	_, err := os.Lstat(work.request.Profile.StatePath)
	work.existing = err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if work.existing {
		if !work.marker.Maintenance {
			if err := Inspect(ctx, work.request.Profile.StatePath); err != nil {
				return err
			}
		}
		work.accepted, err = ReadCatalog(ctx, work.request.Profile.StatePath)
		if err != nil {
			return err
		}
	}
	return nil
}

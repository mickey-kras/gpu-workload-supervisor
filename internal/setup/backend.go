package setup

import (
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
	"github.com/mickey-kras/gpu-workload-supervisor/internal/strictjson"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/supervisor"
	"golang.org/x/mod/semver"
)

type Profile = deployment.Profile

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
	if err := strictjson.DecodeLimited(reader, 262144, &request); err != nil {
		return request, err
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
	for i, p := range request.Catalog.Profiles {
		for _, q := range request.Catalog.Profiles[:i] {
			if control.SharedOllamaUnit(p, q) {
				return errors.New("shared Ollama units are catalog-only: apply the catalog with gpu-mode configure; setup does not create or verify shared-unit bindings")
			}
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
	return SystemBackend().Plan(home, request)
}

func (b Backend) Plan(home string, request Request) (Preview, error) {
	if err := Validate(request); err != nil {
		return Preview{}, err
	}
	if _, err := b.makeRuntime(request); err != nil {
		return Preview{}, err
	}
	profile := request.Profile
	profile.ActivatedRelease = deployment.Release
	return Preview{Release: deployment.Release, Profile: profile, Catalog: request.Catalog, Changes: []string{filepath.Join(home, ".config/gpu-workload-supervisor/operator.json"), "Commit validated workload catalog to " + profile.StatePath, "Enable packaged user reconciliation for future logins (no workload is started now)", "Retain verified binary/configuration/state backups; user units and models are unchanged"}}, nil
}

// Backend holds the host interactions setup performs. Tests inject fakes
// instead of touching the real runtime, unit manager, or package binaries.
type Backend struct {
	makeRuntime      func(Request) (gpuruntime.Manager, error)
	runCommand       func(ctx context.Context, name string, args ...string) ([]byte, error)
	probeApplication func(ctx context.Context, request ProbeRequest) (ApplicationCandidate, error)
	binaryDirectory  string
	packageBinaryUID uint32
}

func SystemBackend() Backend {
	return Backend{makeRuntime: runtimeFor, runCommand: boundedCommand, probeApplication: Probe, binaryDirectory: "/usr/bin"}
}

func runtimeFor(request Request) (gpuruntime.Manager, error) {
	return gpuruntime.NewSystemdManager(request.Profile.SystemdConfig(&request.Catalog))
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
	backend        Backend
	home, root     string
	request        Request
	old            Profile
	oldProfileData []byte
	marker         deployment.Marker
	existing       bool
	accepted       control.CatalogSnapshot
}

func Apply(ctx context.Context, home string, request Request) error {
	return SystemBackend().Apply(ctx, home, request)
}

func (b Backend) Apply(ctx context.Context, home string, request Request) error {
	if err := Validate(request); err != nil {
		return err
	}
	if !request.ConfirmQuiesced {
		return errors.New("review preview and explicitly confirm quiescent activation")
	}
	work := activationWork{backend: b, home: home, root: filepath.Join(home, ".config/gpu-workload-supervisor"), request: request}
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
	return b.commitConfiguration(ctx, home, work.root, request, manager, progress.Fresh)
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
	manager, err := work.backend.makeRuntime(work.request)
	if err != nil {
		return nil, err
	}
	if err := manager.ReleasedFor(ctx, control.WorkloadIdle); err != nil {
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
		oldManager, err := work.backend.makeRuntime(previous)
		if err != nil {
			return nil, err
		}
		if err := oldManager.ReleasedFor(ctx, control.WorkloadIdle); err != nil {
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
		return copyActivation(ctx, work.root, directory, work.oldProfileData)
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

func (b Backend) commitConfiguration(ctx context.Context, home, root string, request Request, manager gpuruntime.Manager, fresh bool) error {
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
	err = tx.Apply(Hooks{Quiescent: func() error { return manager.ReleasedFor(ctx, control.WorkloadIdle) }, Committed: committed, Commit: func() error {
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
	if err := b.retainBinaries(root); err != nil {
		return err
	}
	// systemctl enable installs only the package-owned unit link, without --now.
	// Reject preexisting user overrides before invoking the standard unit manager.
	if err := b.enableReconciliation(ctx, home, profile.SystemctlPath); err != nil {
		return err
	}
	if err := b.enableIdleTimer(ctx, home, profile.SystemctlPath); err != nil {
		return err
	}
	return deployment.Write(profile.StatePath, deployment.Marker{Version: 1, Release: deployment.Release})
}

// newer permits only a stable target with a strictly higher release core.
// Snapshot hashes do not establish chronology, and a snapshot may postdate the
// stable tag with the same core. Exact-release reapplication is handled by inspect.
func newer(next, previous string) bool {
	next = "v" + strings.TrimPrefix(next, "v")
	previous = "v" + strings.TrimPrefix(previous, "v")
	for _, version := range []string{next, previous} {
		if !semver.IsValid(version) {
			return false
		}
		// Reject x/mod's major-only and major.minor shorthand versions.
		if semver.Canonical(version) != strings.TrimSuffix(version, semver.Build(version)) {
			return false
		}
	}
	if semver.Prerelease(next) != "" {
		return false
	}
	previousCore := strings.TrimSuffix(semver.Canonical(previous), semver.Prerelease(previous))
	return semver.Compare(next, previousCore) > 0
}

type integration struct {
	Version int    `json:"version"`
	Unit    string `json:"unit"`
	Target  string `json:"target"`
}

const (
	reconcileUnit = "gpu-workload-supervisor-reconcile.service"
	idleTimerUnit = "gpu-workload-supervisor-idle.timer"
)

// unitEnablement describes one packaged user unit's enablement: the unit
// name, the refusal-message label (stable: the packaged lifecycle fixture
// matches on it), the wants directory, and the ownership record file.
type unitEnablement struct {
	unit           string
	label          string
	wantsDirectory string
	recordName     string
}

var (
	reconcileEnablement = unitEnablement{reconcileUnit, "reconciliation", "default.target.wants", "integration.json"}
	idleTimerEnablement = unitEnablement{idleTimerUnit, "idle timer", "timers.target.wants", "integration-idle.json"}
)

func (b Backend) enableReconciliation(ctx context.Context, home, systemctl string) error {
	return b.enableUserUnit(ctx, home, systemctl, reconcileEnablement)
}

// enableIdleTimer mirrors the reconciliation enablement exactly: the packaged
// timer is only link-enabled (no --now, no daemon), preexisting user overrides
// are refused, and ownership is recorded for symmetric removal.
func (b Backend) enableIdleTimer(ctx context.Context, home, systemctl string) error {
	return b.enableUserUnit(ctx, home, systemctl, idleTimerEnablement)
}

func (b Backend) enableUserUnit(ctx context.Context, home, systemctl string, e unitEnablement) error {
	userDir := filepath.Join(home, ".config/systemd/user")
	if err := mkdirTrusted(userDir); err != nil {
		return err
	}
	userUnit := filepath.Join(userDir, e.unit)
	if _, err := os.Lstat(userUnit); err == nil {
		return fmt.Errorf("user %s unit exists; refusing override", e.label)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	wants := filepath.Join(userDir, e.wantsDirectory)
	if err := mkdirTrusted(wants); err != nil {
		return err
	}
	target := "/usr/lib/systemd/user/" + e.unit
	link := filepath.Join(wants, e.unit)
	if info, err := os.Lstat(link); err == nil {
		destination, err := os.Readlink(link)
		if err != nil || info.Mode()&os.ModeSymlink == 0 || destination != target {
			return fmt.Errorf("unowned %s enablement exists", e.label)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// Record planned ownership before the standard unit manager creates the link.
	if err := writeJSON(filepath.Join(home, ".config/gpu-workload-supervisor", e.recordName), integration{1, e.unit, target}); err != nil {
		return err
	}
	// Subprocess output is untrusted terminal input (control characters, unit
	// payload echoes); the error carries only the exit status.
	if _, err := b.runCommand(ctx, systemctl, "--user", "enable", e.unit); err != nil {
		return fmt.Errorf("enable %s: %w", e.label, err)
	}
	return nil
}

// RemoveIntegration removes only the recorded enablement links. It does not stop
// any service and preserves profiles, state, audit, models and user workload units.
func RemoveIntegration(home string) error {
	return SystemBackend().RemoveIntegration(context.Background(), home)
}

// RemoveIntegration is all-or-nothing: both integrations (records and links)
// are validated and the idle timer is stopped before either owned link is
// deleted, so a failed removal leaves the installation fully intact.
func (b Backend) RemoveIntegration(ctx context.Context, home string) error {
	reconcile, err := inspectIntegration(home, reconcileEnablement)
	if err != nil {
		return err
	}
	idle, err := inspectIntegration(home, idleTimerEnablement)
	if err != nil {
		return err
	}
	// A login may have activated the idle timer: removing only the wants link
	// leaves the loaded unit firing every 60 seconds until the user manager
	// exits, and the preserved operator profile keeps the service condition
	// true. Stop the timer before deleting anything; if it was never enabled
	// there is nothing to stop. (The reconcile unit is a login-triggered
	// oneshot, so nothing recurring persists for it.)
	if idle.recorded {
		if _, err := b.runCommand(ctx, "/usr/bin/systemctl", "--user", "stop", idleTimerUnit); err != nil {
			return fmt.Errorf("stop idle timer: %w", err)
		}
	}
	if err := idle.remove(); err != nil {
		return err
	}
	return reconcile.remove()
}

// pendingRemoval is a validated integration awaiting deletion.
type pendingRemoval struct {
	recordPath string
	linkPath   string
	recorded   bool
	hasLink    bool
}

// inspectIntegration validates the ownership record and the recorded link
// without touching anything; a foreign or modified link is reported, never
// removed.
func inspectIntegration(home string, e unitEnablement) (pendingRemoval, error) {
	root := filepath.Join(home, ".config/gpu-workload-supervisor")
	if err := TrustedDirectory(root); err != nil {
		return pendingRemoval{}, err
	}
	recordPath := filepath.Join(root, e.recordName)
	data, err := privateRead(recordPath)
	if errors.Is(err, os.ErrNotExist) {
		return pendingRemoval{}, nil
	}
	if err != nil {
		return pendingRemoval{}, err
	}
	var owned integration
	if err := json.Unmarshal(data, &owned); err != nil {
		return pendingRemoval{}, err
	}
	if owned.Version != 1 || owned.Unit != e.unit || owned.Target != "/usr/lib/systemd/user/"+e.unit {
		return pendingRemoval{}, errors.New("invalid integration ownership record")
	}
	directory := filepath.Join(home, ".config/systemd/user", e.wantsDirectory)
	if err := TrustedDirectory(directory); err != nil {
		return pendingRemoval{}, err
	}
	linkPath := filepath.Join(directory, e.unit)
	target, err := os.Readlink(linkPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return pendingRemoval{}, err
	}
	removal := pendingRemoval{recordPath: recordPath, linkPath: linkPath, recorded: true, hasLink: err == nil}
	if removal.hasLink && target != owned.Target {
		return pendingRemoval{}, errors.New("modified integration link is preserved")
	}
	return removal, nil
}

func (p pendingRemoval) remove() error {
	if !p.recorded {
		return nil
	}
	if p.hasLink {
		if err := os.Remove(p.linkPath); err != nil {
			return err
		}
	}
	return os.Remove(p.recordPath)
}

func Reconcile(ctx context.Context, home string) error {
	return SystemBackend().Reconcile(ctx, home)
}

func (b Backend) Reconcile(ctx context.Context, home string) error {
	controller, cleanup, err := b.activatedController(ctx, home)
	if err != nil {
		return err
	}
	defer cleanup()
	_, err = controller.Reconcile(ctx)
	return err
}

// PolicyTick evaluates the inactivity policy once against the persisted
// operator profile. It is the profile-aware oneshot behind the idle user
// timer: an unreadable operator.json fails loudly rather than silently
// falling back to default paths.
func PolicyTick(ctx context.Context, home string) error {
	return SystemBackend().PolicyTick(ctx, home)
}

func (b Backend) PolicyTick(ctx context.Context, home string) error {
	controller, cleanup, err := b.activatedController(ctx, home)
	if err != nil {
		return err
	}
	defer cleanup()
	// No qualified evidence provider ships with the supervisor, so an enabled
	// policy fails closed and an Off policy is a clean no-op.
	return controller.PolicyTick(ctx, nil)
}

// activatedController loads the persisted operator.json profile, validates
// activation, and builds a controller against the profile's state path and
// runtime configuration. The returned cleanup releases the store and gate.
func (b Backend) activatedController(ctx context.Context, home string) (*supervisor.Controller, func(), error) {
	data, err := privateRead(filepath.Join(home, ".config/gpu-workload-supervisor/operator.json"))
	if err != nil {
		return nil, nil, err
	}
	var profile Profile
	if err := json.Unmarshal(data, &profile); err != nil {
		return nil, nil, err
	}
	if profile.Version != 1 || profile.ActivatedRelease == "" {
		return nil, nil, errors.New("an activated managed profile is required")
	}
	gate, err := lock.TryAcquire(profile.StatePath + ".lock")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { gate.Close() }
	if err := deployment.Check(profile.StatePath, profile.ActivatedRelease); err != nil {
		cleanup()
		return nil, nil, err
	}
	file, err := deployment.OpenPrivate(profile.StatePath)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	file.Close()
	stateStore, err := store.Open(ctx, profile.StatePath)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	cleanup = func() {
		stateStore.Close()
		gate.Close()
	}
	snapshot, err := stateStore.Catalog(ctx)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	manager, err := b.makeRuntime(Request{Profile: profile, Catalog: snapshot.Catalog})
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	controller, err := supervisor.New(stateStore, manager, supervisor.Config{Catalog: &snapshot, DrainTimeout: 2 * time.Minute, VerifyTimeout: time.Minute, ActionTimeout: time.Minute, CleanupTimeout: time.Minute, FinalizeTimeout: 10 * time.Second, PollInterval: 250 * time.Millisecond})
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	return controller, cleanup, nil
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

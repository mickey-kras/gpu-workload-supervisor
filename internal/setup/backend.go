package setup

import (
	"context"
	"encoding/json"
	"errors"
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

// Reserve a bounded envelope allowance for deployment paths, revision and
// confirmation fields in addition to a maximum-size catalog.
const maxSetupRequestBytes = control.MaxCatalogBytes + 64*1024

func Decode(reader io.Reader) (Request, error) {
	var request Request
	if err := strictjson.DecodeLimited(reader, maxSetupRequestBytes, &request); err != nil {
		return request, err
	}
	// Decoding preserves the original request for interrupted activations.
	// Only pending peer edits need a derived clone to validate the new graph;
	// its final size is checked by the home-aware preview/apply path.
	for _, p := range request.Catalog.Profiles {
		if p.NativeModel != nil && p.NativeModel.Owned != nil {
			if _, err := ownedRenderChecked(p); err != nil {
				return request, err
			}
		}
	}
	if err := Validate(request); err == nil {
		return request, nil
	}
	candidate, err := prepareOwnedBackstops(request)
	if err != nil {
		return request, err
	}
	return request, Validate(candidate)
}
func Validate(request Request) error {
	encoded, err := json.Marshal(request)
	if err != nil {
		return err
	}
	if len(encoded) > maxSetupRequestBytes {
		return errors.New("setup request exceeds 320 KiB")
	}
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
	return SystemBackend().Plan(home, request)
}

func (b Backend) Plan(home string, request Request) (Preview, error) {
	request, err := prepareOwnedBackstopsForActivation(home, request)
	if err != nil {
		return Preview{}, err
	}
	if err := Validate(request); err != nil {
		return Preview{}, err
	}
	if _, err := b.makeRuntime(request); err != nil {
		return Preview{}, err
	}
	profile := request.Profile
	profile.ActivatedRelease = deployment.Release
	changes := []string{
		filepath.Join(home, ".config/gpu-workload-supervisor/operator.json"),
		"Commit validated workload catalog to " + profile.StatePath,
		"Enable packaged user reconciliation for future logins (no workload is started now)",
		"Enable the packaged user idle-policy timer (per-minute policy tick; no daemon)",
	}
	ownedChanges, err := b.previewOwnedUnitChanges(home, request)
	if err != nil {
		return Preview{}, err
	}
	if len(ownedChanges) == 0 {
		changes = append(changes, "Retain verified binary/configuration/state backups; user units and models are unchanged")
	} else {
		changes = append(changes, ownedChanges...)
		changes = append(changes, "Retain verified binary/configuration/state backups")
	}
	return Preview{Release: deployment.Release, Profile: profile, Catalog: request.Catalog, Changes: changes}, nil
}

// Backend holds the host interactions setup performs. Tests inject fakes
// instead of touching the real runtime, unit manager, or package binaries.
type Backend struct {
	temporaryProfile func() (Profile, error)
	makeRuntime      func(Request) (gpuruntime.Manager, error)
	runCommand       func(ctx context.Context, name string, args ...string) ([]byte, error)
	probeApplication func(ctx context.Context, request ProbeRequest) (ApplicationCandidate, error)
	inspectAutomatic func(string, string) (gpuruntime.AutomaticLaunch, error)
	// qualifyOwned renders and qualifies an owned profile against the host
	// before anything becomes durable; nil selects the runtime default.
	qualifyOwned     func(control.WorkloadProfile) error
	binaryDirectory  string
	packageBinaryUID uint32
}

func SystemBackend() Backend {
	return Backend{makeRuntime: runtimeFor, runCommand: boundedCommand, probeApplication: Probe, binaryDirectory: "/usr/bin"}
}

func runtimeFor(request Request) (gpuruntime.Manager, error) {
	return gpuruntime.NewSystemdManager(request.Profile.SystemdConfig(&request.Catalog))
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
	request, err := prepareValidatedOwnedBackstopsForActivation(home, request)
	if err != nil {
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
	if err := b.resumeOwnedUnitJournal(ctx, home, request); err != nil {
		return err
	}
	if err := work.inspect(ctx); err != nil {
		return err
	}
	if err := work.verifySharedPairBindings(ctx); err != nil {
		return err
	}
	// Owned unit writes precede the quiescence check so newly rendered units
	// are loaded when release evidence is gathered; deletes stay post-commit.
	plan, err := b.planOwnedUnits(request, work.accepted, home)
	if err != nil {
		return err
	}
	if !plan.empty() {
		journal := newOwnedUnitJournal(plan, request.Profile.StatePath)
		if err := writeOwnedUnitJournal(work.root, journal); err != nil {
			return err
		}
		if err := b.prepareOwnedUnitWrites(ctx, home, request, plan, journal); err != nil {
			return b.abortOwnedUnitWrites(ctx, home, work.root, request, plan, err)
		}
	}
	return work.finishActivation(ctx, plan)
}

// commitOptions carries the durable commit's inputs: the activation context,
// the runtime evidence source, and the owned-unit plan.
type commitOptions struct {
	home    string
	root    string
	request Request
	manager gpuruntime.Manager
	fresh   bool
	plan    unitPlan
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

func (b Backend) commitConfiguration(ctx context.Context, c commitOptions) error {
	// A crash from this point intentionally leaves the maintenance fence in place.
	home, root, request, manager, plan := c.home, c.root, c.request, c.manager, c.plan
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
	err = tx.Apply(Hooks{Quiescent: func() error {
		if err := verifyActivationBindings(ctx, manager, plan); err != nil {
			return err
		}
		return manager.ReleasedFor(ctx, control.WorkloadIdle)
	}, Committed: committed, Commit: func() error {
		return commitRequestedCatalog(ctx, stateStore, request, committed)
	}})
	if err != nil {
		return err
	}
	if c.fresh {
		if err := initializeIdleState(ctx, stateStore); err != nil {
			return err
		}
	}
	// Pin the journal to committed the moment the catalog commit is durable and
	// before the fence clears, so writes-pending reliably means "no commit".
	if err := commitOwnedUnitJournal(root, plan); err != nil {
		return err
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
	if err := deployment.Write(profile.StatePath, deployment.Marker{Version: 1, Release: deployment.Release}); err != nil {
		return err
	}
	// The maintenance fence is cleared; owned-unit deletes run post-commit so a
	// failed commit never loses files and deletes never need rollback.
	if err := b.finalizeOwnedUnits(ctx, home, root, request, plan); err != nil {
		return err
	}
	// Retire the activation record so only a genuinely interrupted activation
	// guards the owned-units journal.
	if err := os.Remove(filepath.Join(root, "activation.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
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
		if err := checkNoTemporaryDiscovery(ctx, work.request.Profile.StatePath); err != nil {
			return err
		}
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

func (b Backend) prepareOwnedUnitWrites(ctx context.Context, home string, request Request, plan unitPlan, journal unitJournal) error {
	// From the first unit write onward the installation is mutated: any
	// failure, including a partial write loop or a failed reload, must roll
	// back with the content snapshots.
	if err := b.applyOwnedUnitWrites(ctx, home, plan, journal); err != nil {
		return err
	}
	if err := b.daemonReloadOwnedUnits(ctx, request.Profile.SystemctlPath, plan.writeNames()); err != nil {
		return err
	}
	// The loaded binding must match the written file before commit; a
	// foreign drop-in or fragment would wedge the next supervisor preflight.
	if err := b.verifyOwnedUnitBindings(ctx, request.Profile.SystemctlPath, home, plan.writeNames()); err != nil {
		return err
	}
	return nil
}

func initializeIdleState(ctx context.Context, stateStore *store.Store) error {
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
	return nil
}

func (work activationWork) finishActivation(ctx context.Context, plan unitPlan) error {
	b, home, request := work.backend, work.home, work.request
	// Any pre-commit failure must leave the committed installation untouched:
	// roll back the journaled writes with their content snapshots.
	manager, err := work.verifyRuntimes(ctx, plan)
	if err != nil {
		return b.abortOwnedUnitWrites(ctx, home, work.root, request, plan, err)
	}
	if err := work.backup(ctx); err != nil {
		return b.abortOwnedUnitWrites(ctx, home, work.root, request, plan, err)
	}
	progress, err := work.enterMaintenance()
	if err != nil {
		return b.abortOwnedUnitWrites(ctx, home, work.root, request, plan, err)
	}
	return b.commitConfiguration(ctx, commitOptions{home: home, root: work.root, request: request, manager: manager, fresh: progress.Fresh, plan: plan})
}

func commitRequestedCatalog(ctx context.Context, stateStore *store.Store, request Request, committed func() (bool, error)) error {
	same, err := committed()
	if err != nil {
		return err
	}
	if same {
		return nil
	}
	_, err = stateStore.ReplaceCatalog(ctx, request.ExpectedRevision, request.Catalog)
	return err
}

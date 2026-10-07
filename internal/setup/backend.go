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
	return request.Catalog.Validate()
}

// verifySharedPairPreservation is the §2.3 setup gate, enforced in Apply after
// inspect because Validate is pure: a shared Ollama pair is appliable only when
// both profiles are owned (setup renders the shared unit) or both are carried
// verbatim from the accepted catalog.
func (work *activationWork) verifySharedPairPreservation() error {
	for i, p := range work.request.Catalog.Profiles {
		for _, q := range work.request.Catalog.Profiles[:i] {
			if !control.SharedOllamaUnit(p, q) {
				continue
			}
			if p.NativeModel.Owned != nil && q.NativeModel.Owned != nil {
				continue
			}
			acceptedP, okP := work.accepted.Catalog.Profile(p.ID)
			acceptedQ, okQ := work.accepted.Catalog.Profile(q.ID)
			if !okP || !okQ || !reflect.DeepEqual(acceptedP, p) || !reflect.DeepEqual(acceptedQ, q) {
				return errors.New("shared Ollama units are catalog-only: apply the catalog with gpu-mode configure; setup does not create or verify adopted shared-unit bindings")
			}
		}
	}
	return nil
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
	if err := b.resumeOwnedUnitJournal(ctx, home, request); err != nil {
		return err
	}
	if err := work.inspect(ctx); err != nil {
		return err
	}
	if err := work.verifySharedPairPreservation(); err != nil {
		return err
	}
	// Owned unit writes precede the quiescence check so newly rendered units
	// are loaded when release evidence is gathered; deletes stay post-commit.
	plan, err := b.planOwnedUnits(request, work.accepted, home)
	if err != nil {
		return err
	}
	if !plan.empty() {
		journal := newOwnedUnitJournal(plan)
		if err := writeOwnedUnitJournal(work.root, journal); err != nil {
			return err
		}
		if err := b.applyOwnedUnitWrites(ctx, home, plan, journal); err != nil {
			return err
		}
		if err := b.daemonReloadOwnedUnits(ctx, request.Profile.SystemctlPath, plan.writeNames()); err != nil {
			return err
		}
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
	return b.commitConfiguration(ctx, home, work.root, request, manager, progress.Fresh, plan)
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

func (b Backend) commitConfiguration(ctx context.Context, home, root string, request Request, manager gpuruntime.Manager, fresh bool, plan unitPlan) error {
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
	if err := deployment.Write(profile.StatePath, deployment.Marker{Version: 1, Release: deployment.Release}); err != nil {
		return err
	}
	// The maintenance fence is cleared; owned-unit deletes run post-commit so a
	// failed commit never loses files and deletes never need rollback.
	return b.finalizeOwnedUnits(ctx, home, root, request, plan)
}

// finalizeOwnedUnits performs the post-commit owned-unit effects: journaled
// content-proof deletes, one daemon-reload, and verification that every owned
// profile in the committed catalog has its exact rendering on disk.
func (b Backend) finalizeOwnedUnits(ctx context.Context, home, root string, request Request, plan unitPlan) error {
	if plan.empty() {
		return nil
	}
	journal, present, err := readOwnedUnitJournal(root)
	if err != nil {
		return err
	}
	if !present {
		return errors.New("owned-units journal missing after catalog commit")
	}
	journal.Phase = ownedJournalCommitted
	if err := writeOwnedUnitJournal(root, journal); err != nil {
		return err
	}
	if err := b.applyOwnedUnitDeletes(ctx, home, journal); err != nil {
		return err
	}
	if err := b.daemonReloadOwnedUnits(ctx, request.Profile.SystemctlPath, plan.unitNames()); err != nil {
		return err
	}
	for _, p := range request.Catalog.Profiles {
		if p.NativeModel == nil || p.NativeModel.Owned == nil {
			continue
		}
		raw, err := ownedRenderChecked(p)
		if err != nil {
			return err
		}
		data, err := privateRead(filepath.Join(ownedUnitDirectory(home), p.Unit))
		if err != nil {
			return err
		}
		if digest(data) != digest(raw) {
			return fmt.Errorf("%w: %s", gpuruntime.ErrLaunchChanged, p.Unit)
		}
	}
	return clearOwnedUnitJournal(root)
}

// resumeOwnedUnitJournal replays a stale owned-units journal from a previous
// post-commit crash before planning. Deletes are idempotent; a new request
// that rewrites a journaled delete drops it only when the digests agree, and
// divergent content is never deleted or overwritten without proof.
func (b Backend) resumeOwnedUnitJournal(ctx context.Context, home string, req Request) error {
	root := filepath.Join(home, ".config/gpu-workload-supervisor")
	journal, present, err := readOwnedUnitJournal(root)
	if err != nil || !present {
		return err
	}
	marker, err := deployment.Read(req.Profile.StatePath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if marker.Maintenance {
		// The activation fence resumes the original request verbatim, and its
		// plan rewrites this journal.
		return nil
	}
	if journal.Phase == ownedJournalPending {
		return clearOwnedUnitJournal(root)
	}
	reloaded := false
	for name, proof := range journal.Deletes {
		if p, ok := ownedProfileForUnit(req.Catalog, name); ok {
			raw, err := ownedRenderChecked(p)
			if err != nil {
				return err
			}
			if digest(raw) != proof {
				return fmt.Errorf("%w: %s", ErrOwnedUnitModified, name)
			}
			continue
		}
		path := filepath.Join(ownedUnitDirectory(home), name)
		current, err := privateRead(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if digest(current) != proof {
			return fmt.Errorf("%w: %s", ErrOwnedUnitModified, name)
		}
		if err := os.Remove(path); err != nil {
			return err
		}
		reloaded = true
	}
	if reloaded {
		if _, err := b.runCommand(ctx, req.Profile.SystemctlPath, "--user", "daemon-reload"); err != nil {
			return fmt.Errorf("daemon-reload: %w", err)
		}
	}
	return clearOwnedUnitJournal(root)
}

func ownedProfileForUnit(c control.Catalog, unit string) (control.WorkloadProfile, bool) {
	for _, p := range c.Profiles {
		if p.NativeModel != nil && p.NativeModel.Owned != nil && p.Unit == unit {
			return p, true
		}
	}
	return control.WorkloadProfile{}, false
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

const reconcileUnit = "gpu-workload-supervisor-reconcile.service"

func (b Backend) enableReconciliation(ctx context.Context, home, systemctl string) error {
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
	// Subprocess output is untrusted terminal input (control characters, unit
	// payload echoes); the error carries only the exit status.
	if _, err := b.runCommand(ctx, systemctl, "--user", "enable", reconcileUnit); err != nil {
		return fmt.Errorf("enable reconciliation: %w", err)
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
	return SystemBackend().Reconcile(ctx, home)
}

func (b Backend) Reconcile(ctx context.Context, home string) error {
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
	manager, err := b.makeRuntime(Request{Profile: profile, Catalog: snapshot.Catalog})
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

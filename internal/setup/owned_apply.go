package setup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/deployment"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
)

// abortOwnedUnitWrites rolls back pre-commit unit writes and drops the pending
// journal so a later apply starts from the committed installation, not from
// half-applied uncommitted content.
func (b Backend) abortOwnedUnitWrites(ctx context.Context, home, root string, request Request, plan unitPlan, cause error) error {
	if plan.empty() {
		return cause
	}
	// The journal survives a failed rollback: its journaled writes still need
	// recovery, and clearing it would leave uncommitted content unaccounted.
	if err := b.rollbackOwnedUnitWrites(ctx, home, request.Profile.SystemctlPath, plan); err != nil {
		return errors.Join(cause, err)
	}
	if err := clearOwnedUnitJournal(root); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

// previewOwnedUnitChanges reports the owned-unit writes and removals the
// request will cause so the activation preview is never materially false.
// The preview mirrors planOwnedUnits semantics: idempotent writes and
// referenced deletions are not changes.
func (b Backend) previewOwnedUnitChanges(home string, request Request) ([]string, error) {
	var changes []string
	dir := ownedUnitDirectory(home)
	for _, p := range request.Catalog.Profiles {
		if p.NativeModel == nil || p.NativeModel.Owned == nil {
			continue
		}
		raw, err := ownedRenderChecked(p)
		if err != nil {
			return nil, err
		}
		current, err := privateRead(filepath.Join(dir, p.Unit))
		if err == nil && digest(current) == digest(raw) {
			continue
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		changes = append(changes, "Write supervisor-owned unit "+p.Unit+" under ~/.config/systemd/user")
	}
	removals, err := b.plannedOwnedUnitRemovals(home, request)
	if err != nil {
		return nil, err
	}
	for _, name := range removals {
		changes = append(changes, "Remove supervisor-owned unit "+name+" from ~/.config/systemd/user")
	}
	return changes, nil
}

// plannedOwnedUnitRemovals lists owned units in the accepted catalog that the
// request drops. Without a state database nothing has been accepted yet.
func (b Backend) plannedOwnedUnitRemovals(home string, request Request) ([]string, error) {
	if _, err := os.Stat(request.Profile.StatePath); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	snapshot, err := ReadCatalog(ctx, request.Profile.StatePath)
	if err != nil {
		return nil, err
	}
	entries, err := acceptedOwnedUnitEntries(snapshot.Catalog, home)
	if err != nil {
		return nil, err
	}
	var removals []string
	for _, e := range entries {
		if ownedUnitStillReferenced(request.Catalog, e.path, e.proof) {
			continue
		}
		if _, err := privateRead(e.path); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, err
		}
		removals = append(removals, e.unit)
	}
	sort.Strings(removals)
	return removals, nil
}

// commitOwnedUnitJournal pins the owned-units journal to committed immediately
// after the catalog transaction resolves, closing the window where a crash
// would otherwise look like a pre-commit crash and lose proven deletes.
func commitOwnedUnitJournal(root string, plan unitPlan) error {
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
	return writeOwnedUnitJournal(root, journal)
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
	if journal.StatePath != request.Profile.StatePath {
		return ErrOwnedJournalStateMismatch
	}
	if journal.Phase != ownedJournalCommitted {
		return errors.New("owned-units journal not pinned to the committed catalog")
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
	// The journal is self-contained proof: it may only be consumed against the
	// state database that journaled it.
	if journal.StatePath != req.Profile.StatePath {
		return ErrOwnedJournalStateMismatch
	}
	marker, err := deployment.Read(req.Profile.StatePath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// The activation record is enforced only while the maintenance fence proves
	// it current: a stale record left by a crash after journal retire must not
	// block recovery of a newer interrupted activation. The journal's own
	// state-path binding above still rejects foreign databases.
	if marker.Maintenance {
		if err := requireMatchingActivation(root, req); err != nil {
			return err
		}
	}
	if journal.Phase == ownedJournalPending {
		if !marker.Maintenance {
			// The journal is pinned to committed before the fence clears, so
			// writes-pending without a fence genuinely means no commit: recover
			// the journaled writes before retiring the journal, or the
			// uncommitted units would be orphaned forever.
			return b.retirePendingJournal(ctx, home, root, req, journal)
		}
		committed, err := catalogMatchesRequest(ctx, req)
		if err != nil {
			return err
		}
		if !committed {
			// Genuine pre-commit crash: the activation fence replays the
			// original request verbatim, and its plan rewrites this journal.
			return nil
		}
		// Crash between the catalog commit and the journal pin: fall through
		// and replay the proven deletes now.
	}
	return b.replayCommittedDeletes(ctx, home, root, req, journal)
}

// replayCommittedDeletes replays a committed journal's proven deletes: units
// the request still renders are kept, everything else is removed with content
// proof, then systemd is refreshed and the journal retired.
func (b Backend) replayCommittedDeletes(ctx context.Context, home, root string, req Request, journal unitJournal) error {
	var reloaded []string
	removed := false
	for name, proof := range journal.Deletes {
		path := filepath.Join(ownedUnitDirectory(home), name)
		current, err := privateRead(path)
		if errors.Is(err, os.ErrNotExist) {
			// The delete already ran; systemd may still have the unit cached,
			// so the reload below must still happen.
			reloaded = append(reloaded, name)
			continue
		}
		if err != nil {
			return err
		}
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
		if digest(current) != proof {
			return fmt.Errorf("%w: %s", ErrOwnedUnitModified, name)
		}
		if err := provenDelete(path, proof); err != nil {
			return err
		}
		reloaded = append(reloaded, name)
		removed = true
	}
	if removed {
		if err := syncDir(ownedUnitDirectory(home)); err != nil {
			return err
		}
	}
	if len(reloaded) > 0 {
		if err := b.daemonReloadOwnedUnits(ctx, req.Profile.SystemctlPath, reloaded); err != nil {
			return err
		}
	}
	return clearOwnedUnitJournal(root)
}

// catalogMatchesRequest reports whether the committed catalog already equals
// the request's, which proves the previous activation committed before it
// crashed.
func catalogMatchesRequest(ctx context.Context, req Request) (bool, error) {
	snapshot, err := ReadCatalog(ctx, req.Profile.StatePath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return reflect.DeepEqual(snapshot.Catalog, req.Catalog), nil
}

// retirePendingJournal recovers a no-fence pending journal: the catalog was
// never committed, so each journaled write is uncommitted content. Units the
// accepted catalog still owns are restored to the accepted rendering; units it
// never owned are removed. Foreign content is never destroyed.
func (b Backend) retirePendingJournal(ctx context.Context, home, root string, req Request, journal unitJournal) error {
	snapshot, err := ReadCatalog(ctx, req.Profile.StatePath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	dir := ownedUnitDirectory(home)
	names := make([]string, 0, len(journal.Writes))
	for name := range journal.Writes {
		names = append(names, name)
	}
	sort.Strings(names)
	// Every journaled write may have reached systemd, even one whose recovery
	// finds the file already restored or removed, so all of them reload.
	reloaded := names
	changed := false
	for _, name := range names {
		path := filepath.Join(dir, name)
		current, err := privateRead(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		accepted, ok := ownedProfileForUnit(snapshot.Catalog, name)
		if !ok {
			// Exact adopted binding (converted profile keeping its file): the
			// catalog proves content by digest alone, so matching bytes are
			// kept and drifted bytes fail loudly — never deleted.
			if bound, sha := adoptedOwnedBinding(snapshot.Catalog, dir, name); bound {
				if digest(current) == sha {
					continue
				}
				return fmt.Errorf("%w: %s", ErrOwnedUnitModified, name)
			}
		}
		if ok {
			raw, err := ownedRenderChecked(accepted)
			if err != nil {
				return err
			}
			if digest(current) == digest(raw) {
				continue
			}
			if digest(current) != journal.Writes[name] {
				return fmt.Errorf("%w: %s", ErrOwnedUnitModified, name)
			}
			if err := provenReplace(path, raw, digest(current)); err != nil {
				return err
			}
		} else {
			if err := provenDelete(path, journal.Writes[name]); err != nil {
				return err
			}
		}
		changed = true
	}
	if len(reloaded) > 0 {
		if changed {
			if err := syncDir(dir); err != nil {
				return err
			}
		}
		if err := b.daemonReloadOwnedUnits(ctx, req.Profile.SystemctlPath, reloaded); err != nil {
			return err
		}
	}
	return clearOwnedUnitJournal(root)
}

// adoptedOwnedBinding resolves an exact adopted binding to the owned-unit
// file at dir/name: path equality plus the profile's proven fingerprint.
func adoptedOwnedBinding(c control.Catalog, dir, name string) (bool, string) {
	for _, p := range c.Profiles {
		if p.AdoptedOwnedFile() && p.NativeModel.LaunchFile == filepath.Join(dir, name) {
			return true, p.NativeModel.LaunchSHA256
		}
	}
	return false, ""
}

func ownedProfileForUnit(c control.Catalog, unit string) (control.WorkloadProfile, bool) {
	for _, p := range c.Profiles {
		if p.NativeModel != nil && p.NativeModel.Owned != nil && p.Unit == unit {
			return p, true
		}
	}
	return control.WorkloadProfile{}, false
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

// requireMatchingActivation blocks journal consumption by any request other
// than the one the in-flight activation recorded. A successful commit retires
// the record, so its presence means the activation is genuinely interrupted.
func requireMatchingActivation(root string, req Request) error {
	saved, err := privateRead(filepath.Join(root, "activation.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var progress activation
	if err := json.Unmarshal(saved, &progress); err != nil {
		return err
	}
	original, _ := json.Marshal(progress.Request)
	requested, _ := json.Marshal(req)
	if digest(original) != digest(requested) {
		return errors.New("interrupted activation must resume its original request")
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

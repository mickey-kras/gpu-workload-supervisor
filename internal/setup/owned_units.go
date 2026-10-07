package setup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/deployment"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
)

var (
	ErrOwnedUnitCollision         = errors.New("owned unit path exists with foreign content")
	ErrOwnedLaunchFileOutsideHome = errors.New("owned launch file must live under the setup home systemd user directory")
)

// syncDir makes directory entries (unit writes, unlinks, journal retirement)
// durable across power loss before the operation is considered complete.
var syncDir = func(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
var ErrOwnedUnitModified = errors.New("owned unit content changed; refusing removal")

type unitPlan struct {
	Writes  map[string][]byte // unit filename -> rendered content (absent in accepted catalog)
	Deletes []string          // unit filenames to remove (owned in accepted, absent in request)
	// proven records the accepted catalog's digest for units a delete or an
	// overwrite may touch, so only content the supervisor rendered is replaced.
	proven map[string]string
	// prior/absent snapshot the pre-write state so a failed pre-commit check
	// can restore the committed installation exactly. written marks the units
	// AtomicWrite actually replaced; rollback touches only those.
	prior   map[string][]byte
	absent  map[string]bool
	written map[string]bool
}

func (p unitPlan) empty() bool {
	return len(p.Writes) == 0 && len(p.Deletes) == 0
}

func (p unitPlan) writeNames() []string {
	names := make([]string, 0, len(p.Writes))
	for name := range p.Writes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (p unitPlan) unitNames() []string {
	return append(p.writeNames(), p.Deletes...)
}

type unitJournal struct {
	Version   int               `json:"version"`
	StatePath string            `json:"statePath"` // state database this journal belongs to
	Writes    map[string]string `json:"writes"`    // name -> sha256 hex
	Deletes   map[string]string `json:"deletes"`   // name -> sha256 hex (content proof captured before delete)
	Phase     string            `json:"phase"`     // "writes-pending" | "committed"
}

// ErrOwnedJournalStateMismatch rejects consuming a journal against a state
// database other than the one that journaled it: the proof is self-contained.
var ErrOwnedJournalStateMismatch = errors.New("owned-units journal belongs to a different state database")

const (
	ownedUnitJournalName  = "owned-units-journal.json"
	ownedJournalPending   = "writes-pending"
	ownedJournalCommitted = "committed"
	ownedUnitFileMode     = 0o644
	ownedUnitNamePrefix   = "gws-owned-"
	ownedUnitNameSuffix   = ".service"
)

func ownedUnitDirectory(home string) string {
	return filepath.Join(home, ".config/systemd/user")
}

// planOwnedUnits diffs requested against accepted owned profiles. Deletes are
// content-proven at plan time: a file whose digest differs from the accepted
// profile's deterministic render is retained and the apply fails before any
// commit, so file and catalog profile both survive.
// ownedUnitEntry is one unit file the accepted catalog accounts for, with
// the content proof that authorizes its deletion.
type ownedUnitEntry struct {
	unit  string
	path  string
	proof string
}

// acceptedOwnedUnitEntries enumerates unit files the accepted catalog
// accounts for — owned profiles and exact adopted bindings — so plan and
// preview enumerate the same set under the same rule.
func acceptedOwnedUnitEntries(accepted control.Catalog, home string) ([]ownedUnitEntry, error) {
	dir := ownedUnitDirectory(home)
	var entries []ownedUnitEntry
	for _, p := range accepted.Profiles {
		if p.NativeModel == nil {
			continue
		}
		if p.NativeModel.Owned != nil {
			raw, err := ownedRenderChecked(p)
			if err != nil {
				return nil, err
			}
			entries = append(entries, ownedUnitEntry{unit: p.Unit, path: filepath.Join(dir, p.Unit), proof: digest(raw)})
			continue
		}
		if p.AdoptedOwnedFile() {
			unit := filepath.Base(p.NativeModel.LaunchFile)
			path := filepath.Join(dir, unit)
			if p.NativeModel.LaunchFile != path {
				continue
			}
			entries = append(entries, ownedUnitEntry{unit: unit, path: path, proof: p.NativeModel.LaunchSHA256})
		}
	}
	return entries, nil
}

func (b Backend) planOwnedUnits(req Request, accepted control.CatalogSnapshot, home string) (unitPlan, error) {
	plan := unitPlan{Writes: map[string][]byte{}, proven: map[string]string{}, prior: map[string][]byte{}, absent: map[string]bool{}, written: map[string]bool{}}
	entries, err := acceptedOwnedUnitEntries(accepted.Catalog, home)
	if err != nil {
		return plan, err
	}
	for _, e := range entries {
		plan.proven[e.unit] = e.proof
	}
	qualify := b.qualifyOwned
	if qualify == nil {
		qualify = gpuruntime.QualifyOwnedUnit
	}
	for _, p := range req.Catalog.Profiles {
		if p.NativeModel == nil || p.NativeModel.Owned == nil {
			continue
		}
		// The catalog layer checks only the suffix (it is host-independent);
		// here the exact home is known, so pin the launch file to it before
		// writing or committing anything.
		if p.NativeModel.LaunchFile != filepath.Join(ownedUnitDirectory(home), p.Unit) {
			return plan, fmt.Errorf("%w: %s", ErrOwnedLaunchFileOutsideHome, p.Unit)
		}
		// Qualify against this host before anything becomes durable: the
		// packaged executable must be present and trusted and the model path
		// must exist with the right type.
		if err := qualify(p); err != nil {
			return plan, err
		}
		raw, err := ownedRenderChecked(p)
		if err != nil {
			return plan, err
		}
		plan.Writes[p.Unit] = raw
	}
	deleted := map[string]bool{}
	for _, e := range entries {
		if _, kept := plan.Writes[e.unit]; kept {
			continue
		}
		if deleted[e.unit] {
			// Shared Ollama pairs carry one unit file across profiles.
			continue
		}
		if ownedUnitStillReferenced(req.Catalog, e.path, e.proof) {
			// The requested catalog still claims this exact file (for example
			// an owned profile converted to adopted); keep it.
			continue
		}
		data, err := privateRead(e.path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return plan, err
		}
		if digest(data) != e.proof {
			return plan, fmt.Errorf("%w: %s", ErrOwnedUnitModified, e.unit)
		}
		plan.Deletes = append(plan.Deletes, e.unit)
		deleted[e.unit] = true
	}
	sort.Strings(plan.Deletes)
	return plan, nil
}

// ownedUnitStillReferenced reports whether any profile in the catalog claims
// the unit file at path with the proven digest, whether owned or adopted.
func ownedUnitStillReferenced(c control.Catalog, path, sha256 string) bool {
	for _, p := range c.Profiles {
		if p.NativeModel != nil && p.NativeModel.LaunchFile == path && p.NativeModel.LaunchSHA256 == sha256 {
			return true
		}
	}
	return false
}

// ownedRenderChecked re-renders an owned profile and pins the spec↔fingerprint
// chain before any file effect trusts the rendering.
func ownedRenderChecked(p control.WorkloadProfile) ([]byte, error) {
	raw, err := gpuruntime.RenderOwnedUnit(p)
	if err != nil {
		return nil, err
	}
	if digest(raw) != p.NativeModel.LaunchSHA256 {
		return nil, fmt.Errorf("%w: %s render diverges from the recorded fingerprint", gpuruntime.ErrOwnedRender, p.Unit)
	}
	return raw, nil
}

func newOwnedUnitJournal(plan unitPlan, statePath string) unitJournal {
	j := unitJournal{Version: 1,
		StatePath: statePath, Writes: map[string]string{}, Deletes: map[string]string{}, Phase: ownedJournalPending}
	for name, raw := range plan.Writes {
		j.Writes[name] = digest(raw)
	}
	for _, name := range plan.Deletes {
		j.Deletes[name] = plan.proven[name]
	}
	return j
}

func ownedUnitJournalPath(root string) string {
	return filepath.Join(root, ownedUnitJournalName)
}

func readOwnedUnitJournal(root string) (unitJournal, bool, error) {
	var j unitJournal
	data, err := privateRead(ownedUnitJournalPath(root))
	if errors.Is(err, os.ErrNotExist) {
		return j, false, nil
	}
	if err != nil {
		return j, false, err
	}
	if err := json.Unmarshal(data, &j); err != nil {
		return j, false, err
	}
	if j.Version != 1 || j.StatePath == "" || j.Writes == nil || j.Deletes == nil || (j.Phase != ownedJournalPending && j.Phase != ownedJournalCommitted) {
		return j, false, errors.New("unsupported owned-units journal")
	}
	return j, true, nil
}

func writeOwnedUnitJournal(root string, j unitJournal) error {
	return writeJSON(ownedUnitJournalPath(root), j)
}

func clearOwnedUnitJournal(root string) error {
	err := os.Remove(ownedUnitJournalPath(root))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return syncDir(root)
}

// applyOwnedUnitWrites replays journaled writes idempotently: digest equality
// means done. Existing content is overwritten only when it matches the
// rendering or an accepted-catalog rendering the supervisor can prove it owns.
func (b Backend) applyOwnedUnitWrites(ctx context.Context, home string, plan unitPlan, j unitJournal) error {
	dir := ownedUnitDirectory(home)
	if err := mkdirTrusted(dir); err != nil {
		return err
	}
	names := make([]string, 0, len(plan.Writes))
	for name := range plan.Writes {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		raw := plan.Writes[name]
		if j.Writes[name] != digest(raw) {
			return fmt.Errorf("owned-units journal disagrees with the plan for %s", name)
		}
		path := filepath.Join(dir, name)
		current, err := privateRead(path)
		if err == nil {
			// Snapshot before any branch: rollback must restore exactly these
			// bytes, including when the write below is skipped as idempotent.
			plan.prior[name] = current
			currentDigest := digest(current)
			if currentDigest == digest(raw) {
				continue
			}
			if plan.proven[name] != currentDigest {
				return fmt.Errorf("%w: %s", ErrOwnedUnitCollision, name)
			}
			// Overwrite with the ownership proof re-checked against the exact
			// inode being replaced, so a concurrent atomic replace cannot be
			// silently destroyed.
			if err := provenReplace(path, raw, currentDigest); err != nil {
				return err
			}
			plan.written[name] = true
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		} else {
			plan.absent[name] = true
		}
		// AtomicWrite creates the file 0600, matching the setup private-file
		// convention that privateRead trust checks enforce.
		if err := deployment.AtomicWrite(path, raw); err != nil {
			return err
		}
		plan.written[name] = true
	}
	return nil
}

// ownedStat/ownedAtomicWrite are seams for concurrency-fault injection.
var ownedStat = os.Stat
var ownedAtomicWrite = deployment.AtomicWrite

// provenReplace replaces path with raw only while the inode whose content
// matched proof is still the one at path. Without rename-at-exchange the
// check/rename pair cannot be fully atomic; the window is narrowed to the
// rename syscall itself, and the landed content is re-verified afterwards.
func provenReplace(path string, raw []byte, proof string) error {
	file, err := deployment.OpenPrivate(path)
	if err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return err
	}
	current, err := io.ReadAll(file)
	file.Close()
	if err != nil {
		return err
	}
	if digest(current) != proof {
		return fmt.Errorf("%w: %s", ErrOwnedUnitModified, filepath.Base(path))
	}
	latest, err := ownedStat(path)
	if err != nil {
		return err
	}
	if !os.SameFile(info, latest) {
		return fmt.Errorf("%w: %s", ErrOwnedUnitCollision, filepath.Base(path))
	}
	if err := ownedAtomicWrite(path, raw); err != nil {
		return err
	}
	landed, err := privateRead(path)
	if err != nil {
		return err
	}
	if digest(landed) != digest(raw) {
		return fmt.Errorf("%w: %s", ErrOwnedUnitCollision, filepath.Base(path))
	}
	return nil
}

// provenDelete unlinks path only while the inode whose content matched proof
// is still the one at path, mirroring provenReplace. Without unlink-by-inode
// the check/unlink pair cannot be fully atomic; the window is narrowed to the
// unlink syscall itself. Foreign content is never destroyed.
func provenDelete(path, proof string) error {
	file, err := deployment.OpenPrivate(path)
	if err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return err
	}
	current, err := io.ReadAll(file)
	file.Close()
	if err != nil {
		return err
	}
	if digest(current) != proof {
		return fmt.Errorf("%w: %s", ErrOwnedUnitModified, filepath.Base(path))
	}
	latest, err := ownedStat(path)
	if err != nil {
		return err
	}
	if !os.SameFile(info, latest) {
		return fmt.Errorf("%w: %s", ErrOwnedUnitCollision, filepath.Base(path))
	}
	return os.Remove(path)
}

// rollbackOwnedUnitWrites restores the pre-apply state after a failed
// pre-commit check: units the write created are removed, overwritten units get
// their snapshotted content back. Files whose content no longer matches the
// write are foreign modifications and are never destroyed.
func (b Backend) rollbackOwnedUnitWrites(ctx context.Context, home, systemctl string, plan unitPlan) error {
	dir := ownedUnitDirectory(home)
	var failed []string
	changed := false
	for _, name := range plan.writeNames() {
		if !plan.written[name] {
			// Never written (idempotent skip or unreached): leave untouched.
			continue
		}
		path := filepath.Join(dir, name)
		current, err := privateRead(path)
		switch {
		case err == nil && digest(current) == digest(plan.Writes[name]):
			// The written content is intact and safe to replace.
		case errors.Is(err, os.ErrNotExist) && !plan.absent[name]:
			// The overwrite target vanished; restoring it is still correct.
		case errors.Is(err, os.ErrNotExist):
			continue
		default:
			failed = append(failed, name)
			continue
		}
		if plan.absent[name] {
			if err := os.Remove(path); err != nil {
				failed = append(failed, name)
			}
			changed = true
			continue
		}
		if err := ownedAtomicWrite(path, plan.prior[name]); err != nil {
			failed = append(failed, name)
		}
		changed = true
	}
	if len(failed) > 0 {
		return fmt.Errorf("%w: %s", ErrOwnedUnitCollision, strings.Join(failed, ", "))
	}
	if changed {
		if err := syncDir(dir); err != nil {
			return err
		}
	}
	return b.daemonReloadOwnedUnits(ctx, systemctl, plan.writeNames())
}

// applyOwnedUnitDeletes removes only content the journal proves the supervisor
// rendered, re-reading immediately before removal. A digest mismatch retains
// the file: the catalog is already committed without the profile, so the file
// becomes an orphan the preflight scan flags for explicit operator recovery.
func (b Backend) applyOwnedUnitDeletes(ctx context.Context, home string, j unitJournal) error {
	dir := ownedUnitDirectory(home)
	names := make([]string, 0, len(j.Deletes))
	for name := range j.Deletes {
		names = append(names, name)
	}
	sort.Strings(names)
	removed := false
	for _, name := range names {
		path := filepath.Join(dir, name)
		err := provenDelete(path, j.Deletes[name])
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		removed = true
	}
	if removed {
		return syncDir(dir)
	}
	return nil
}

// daemonReloadOwnedUnits reloads the user manager once and requires systemd to
// report no pending reload for every touched unit before evidence is trusted.
// verifyOwnedUnitBindings mirrors the runtime preflight rule before commit:
// each written unit must load from exactly the file setup wrote, with no
// drop-ins, or the applied catalog would wedge the next supervisor start.
func (b Backend) verifyOwnedUnitBindings(ctx context.Context, systemctl, home string, units []string) error {
	for _, unit := range units {
		out, err := b.runCommand(ctx, systemctl, "--user", "show", "--property=FragmentPath", "--property=DropInPaths", "--", unit)
		if err != nil {
			return fmt.Errorf("inspect %s: %w", unit, err)
		}
		values, err := gpuruntime.ParseUnitProperties(out)
		if err != nil {
			return err
		}
		if err := gpuruntime.CheckNativeBinding(values, unit, filepath.Join(ownedUnitDirectory(home), unit)); err != nil {
			return err
		}
	}
	return nil
}

func (b Backend) daemonReloadOwnedUnits(ctx context.Context, systemctl string, units []string) error {
	if _, err := b.runCommand(ctx, systemctl, "--user", "daemon-reload"); err != nil {
		return fmt.Errorf("daemon-reload: %w", err)
	}
	for _, unit := range units {
		out, err := b.runCommand(ctx, systemctl, "--user", "show", "--property=NeedDaemonReload", "--", unit)
		if err != nil {
			return fmt.Errorf("inspect %s: %w", unit, err)
		}
		if strings.TrimSpace(string(out)) != "NeedDaemonReload=no" {
			return fmt.Errorf("%s still pending daemon reload", unit)
		}
	}
	return nil
}

func mkdirTrusted(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	return TrustedDirectory(path)
}

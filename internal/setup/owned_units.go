package setup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/deployment"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
)

var ErrOwnedUnitCollision = errors.New("owned unit path exists with foreign content")
var ErrOwnedUnitModified = errors.New("owned unit content changed; refusing removal")

type unitPlan struct {
	Writes  map[string][]byte // unit filename -> rendered content (absent in accepted catalog)
	Deletes []string          // unit filenames to remove (owned in accepted, absent in request)
	// proven records the accepted catalog's digest for units a delete or an
	// overwrite may touch, so only content the supervisor rendered is replaced.
	proven map[string]string
	// prior/absent snapshot the pre-write state so a failed pre-commit check
	// can restore the committed installation exactly.
	prior  map[string][]byte
	absent map[string]bool
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
	Version int               `json:"version"`
	Writes  map[string]string `json:"writes"`  // name -> sha256 hex
	Deletes map[string]string `json:"deletes"` // name -> sha256 hex (content proof captured before delete)
	Phase   string            `json:"phase"`   // "writes-pending" | "committed" | "done"
}

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
func (b Backend) planOwnedUnits(req Request, accepted control.CatalogSnapshot, home string) (unitPlan, error) {
	plan := unitPlan{Writes: map[string][]byte{}, proven: map[string]string{}, prior: map[string][]byte{}, absent: map[string]bool{}}
	for _, p := range accepted.Catalog.Profiles {
		if p.NativeModel == nil || p.NativeModel.Owned == nil {
			continue
		}
		raw, err := ownedRenderChecked(p)
		if err != nil {
			return plan, err
		}
		plan.proven[p.Unit] = digest(raw)
	}
	qualify := b.qualifyOwned
	if qualify == nil {
		qualify = gpuruntime.QualifyOwnedUnit
	}
	for _, p := range req.Catalog.Profiles {
		if p.NativeModel == nil || p.NativeModel.Owned == nil {
			continue
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
	for _, p := range accepted.Catalog.Profiles {
		if p.NativeModel == nil || p.NativeModel.Owned == nil {
			continue
		}
		if _, kept := plan.Writes[p.Unit]; kept {
			continue
		}
		if deleted[p.Unit] {
			// Shared Ollama pairs carry one unit file across profiles.
			continue
		}
		path := filepath.Join(ownedUnitDirectory(home), p.Unit)
		if ownedUnitStillReferenced(req.Catalog, path, plan.proven[p.Unit]) {
			// The requested catalog still claims this exact file (for example
			// an owned profile converted to adopted); keep it.
			continue
		}
		data, err := privateRead(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return plan, err
		}
		if digest(data) != plan.proven[p.Unit] {
			return plan, fmt.Errorf("%w: %s", ErrOwnedUnitModified, p.Unit)
		}
		plan.Deletes = append(plan.Deletes, p.Unit)
		deleted[p.Unit] = true
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

func newOwnedUnitJournal(plan unitPlan) unitJournal {
	j := unitJournal{Version: 1, Writes: map[string]string{}, Deletes: map[string]string{}, Phase: ownedJournalPending}
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
	if j.Version != 1 || j.Writes == nil || j.Deletes == nil || (j.Phase != ownedJournalPending && j.Phase != ownedJournalCommitted) {
		return j, false, errors.New("unsupported owned-units journal")
	}
	return j, true, nil
}

func writeOwnedUnitJournal(root string, j unitJournal) error {
	return writeJSON(ownedUnitJournalPath(root), j)
}

func clearOwnedUnitJournal(root string) error {
	if err := os.Remove(ownedUnitJournalPath(root)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
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
			currentDigest := digest(current)
			if currentDigest == digest(raw) {
				continue
			}
			if plan.proven[name] != currentDigest {
				return fmt.Errorf("%w: %s", ErrOwnedUnitCollision, name)
			}
			plan.prior[name] = current
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
	}
	return nil
}

// rollbackOwnedUnitWrites restores the pre-apply state after a failed
// pre-commit check: units the write created are removed, overwritten units get
// their snapshotted content back. Files whose content no longer matches the
// write are foreign modifications and are never destroyed.
func (b Backend) rollbackOwnedUnitWrites(ctx context.Context, home, systemctl string, plan unitPlan) error {
	dir := ownedUnitDirectory(home)
	var failed []string
	for _, name := range plan.writeNames() {
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
			continue
		}
		if err := deployment.AtomicWrite(path, plan.prior[name]); err != nil {
			failed = append(failed, name)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("%w: %s", ErrOwnedUnitCollision, strings.Join(failed, ", "))
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
	for _, name := range names {
		path := filepath.Join(dir, name)
		current, err := privateRead(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if digest(current) != j.Deletes[name] {
			return fmt.Errorf("%w: %s", ErrOwnedUnitModified, name)
		}
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	return nil
}

// daemonReloadOwnedUnits reloads the user manager once and requires systemd to
// report no pending reload for every touched unit before evidence is trusted.
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

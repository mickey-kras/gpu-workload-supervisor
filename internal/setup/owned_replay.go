package setup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

// replayCommittedDeletes replays a committed journal's proven deletes: units
// the request still renders are kept, everything else is removed with content
// proof, then systemd is refreshed and the journal retired.
func (b Backend) replayCommittedDeletes(ctx context.Context, home, root string, req Request, journal unitJournal) error {
	// With maintenance clear, req can be a different request than the one
	// that wrote the journal. Keep a key that the committed catalog in the
	// journal's state database or the request still uses.
	snapshot, err := ReadCatalog(ctx, journal.StatePath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var reloaded []string
	removed := false
	for name, proof := range journal.Deletes {
		deleted, err := replayCommittedDelete(home, name, proof, snapshot.Catalog, req.Catalog)
		if err != nil {
			return err
		}
		// A delete that already ran still needs the reload below: systemd may
		// still have the unit cached.
		reloaded = append(reloaded, name)
		removed = removed || deleted
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

// replayCommittedDelete removes one committed unit with content proof, and its
// key file. A committed delete is finished even when the new request re-adds
// the unit: the accepted catalog already excludes it, so preserving the file
// would leave it unjournaled if this attempt then fails. The resumed plan
// recreates the unit and its key from the request instead.
func replayCommittedDelete(home, name, proof string, catalogs ...control.Catalog) (bool, error) {
	path := filepath.Join(ownedUnitDirectory(home), name)
	current, err := privateRead(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	deleted := err == nil
	if deleted {
		if digest(current) != proof {
			return false, fmt.Errorf(ownedUnitErrorFormat, ErrOwnedUnitModified, name)
		}
		if err := provenDelete(path, proof); err != nil {
			return false, err
		}
	}
	return deleted, removeOwnedKey(home, name, catalogs...)
}

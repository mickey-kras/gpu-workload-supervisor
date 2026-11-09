package setup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// retireActivation removes the activation record so only a genuinely
// interrupted activation guards the owned-units journal, then bounds the
// retained rollback tuples.
func retireActivation(root string) error {
	if err := os.Remove(filepath.Join(root, "activation.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := pruneActivationBackups(filepath.Join(root, "backups"), retainedActivationBackups); err != nil {
		return fmt.Errorf("activation committed; pruning old activation backups failed: %w", err)
	}
	return nil
}

// retainedActivationBackups bounds backups/activation-* growth. The newest
// tuple is the rollback for the activation that just committed.
const retainedActivationBackups = 3

// pruneActivationBackups removes all but the newest keep activation-* tuples.
// It runs only after a committed activation, so no tuple is in use.
func pruneActivationBackups(root string, keep int) error {
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	type tuple struct {
		name     string
		modified time.Time
	}
	var tuples []tuple
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "activation-") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		tuples = append(tuples, tuple{entry.Name(), info.ModTime()})
	}
	if len(tuples) <= keep {
		return nil
	}
	slices.SortFunc(tuples, func(a, b tuple) int { return b.modified.Compare(a.modified) })
	for _, old := range tuples[keep:] {
		if err := os.RemoveAll(filepath.Join(root, old.name)); err != nil {
			return err
		}
	}
	return syncDirectory(root)
}

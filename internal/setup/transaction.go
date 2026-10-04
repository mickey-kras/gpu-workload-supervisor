// Package setup applies only explicitly owned user integration files.
package setup

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/deployment"
)

type Transaction struct {
	Root    string
	Changes map[string][]byte
}
type Hooks struct {
	Quiescent func() error
	Commit    func() error
	Committed func() (bool, error)
}
type entry struct {
	Before  []byte `json:"before,omitempty"`
	Existed bool   `json:"existed"`
	After   []byte `json:"after"`
}
type journal struct {
	Version int              `json:"version"`
	Files   map[string]entry `json:"files"`
}
type ownership struct {
	Version int               `json:"version"`
	Files   map[string]string `json:"files"`
}

const manifestName = "ownership.json"
const journalName = "transaction.json"

// TrustedDirectory checks every ancestor without following symlinks. The effective
// user or root may own ancestors; only the effective user may own the leaf.
func TrustedDirectory(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("directory must be clean and absolute")
	}
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		uid := info.Sys().(*syscall.Stat_t).Uid
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || (info.Mode().Perm()&0022 != 0 && !(current != path && uid == 0 && info.Mode()&os.ModeSticky != 0)) || (uid != 0 && uid != uint32(os.Geteuid())) || (current == path && uid != uint32(os.Geteuid())) {
			return fmt.Errorf("untrusted directory %s", current)
		}
		if current == "/" {
			break
		}
	}
	return nil
}
func digest(data []byte) string { hash := sha256.Sum256(data); return hex.EncodeToString(hash[:]) }
func privateRead(path string) ([]byte, error) {
	file, err := deployment.OpenPrivate(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 128*1024*1024+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 128*1024*1024 {
		return nil, errors.New("owned file exceeds size limit")
	}
	return data, nil
}
func writeJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return deployment.AtomicWrite(path, data)
}
func (tx Transaction) paths() (string, string, error) {
	if err := TrustedDirectory(tx.Root); err != nil {
		return "", "", err
	}
	for path := range tx.Changes {
		if path == "" || strings.ContainsAny(path, "/\\") || path == manifestName || path == journalName || path == "." || path == ".." {
			return "", "", errors.New("invalid integration filename")
		}
	}
	return filepath.Join(tx.Root, manifestName), filepath.Join(tx.Root, journalName), nil
}

// Apply is called with controller and proxy maintenance gates held. It records
// before/after images before applying prerequisites, and resolves ambiguous
// commit outcomes from durable catalog state instead of rolling it back.
func (tx Transaction) Apply(h Hooks) error {
	manifestPath, journalPath, err := tx.paths()
	if err != nil {
		return err
	}
	owned := ownership{Version: 1, Files: map[string]string{}}
	if data, err := privateRead(manifestPath); err == nil {
		if err = json.Unmarshal(data, &owned); err != nil {
			return err
		}
		if owned.Version != 1 {
			return errors.New("unsupported ownership manifest")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	pending := journal{Version: 1, Files: map[string]entry{}}
	if data, err := privateRead(journalPath); err == nil {
		if err = json.Unmarshal(data, &pending); err != nil {
			return err
		}
		if pending.Version != 1 {
			return errors.New("unsupported setup journal")
		}
		// Resume the recorded plan, never a newly submitted replacement.
		if len(pending.Files) != len(tx.Changes) {
			return errors.New("pending setup differs; resume original plan")
		}
		for name, e := range pending.Files {
			desired, exists := tx.Changes[name]
			if !exists || digest(desired) != digest(e.After) {
				return errors.New("pending setup differs; resume original plan")
			}
		}
		committed, err := h.Committed()
		if err != nil {
			return err
		}
		if committed {
			return tx.finalize(manifestPath, journalPath, owned, pending)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	} else {
		for name, after := range tx.Changes {
			before, err := privateRead(filepath.Join(tx.Root, name))
			exists := err == nil
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			expected, isOwned := owned.Files[name]
			if exists && (!isOwned || digest(before) != expected) {
				return fmt.Errorf("refusing unowned or modified integration %s", name)
			}
			pending.Files[name] = entry{Before: before, Existed: exists, After: after}
		}
		if err := writeJSON(journalPath, pending); err != nil {
			return err
		}
	}
	if err := h.Quiescent(); err != nil {
		return err
	}
	names := make([]string, 0, len(pending.Files))
	for name := range pending.Files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		e := pending.Files[name]
		current, err := privateRead(filepath.Join(tx.Root, name))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil && digest(current) != digest(e.Before) && digest(current) != digest(e.After) {
			return errors.New("integration changed during setup")
		}
		if err := deployment.AtomicWrite(filepath.Join(tx.Root, name), e.After); err != nil {
			return errors.Join(err, tx.rollback(pending))
		}
	}
	if err := h.Commit(); err != nil {
		committed, inspectErr := h.Committed()
		if inspectErr != nil {
			return errors.Join(err, inspectErr)
		}
		if !committed {
			return errors.Join(err, tx.rollback(pending))
		}
	}
	return tx.finalize(manifestPath, journalPath, owned, pending)
}
func (tx Transaction) rollback(pending journal) error {
	for name, e := range pending.Files {
		path := filepath.Join(tx.Root, name)
		current, err := privateRead(path)
		if errors.Is(err, os.ErrNotExist) && !e.Existed {
			continue
		}
		if err != nil {
			return err
		}
		if digest(current) != digest(e.After) && digest(current) != digest(e.Before) {
			return errors.New("refusing rollback of modified integration")
		}
		if e.Existed {
			if err := deployment.AtomicWrite(path, e.Before); err != nil {
				return err
			}
		} else if err := os.Remove(path); err != nil {
			return err
		}
	}
	return os.Remove(filepath.Join(tx.Root, journalName))
}
func (tx Transaction) finalize(manifestPath, journalPath string, owned ownership, pending journal) error {
	for name, e := range pending.Files {
		data, err := privateRead(filepath.Join(tx.Root, name))
		if err != nil {
			return err
		}
		if digest(data) != digest(e.After) {
			return errors.New("committed integration changed; repair forward")
		}
		owned.Files[name] = digest(e.After)
	}
	if err := writeJSON(manifestPath, owned); err != nil {
		return err
	}
	return os.Remove(journalPath)
}

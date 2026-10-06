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
	write   func(path string, data []byte) error
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
	owned, err := readOwnership(manifestPath)
	if err != nil {
		return err
	}
	pending, alreadyCommitted, err := tx.prepare(journalPath, owned, h)
	if err != nil {
		return err
	}
	if err := h.Quiescent(); err != nil {
		return err
	}
	if err := tx.applyFiles(pending, alreadyCommitted); err != nil {
		return err
	}
	if !alreadyCommitted {
		if err := tx.commit(pending, h); err != nil {
			return err
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

func readOwnership(path string) (ownership, error) {
	owned := ownership{Version: 1, Files: map[string]string{}}
	data, err := privateRead(path)
	if errors.Is(err, os.ErrNotExist) {
		return owned, nil
	}
	if err != nil {
		return owned, err
	}
	if err := json.Unmarshal(data, &owned); err != nil {
		return owned, err
	}
	if owned.Version != 1 || owned.Files == nil {
		return owned, errors.New("unsupported ownership manifest")
	}
	return owned, nil
}
func (tx Transaction) prepare(path string, owned ownership, h Hooks) (journal, bool, error) {
	pending := journal{Version: 1, Files: map[string]entry{}}
	data, err := privateRead(path)
	if err == nil {
		return tx.resume(data, pending, h)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return pending, false, err
	}
	for name, after := range tx.Changes {
		before, err := privateRead(filepath.Join(tx.Root, name))
		exists := err == nil
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return pending, false, err
		}
		expected, isOwned := owned.Files[name]
		if exists && (!isOwned || digest(before) != expected) {
			return pending, false, fmt.Errorf("refusing unowned or modified integration %s", name)
		}
		pending.Files[name] = entry{Before: before, Existed: exists, After: after}
	}
	return pending, false, writeJSON(path, pending)
}

func (tx Transaction) applyFiles(pending journal, alreadyCommitted bool) error {
	write := tx.write
	if write == nil {
		write = deployment.AtomicWrite
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
		if err := write(filepath.Join(tx.Root, name), e.After); err != nil {
			if alreadyCommitted {
				return err
			}
			return errors.Join(err, tx.rollback(pending))
		}
	}
	return nil
}

func (tx Transaction) commit(pending journal, h Hooks) error {
	if err := h.Commit(); err != nil {
		committed, inspectErr := h.Committed()
		if inspectErr != nil {
			return errors.Join(err, inspectErr)
		}
		if !committed {
			return errors.Join(err, tx.rollback(pending))
		}
	}
	return nil
}

func (tx Transaction) resume(data []byte, pending journal, h Hooks) (journal, bool, error) {
	if err := json.Unmarshal(data, &pending); err != nil {
		return pending, false, err
	}
	if pending.Version != 1 {
		return pending, false, errors.New("unsupported setup journal")
	}
	if len(pending.Files) != len(tx.Changes) {
		return pending, false, errors.New("pending setup differs; resume original plan")
	}
	for name, e := range pending.Files {
		desired, exists := tx.Changes[name]
		if !exists || digest(desired) != digest(e.After) {
			return pending, false, errors.New("pending setup differs; resume original plan")
		}
	}
	committed, err := h.Committed()
	return pending, committed, err
}

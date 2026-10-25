// Package modelseal produces model attestation material and checks signed
// attestations with the shared internal/attestation verifier. It never
// receives, stores or uses a signing key.
package modelseal

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// Output file names inside a sealed directory.
const (
	EnvelopeFile     = "envelope.json"
	SigningBytesFile = "envelope.signing-bytes"
	ReportFile       = "report.json"
)

// cleanAbsolute accepts clean absolute paths other than the file system root.
func cleanAbsolute(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && path != "/" && !strings.ContainsAny(path, "\x00\r\n")
}

// within reports whether path is root or below it.
func within(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && (rel == "." || filepath.IsLocal(rel))
}

// noLinks rejects a missing path and a symbolic link at any level.
func noLinks(path string) error {
	for current := path; current != "/"; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("path is missing or contains a link")
		}
	}
	return nil
}

// trustedOwner accepts the effective user and root.
func trustedOwner(stat unix.Stat_t) bool {
	return stat.Uid == 0 || stat.Uid == uint32(os.Geteuid())
}

// privateDirectory accepts an unlinked directory that only its trusted owner
// can read, write or enter.
func privateDirectory(path string) error {
	if !cleanAbsolute(path) || noLinks(path) != nil {
		return errors.New("output parent must be a clean absolute path without links")
	}
	var stat unix.Stat_t
	if unix.Lstat(path, &stat) != nil || stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Mode&0o077 != 0 || !trustedOwner(stat) {
		return errors.New("output parent must be a private directory of the current user")
	}
	return nil
}

// openRegular opens one unlinked regular file without blocking on FIFOs.
func openRegular(path string) (*os.File, error) {
	if !cleanAbsolute(path) || noLinks(path) != nil {
		return nil, errors.New("input must be a clean absolute path without links")
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NONBLOCK|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, errors.New("input is unreadable")
	}
	f := os.NewFile(uintptr(fd), path)
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, errors.New("input must be a regular file")
	}
	return f, nil
}

// readTrusted reads one bounded regular file that no other user can change.
func readTrusted(path string, limit int64) ([]byte, error) {
	f, err := openRegular(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var stat unix.Stat_t
	if unix.Fstat(int(f.Fd()), &stat) != nil || stat.Mode&0o022 != 0 || !trustedOwner(stat) {
		return nil, errors.New("input ownership or permissions are unsafe")
	}
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(raw)) > limit {
		return nil, errors.New("input is unreadable or exceeds its limit")
	}
	return raw, nil
}

// output is one file that publishDirectory writes.
type output struct {
	name string
	data []byte
}

// publishDirectory writes the files into a private staging directory beside
// destination, then renames it into place. The rename never replaces an
// existing path, so readers see either no directory or the complete set.
func publishDirectory(destination string, files []output) (err error) {
	parent := filepath.Dir(destination)
	if !cleanAbsolute(destination) || privateDirectory(parent) != nil {
		return errors.New("output must be a new path in a private directory")
	}
	staging, err := os.MkdirTemp(parent, ".gpu-model-seal-*")
	if err != nil {
		return errors.New("output staging directory cannot be created")
	}
	defer func() {
		if err != nil {
			if os.RemoveAll(staging) != nil {
				err = errors.Join(err, errors.New("output staging cleanup failed"))
			}
		}
	}()
	for _, file := range files {
		if err := writeNew(filepath.Join(staging, file.name), file.data); err != nil {
			return err
		}
	}
	if syncDirectory(staging) != nil {
		return errors.New("output staging sync failed")
	}
	if unix.Renameat2(unix.AT_FDCWD, staging, unix.AT_FDCWD, destination, unix.RENAME_NOREPLACE) != nil {
		return errors.New("output cannot be published; destination must not exist")
	}
	if syncDirectory(parent) != nil {
		// The complete set is already in place; report the durability failure.
		return errors.New("output directory sync failed after publication")
	}
	return nil
}

func writeNew(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return errors.New("output file cannot be created")
	}
	_, writeErr := f.Write(data)
	syncErr := f.Sync()
	if errors.Join(writeErr, syncErr, f.Close()) != nil {
		return errors.New("output file write failed")
	}
	return nil
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

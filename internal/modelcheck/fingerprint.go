package modelcheck

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// FileStat is the file system identity of one model file at check time. The
// launch gate compares it instead of hashing the weights again: a write,
// a replacement or a restored copy changes the inode or the change time,
// which no user process can set back.
type FileStat struct {
	Size       int64  `json:"size"`
	Device     uint64 `json:"device"`
	Inode      uint64 `json:"inode"`
	ModifiedNs int64  `json:"modifiedNs"`
	ChangedNs  int64  `json:"changedNs"`
}

func statOf(f *os.File) (FileStat, error) {
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return FileStat{}, err
	}
	return FileStat{
		Size: st.Size, Device: st.Dev, Inode: st.Ino,
		ModifiedNs: st.Mtim.Nano(), ChangedNs: st.Ctim.Nano(),
	}, nil
}

// errFilesChanged tells that the model files differ from the checked files.
var errFilesChanged = errors.New("the model files changed after the check; check the model again")

// filesUnchanged confirms, without reading file contents, that root holds
// exactly the checked files and that each has its checked file identity.
func filesUnchanged(root string, stats map[string]FileStat) error {
	if err := noParentLinks(root); err != nil {
		return errFilesChanged
	}
	entries, err := readDirectory(root, MaxModelFiles+1)
	if err != nil {
		return errFilesChanged
	}
	seen := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() && name == toolMetadata {
			continue
		}
		want, ok := stats[name]
		if !ok {
			return errFilesChanged
		}
		if got, err := statPath(filepath.Join(root, name)); err != nil || got != want {
			return errFilesChanged
		}
		seen++
	}
	if seen != len(stats) {
		return errFilesChanged
	}
	return nil
}

func statPath(path string) (FileStat, error) {
	f, err := openRegular(path)
	if err != nil {
		return FileStat{}, err
	}
	defer f.Close()
	return statOf(f)
}

// validFileStats needs one fingerprint for each recorded file.
func validFileStats(files map[string]string, stats map[string]FileStat) error {
	if stats == nil {
		return nil
	}
	if len(stats) != len(files) {
		return fmt.Errorf("model check record needs one file fingerprint per file")
	}
	for name, stat := range stats {
		if _, ok := files[name]; !ok || stat.Size < 0 || stat.Inode == 0 {
			return fmt.Errorf("model check record has an invalid file fingerprint")
		}
	}
	return nil
}

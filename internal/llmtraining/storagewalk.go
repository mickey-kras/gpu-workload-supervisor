package llmtraining

import (
	"errors"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// statEntry reads one entry of an open directory without following a link.
func statEntry(dirfd int, name string, stat *unix.Stat_t) error {
	return unix.Fstatat(dirfd, name, stat, unix.AT_SYMLINK_NOFOLLOW)
}

// listNames reads at most limit entry names of an open directory.
func listNames(fd int, limit int) ([]string, error) {
	dup, err := unix.Dup(fd)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(dup), "storage directory")
	defer f.Close()
	names, err := f.Readdirnames(limit)
	if errors.Is(err, io.EOF) {
		return names, nil
	}
	return names, err
}

// storageWalk sums allocated blocks below one root. It opens every directory
// relative to the descriptor of its parent with O_NOFOLLOW, so a link that
// replaces a directory during the walk cannot redirect it. It stays on the
// device of the root: other mounts are not part of the total.
type storageWalk struct {
	device    uint64
	total     uint64
	remaining int
	limit     walkLimit
	hooks     walkHooks
}

// walkHooks are the filesystem calls of one walk.
type walkHooks struct {
	stat func(int, string, *unix.Stat_t) error
	list func(int, int) ([]string, error)
}

// allocatedBytes sums allocated blocks below root. Entries can disappear during
// a save; the walk skips them. The walk is not complete when it reaches the
// entry limit, the deadline or a stop, or cannot read an entry that exists.
func (h walkHooks) allocatedBytes(root string, remaining int, limit walkLimit) (uint64, int, bool) {
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return 0, remaining, true
	}
	if err != nil {
		return 0, remaining, false
	}
	defer unix.Close(fd)
	var stat unix.Stat_t
	if remaining == 0 || unix.Fstat(fd, &stat) != nil {
		return 0, remaining, false
	}
	w := storageWalk{device: stat.Dev, total: uint64(stat.Blocks) * 512, remaining: remaining - 1, limit: limit, hooks: h}
	complete := w.directory(fd)
	return w.total, w.remaining, complete
}

// directory adds the entries of one open directory, depth first. At most one
// descriptor per directory level is open at a time.
func (w *storageWalk) directory(fd int) bool {
	if w.limit.reached() {
		return false
	}
	names, err := w.hooks.list(fd, w.remaining+1)
	if err != nil || len(names) > w.remaining {
		return false
	}
	for _, name := range names {
		if w.remaining == 0 || w.limit.reached() || !w.entry(fd, name) {
			return false
		}
	}
	return true
}

func (w *storageWalk) entry(parent int, name string) bool {
	var stat unix.Stat_t
	if err := w.hooks.stat(parent, name, &stat); err != nil {
		// Only an entry that a save removed is not part of the total.
		return errors.Is(err, unix.ENOENT)
	}
	if stat.Dev != w.device {
		return true
	}
	w.remaining--
	w.total += uint64(stat.Blocks) * 512
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return true
	}
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return true
	}
	if err != nil {
		return false
	}
	defer unix.Close(fd)
	return w.directory(fd)
}

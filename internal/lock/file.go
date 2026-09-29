package lock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

type File struct {
	file *os.File
}

func Acquire(path string) (*File, error) {
	return acquire(path, unix.LOCK_EX)
}

// AcquireShared holds a lifetime lock used by every proxy for a state store.
func AcquireShared(path string) (*File, error) {
	return acquire(path, unix.LOCK_SH)
}

// TryAcquire takes an exclusive lock without waiting for active proxies.
func TryAcquire(path string) (*File, error) {
	return acquire(path, unix.LOCK_EX|unix.LOCK_NB)
}

func acquire(path string, operation int) (*File, error) {
	if path == "" {
		return nil, errors.New("lock path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	if err := unix.Flock(fd, operation); err != nil {
		file.Close()
		return nil, fmt.Errorf("acquire lock: %w", err)
	}
	return &File{file: file}, nil
}

func (f *File) Close() error {
	if f == nil || f.file == nil {
		return nil
	}
	unlockErr := unix.Flock(int(f.file.Fd()), unix.LOCK_UN)
	closeErr := f.file.Close()
	return errors.Join(unlockErr, closeErr)
}

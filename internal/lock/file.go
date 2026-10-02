package lock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

type File struct {
	mu   sync.Mutex
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
	if f == nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.file == nil {
		return nil
	}
	unlockErr := unix.Flock(int(f.file.Fd()), unix.LOCK_UN)
	closeErr := f.file.Close()
	f.file = nil
	return errors.Join(unlockErr, closeErr)
}

// AcquireContext waits for a cross-process request gate until ctx is canceled.
// Each acquisition has its own descriptor so concurrent readers release only
// their own shared lock.
func AcquireContext(ctx context.Context, path string, shared bool) (*File, error) {
	operation := unix.LOCK_EX | unix.LOCK_NB
	if shared {
		operation = unix.LOCK_SH | unix.LOCK_NB
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		gate, err := acquire(path, operation)
		if err == nil {
			return gate, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			return nil, err
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

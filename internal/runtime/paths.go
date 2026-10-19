package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Trust model: single-admin host. A path is trusted when, after symlink
// resolution, every component is owned by the current user or root and not
// world-writable (sticky bit exempts, e.g. /tmp). Group write by own groups
// (umask 002) is normal and allowed. Symlinks are followed; broken ones fail.

type pathKind int

const (
	kindDir pathKind = iota
	kindFile
	kindExec
)

func trustedComponent(path string, info os.FileInfo) error {
	if err := trustedOwner(path, info); err != nil {
		return err
	}
	if info.Mode().Perm()&0o002 != 0 && info.Mode()&os.ModeSticky == 0 {
		return fmt.Errorf("%s: world-writable; remove other-write or choose another path", path)
	}
	return nil
}

func trustedOwner(path string, info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%s: cannot inspect ownership", path)
	}
	if uid := stat.Uid; uid != 0 && uid != uint32(os.Getuid()) {
		return fmt.Errorf("%s: owned by uid %d, not by you or root", path, uid)
	}
	return nil
}

// trustPath resolves symlinks and checks leaf kind plus owner and
// world-write on the leaf and every ancestor directory.
func trustPath(path string, kind pathKind) (string, error) {
	resolved, err := trustPathCore(path, kind)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrLaunchUnsupported, err)
	}
	return resolved, nil
}

func trustPathCore(path string, kind pathKind) (string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", fmt.Errorf("%s: path must be absolute and clean", path)
	}
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		dirInfo, err := os.Lstat(dir)
		if err != nil {
			return "", fmt.Errorf("%s: parent directory unavailable", dir)
		}
		// Merged-usr aliases (/bin -> usr/bin) are symlinks: link modes are
		// meaningless, only their owner can retarget them.
		if dirInfo.Mode()&os.ModeSymlink != 0 {
			err = trustedOwner(dir, dirInfo)
		} else {
			err = trustedComponent(dir, dirInfo)
		}
		if err != nil {
			return "", err
		}
		if dir == string(filepath.Separator) {
			break
		}
	}
	current := "/"
	for _, part := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return "", fmt.Errorf("%s: missing or broken link", current)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			if err := trustedOwner(current, info); err != nil {
				return "", err
			}
		}
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("%s: missing or broken link", path)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("%s: missing or broken link", path)
	}
	switch kind {
	case kindDir:
		if !info.IsDir() {
			return "", fmt.Errorf("%s: not a directory", path)
		}
	case kindFile:
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("%s: not a regular file", path)
		}
	case kindExec:
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			return "", fmt.Errorf("%s: not an executable file", path)
		}
	}
	if err := trustedComponent(resolved, info); err != nil {
		return "", err
	}
	for dir := filepath.Dir(resolved); ; dir = filepath.Dir(dir) {
		dirInfo, err := os.Stat(dir)
		if err != nil {
			return "", fmt.Errorf("%s: parent directory unavailable", dir)
		}
		if err := trustedComponent(dir, dirInfo); err != nil {
			return "", err
		}
		if dir == string(filepath.Separator) {
			break
		}
	}
	return resolved, nil
}

func validateExecutable(path string) (string, error) {
	return trustPath(path, kindExec)
}

// ValidateGPUInventoryExecutable checks the fixed read-only GPU inventory
// command before setup runs it.
func ValidateGPUInventoryExecutable(path string) error {
	_, err := validateExecutable(path)
	return err
}

// ValidateSystemctlExecutable returns the canonical systemctl target to invoke.
func ValidateSystemctlExecutable(path string) (string, error) {
	return validateExecutable(path)
}

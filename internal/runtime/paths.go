package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

// Trust model: single-admin host. A path is trusted when, after symlink
// resolution, every component is owned by the current user or root and not
// world-writable. Root-owned sticky ancestor directories (e.g. /tmp) are
// permitted, but leaves must never be world-writable. Group write by own groups
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
	if info.Mode().Perm()&0o002 != 0 {
		return fmt.Errorf("%s: world-writable; remove other-write or choose another path", path)
	}
	if info.Mode().Perm()&0o020 != 0 {
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !myGroups()[stat.Gid] {
			return fmt.Errorf("%s: group-writable by gid %d, a group you are not in", path, gidOf(info))
		}
	}
	return nil
}

var myGroups = sync.OnceValue(func() map[uint32]bool {
	groups := map[uint32]bool{uint32(os.Getgid()): true}
	extra, _ := os.Getgroups()
	for _, gid := range extra {
		groups[uint32(gid)] = true
	}
	return groups
})

func gidOf(info os.FileInfo) uint32 {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return stat.Gid
	}
	return 0
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
	return trustPathCoreMode(path, kind, false)
}

func trustPathCoreMode(path string, kind pathKind, rootOnly bool) (string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", fmt.Errorf("%s: path must be absolute and clean", path)
	}
	resolved, err := resolveTrustedPath(path, rootOnly)
	if err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	if err := checkResolvedLeaf(resolved, kind); err != nil {
		return "", err
	}
	return resolved, nil
}

// Walk link targets before consuming the remaining path. Checking only the
// original and canonical paths misses replaceable intermediate link parents.
// Keep target components uncleaned so even directories traversed before .. are
// qualified, and bound expansions to reject cycles and excessive alias chains.
func resolveTrustedPath(path string, rootOnly bool) (string, error) {
	pending := append([]string{"/"}, strings.Split(strings.TrimPrefix(path, "/"), "/")...)
	prefix := "/"
	hops := 0
	for len(pending) > 0 {
		part := pending[0]
		pending = pending[1:]
		switch part {
		case "", ".":
			continue
		case "..":
			prefix = filepath.Dir(prefix)
			continue
		}
		current := filepath.Join(prefix, part)
		info, err := os.Lstat(current)
		if err != nil {
			return "", fmt.Errorf("%s: missing or broken link", current)
		}
		if err := checkPathComponent(current, info, len(pending) > 0, rootOnly); err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink == 0 {
			prefix = current
			continue
		}
		hops++
		target, err := trustedLinkTarget(current, hops)
		if err != nil {
			return "", err
		}
		if filepath.IsAbs(target) {
			prefix = "/"
		}
		pending = append(strings.Split(target, "/"), pending...)
	}
	return prefix, nil
}

func trustedLinkTarget(path string, hops int) (string, error) {
	if hops > 40 {
		return "", fmt.Errorf("%s: symlink resolution exceeds 40 hops", path)
	}
	target, err := os.Readlink(path)
	if err != nil || target == "" {
		return "", fmt.Errorf("%s: missing or broken link", path)
	}
	return target, nil
}

func checkPathComponent(path string, info os.FileInfo, ancestor, rootOnly bool) error {
	if rootOnly {
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != 0 || (info.Mode()&os.ModeSymlink == 0 && info.Mode().Perm()&0o022 != 0) {
			return fmt.Errorf("%s: account source must be root-owned and not group or world writable", path)
		}
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return trustedOwner(path, info)
	}
	if ancestor {
		if !info.IsDir() {
			return fmt.Errorf("%s: not a directory", path)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if ok && stat.Uid == 0 && info.Mode()&os.ModeSticky != 0 {
			return nil
		}
	}
	return trustedComponent(path, info)
}

func checkResolvedLeaf(resolved string, kind pathKind) error {
	info, err := os.Stat(resolved)
	if err != nil {
		return fmt.Errorf("%s: missing or broken link", resolved)
	}
	switch kind {
	case kindDir:
		if !info.IsDir() {
			return fmt.Errorf("%s: not a directory", resolved)
		}
	case kindFile:
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o444 == 0 {
			return fmt.Errorf("%s: not a readable regular file", resolved)
		}
	case kindExec:
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			return fmt.Errorf("%s: not an executable file", resolved)
		}
	}
	return trustedComponent(resolved, info)
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

package runtime

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

func validateExecutable(path string) (string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", errors.New("path must be absolute and clean")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", errors.New("executable target unavailable; select an existing trusted executable")
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", errors.New("executable target unavailable; select an existing trusted executable")
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", errors.New("target must be a regular executable")
	}
	if info.Mode().Perm()&0o022 != 0 {
		return "", errors.New("executable target is group or world writable; remove only the unintended write permission on this file or reinstall it in a trusted location")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 {
		return "", errors.New("executable target is not root-owned; install this executable through the system package manager or a trusted root-owned location")
	}
	if err := trustedAncestors(resolved); err != nil {
		return "", err
	}
	if err := trustedOriginalExecutablePath(path); err != nil {
		return "", err
	}
	return resolved, nil
}

func trustedAncestors(resolved string) error {
	for directory := filepath.Dir(resolved); ; directory = filepath.Dir(directory) {
		info, err := os.Stat(directory)
		if err != nil {
			return errors.New("executable ancestor unavailable; inspect access to its parent directories")
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != 0 || info.Mode().Perm()&0o022 != 0 {
			reason := "not root-owned"
			if info.Mode().Perm()&0o022 != 0 {
				reason = "group or world writable"
			}
			depth := len(strings.Split(strings.Trim(directory, "/"), "/"))
			return fmt.Errorf("executable ancestor component %d is %s; inspect that ancestor and install the executable in a trusted root-owned location", depth, reason)
		}
		if directory == string(filepath.Separator) {
			break
		}
	}
	return nil
}

// Both the canonical target and the retained command path must be trusted.
// A root-trusted target does not make a replaceable ancestor alias safe: the
// unit executes the retained path later, after inspection. Root-owned aliases
// under root-owned, non-writable directories (including merged-/usr) remain
// supported, while desktop-owned or writable path components cannot redirect
// the command between qualification and startup.
func trustedOriginalExecutablePath(path string) error {
	prefix := "/"
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for i, part := range append([]string{""}, parts...) {
		prefix = filepath.Join(prefix, part)
		info, err := os.Lstat(prefix)
		if err != nil {
			return errors.New("original executable path component unavailable; select an existing trusted executable")
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		reason := "not root-owned"
		if info.Mode()&os.ModeSymlink == 0 && info.Mode().Perm()&0022 != 0 {
			reason = "group or world writable"
		} else if ok && stat.Uid == 0 {
			continue
		}
		return fmt.Errorf("original executable path component %d is %s; install or select the executable through trusted root-owned directories and aliases", i, reason)
	}
	return nil
}

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
	if !filepath.IsAbs(path) {
		return "", errors.New("path must be absolute")
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

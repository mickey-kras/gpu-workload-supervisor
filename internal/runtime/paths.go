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

// Qualification follows every link hop, including ancestors hidden inside a
// link target. Checking just lexical prefixes and the final canonical target
// would miss replaceable intermediate links. Root-controlled merged-/usr
// aliases remain supported; errors identify component numbers without paths.
func trustedOriginalExecutablePath(path string) error {
	_, err := resolveTrustedRootPath(path, os.Lstat, os.Readlink)
	return err
}

func resolveTrustedRootPath(path string, lstat func(string) (os.FileInfo, error), readlink func(string) (string, error)) (string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", errors.New("trusted path must be absolute and clean")
	}
	pending := append([]string{"/"}, strings.Split(strings.TrimPrefix(path, "/"), "/")...)
	prefix := "/"
	hops, component := 0, 0
	for len(pending) > 0 {
		part := pending[0]
		pending = pending[1:]
		if part == "" || part == "." {
			continue
		}
		if part == ".." {
			prefix = filepath.Dir(prefix)
			continue
		}
		current := filepath.Join(prefix, part)
		if part == "/" {
			current = "/"
		}
		info, err := lstat(current)
		if err != nil {
			return "", errors.New("original executable path component unavailable; inspect the selected path and all alias targets")
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		reason := "not root-owned"
		link := info.Mode()&os.ModeSymlink != 0
		if !link && info.Mode().Perm()&0022 != 0 {
			reason = "group or world writable"
		} else if ok && stat.Uid == 0 {
			reason = ""
		}
		if reason != "" {
			return "", fmt.Errorf("original executable path component %d is %s; install or select the executable through trusted root-owned directories and aliases", component, reason)
		}
		component++
		if link {
			hops++
			if hops > 40 {
				return "", errors.New("executable alias resolution exceeds the supported hop limit; select a direct trusted target")
			}
			target, err := readlink(current)
			if err != nil || target == "" {
				return "", errors.New("executable alias target unavailable; select a direct trusted target")
			}
			next := strings.Split(strings.TrimPrefix(target, "/"), "/")
			if filepath.IsAbs(target) {
				prefix = "/"
				next = append([]string{"/"}, next...)
			}
			pending = append(next, pending...)
			continue
		}
		if len(pending) > 0 && !info.IsDir() {
			return "", errors.New("executable alias ancestry is not a directory; select a direct trusted target")
		}
		prefix = current
	}
	return prefix, nil
}

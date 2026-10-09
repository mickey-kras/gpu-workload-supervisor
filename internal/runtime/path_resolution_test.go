package runtime

import (
	"errors"
	"os"
	"syscall"
	"testing"
	"time"
)

type pathInfo struct {
	mode os.FileMode
	uid  uint32
}

func (i pathInfo) Name() string       { return "metadata-component" }
func (i pathInfo) Size() int64        { return 0 }
func (i pathInfo) Mode() os.FileMode  { return i.mode }
func (i pathInfo) ModTime() time.Time { return time.Time{} }
func (i pathInfo) IsDir() bool        { return i.mode.IsDir() }
func (i pathInfo) Sys() any           { return &syscall.Stat_t{Uid: i.uid} }

func TestTrustedPathRejectsHiddenAliasTargetsAndPreservesTrustedAliases(t *testing.T) {
	metadata := map[string]pathInfo{
		"/": {mode: os.ModeDir | 0755}, "/trusted": {mode: os.ModeDir | 0755}, "/usr": {mode: os.ModeDir | 0755}, "/usr/bin": {mode: os.ModeDir | 0755}, "/usr/bin/ollama": {mode: 0755},
		"/trusted/alias": {mode: os.ModeSymlink | 0777}, "/tmp": {mode: os.ModeDir | 0777}, "/tmp/second": {mode: os.ModeSymlink | 0777},
		"/bin": {mode: os.ModeSymlink | 0777}, "/trusted/relative": {mode: os.ModeSymlink | 0777}, "/trusted/loop": {mode: os.ModeSymlink | 0777},
		"/trusted/non-root": {mode: os.ModeSymlink | 0777, uid: 1000}, "/usr/bin/user-owned": {mode: 0755, uid: 1000}, "/trusted/alias2": {mode: os.ModeSymlink | 0777},
	}
	targets := map[string]string{"/trusted/alias": "/tmp/second", "/tmp/second": "/usr/bin", "/bin": "usr/bin", "/trusted/relative": "../usr/bin", "/trusted/loop": "loop", "/trusted/non-root": "/usr/bin", "/trusted/alias2": "/trusted/alias"}
	lstat := func(path string) (os.FileInfo, error) {
		info, ok := metadata[path]
		if !ok {
			return nil, os.ErrNotExist
		}
		return info, nil
	}
	readlink := func(path string) (string, error) {
		value, ok := targets[path]
		if !ok {
			return "", os.ErrNotExist
		}
		return value, nil
	}
	for _, path := range []string{"/trusted/alias/ollama", "/trusted/alias2/ollama", "/trusted/non-root/ollama", "/usr/bin/user-owned", "/trusted/loop/ollama", "/trusted/missing/ollama", "relative", "/usr/bin/ollama/child"} {
		if _, err := resolveTrustedRootPath(path, lstat, readlink); err == nil {
			t.Fatalf("untrusted or unresolvable alias chain accepted: %s", path)
		}
	}
	for _, path := range []string{"/bin/ollama", "/trusted/relative/ollama", "/usr/bin/ollama"} {
		got, err := resolveTrustedRootPath(path, lstat, readlink)
		if err != nil || got != "/usr/bin/ollama" {
			t.Fatalf("trusted alias rejected: %s %q %v", path, got, err)
		}
	}
	metadata["/tmp"] = pathInfo{mode: os.ModeDir | 0755}
	if got, err := resolveTrustedRootPath("/trusted/alias2/ollama", lstat, readlink); err != nil || got != "/usr/bin/ollama" {
		t.Fatalf("fully trusted multi-hop chain rejected: %q %v", got, err)
	}
	brokenReadlink := func(string) (string, error) { return "", errors.New("private alias error") }
	if _, err := resolveTrustedRootPath("/bin/ollama", lstat, brokenReadlink); err == nil {
		t.Fatal("unreadable alias accepted")
	}
}

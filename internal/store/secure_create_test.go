package store

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestStateFilePrecreationIsPrivateUnderPermissiveUmask(t *testing.T) {
	parent := t.TempDir()
	if err := os.Chmod(parent, 0755); err != nil {
		t.Fatal(err)
	}
	previous := syscall.Umask(0)
	defer syscall.Umask(previous)
	path := filepath.Join(parent, "state.db")
	if err := preparePath(path); err != nil {
		t.Fatal(err)
	}
	if err := secureStateFile(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 || info.Size() != 0 {
		t.Fatalf("initial file: %v size %d", info.Mode(), info.Size())
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(parent, "target"), path); err != nil {
		t.Fatal(err)
	}
	if err := secureStateFile(path); err == nil {
		t.Fatal("path conflict accepted")
	}
}

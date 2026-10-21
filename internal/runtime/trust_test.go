package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Trust rules: owner you/root, not world-writable, symlinks resolved.
// Group write by own groups is allowed; errors name the offending path.

func trustFixture(t *testing.T) (dir, file, exec string) {
	t.Helper()
	dir = t.TempDir()
	file = filepath.Join(dir, "conf.yaml")
	exec = filepath.Join(dir, "run.sh")
	for _, p := range []string{file, exec} {
		if err := os.WriteFile(p, []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(exec, 0755); err != nil {
		t.Fatal(err)
	}
	return dir, file, exec
}

func TestTrustAcceptsOwnGroupWritablePaths(t *testing.T) {
	dir, file, exec := trustFixture(t)
	if err := os.Chmod(dir, 0775); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(exec, 0775); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(file, 0664); err != nil {
		t.Fatal(err)
	}
	if _, err := trustPath(dir, kindDir); err != nil {
		t.Fatal("group-writable dir rejected", err)
	}
	if _, err := trustPath(file, kindFile); err != nil {
		t.Fatal("group-writable file rejected", err)
	}
	if _, err := trustPath(exec, kindExec); err != nil {
		t.Fatal("group-writable executable rejected", err)
	}
}

func TestTrustRejectsWorldWritableAndMissing(t *testing.T) {
	dir, file, _ := trustFixture(t)
	if err := os.Chmod(dir, 0777); err != nil {
		t.Fatal(err)
	}
	if _, err := trustPath(dir, kindDir); err == nil || !strings.Contains(err.Error(), dir) {
		t.Fatal("world-writable dir accepted or not named", err)
	}
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(file, 0666); err != nil {
		t.Fatal(err)
	}
	if _, err := trustPath(file, kindFile); err == nil || !strings.Contains(err.Error(), file) {
		t.Fatal("world-writable file accepted or not named", err)
	}
	missing := filepath.Join(dir, "gone")
	if _, err := trustPath(missing, kindFile); err == nil || !strings.Contains(err.Error(), missing) {
		t.Fatal("missing path accepted or not named", err)
	}
}

func TestTrustRejectsBrokenSymlinkAndAcceptsGoodOne(t *testing.T) {
	dir, file, exec := trustFixture(t)
	link := filepath.Join(dir, "link")
	if err := os.Symlink(filepath.Join(dir, "gone"), link); err != nil {
		t.Fatal(err)
	}
	if _, err := trustPath(link, kindFile); err == nil || !strings.Contains(err.Error(), link) {
		t.Fatal("broken symlink accepted or not named", err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	resolved, err := trustPath(link, kindFile)
	if err != nil || resolved != file {
		t.Fatal("good symlink not resolved", err)
	}
	if _, err := trustPath(exec, kindExec); err != nil {
		t.Fatal(err)
	}
}

func TestTrustRejectsWrongKind(t *testing.T) {
	dir, file, exec := trustFixture(t)
	if _, err := trustPath(file, kindDir); err == nil {
		t.Fatal("file accepted as directory")
	}
	if _, err := trustPath(dir, kindExec); err == nil {
		t.Fatal("directory accepted as executable")
	}
	if _, err := trustPath(exec, kindFile); err != nil {
		t.Fatal("executable rejected as file", err)
	}
}

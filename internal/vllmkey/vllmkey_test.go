package vllmkey

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func writeKeyFile(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "vllm.env")
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadAcceptsOnePrivateAssignment(t *testing.T) {
	key := strings.Repeat("a", 43)
	for _, content := range []string{Name + "=" + key + "\n", Name + "=" + key} {
		got, err := Read(writeKeyFile(t, content, 0o600))
		if err != nil || got != key {
			t.Fatalf("Read() = %q, %v", got, err)
		}
	}
}

func TestReadRejectsUnsafeOrMalformedFiles(t *testing.T) {
	key := strings.Repeat("b", 43)
	cases := map[string]func(t *testing.T) string{
		"relative":     func(*testing.T) string { return "vllm.env" },
		"unclean":      func(t *testing.T) string { return t.TempDir() + "/./vllm.env" },
		"missing":      func(t *testing.T) string { return filepath.Join(t.TempDir(), "missing") },
		"group read":   func(t *testing.T) string { return writeKeyFile(t, Name+"="+key, 0o640) },
		"short key":    func(t *testing.T) string { return writeKeyFile(t, Name+"=short", 0o600) },
		"other name":   func(t *testing.T) string { return writeKeyFile(t, "OTHER="+key, 0o600) },
		"second line":  func(t *testing.T) string { return writeKeyFile(t, Name+"="+key+"\nOTHER=1\n", 0o600) },
		"too large":    func(t *testing.T) string { return writeKeyFile(t, Name+"="+strings.Repeat("c", 600), 0o600) },
		"quoted value": func(t *testing.T) string { return writeKeyFile(t, Name+"=\""+key+"\"", 0o600) },
		"directory": func(t *testing.T) string {
			path := filepath.Join(t.TempDir(), "dir")
			if err := os.Mkdir(path, 0o600); err != nil {
				t.Fatal(err)
			}
			return path
		},
		"symlink": func(t *testing.T) string {
			target := writeKeyFile(t, Name+"="+key, 0o600)
			link := filepath.Join(t.TempDir(), "link")
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			return link
		},
	}
	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Read(path(t))
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("Read() error = %v", err)
			}
			if err != nil && strings.Contains(err.Error(), key) {
				t.Fatal("error message contains the key")
			}
		})
	}
}

func TestReadRejectsAnotherOwner(t *testing.T) {
	path := writeKeyFile(t, Name+"="+strings.Repeat("d", 43), 0o600)
	original := effectiveUID
	effectiveUID = func() int { return original() + 1 }
	t.Cleanup(func() { effectiveUID = original })
	if _, err := Read(path); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Read() error = %v", err)
	}
}

func TestEnsureCreatesPrivateKeyOnceAndKeepsIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys", "vllm.env")
	if err := Ensure(path); err != nil {
		t.Fatal(err)
	}
	first, err := Read(path)
	if err != nil || len(first) != 43 {
		t.Fatalf("Read() = %q, %v", first, err)
	}
	info, err := os.Stat(filepath.Dir(path))
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("directory mode = %v, %v", info, err)
	}
	if err := Ensure(path); err != nil {
		t.Fatal(err)
	}
	second, _ := Read(path)
	if second != first {
		t.Fatal("Ensure replaced a usable key")
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("temporary files remain: %v", entries)
	}
}

func TestEnsureNeverReplacesUnusableFile(t *testing.T) {
	path := writeKeyFile(t, "OTHER=1\n", 0o600)
	if err := Ensure(path); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Ensure() error = %v", err)
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != "OTHER=1\n" {
		t.Fatal("Ensure changed an existing file")
	}
}

func TestEnsureReportsCreationFailures(t *testing.T) {
	parent := writeKeyFile(t, "file", 0o600)
	if err := Ensure(filepath.Join(parent, "vllm.env")); err == nil {
		t.Fatal("Ensure accepted a file as the key directory")
	}
	original := randomRead
	randomRead = func([]byte) (int, error) { return 0, errors.New("no entropy") }
	t.Cleanup(func() { randomRead = original })
	if err := Ensure(filepath.Join(t.TempDir(), "vllm.env")); err == nil {
		t.Fatal("Ensure accepted a random source failure")
	}
}

func TestPublishReportsFailures(t *testing.T) {
	if err := publish(filepath.Join(t.TempDir(), "missing"), "/unused", nil); err == nil {
		t.Fatal("publish accepted a missing directory")
	}
	dir := t.TempDir()
	existing := writeKeyFile(t, "x", 0o600)
	if err := publish(dir, existing, []byte(Name+"="+strings.Repeat("e", 43))); err == nil {
		t.Fatal("publish replaced an existing file")
	}
	closed, err := os.CreateTemp(dir, "closed")
	if err != nil {
		t.Fatal(err)
	}
	_ = closed.Close()
	if err := writeSynced(closed, []byte("x")); err == nil {
		t.Fatal("writeSynced accepted a closed file")
	}
}

func TestReadErrorsNameTheFileAndTheReason(t *testing.T) {
	large := writeKeyFile(t, Name+"="+strings.Repeat("a", 43)+strings.Repeat("\n", 600), 0o600)
	if _, err := Read(large); err == nil || !strings.Contains(err.Error(), "larger than 512 bytes") || !strings.Contains(err.Error(), large) {
		t.Fatalf("size limit error = %v", err)
	}
	open := writeKeyFile(t, Name+"="+strings.Repeat("a", 43), 0o644)
	if _, err := Read(open); err == nil || !strings.Contains(err.Error(), "0600") || !strings.Contains(err.Error(), open) {
		t.Fatalf("mode error = %v", err)
	}
	missing := filepath.Join(t.TempDir(), "missing.env")
	if _, err := Read(missing); err == nil || !strings.Contains(err.Error(), "mode 0600") || !strings.Contains(err.Error(), "supervisor-managed") {
		t.Fatalf("missing file error = %v", err)
	}
	if err := Ensure(open); err == nil || !strings.Contains(err.Error(), "0600") || !strings.Contains(err.Error(), "correct or remove") {
		t.Fatalf("Ensure error = %v", err)
	}
}

func TestEnsureSyncsNewDirectoriesAndReportsSyncFailures(t *testing.T) {
	root := t.TempDir()
	var synced []string
	saved := syncDirectory
	syncDirectory = func(dir string) error { synced = append(synced, dir); return saved(dir) }
	t.Cleanup(func() { syncDirectory = saved })
	path := filepath.Join(root, "a", "b", "vllm.env")
	if err := Ensure(path); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{root: true, filepath.Join(root, "a"): true, filepath.Join(root, "a", "b"): true}
	for _, dir := range synced {
		delete(want, dir)
	}
	if len(want) != 0 {
		t.Fatalf("directories not synced: %v (synced %v)", want, synced)
	}
	for _, failing := range []string{filepath.Join(root, "c"), filepath.Join(root, "d", "e")} {
		syncDirectory = func(dir string) error {
			if dir == failing {
				return errors.New("sync failed")
			}
			return nil
		}
		if err := Ensure(filepath.Join(failing, "vllm.env")); err == nil {
			t.Fatalf("sync failure of %s ignored", failing)
		}
	}
	syncDirectory = func(dir string) error {
		if dir == root {
			return errors.New("sync failed")
		}
		return nil
	}
	if err := Ensure(filepath.Join(root, "f", "vllm.env")); err == nil {
		t.Fatal("parent sync failure ignored")
	}
	syncDirectory = saved
	if err := syncDirectory(filepath.Join(root, "missing")); err == nil {
		t.Fatal("missing directory synced")
	}
}

// keyFileCommand is the documented way to create a key file for an existing
// launch (docs/WORKLOADS.md). KEY_FILE names the file.
const keyFileCommand = `umask 077 && printf 'VLLM_API_KEY=%s\n' "$(head -c 32 /dev/urandom | base64 | tr '+/' '-_' | tr -d '=\n')" > "$KEY_FILE"`

func TestDocumentedKeyFileCommandCreatesAUsableFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vllm.env")
	command := exec.Command("/bin/sh", "-c", keyFileCommand)
	command.Env = append(os.Environ(), "KEY_FILE="+path)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, output)
	}
	key, err := Read(path)
	if err != nil || len(key) != 43 {
		t.Fatalf("documented key file is not usable: %q %v", key, err)
	}
	raw, _ := os.ReadFile(filepath.Join("..", "..", "docs", "WORKLOADS.md"))
	if !strings.Contains(string(raw), strings.ReplaceAll(keyFileCommand, `"$KEY_FILE"`, `"$HOME/.config/vllm-api-key.env"`)) {
		t.Fatal("docs/WORKLOADS.md does not show the tested command")
	}
}

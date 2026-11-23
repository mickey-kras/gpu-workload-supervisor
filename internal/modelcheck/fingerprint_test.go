package modelcheck

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestAllowLaunchComparesFileFingerprints changes the checked model in each
// way that the gate must see, without hashing the weights again.
func TestAllowLaunchComparesFileFingerprints(t *testing.T) {
	for name, change := range map[string]func(t *testing.T, root string){
		"rewritten file": func(t *testing.T, root string) {
			path := filepath.Join(root, weightFile(t, root))
			writeFile(t, path, readFile(t, path))
		},
		"replaced file": func(t *testing.T, root string) {
			path := filepath.Join(root, weightFile(t, root))
			raw := readFile(t, path)
			writeFile(t, path+".new", raw)
			if err := os.Rename(path+".new", path); err != nil {
				t.Fatal(err)
			}
		},
		"touched file": func(t *testing.T, root string) {
			later := time.Now().Add(time.Hour)
			if err := os.Chtimes(filepath.Join(root, "config.json"), later, later); err != nil {
				t.Fatal(err)
			}
		},
		"extra file": func(t *testing.T, root string) {
			writeFile(t, filepath.Join(root, "extra.bin"), []byte("pickle"))
		},
		"missing file": func(t *testing.T, root string) {
			if err := os.Remove(filepath.Join(root, "config.json")); err != nil {
				t.Fatal(err)
			}
		},
		"linked file": func(t *testing.T, root string) {
			path := filepath.Join(root, "config.json")
			writeFile(t, path+".real", readFile(t, path))
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(path+".real", path); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			record := launchRecord(t)
			if len(record.FileStats) != len(record.Files) {
				t.Fatalf("record has %d fingerprints for %d files", len(record.FileStats), len(record.Files))
			}
			if err := record.AllowLaunch(record.Root, record.ChatTemplateSHA256); err != nil {
				t.Fatalf("unchanged model = %v", err)
			}
			change(t, record.Root)
			err := record.AllowLaunch(record.Root, record.ChatTemplateSHA256)
			if !errors.Is(err, ErrLaunchRefused) || !strings.Contains(err.Error(), "files changed") || strings.Contains(err.Error(), record.Root) {
				t.Fatalf("AllowLaunch = %v; want a refusal without a path", err)
			}
		})
	}
}

func TestAllowLaunchNeedsFingerprint(t *testing.T) {
	record := launchRecord(t)
	record.FileStats = nil
	if err := record.AllowLaunch(record.Root, record.ChatTemplateSHA256); !errors.Is(err, ErrLaunchRefused) || !strings.Contains(err.Error(), "no file fingerprint") {
		t.Fatalf("record without fingerprint = %v", err)
	}
	record.FileStats = map[string]FileStat{"other": {Size: 1, Inode: 1}}
	if err := record.Validate(); err == nil {
		t.Fatal("record with fingerprints of other files validated")
	}
}

// weightFile names the first safetensors file of the model fixture.
func weightFile(t *testing.T, root string) string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(root, "*.safetensors"))
	if err != nil || len(matches) == 0 {
		t.Fatalf("no weight file in the fixture: %v", err)
	}
	return filepath.Base(matches[0])
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func writeFile(t *testing.T, path string, raw []byte) {
	t.Helper()
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

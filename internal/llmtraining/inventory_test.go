package llmtraining

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileDigestNeedsACleanAbsolutePath(t *testing.T) {
	root := t.TempDir()
	want := writeFixture(t, filepath.Join(root, "one.safetensors"), "shard")
	if digest, err := FileDigest(filepath.Join(root, "one.safetensors")); err != nil || digest != want {
		t.Fatal("file digest differs from the fixture digest", err)
	}
	for _, path := range []string{"relative", root + "/", "/"} {
		if _, err := FileDigest(path); err == nil {
			t.Fatalf("file %q was accepted", path)
		}
	}
}

func TestCheckShardIndexBindsDigestAndLimit(t *testing.T) {
	root := t.TempDir()
	index := `{"weight_map":{"a":"one.safetensors"}}`
	inventory := Inventory{Root: root, Files: map[string]string{
		"one.safetensors":              writeFixture(t, filepath.Join(root, "one.safetensors"), "shard"),
		"model.safetensors.index.json": writeFixture(t, filepath.Join(root, "model.safetensors.index.json"), index),
	}}
	if err := CheckShardIndex(inventory, "model.safetensors.index.json", int64(len(index))); err != nil {
		t.Fatal(err)
	}
	if err := CheckShardIndex(inventory, "model.safetensors.index.json", int64(len(index))-1); !errors.Is(err, ErrShardIndexTooLarge) {
		t.Fatal("an index above the limit must report its size", err)
	}
	large := `{"weight_map":{"a":"one.safetensors"},"metadata":{"pad":"` + strings.Repeat("x", 1<<20) + `"}}`
	inventory.Files["model.safetensors.index.json"] = writeFixture(t, filepath.Join(root, "model.safetensors.index.json"), large)
	if err := CheckShardIndex(inventory, "model.safetensors.index.json", 2<<20); err != nil {
		t.Fatal("a caller limit above 1 MiB must admit a larger index", err)
	}
	if err := inspectShardIndexes(inventory); !errors.Is(err, ErrShardIndexTooLarge) {
		t.Fatal("training keeps its 1 MiB limit", err)
	}
	if err := os.WriteFile(filepath.Join(root, "model.safetensors.index.json"), []byte(`{"weight_map":{"b":"one.safetensors"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := inspectShardIndexes(inventory); !errors.Is(err, ErrShardIndexChanged) {
		t.Fatal("a shard index changed after the inventory was accepted", err)
	}
}

package llmtraining

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInventoryDirectoryBindsEveryFileAndShardIndex(t *testing.T) {
	root := t.TempDir()
	want := map[string]string{
		"config.json":                  writeFixture(t, filepath.Join(root, "config.json"), "{}"),
		"one.safetensors":              writeFixture(t, filepath.Join(root, "one.safetensors"), "shard"),
		"model.safetensors.index.json": writeFixture(t, filepath.Join(root, "model.safetensors.index.json"), `{"weight_map":{"a":"one.safetensors"}}`),
	}
	inventory, err := InventoryDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	if inventory.Root != root || len(inventory.Files) != len(want) {
		t.Fatalf("unexpected inventory %+v", inventory)
	}
	for name, digest := range want {
		if inventory.Files[name] != digest {
			t.Fatalf("file %s has digest %s, want %s", name, inventory.Files[name], digest)
		}
	}
	if digest, err := FileDigest(filepath.Join(root, "one.safetensors")); err != nil || digest != want["one.safetensors"] {
		t.Fatal("file digest differs from the inventory digest", err)
	}
	writeFixture(t, filepath.Join(root, "extra.safetensors.index.json"), `{"weight_map":{"a":"absent.safetensors"}}`)
	if _, err := InventoryDirectory(root); err == nil {
		t.Fatal("a shard index that names an absent shard was accepted")
	}
	for _, path := range []string{"relative", root + "/", "/"} {
		if _, err := InventoryDirectory(path); err == nil {
			t.Fatalf("root %q was accepted", path)
		}
		if _, err := FileDigest(path); err == nil {
			t.Fatalf("file %q was accepted", path)
		}
	}
	empty := t.TempDir()
	if _, err := InventoryDirectory(empty); err == nil {
		t.Fatal("an empty inventory was accepted")
	}
	if err := os.Symlink(root, filepath.Join(empty, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := InventoryDirectory(filepath.Join(empty, "link")); err == nil {
		t.Fatal("a linked root was accepted")
	}
}

func TestShardIndexMustMatchItsInventoryDigest(t *testing.T) {
	root := t.TempDir()
	inventory := Inventory{Root: root, Files: map[string]string{
		"one.safetensors":              writeFixture(t, filepath.Join(root, "one.safetensors"), "shard"),
		"model.safetensors.index.json": writeFixture(t, filepath.Join(root, "model.safetensors.index.json"), `{"weight_map":{"a":"one.safetensors"}}`),
	}}
	if err := inspectShardIndexes(inventory); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "model.safetensors.index.json"), []byte(`{"weight_map":{"b":"one.safetensors"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := inspectShardIndexes(inventory); err == nil {
		t.Fatal("a shard index changed after the inventory was accepted")
	}
}

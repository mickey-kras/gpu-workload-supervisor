package modelcheck

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestFileDigestChecksAllBytesAndRejectsOversizedFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "model.safetensors")
	content := []byte("the complete weight file")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := FileDigest(context.Background(), path)
	sum := sha256.Sum256(content)
	if err != nil || got != hex.EncodeToString(sum[:]) {
		t.Fatalf("digest: %s, %v", got, err)
	}
	// Sparse file: the bound must be rejected before reading a terabyte.
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	err = f.Truncate((1 << 40) + 1)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if digest, err := FileDigest(context.Background(), path); err == nil || digest != "" {
		t.Fatal("oversized file produced a prefix digest")
	}
}

func TestFileDigestRejectsLinkedAncestors(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "real")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "model.safetensors"), []byte("w"), 0o600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(target, alias); err != nil {
		t.Fatal(err)
	}
	if digest, err := FileDigest(context.Background(), filepath.Join(alias, "model.safetensors")); err == nil || digest != "" {
		t.Fatal("linked directory was hashed")
	}
}

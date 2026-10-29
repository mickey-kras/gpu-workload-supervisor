package modelcheck

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/llmtraining"
	"golang.org/x/sys/unix"
)

// Inventory bounds.
const (
	MaxModelFiles = 4096
	// maxMetadataEntries bounds the download metadata of the hub tool.
	maxMetadataEntries = 2*MaxModelFiles + 1
)

// Reasons that the inventory gives to the file allowlist.
const (
	ReasonLinkOrNonRegular = "link-or-nonregular-file"
	ReasonTooManyFiles     = "too-many-files"
	ReasonInvalidShard     = "invalid-shard-index"
	ReasonShardTooLarge    = "shard-index-too-large"
)

// toolMetadata is the folder that `hf download --local-dir` writes into
// the model directory. The inventory skips it: no loader reads it, so the
// check never binds it. Anything else in .cache is rejected.
const toolMetadata = ".cache"

// Inventory gives the SHA-256 of each model file at the top level of root,
// as the supervisor calculates it. A reason that is not empty is a problem
// with the files: the digests then stop at the problem, in name order. An
// error means that the inventory could not complete.
func Inventory(root string) (map[string]string, string, error) {
	if !cleanAbsolute(root) {
		return nil, "", errors.New("model root must be a clean absolute path")
	}
	entries, err := readDirectory(root, MaxModelFiles+1)
	if errors.Is(err, errTooManyEntries) {
		return map[string]string{}, ReasonTooManyFiles, nil
	}
	if err != nil {
		return nil, "", err
	}
	files := map[string]string{}
	for _, entry := range entries {
		reason, err := addEntry(root, entry, files)
		if err != nil || reason != "" {
			return files, reason, err
		}
	}
	if len(files) > MaxModelFiles {
		return files, ReasonTooManyFiles, nil
	}
	reason, err := checkShardIndexes(root, files)
	return files, reason, err
}

func addEntry(root string, entry os.DirEntry, files map[string]string) (string, error) {
	name := entry.Name()
	switch {
	case entry.IsDir() && name == toolMetadata:
		return checkToolMetadata(filepath.Join(root, name))
	case entry.IsDir():
		return ReasonNestedFile, nil
	case !entry.Type().IsRegular():
		return ReasonLinkOrNonRegular, nil
	}
	digest, err := llmtraining.FileDigest(filepath.Join(root, name))
	if err != nil {
		return "", fmt.Errorf("model file is unreadable: %w", err)
	}
	files[name] = digest
	return "", nil
}

// checkToolMetadata accepts only .cache/huggingface/.gitignore and regular
// *.metadata and *.lock files in .cache/huggingface/download.
func checkToolMetadata(cache string) (string, error) {
	levels := []func(os.DirEntry) bool{
		func(e os.DirEntry) bool { return e.IsDir() && e.Name() == "huggingface" },
		func(e os.DirEntry) bool {
			return (e.Type().IsRegular() && e.Name() == ".gitignore") || (e.IsDir() && e.Name() == "download")
		},
		func(e os.DirEntry) bool {
			name := e.Name()
			stem := strings.TrimSuffix(strings.TrimSuffix(name, ".metadata"), ".lock")
			return e.Type().IsRegular() && stem != name && stem != ""
		},
	}
	directory := cache
	for _, accepts := range levels {
		next, reason, err := metadataLevel(directory, accepts)
		if err != nil || reason != "" || next == "" {
			return reason, err
		}
		directory = next
	}
	return "", nil
}

// metadataLevel checks one metadata directory and gives its accepted
// subdirectory, if any.
func metadataLevel(directory string, accepts func(os.DirEntry) bool) (string, string, error) {
	entries, err := readDirectory(directory, maxMetadataEntries)
	if errors.Is(err, errTooManyEntries) {
		return "", ReasonUnlistedFile, nil
	}
	if err != nil {
		return "", "", err
	}
	next := ""
	for _, entry := range entries {
		if !accepts(entry) {
			return "", ReasonUnlistedFile, nil
		}
		if entry.IsDir() {
			next = filepath.Join(directory, entry.Name())
		}
	}
	return next, "", nil
}

func checkShardIndexes(root string, files map[string]string) (string, error) {
	inventory := llmtraining.Inventory{Root: root, Files: files}
	for _, name := range sortedNames(files) {
		if !strings.HasSuffix(name, ".safetensors.index.json") {
			continue
		}
		err := llmtraining.CheckShardIndex(inventory, name, MaxJSONFileBytes)
		switch {
		case errors.Is(err, llmtraining.ErrShardIndexTooLarge):
			return ReasonShardTooLarge, nil
		case errors.Is(err, llmtraining.ErrShardIndexChanged):
			return "", errChanged
		case err != nil:
			return ReasonInvalidShard, nil
		}
	}
	return "", nil
}

var errTooManyEntries = errors.New("model directory has too many entries")

// readDirectory reads at most limit entries of one directory without
// following a link. Entries come in name order.
func readDirectory(path string, limit int) ([]os.DirEntry, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil, os.ErrNotExist
	}
	if err != nil {
		return nil, errors.New("model directory is unreadable or a link")
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	entries, err := f.ReadDir(limit + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, errors.New("model directory is unreadable")
	}
	if len(entries) > limit {
		return nil, errTooManyEntries
	}
	slices.SortFunc(entries, func(a, b os.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	return entries, nil
}

package llmtraining

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/strictjson"
	"golang.org/x/sys/unix"
)

const safetensorsExtension = ".safetensors"

// Inspect checks a snapshot in time. It neither locks these files against later
// changes nor approves executable contents or dependencies outside the inventory.
func (i Inventory) Inspect() error {
	if err := i.validate(); err != nil {
		return err
	}
	if err := noLinks(i.Root); err != nil {
		return err
	}
	seen := 0
	remaining := 65536 // Count entries below the root before descending.
	directories := []string{i.Root}
	for len(directories) != 0 {
		last := len(directories) - 1
		path := directories[last]
		directories = directories[:last]
		entries, err := boundedDirectoryEntries(path, remaining)
		if err != nil {
			return err
		}
		remaining -= len(entries)
		children, files, err := i.inspectEntries(path, entries)
		if err != nil {
			return err
		}
		directories = append(directories, children...)
		seen += files
	}
	if seen != len(i.Files) {
		return errors.New("candidate inventory files are missing")
	}
	return nil
}

func (i Inventory) inspectEntries(directory string, entries []fs.DirEntry) ([]string, int, error) {
	var directories []string
	seen := 0
	for _, entry := range entries {
		path := filepath.Join(directory, entry.Name())
		if entry.IsDir() {
			directories = append(directories, path)
			continue
		}
		rel, err := filepath.Rel(i.Root, path)
		if err != nil || !entry.Type().IsRegular() || i.Files[rel] == "" {
			return nil, 0, errors.New("candidate inventory contains an undeclared or nonregular file")
		}
		if err := matchFile(path, i.Files[rel], 1<<40); err != nil {
			return nil, 0, err
		}
		seen++
	}
	return directories, seen, nil
}

// Read at most the remaining global budget plus one overflow witness. Opening
// only directories without following a leaf link also avoids blocking on FIFOs.
func boundedDirectoryEntries(path string, remaining int) ([]fs.DirEntry, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, errors.New("candidate directory is unreadable")
	}
	f := os.NewFile(uintptr(fd), path)
	entries, readErr := f.ReadDir(remaining + 1)
	closeErr := f.Close()
	if (readErr != nil && !errors.Is(readErr, io.EOF)) || closeErr != nil {
		return nil, errors.New("candidate directory is unreadable")
	}
	if len(entries) > remaining {
		return nil, errors.New("candidate inventory exceeds the entry limit")
	}
	return entries, nil
}

func noLinks(path string) error {
	for current := path; current != "/"; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("candidate path is missing or contains a link")
		}
	}
	return nil
}

func matchFile(path, digest string, limit int64) error {
	f, err := openRegular(path)
	if err != nil {
		return errors.New("candidate file is unreadable")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > limit {
		return errors.New("candidate file is not a bounded regular file")
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, limit+1))
	if err != nil || n > limit || hex.EncodeToString(h.Sum(nil)) != digest {
		return errors.New("candidate file digest mismatch")
	}
	return nil
}

func inspectSnapshots(r Recipe) error {
	if r.Model.Inventory.Files["config.json"] == "" || r.Tokenizer.Inventory.Files["tokenizer_config.json"] == "" {
		return errors.New("candidate model or tokenizer configuration is missing")
	}
	weights := false
	for _, inventory := range []Inventory{r.Model.Inventory, r.Tokenizer.Inventory} {
		for path := range inventory.Files {
			ext := filepath.Ext(path)
			if ext != ".json" && ext != safetensorsExtension && ext != ".model" && ext != ".txt" {
				return errors.New("candidate assets must use local training weights and tokenizer data")
			}
		}
	}
	for path := range r.Model.Inventory.Files {
		weights = weights || strings.HasSuffix(path, safetensorsExtension)
	}
	if !weights {
		return errors.New("candidate requires safetensors training weights")
	}
	if err := inspectShardIndexes(r.Model.Inventory); err != nil {
		return err
	}
	if err := inspectConfig(filepath.Join(r.Model.Inventory.Root, "config.json"), true); err != nil {
		return err
	}
	return inspectConfig(filepath.Join(r.Tokenizer.Inventory.Root, "tokenizer_config.json"), false)
}

func inspectShardIndexes(inventory Inventory) error {
	for name := range inventory.Files {
		if strings.HasSuffix(name, ".safetensors.index.json") {
			if err := inspectShardIndex(inventory, name); err != nil {
				return err
			}
		}
	}
	return nil
}

func inspectShardIndex(inventory Inventory, name string) error {
	f, err := openRegular(filepath.Join(inventory.Root, name))
	if err != nil {
		return errors.New("candidate shard index is unreadable")
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	var index map[string]json.RawMessage
	if err != nil || !utf8.Valid(raw) || strictjson.DecodeLimited(bytes.NewReader(raw), 1<<20, &index) != nil {
		return errors.New("candidate shard index is invalid")
	}
	var weights map[string]string
	if json.Unmarshal(index["weight_map"], &weights) != nil || len(weights) == 0 {
		return errors.New("candidate shard index requires a nonempty weight map")
	}
	for _, path := range weights {
		if !relative(path) || filepath.Ext(path) != safetensorsExtension || inventory.Files[path] == "" {
			return errors.New("candidate shard index must reference inventoried safetensors weights")
		}
	}
	return nil
}

func inspectConfig(path string, model bool) error {
	f, err := openRegular(path)
	if err != nil {
		return errors.New("candidate asset configuration is unreadable")
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	var config map[string]json.RawMessage
	if err != nil || !utf8.Valid(raw) || strictjson.DecodeLimited(bytes.NewReader(raw), 1<<20, &config) != nil || config == nil {
		return errors.New("candidate asset configuration is invalid")
	}
	if _, exists := config["auto_map"]; exists {
		return errors.New("candidate remote-code mappings are unsupported")
	}
	if _, exists := config["quantization_config"]; model && exists {
		return errors.New("candidate requires original unquantized training weights")
	}
	if _, exists := config["transformers_weights"]; model && exists {
		return errors.New("candidate weight filename overrides are unsupported")
	}
	for _, key := range []string{"configuration_files", "attribute_map", "__dict__"} {
		if _, exists := config[key]; model && exists {
			return errors.New("candidate model configuration indirection is unsupported")
		}
	}
	var family string
	if model && (json.Unmarshal(config["model_type"], &family) != nil || family != "llama") {
		return errors.New("candidate supports only the llama model family")
	}
	return nil
}

func inspectPaths(r Recipe) error {
	if err := noLinks(r.OutputDirectory); err != nil {
		return err
	}
	if _, err := boundedDirectoryEntries(r.OutputDirectory, 0); err != nil {
		return errors.New("candidate output directory must exist and be empty")
	}
	for _, path := range []string{r.Model.Inventory.Root, r.Tokenizer.Inventory.Root, r.Runtime.Inventory.Root, r.Train.Path, r.HeldOut.Path} {
		if err := noLinks(path); err != nil {
			return err
		}
		if within(path, r.OutputDirectory) || within(r.OutputDirectory, path) {
			return errors.New("candidate output overlaps an input")
		}
	}
	return nil
}

func within(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && (rel == "." || filepath.IsLocal(rel))
}

func inspectDatasets(train, heldOut Dataset) (int, int, error) {
	seen := make(map[[32]byte]bool)
	trainRows, err := inspectDataset(train, seen, false)
	if err != nil {
		return 0, 0, err
	}
	heldOutRows, err := inspectDataset(heldOut, seen, true)
	return trainRows, heldOutRows, err
}

func inspectDataset(dataset Dataset, train map[[32]byte]bool, heldOut bool) (int, error) {
	f, err := openRegular(dataset.Path)
	if err != nil {
		return 0, errors.New("candidate dataset is unreadable")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 64<<20 {
		return 0, errors.New("candidate dataset exceeds 64 MiB or is not regular")
	}
	h := sha256.New()
	scanner := bufio.NewScanner(io.TeeReader(io.LimitReader(f, (64<<20)+1), h))
	scanner.Buffer(make([]byte, 4096), 64<<10)
	rows := 0
	for scanner.Scan() {
		digest, err := recordDigest(scanner.Bytes())
		if err != nil {
			return 0, err
		}
		if heldOut && train[digest] {
			return 0, errors.New("candidate held-out text overlaps training text")
		}
		if !heldOut {
			train[digest] = true
		}
		rows++
		if rows > 50000 {
			return 0, errors.New("candidate dataset exceeds 50000 records")
		}
	}
	if scanner.Err() != nil || rows == 0 || hex.EncodeToString(h.Sum(nil)) != dataset.SHA256 {
		return 0, errors.New("candidate dataset is invalid or its digest changed")
	}
	return rows, nil
}

func recordDigest(raw []byte) ([32]byte, error) {
	var record map[string]string
	if !utf8.Valid(raw) || strictjson.DecodeLimited(bytes.NewReader(raw), 64<<10, &record) != nil || len(record) != 1 || strings.TrimSpace(record["text"]) == "" {
		return [32]byte{}, errors.New("candidate dataset requires one bounded text object per line")
	}
	return sha256.Sum256([]byte(record["text"])), nil
}

// Nonblocking opens avoid waiting on FIFOs before checking their type. Reject
// leaf links as well; parent directories are only inspected, never locked.
func openRegular(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NONBLOCK|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, errors.New("candidate file is unreadable")
	}
	f := os.NewFile(uintptr(fd), path)
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		return nil, errors.New("candidate input must be a regular file")
	}
	return f, nil
}

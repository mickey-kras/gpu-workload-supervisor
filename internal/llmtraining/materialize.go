package llmtraining

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"path/filepath"
	"unicode/utf8"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/strictjson"
)

type LocalSnapshot struct {
	Root     string `json:"root"`
	Revision string `json:"revision"`
}

type MaterializeRequest struct {
	Version         int           `json:"version"`
	Preset          string        `json:"preset"`
	Model           LocalSnapshot `json:"model"`
	Tokenizer       LocalSnapshot `json:"tokenizer"`
	RuntimeRoot     string        `json:"runtimeRoot"`
	Train           string        `json:"train"`
	HeldOut         string        `json:"heldOut"`
	OutputDirectory string        `json:"outputDirectory"`
	Budget          Budget        `json:"budget"`
}

// Materialize records observed local bytes, not their authenticity or runtime approval.
func Materialize(inputPath, recipePath string) error {
	if !absolute(inputPath) || !absolute(recipePath) || inputPath == recipePath {
		return errors.New("materialization requires distinct absolute input and recipe paths")
	}
	if err := privateJobDirectory(filepath.Dir(recipePath)); err != nil {
		return err
	}
	request, err := readMaterializeRequest(inputPath)
	if err != nil {
		return err
	}
	r := request.recipe()
	if jobPathsOverlap(r, inputPath, recipePath) {
		return errors.New("materialization files overlap training inputs or output")
	}
	if err := inventoryRecipe(&r); err != nil {
		return err
	}
	ram, storage, err := available(r.OutputDirectory)
	if err != nil {
		return err
	}
	if _, err := Compile(r, ram, storage); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil || len(raw) >= 8<<20 {
		return errors.New("materialized recipe exceeds its encoding limit")
	}
	return publishPlan(recipePath, append(raw, '\n'))
}

func readMaterializeRequest(path string) (MaterializeRequest, error) {
	var request MaterializeRequest
	raw, err := readJobInput(path, 64<<10, false)
	if err != nil {
		return request, err
	}
	if !utf8.Valid(raw) || strictjson.DecodeLimited(bytes.NewReader(raw), 64<<10, &request) != nil {
		return MaterializeRequest{}, errors.New("invalid materialization input JSON")
	}
	canonical, err := json.Marshal(request)
	if err != nil || !matchingObjectKeys(raw, canonical) {
		return MaterializeRequest{}, errors.New("invalid materialization input JSON")
	}
	return request, request.validate()
}

func (r MaterializeRequest) validate() error {
	if r.Version != 1 || (r.Preset != "smoke-lora" && r.Preset != "representative-qlora") {
		return errors.New("unsupported materialization version or preset")
	}
	if !revisionPattern.MatchString(r.Model.Revision) || !revisionPattern.MatchString(r.Tokenizer.Revision) {
		return errors.New("materialization requires exact model and tokenizer revisions")
	}
	for _, path := range []string{r.Model.Root, r.Tokenizer.Root, r.RuntimeRoot, r.Train, r.HeldOut, r.OutputDirectory} {
		if !absolute(path) {
			return errors.New("materialization requires clean absolute local paths")
		}
	}
	_, err := r.Budget.StorageBytes()
	return err
}

func (r MaterializeRequest) recipe() Recipe {
	result := Recipe{
		Version: 1, Model: Snapshot{Revision: r.Model.Revision, Inventory: Inventory{Root: r.Model.Root}},
		Tokenizer: Snapshot{Revision: r.Tokenizer.Revision, Inventory: Inventory{Root: r.Tokenizer.Root}},
		Runtime:   Runtime{Inventory: Inventory{Root: r.RuntimeRoot}, Entrypoint: "bin/python3"},
		Train:     Dataset{Path: r.Train}, HeldOut: Dataset{Path: r.HeldOut}, OutputDirectory: r.OutputDirectory, Budget: r.Budget,
		Adapter: "lora", Rank: 8, Alpha: 16, SequenceLength: 128, MicroBatchSize: 1,
		GradientAccumulation: 1, MaxSteps: 2, LearningRate: 0.0002, Seed: 42,
	}
	if r.Preset == "representative-qlora" {
		result.Adapter, result.Rank, result.Alpha = "qlora", 16, 32
		result.SequenceLength, result.GradientAccumulation, result.MaxSteps = 2048, 4, 100
	}
	return result
}

func inventoryRecipe(r *Recipe) error {
	for _, inventory := range []*Inventory{&r.Model.Inventory, &r.Tokenizer.Inventory, &r.Runtime.Inventory} {
		files, err := inventoryLocalFiles(inventory.Root)
		if err != nil {
			return err
		}
		inventory.Files = files
	}
	for _, dataset := range []*Dataset{&r.Train, &r.HeldOut} {
		digest, err := localDigest(dataset.Path, 64<<20)
		if err != nil {
			return err
		}
		dataset.SHA256 = digest
	}
	return nil
}

// FileDigest is the lowercase hex SHA-256 of one regular file at a clean
// absolute path without links.
func FileDigest(path string) (string, error) {
	if !absolute(path) {
		return "", errors.New("file digest requires a clean absolute path")
	}
	return localDigest(path, 1<<40)
}

func inventoryLocalFiles(root string) (map[string]string, error) {
	if err := noLinks(root); err != nil {
		return nil, err
	}
	files := map[string]string{}
	directories := []string{root}
	remaining := 65536
	for len(directories) > 0 {
		path := directories[len(directories)-1]
		directories = directories[:len(directories)-1]
		entries, err := boundedDirectoryEntries(path, remaining)
		if err != nil {
			return nil, err
		}
		remaining -= len(entries)
		children, err := inventoryLocalEntries(root, path, entries, files)
		if err != nil {
			return nil, err
		}
		directories = append(directories, children...)
	}
	return files, nil
}

func inventoryLocalEntries(root, directory string, entries []fs.DirEntry, files map[string]string) ([]string, error) {
	var directories []string
	for _, entry := range entries {
		path := filepath.Join(directory, entry.Name())
		if entry.IsDir() {
			directories = append(directories, path)
			continue
		}
		if !entry.Type().IsRegular() || len(files) >= 32768 {
			return nil, errors.New("materialization inventory exceeds its limit or contains a nonregular file")
		}
		rel, err := filepath.Rel(root, path)
		if err != nil || !relative(rel) {
			return nil, errors.New("invalid materialization inventory path")
		}
		digest, err := localDigest(path, 1<<40)
		if err != nil {
			return nil, err
		}
		files[rel] = digest
	}
	return directories, nil
}

func localDigest(path string, limit int64) (string, error) {
	if err := noLinks(path); err != nil {
		return "", err
	}
	f, err := openRegular(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.Size() > limit {
		return "", errors.New("materialization input exceeds its file limit")
	}
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(f, limit+1))
	if err != nil || n > limit {
		return "", errors.New("materialization input could not be hashed within its limit")
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

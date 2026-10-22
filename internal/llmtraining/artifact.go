package llmtraining

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"path/filepath"
	"reflect"
	"unicode/utf8"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/strictjson"
)

const provenanceName = "supervisor-provenance.json"

var artifactNames = map[string]bool{
	"adapter_model.safetensors": true, "adapter_config.json": true,
	"tokenizer_config.json": true, "tokenizer.json": true, "tokenizer.model": true,
	"special_tokens_map.json": true, "added_tokens.json": true,
	"generation_config.json": true, "config.json": true, "vocab.json": true,
	"merges.txt": true, "vocab.txt": true, "chat_template.jinja": true,
}

type Provenance struct {
	Version           int               `json:"version"`
	SourceRevision    string            `json:"sourceRevision"`
	RecipeSHA256      string            `json:"recipeSHA256"`
	ModelRevision     string            `json:"modelRevision"`
	TokenizerRevision string            `json:"tokenizerRevision"`
	ModelSHA256       string            `json:"modelInventorySHA256"`
	TokenizerSHA256   string            `json:"tokenizerInventorySHA256"`
	RuntimeSHA256     string            `json:"runtimeInventorySHA256"`
	WorkerSHA256      string            `json:"workerSHA256"`
	InvocationID      string            `json:"invocationId"`
	AdapterSHA256     string            `json:"adapterSHA256"`
	EvalLoss          float64           `json:"evalLoss"`
	Files             map[string]string `json:"files"`
}

func digestJSON[T Inventory | Runtime](value T) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", errors.New("training identity cannot be encoded")
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}

func artifactFiles(r Recipe) (map[string]string, error) {
	if err := privateExecutionDirectory(r.OutputDirectory); err != nil {
		return nil, err
	}
	entries, err := boundedDirectoryEntries(r.OutputDirectory, 1024)
	if err != nil {
		return nil, err
	}
	files := make(map[string]string)
	var total uint64
	for _, entry := range entries {
		name := entry.Name()
		if !artifactNames[name] {
			if name == "adapter_model.bin" || name == "adapter_model.safetensors.index.json" || name == "chat_templates" {
				return nil, errors.New("unsupported training export layout")
			}
			continue
		}
		digest, size, err := hashArtifact(filepath.Join(r.OutputDirectory, name), r.Budget.ExportBytes-total)
		if err != nil {
			return nil, err
		}
		total += size
		files[name] = digest
	}
	if files["adapter_model.safetensors"] == "" || files["adapter_config.json"] == "" || files["tokenizer_config.json"] == "" || (files["tokenizer.json"] == "" && files["tokenizer.model"] == "") {
		return nil, errors.New("training export requires adapter weights, configuration and saved tokenizer")
	}
	if err := validateArtifactConfigs(r); err != nil {
		return nil, err
	}
	return files, nil
}

func hashArtifact(path string, remaining uint64) (string, uint64, error) {
	f, err := openRegular(path)
	if err != nil {
		return "", 0, errors.New("training export contains an unreadable or linked asset")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.Size() <= 0 || uint64(info.Size()) > remaining {
		return "", 0, errors.New("training export exceeds its budget or contains an empty asset")
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, int64(min(remaining, 1<<40))+1))
	if err != nil || n <= 0 || uint64(n) > remaining || n > 1<<40 {
		return "", 0, errors.New("training export cannot be hashed within its budget")
	}
	return hex.EncodeToString(h.Sum(nil)), uint64(n), nil
}

func artifactConfig(path string) (map[string]json.RawMessage, error) {
	f, err := openRegular(path)
	if err != nil {
		return nil, errors.New("training export configuration is unreadable")
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	var result map[string]json.RawMessage
	if err != nil || !utf8.Valid(raw) || strictjson.DecodeLimited(bytes.NewReader(raw), 1<<20, &result) != nil || result == nil {
		return nil, errors.New("training export configuration is invalid")
	}
	return result, nil
}

func validateArtifactConfigs(r Recipe) error {
	config, err := artifactConfig(filepath.Join(r.OutputDirectory, "adapter_config.json"))
	if err != nil {
		return err
	}
	for key, expected := range map[string]string{"peft_type": "LORA", "task_type": "CAUSAL_LM", "base_model_name_or_path": r.Model.Inventory.Root} {
		var actual string
		if json.Unmarshal(config[key], &actual) != nil || actual != expected {
			return errors.New("training export adapter does not match its base or supported format")
		}
	}
	for _, name := range []string{"adapter_config.json", "tokenizer_config.json"} {
		if err := rejectArtifactRedirects(filepath.Join(r.OutputDirectory, name)); err != nil {
			return err
		}
	}
	return nil
}

func rejectArtifactRedirects(path string) error {
	config, err := artifactConfig(path)
	if err != nil {
		return err
	}
	for _, key := range []string{"auto_map", "auto_mapping", "fast_tokenizer_files", "tokenizer_file", "vocab_file", "merges_file"} {
		if value, exists := config[key]; exists && !bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return errors.New("training export configuration redirects are unsupported")
		}
	}
	return nil
}

func newProvenance(r Recipe, job control.TrainingJob, invocation string, loss float64, files map[string]string) (Provenance, error) {
	model, err := digestJSON(r.Model.Inventory)
	if err != nil {
		return Provenance{}, err
	}
	tokenizer, err := digestJSON(r.Tokenizer.Inventory)
	if err != nil {
		return Provenance{}, err
	}
	runtime, err := digestJSON(r.Runtime)
	if err != nil {
		return Provenance{}, err
	}
	worker := sha256.Sum256(workerSource)
	return Provenance{2, SourceRevision, job.RecipeSHA256, r.Model.Revision, r.Tokenizer.Revision, model, tokenizer, runtime, hex.EncodeToString(worker[:]), invocation, files["adapter_model.safetensors"], loss, files}, nil
}

func publishProvenance(r Recipe, job control.TrainingJob, invocation string, loss float64, expected map[string]string) error {
	files, err := artifactFiles(r)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(files, expected) {
		return errors.New("training export changed during reload")
	}
	provenance, err := newProvenance(r, job, invocation, loss, files)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(provenance)
	if err != nil {
		return errors.New("training provenance cannot be encoded")
	}
	return publishPlan(filepath.Join(r.OutputDirectory, provenanceName), append(raw, '\n'))
}

// PrepareImport validates local PEFT artifact identity only. It neither qualifies
// a serving runtime nor copies, converts, registers or activates any model.
func PrepareImport(recipePath, planPath, format string) error {
	if format != "peft" || !absolute(recipePath) || !absolute(planPath) || recipePath == planPath {
		return errors.New("import preparation requires PEFT format and distinct absolute paths")
	}
	if err := privateExecutionDirectory(filepath.Dir(planPath)); err != nil {
		return err
	}
	raw, err := readJobInput(recipePath, 8<<20, false)
	if err != nil {
		return err
	}
	r, err := ReadRecipe(bytes.NewReader(raw))
	if err != nil {
		return err
	}
	for _, path := range []string{r.OutputDirectory, r.Model.Inventory.Root, r.Tokenizer.Inventory.Root, r.Runtime.Inventory.Root, r.Train.Path, r.HeldOut.Path} {
		if within(planPath, path) {
			return errors.New("import plan overlaps a training input or output")
		}
	}
	provenance, err := validateImport(r, raw)
	if err != nil {
		return err
	}
	plan := struct {
		Status           string     `json:"status"`
		Format           string     `json:"format"`
		Directory        string     `json:"directory"`
		ServingQualified bool       `json:"servingQualified"`
		Provenance       Provenance `json:"provenance"`
	}{"unqualified", "peft", r.OutputDirectory, false, provenance}
	encoded, err := json.Marshal(plan)
	if err != nil {
		return errors.New("import plan cannot be encoded")
	}
	return publishPlan(planPath, append(encoded, '\n'))
}

func validateImport(r Recipe, recipe []byte) (Provenance, error) {
	var provenance Provenance
	if err := privateExecutionDirectory(r.OutputDirectory); err != nil {
		return provenance, err
	}
	for _, inventory := range []Inventory{r.Model.Inventory, r.Tokenizer.Inventory, r.Runtime.Inventory} {
		if err := inventory.Inspect(); err != nil {
			return provenance, err
		}
	}
	raw, err := readJobInput(filepath.Join(r.OutputDirectory, provenanceName), 64<<10, true)
	if err != nil {
		return provenance, err
	}
	if strictjson.DecodeLimited(bytes.NewReader(raw), 64<<10, &provenance) != nil {
		return Provenance{}, errors.New("training provenance is invalid")
	}
	return matchImportProvenance(r, recipe, raw, provenance)
}

func matchImportProvenance(r Recipe, recipe, raw []byte, provenance Provenance) (Provenance, error) {
	files, err := artifactFiles(r)
	if err != nil {
		return Provenance{}, err
	}
	digest := sha256.Sum256(recipe)
	expected, err := newProvenance(r, control.TrainingJob{RecipeSHA256: hex.EncodeToString(digest[:])}, provenance.InvocationID, provenance.EvalLoss, files)
	if err != nil {
		return Provenance{}, err
	}
	invocation, decodeErr := hex.DecodeString(provenance.InvocationID)
	canonical, marshalErr := json.Marshal(provenance)
	if decodeErr != nil || len(invocation) != 16 || hex.EncodeToString(invocation) != provenance.InvocationID || provenance.InvocationID == "00000000000000000000000000000000" || math.IsNaN(provenance.EvalLoss) || math.IsInf(provenance.EvalLoss, 0) || provenance.EvalLoss < 0 || marshalErr != nil || !matchingObjectKeys(raw, canonical) || !reflect.DeepEqual(provenance, expected) {
		return Provenance{}, errors.New("training provenance does not match the recipe and exported assets")
	}
	return provenance, nil
}

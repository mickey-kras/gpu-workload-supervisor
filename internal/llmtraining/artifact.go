package llmtraining

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"math"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/strictjson"
)

const (
	provenanceName      = "supervisor-provenance.json"
	adapterConfigName   = "adapter_config.json"
	adapterWeightsName  = "adapter_model.safetensors"
	tokenizerConfigName = "tokenizer_config.json"
	// modelConfigName is the model configuration in the model snapshot and the
	// export.
	modelConfigName = "config.json"
	// generationConfigName is the generation configuration of the model
	// snapshot.
	generationConfigName = "generation_config.json"
)

var artifactNames = map[string]bool{
	adapterWeightsName: true, adapterConfigName: true,
	tokenizerConfigName: true, "tokenizer.json": true, "tokenizer.model": true,
	"special_tokens_map.json": true, "added_tokens.json": true,
	generationConfigName: true, modelConfigName: true, "vocab.json": true,
	"merges.txt": true, "vocab.txt": true, "chat_template.jinja": true,
	// Training saves the processor of the image-text model; the export binds
	// and checks it like the other configuration files.
	processorConfigName: true, imageProcessorName: true, videoProcessorName: true,
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

// exportRejected names outputs that a loader reads but the provenance cannot
// bind: pickle and sharded adapters, and the unbound loader files
// (unboundLoaderNames).
var exportRejected = func() map[string]bool {
	rejected := map[string]bool{"adapter_model.bin": true, "adapter_model.safetensors.index.json": true}
	for _, name := range unboundLoaderNames {
		rejected[name] = true
	}
	return rejected
}()

func artifactFiles(r Recipe) (map[string]string, error) {
	if err := privateExecutionDirectory(r.OutputDirectory); err != nil {
		return nil, err
	}
	entries, err := boundedDirectoryEntries(r.OutputDirectory, 1024)
	if err != nil {
		return nil, err
	}
	files := make(map[string]string)
	contents := make(map[string][]byte)
	var total uint64
	for _, entry := range entries {
		size, err := bindArtifact(r, entry.Name(), r.Budget.ExportBytes-total, files, contents)
		if err != nil {
			return nil, err
		}
		total += size
	}
	if files[adapterWeightsName] == "" || files[adapterConfigName] == "" || files[tokenizerConfigName] == "" || (files["tokenizer.json"] == "" && files["tokenizer.model"] == "") {
		return nil, errors.New("training export requires adapter weights, configuration and saved tokenizer")
	}
	if err := validateArtifactConfigs(r, files, contents); err != nil {
		return nil, err
	}
	return files, nil
}

// bindArtifact hashes one export entry into files, keeps the bytes of the
// export configurations in contents, and returns the bytes it used of the
// budget. It skips names outside the export and rejects unbound loader files.
func bindArtifact(r Recipe, name string, remaining uint64, files map[string]string, contents map[string][]byte) (uint64, error) {
	if !artifactNames[name] {
		if exportRejected[name] {
			return 0, errors.New("unsupported training export layout: " + name)
		}
		return 0, nil
	}
	digest, content, err := hashArtifact(filepath.Join(r.OutputDirectory, name), remaining, exportJSONNames[name])
	if errors.Is(err, errConfigurationTooLarge) {
		return 0, errors.New("training export " + name + " is larger than " + strconv.Itoa(configurationBytes>>20) + " MiB")
	}
	if err != nil {
		return 0, err
	}
	files[name] = digest
	if content.raw != nil {
		contents[name] = content.raw
	}
	exportHashed(name)
	return uint64(content.size), nil
}

// exportStatted runs after an export entry is opened and its size checked.
// Tests use it to grow a file before the copy.
var exportStatted = func(string) {}

// exportHashed runs after each export entry is hashed. Tests use it to change
// a file after its digest, which validation must not see.
var exportHashed = func(string) {}

// exportJSONNames are the export configurations that the export validates.
// It keeps their bytes while hashing, so it validates the bytes it binds.
var exportJSONNames = map[string]bool{
	adapterConfigName: true, tokenizerConfigName: true, modelConfigName: true,
	imageProcessorName: true, videoProcessorName: true, processorConfigName: true,
}

// configurationBytes bounds each JSON configuration, as readJSONObject does.
const configurationBytes = 1 << 20

var errConfigurationTooLarge = errors.New("training export configuration exceeds its size limit")

// limitedBuffer keeps the first limit bytes written to it and drops the rest
// without an error, so the hash still covers the whole stream.
type limitedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.Len(); room > 0 {
		b.Buffer.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

// hashedArtifact is the size of a hashed artifact and, when kept, its bytes.
type hashedArtifact struct {
	size int64
	raw  []byte
}

func hashArtifact(path string, remaining uint64, keep bool) (string, hashedArtifact, error) {
	f, err := openRegular(path)
	if err != nil {
		return "", hashedArtifact{}, errors.New("training export contains an unreadable or linked asset")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.Size() <= 0 || uint64(info.Size()) > remaining {
		return "", hashedArtifact{}, errors.New("training export exceeds its budget or contains an empty asset")
	}
	exportStatted(path)
	if keep && info.Size() > configurationBytes {
		return "", hashedArtifact{}, errConfigurationTooLarge
	}
	h := sha256.New()
	// The kept copy holds at most one byte above the limit, so a file that
	// grows during the copy still fails validation.
	kept := &limitedBuffer{limit: configurationBytes + 1}
	sink := io.Writer(h)
	if keep {
		sink = io.MultiWriter(h, kept)
	}
	n, err := io.Copy(sink, io.LimitReader(f, int64(min(remaining, 1<<40))+1))
	if err != nil || n <= 0 || uint64(n) > remaining || n > 1<<40 {
		return "", hashedArtifact{}, errors.New("training export cannot be hashed within its budget")
	}
	result := hashedArtifact{size: n}
	if keep {
		result.raw = kept.Bytes()
	}
	return hex.EncodeToString(h.Sum(nil)), result, nil
}

// qloraQuantizationConfig is the quantization_config that the locked
// transformers saves for the bitsandbytes NF4 settings that Axolotl requests
// for QLoRA: 4-bit NF4, double quantization, bfloat16 compute and storage.
const qloraQuantizationConfig = `{"_load_in_4bit":true,"_load_in_8bit":false,"bnb_4bit_compute_dtype":"bfloat16",
"bnb_4bit_quant_storage":"bfloat16","bnb_4bit_quant_type":"nf4","bnb_4bit_use_double_quant":true,
"llm_int8_enable_fp32_cpu_offload":false,"llm_int8_has_fp16_weight":false,"llm_int8_skip_modules":null,
"llm_int8_threshold":6.0,"load_in_4bit":true,"load_in_8bit":false,"quant_method":"bitsandbytes"}`

// qloraQuantizationMismatch describes how raw differs from
// qloraQuantizationConfig, or returns "" when they are equal. It names only
// expected field names, never content from the file.
func qloraQuantizationMismatch(raw json.RawMessage) string {
	var saved, expected map[string]any
	if json.Unmarshal(raw, &saved) != nil || saved == nil || json.Unmarshal([]byte(qloraQuantizationConfig), &expected) != nil {
		return "quantization_config must be an object"
	}
	for _, field := range slices.Sorted(maps.Keys(expected)) {
		if value, exists := saved[field]; !exists || !reflect.DeepEqual(value, expected[field]) {
			return "quantization_config field " + field + " differs from the bitsandbytes NF4 setting that QLoRA requests"
		}
	}
	if len(saved) != len(expected) {
		return "quantization_config has an extra field"
	}
	return ""
}

func validateArtifactConfigs(r Recipe, files map[string]string, contents map[string][]byte) error {
	config, err := parseJSONObject(contents[adapterConfigName], "training export configuration")
	if err != nil {
		return err
	}
	for key, expected := range map[string]string{"peft_type": "LORA", "task_type": "CAUSAL_LM", "base_model_name_or_path": r.Model.Inventory.Root} {
		var actual string
		if json.Unmarshal(config[key], &actual) != nil || actual != expected {
			return errors.New("training export adapter does not match its base or supported format")
		}
	}
	for _, name := range []string{adapterConfigName, tokenizerConfigName} {
		if err := checkExportLoaderKeys(name, contents[name], files); err != nil {
			return err
		}
	}
	// Training always saves the model config.json (observed in the CPU smoke).
	if files[modelConfigName] == "" {
		return errors.New("training export requires " + modelConfigName)
	}
	if err := checkExportModelConfig(r, files, contents[modelConfigName]); err != nil {
		return err
	}
	return checkExportProcessors(files, contents)
}

// checkExportModelConfig checks the model config.json that training saves
// with the adapter. A processor loader reads it with the processor files, so
// it gets the loader key lists and the architecture check. Unlike the
// candidate, it can record the 4-bit loading that a QLoRA recipe requested.
func checkExportModelConfig(r Recipe, files map[string]string, raw []byte) error {
	owner := "training export " + modelConfigName
	config, err := parseJSONObject(raw, owner)
	if err != nil {
		return err
	}
	if err := inspectLoaderKeys(owner, config, true, files); err != nil {
		return err
	}
	// transformers reads quantization_config at the top level and in
	// text_config. Only the top level can hold the QLoRA setting.
	nested := maps.Clone(config)
	delete(nested, quantizationConfigKey)
	if _, key := deepLoaderKey(nested, []string{quantizationConfigKey}, modelSubConfigs); key != "" {
		return errors.New(owner + " can have quantization_config only at the top level")
	}
	if raw, exists := config[quantizationConfigKey]; exists {
		if r.Adapter != "qlora" {
			return errors.New(owner + " quantization_config requires a QLoRA recipe")
		}
		if problem := qloraQuantizationMismatch(raw); problem != "" {
			return errors.New(owner + " " + problem)
		}
	}
	return inspectArchitecture(owner, config)
}

// checkExportLoaderKeys applies the shared loader key lists to a saved
// adapter or tokenizer configuration, with file-path keys checked against the
// bound export files. Saved configurations write unset fields as null, for
// example the PEFT auto_mapping, so a null value is allowed.
func checkExportLoaderKeys(name string, raw []byte, files map[string]string) error {
	config, err := parseJSONObject(raw, "training export configuration")
	if err != nil {
		return err
	}
	set := maps.Clone(config)
	maps.DeleteFunc(set, func(_ string, value json.RawMessage) bool {
		return bytes.Equal(bytes.TrimSpace(value), []byte("null"))
	})
	return inspectLoaderKeys("training export "+name, set, false, files)
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
	return Provenance{2, SourceRevision, job.RecipeSHA256, r.Model.Revision, r.Tokenizer.Revision, model, tokenizer, runtime, hex.EncodeToString(worker[:]), invocation, files[adapterWeightsName], loss, files}, nil
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

package llmtraining

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
)

type ConfigDataset struct {
	Path            string `json:"path"`
	DataType        string `json:"ds_type"`
	Type            string `json:"type"`
	Field           string `json:"field"`
	Split           string `json:"split"`
	TrustRemoteCode bool   `json:"trust_remote_code"`
}

// Config is deliberately closed: callers cannot supply arbitrary trainer keys,
// hooks, commands, plugins, remote paths or launcher arguments in a recipe.
type Config struct {
	BaseModel             string          `json:"base_model"`
	Revision              string          `json:"revision_of_model"`
	Tokenizer             string          `json:"tokenizer_config"`
	TrustRemoteCode       bool            `json:"trust_remote_code"`
	Datasets              []ConfigDataset `json:"datasets"`
	TestDatasets          []ConfigDataset `json:"test_datasets"`
	DatasetPreparedPath   string          `json:"dataset_prepared_path"`
	OutputDirectory       string          `json:"output_dir"`
	Adapter               string          `json:"adapter"`
	Load4Bit              bool            `json:"load_in_4bit"`
	Load8Bit              bool            `json:"load_in_8bit"`
	BF16                  bool            `json:"bf16"`
	FP16                  bool            `json:"fp16"`
	Rank                  int             `json:"lora_r"`
	Alpha                 int             `json:"lora_alpha"`
	Dropout               float64         `json:"lora_dropout"`
	Targets               []string        `json:"lora_target_modules"`
	TargetLinear          bool            `json:"lora_target_linear"`
	SequenceLength        int             `json:"sequence_len"`
	MicroBatchSize        int             `json:"micro_batch_size"`
	GradientAccumulation  int             `json:"gradient_accumulation_steps"`
	MaxSteps              int             `json:"max_steps"`
	LearningRate          float64         `json:"learning_rate"`
	Seed                  int             `json:"seed"`
	TrainOnInputs         bool            `json:"train_on_inputs"`
	SamplePacking         bool            `json:"sample_packing"`
	GradientCheckpointing bool            `json:"gradient_checkpointing"`
	Optimizer             string          `json:"optimizer"`
	Scheduler             string          `json:"lr_scheduler"`
	Attention             string          `json:"attn_implementation"`
	EvalStrategy          string          `json:"eval_strategy"`
	EvalSteps             int             `json:"eval_steps"`
	SaveStrategy          string          `json:"save_strategy"`
	SaveSteps             int             `json:"save_steps"`
	SaveTotalLimit        int             `json:"save_total_limit"`
	SaveOnlyModel         bool            `json:"save_only_model"`
	AutoResume            bool            `json:"auto_resume_from_checkpoints"`
	Plugins               []string        `json:"plugins"`
	UseWandB              bool            `json:"use_wandb"`
	UseMLFlow             bool            `json:"use_mlflow"`
	UseTensorBoard        bool            `json:"use_tensorboard"`
	UseComet              bool            `json:"use_comet"`
	UseTrackio            bool            `json:"use_trackio"`
	UseOTelMetrics        bool            `json:"use_otel_metrics"`
	WandBMode             string          `json:"wandb_mode"`
	HubModelID            *string         `json:"hub_model_id"`
	PushDatasetToHub      *string         `json:"push_dataset_to_hub"`
	HFUseAuthToken        bool            `json:"hf_use_auth_token"`
}

type Plan struct {
	Status                string `json:"status"`
	SourceRevision        string `json:"axolotlSourceRevision"`
	RecipeSHA256          string `json:"canonicalRecipeSHA256"`
	EstimatedStorageBytes uint64 `json:"estimatedStorageBytes"`
	EstimatedRAMBytes     uint64 `json:"estimatedRAMBytes"`
	TrainRows             int    `json:"trainRows"`
	HeldOutRows           int    `json:"heldOutRows"`
	Config                Config `json:"config"`
}

// Compile audits the local inputs and compares user estimates with available
// resources. It does not import the runtime, tokenize, execute or qualify it.
func Compile(r Recipe, availableRAM, availableStorage uint64) (Plan, error) {
	if err := r.Validate(); err != nil {
		return Plan{}, err
	}
	storage, err := r.Budget.StorageBytes()
	if err != nil {
		return Plan{}, err
	}
	if availableRAM < r.Budget.RAMBytes || availableStorage < storage {
		return Plan{}, errors.New("available RAM or storage is below the candidate estimate")
	}
	if err := inspectPaths(r); err != nil {
		return Plan{}, err
	}
	for _, inventory := range []Inventory{r.Model.Inventory, r.Tokenizer.Inventory, r.Runtime.Inventory} {
		if err := inventory.Inspect(); err != nil {
			return Plan{}, err
		}
	}
	if err := inspectSnapshots(r); err != nil {
		return Plan{}, err
	}
	trainRows, heldOutRows, err := inspectDatasets(r.Train, r.HeldOut)
	if err != nil {
		return Plan{}, err
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return Plan{}, errors.New("cannot encode candidate recipe")
	}
	digest := sha256.Sum256(raw)
	return Plan{Status: "unqualified", SourceRevision: SourceRevision, RecipeSHA256: hex.EncodeToString(digest[:]), EstimatedStorageBytes: storage, EstimatedRAMBytes: r.Budget.RAMBytes, TrainRows: trainRows, HeldOutRows: heldOutRows, Config: compileConfig(r)}, nil
}

func compileConfig(r Recipe) Config {
	dataset := func(path string) ConfigDataset {
		return ConfigDataset{Path: path, DataType: "json", Type: "completion", Field: "text", Split: "train"}
	}
	return Config{
		BaseModel: r.Model.Inventory.Root, Revision: r.Model.Revision, Tokenizer: r.Tokenizer.Inventory.Root,
		Datasets: []ConfigDataset{dataset(r.Train.Path)}, TestDatasets: []ConfigDataset{dataset(r.HeldOut.Path)},
		DatasetPreparedPath: filepath.Join(r.OutputDirectory, "cache"), OutputDirectory: r.OutputDirectory,
		Adapter: r.Adapter, Load4Bit: r.Adapter == "qlora", BF16: true,
		Rank: r.Rank, Alpha: r.Alpha, Dropout: 0.05, Targets: []string{"q_proj", "v_proj"},
		SequenceLength: r.SequenceLength, MicroBatchSize: r.MicroBatchSize, GradientAccumulation: r.GradientAccumulation,
		MaxSteps: r.MaxSteps, LearningRate: r.LearningRate, Seed: r.Seed, TrainOnInputs: true, GradientCheckpointing: true,
		Optimizer: "adamw_torch", Scheduler: "constant", Attention: "sdpa", EvalStrategy: "steps", EvalSteps: r.MaxSteps,
		SaveStrategy: "steps", SaveSteps: r.MaxSteps, SaveTotalLimit: int(r.Budget.RetainedCheckpoints), Plugins: []string{}, WandBMode: "disabled",
	}
}

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/deployment"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/llmtraining"
)

func TestCLIRejectsOverridesWithoutEchoingArguments(t *testing.T) {
	for _, args := range [][]string{nil, {"-plugin", "SECRET"}, {"-recipe", "SECRET"}, {"-recipe", "SECRET", "-output", "/tmp/plan", "SECRET"}, {"-recipe", "relative", "-output", "/tmp/plan"}, {"-recipe", "SECRET", "-job-file", "SECRET", "-output", "/tmp/plan"}, {"-job-file", "SECRET", "-output", "/tmp/plan"}} {
		var stderr bytes.Buffer
		if exitCode(args, io.Discard, &stderr) != 1 || strings.Contains(stderr.String(), "SECRET") {
			t.Fatal("unexpected diagnostic", stderr.String())
		}
	}
	for _, args := range [][]string{{"-materialize", "SECRET"}, {"-materialize", "SECRET", "-output", "/tmp/plan"}, {"-materialize", "SECRET", "-recipe", "SECRET", "-output", "/tmp/plan"}, {"-materialize", "SECRET", "-job-file", "SECRET", "-output", "/tmp/plan"}} {
		var stderr bytes.Buffer
		if exitCode(args, io.Discard, &stderr) != 1 || strings.Contains(stderr.String(), "SECRET") {
			t.Fatal("unexpected materialization diagnostic", stderr.String())
		}
	}
}

func TestCLIPreparesPrivateUnqualifiedPlan(t *testing.T) {
	root := t.TempDir()
	write := func(path, content string) string {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256([]byte(content))
		return hex.EncodeToString(digest[:])
	}
	inventory := func(name string, files map[string]string) llmtraining.Inventory {
		t.Helper()
		dir := filepath.Join(root, name)
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		result := llmtraining.Inventory{Root: dir, Files: map[string]string{}}
		for file, content := range files {
			result.Files[file] = write(filepath.Join(dir, file), content)
		}
		return result
	}
	r := llmtraining.Recipe{Version: 1, Adapter: "lora", Rank: 8, Alpha: 16, SequenceLength: 128, MicroBatchSize: 1, GradientAccumulation: 1, MaxSteps: 1, LearningRate: 0.0002, Model: llmtraining.Snapshot{Revision: strings.Repeat("a", 40), Inventory: inventory("model", map[string]string{"config.json": `{"model_type":"qwen3_5","architectures":["Qwen3_5ForConditionalGeneration"],"text_config":{"model_type":"qwen3_5_text"}}`, "model.safetensors": "fixture"})}, Tokenizer: llmtraining.Snapshot{Revision: strings.Repeat("b", 40), Inventory: inventory("tokenizer", map[string]string{"preprocessor_config.json": `{"processor_class":"Qwen3VLProcessor"}`, "video_preprocessor_config.json": `{"processor_class":"Qwen3VLProcessor"}`, "tokenizer_config.json": `{}`})}, Runtime: llmtraining.Runtime{Entrypoint: "axolotl", Inventory: inventory("runtime", map[string]string{"axolotl": "not executable"})}, Budget: llmtraining.Budget{RAMBytes: 1, WeightsBytes: 1, CacheBytes: 1, CheckpointBytes: 1, RetainedCheckpoints: 1, ExportBytes: 1, ReserveBytes: 1}, OutputDirectory: filepath.Join(root, "output")}
	if err := os.Mkdir(r.OutputDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	r.Train.Path = filepath.Join(root, "train.jsonl")
	r.Train.SHA256 = write(r.Train.Path, `{"text":"train"}`)
	r.HeldOut.Path = filepath.Join(root, "eval.jsonl")
	r.HeldOut.SHA256 = write(r.HeldOut.Path, `{"text":"evaluation"}`)
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	recipePath := filepath.Join(root, "recipe.json")
	write(recipePath, string(raw))
	planPath := filepath.Join(root, "plan.json")
	var stderr bytes.Buffer
	if exitCode([]string{"-recipe", recipePath, "-output", planPath}, io.Discard, &stderr) != 0 || stderr.Len() != 0 {
		t.Fatal(stderr.String())
	}
	data, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatal(err)
	}
	var plan llmtraining.Plan
	if err = json.Unmarshal(data, &plan); err != nil || plan.Status != "unqualified" {
		t.Fatal("invalid plan", err)
	}
	// Job preparation consumes the supervisor's launch envelope, but publishes
	// no completion credential and does not change the original candidate mode.
	job := struct {
		Version      int                 `json:"version"`
		Job          control.TrainingJob `json:"job"`
		RecipeFile   string              `json:"recipeFile"`
		TerminalFile string              `json:"terminalFile"`
	}{1, control.TrainingJob{ID: "job", Token: "private-completion-credential", Workload: "trainer", Fence: control.Fence{Incarnation: "supervisor", Epoch: 1}, CatalogRevision: "revision", RecipeSHA256: write(recipePath, string(raw)), Status: "launching", UpdatedAt: time.Now().UTC()}, recipePath, filepath.Join(root, "terminal.json")}
	jobRaw, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	jobPath := filepath.Join(root, "job.json")
	write(jobPath, string(jobRaw))
	jobPlanPath := filepath.Join(root, "job-plan.json")
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("INVOCATION_ID", strings.Repeat("a", 32))
	if exitCode([]string{"-job-file", jobPath, "-output", jobPlanPath}, io.Discard, &stderr) != 0 || stderr.Len() != 0 {
		t.Fatal(stderr.String())
	}
	data, err = os.ReadFile(jobPlanPath)
	if err != nil {
		t.Fatal(err)
	}
	var jobPlan llmtraining.JobPlan
	if err := json.Unmarshal(data, &jobPlan); err != nil || jobPlan.ID != "job" || jobPlan.Candidate.Status != "unqualified" || strings.Contains(string(data), job.Job.Token) {
		t.Fatal("invalid or sensitive job plan", err)
	}
	if err := os.Mkdir(filepath.Join(r.Runtime.Inventory.Root, "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(r.Runtime.Inventory.Root, "bin/python3"), "not executable")
	request := llmtraining.MaterializeRequest{Version: 1, Preset: "smoke-lora", Model: llmtraining.LocalSnapshot{Root: r.Model.Inventory.Root, Revision: r.Model.Revision}, Tokenizer: llmtraining.LocalSnapshot{Root: r.Tokenizer.Inventory.Root, Revision: r.Tokenizer.Revision}, RuntimeRoot: r.Runtime.Inventory.Root, Train: r.Train.Path, HeldOut: r.HeldOut.Path, OutputDirectory: r.OutputDirectory, Budget: r.Budget}
	requestRaw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	inputPath := filepath.Join(root, "inputs.json")
	write(inputPath, string(requestRaw))
	materializedPath := filepath.Join(root, "materialized.json")
	if exitCode([]string{"-materialize", inputPath, "-output", materializedPath}, io.Discard, &stderr) != 0 || stderr.Len() != 0 {
		t.Fatal("materialization CLI failed", stderr.String())
	}
	materialized, err := os.ReadFile(materializedPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := llmtraining.ReadRecipe(bytes.NewReader(materialized)); err != nil {
		t.Fatal("materialization CLI did not publish a recipe", err)
	}
}

func TestVersionReportsReleaseWithoutPreparing(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if exitCode([]string{"-version"}, &stdout, &stderr) != 0 || stdout.String() != deployment.Release+"\n" || stderr.Len() != 0 {
		t.Fatal("version did not report the release")
	}
	for _, flag := range []string{"-recipe", "-job-file", "-materialize", "-output"} {
		stdout.Reset()
		stderr.Reset()
		if exitCode([]string{"-version", flag, "SECRET"}, &stdout, &stderr) != 1 || stdout.Len() != 0 || strings.Contains(stderr.String(), "SECRET") {
			t.Fatal("version accepted preparation arguments")
		}
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, errors.New("output unavailable")
}

func TestVersionReportsOutputFailure(t *testing.T) {
	var stderr bytes.Buffer
	if exitCode([]string{"-version"}, failingWriter{}, &stderr) != 1 || stderr.Len() == 0 {
		t.Fatal("version output failure was ignored")
	}
}

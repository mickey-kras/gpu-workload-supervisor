package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/llmtraining"
)

func TestCLIRejectsOverridesWithoutEchoingArguments(t *testing.T) {
	for _, args := range [][]string{nil, {"-plugin", "SECRET"}, {"-recipe", "SECRET"}, {"-recipe", "SECRET", "-output", "/tmp/plan", "SECRET"}, {"-recipe", "relative", "-output", "/tmp/plan"}, {"-recipe", "SECRET", "-job-file", "SECRET", "-output", "/tmp/plan"}, {"-job-file", "SECRET", "-output", "/tmp/plan"}} {
		var stderr bytes.Buffer
		if exitCode(args, &stderr) != 1 || strings.Contains(stderr.String(), "SECRET") {
			t.Fatal("unexpected diagnostic", stderr.String())
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
	r := llmtraining.Recipe{Version: 1, Adapter: "lora", Rank: 8, Alpha: 16, SequenceLength: 128, MicroBatchSize: 1, GradientAccumulation: 1, MaxSteps: 1, LearningRate: 0.0002, Model: llmtraining.Snapshot{Revision: strings.Repeat("a", 40), Inventory: inventory("model", map[string]string{"config.json": `{"model_type":"llama"}`, "model.safetensors": "fixture"})}, Tokenizer: llmtraining.Snapshot{Revision: strings.Repeat("b", 40), Inventory: inventory("tokenizer", map[string]string{"tokenizer_config.json": `{}`})}, Runtime: llmtraining.Runtime{Entrypoint: "axolotl", Inventory: inventory("runtime", map[string]string{"axolotl": "not executable"})}, Budget: llmtraining.Budget{RAMBytes: 1, WeightsBytes: 1, CacheBytes: 1, CheckpointBytes: 1, RetainedCheckpoints: 1, ExportBytes: 1, ReserveBytes: 1}, OutputDirectory: filepath.Join(root, "output")}
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
	if exitCode([]string{"-recipe", recipePath, "-output", planPath}, &stderr) != 0 || stderr.Len() != 0 {
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
	if exitCode([]string{"-job-file", jobPath, "-output", jobPlanPath}, &stderr) != 0 || stderr.Len() != 0 {
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
}

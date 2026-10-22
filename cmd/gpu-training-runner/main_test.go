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
	"github.com/mickey-kras/gpu-workload-supervisor/internal/deployment"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/llmtraining"
)

func TestRunnerVersionMatchesPackagedRelease(t *testing.T) {
	var output bytes.Buffer
	if exitCode([]string{"-version"}, &output, &output) != 0 || strings.TrimSpace(output.String()) != deployment.Release {
		t.Fatal("runner version differs from packaged release")
	}
}

func TestRunnerRejectsUnsupportedInvocation(t *testing.T) {
	for _, args := range [][]string{nil, {"--recipe", "/private/recipe"}, {"--job-file", "/private/job", "extra"}} {
		var stderr bytes.Buffer
		if exitCode(args, &stderr, &stderr) != 1 || !strings.Contains(stderr.String(), "usage:") {
			t.Fatal("accepted unsupported invocation")
		}
	}
}

func TestRunnerReportsExecutionFailureWithoutPrivateValues(t *testing.T) {
	t.Setenv("INVOCATION_ID", "invalid")
	t.Setenv("CUDA_VISIBLE_DEVICES", "0")
	var stderr bytes.Buffer
	if exitCode([]string{"--job-file", "/private/job"}, &stderr, &stderr) != 1 || strings.Contains(stderr.String(), "/private/job") {
		t.Fatal("execution error missing or disclosed private values")
	}
}

func TestRunnerCompletesBoundJobWithExplicitWorkerFixture(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	write := func(path, text string) string {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0700); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256([]byte(text))
		return hex.EncodeToString(digest[:])
	}
	inventory := func(name string, files map[string]string) llmtraining.Inventory {
		t.Helper()
		result := llmtraining.Inventory{Root: filepath.Join(root, name), Files: map[string]string{}}
		for file, text := range files {
			result.Files[file] = write(filepath.Join(result.Root, file), text)
		}
		return result
	}
	output := filepath.Join(root, "output")
	if err := os.Mkdir(output, 0700); err != nil {
		t.Fatal(err)
	}
	worker := "#!/bin/sh\nset -eu\nif [ \"$5\" = train ]; then\n printf '{\"version\":1,\"phase\":\"train\",\"step\":2,\"evalLoss\":1,\"reloaded\":false}' > \"$7\"\nelse\n printf '{\"version\":1,\"phase\":\"reload\",\"step\":2,\"evalLoss\":0,\"reloaded\":true}' > \"$7\"\nfi\nchmod 600 \"$7\"\nprintf x > '" + filepath.Join(output, "adapter_model.safetensors") + "'\n"
	worker += "if [ \"$5\" = train ]; then\n" +
		"printf '%s' '{\"peft_type\":\"LORA\",\"task_type\":\"CAUSAL_LM\",\"base_model_name_or_path\":\"" + filepath.Join(root, "model") + "\"}' > '" + filepath.Join(output, "adapter_config.json") + "'\n" +
		"printf '{}' > '" + filepath.Join(output, "tokenizer_config.json") + "'\n" +
		"printf '{}' > '" + filepath.Join(output, "tokenizer.json") + "'\nfi\n"
	r := llmtraining.Recipe{Version: 1, Adapter: "lora", Rank: 8, Alpha: 16, SequenceLength: 128, MicroBatchSize: 1, GradientAccumulation: 1, MaxSteps: 2, LearningRate: 0.0002, Model: llmtraining.Snapshot{Revision: strings.Repeat("a", 40), Inventory: inventory("model", map[string]string{"config.json": `{"model_type":"llama"}`, "model.safetensors": "fixture"})}, Tokenizer: llmtraining.Snapshot{Revision: strings.Repeat("b", 40), Inventory: inventory("tokenizer", map[string]string{"tokenizer_config.json": `{}`})}, Runtime: llmtraining.Runtime{Entrypoint: "bin/python3", Inventory: inventory("runtime", map[string]string{"bin/python3": worker})}, OutputDirectory: output, Budget: llmtraining.Budget{RAMBytes: 1, WeightsBytes: 1, CacheBytes: 1, CheckpointBytes: 1, RetainedCheckpoints: 1, ExportBytes: 4096, ReserveBytes: 1}}
	r.Train.Path = filepath.Join(root, "train.jsonl")
	r.Train.SHA256 = write(r.Train.Path, `{"text":"train"}`)
	r.HeldOut.Path = filepath.Join(root, "heldout.jsonl")
	r.HeldOut.SHA256 = write(r.HeldOut.Path, `{"text":"held out"}`)
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	recipe := filepath.Join(root, "recipe.json")
	digest := write(recipe, string(raw))
	job := control.TrainingJob{ID: "job", Token: "private-token", Workload: "trainer", Fence: control.Fence{Incarnation: "supervisor", Epoch: 1}, CatalogRevision: "revision", RecipeSHA256: digest, Status: "launching", UpdatedAt: time.Now().UTC()}
	envelope := struct {
		Version      int                 `json:"version"`
		Job          control.TrainingJob `json:"job"`
		RecipeFile   string              `json:"recipeFile"`
		TerminalFile string              `json:"terminalFile"`
	}{1, job, recipe, filepath.Join(root, "terminal.json")}
	raw, err = json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "job.json")
	write(path, string(raw))
	t.Setenv("INVOCATION_ID", strings.Repeat("a", 32))
	t.Setenv("CUDA_VISIBLE_DEVICES", "GPU-aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	var stderr bytes.Buffer
	if exitCode([]string{"--job-file", path}, &stderr, &stderr) != 0 || stderr.Len() != 0 {
		t.Fatal(stderr.String())
	}
}

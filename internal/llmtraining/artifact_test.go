package llmtraining

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func completedArtifact(t *testing.T) (Recipe, jobEnvelope) {
	t.Helper()
	r, e, jobPath := executionFixture(t, executionScript)
	script := executionScript + "if [ \"$5\" = train ]; then printf x > '" + filepath.Join(r.OutputDirectory, "adapter_model.safetensors") + "'; fi\n"
	updateExecutionScript(t, r, &e, jobPath, script)
	if err := RunJob(context.Background(), jobPath, executionInvocation, executionGPU); err != nil {
		t.Fatal(err)
	}
	return r, e
}

func TestExportBindsConfigurationAndTokenizerAcrossReload(t *testing.T) {
	for _, name := range []string{"adapter_config.json", "tokenizer_config.json", "tokenizer.json", "added_tokens.json"} {
		t.Run(name, func(t *testing.T) {
			r, e, jobPath := executionFixture(t, executionScript)
			script := executionScript + "printf x > '" + filepath.Join(r.OutputDirectory, "adapter_model.safetensors") + "'\n"
			script += "if [ \"$5\" = reload ]; then printf '{}' > '" + filepath.Join(r.OutputDirectory, name) + "'; fi\n"
			if name == "tokenizer_config.json" || name == "tokenizer.json" {
				script = strings.ReplaceAll(script, "then printf '{}'", "then printf '{\"changed\":true}'")
			}
			updateExecutionScript(t, r, &e, jobPath, script)
			if RunJob(context.Background(), jobPath, executionInvocation, executionGPU) == nil {
				t.Fatal("accepted changed export")
			}
			for _, path := range []string{e.TerminalFile, filepath.Join(r.OutputDirectory, provenanceName)} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatal("published evidence for changed export", err)
				}
			}
		})
	}
}

func TestImportPreparationBindsRecipeAndEveryExportedAsset(t *testing.T) {
	for _, mutation := range []string{"none", "recipe", "weights", "adapter config", "tokenizer", "extra tokenizer", "model", "runtime", "provenance", "legacy", "null files", "bad invocation", "negative loss", "unknown key"} {
		t.Run(mutation, func(t *testing.T) {
			r, e := completedArtifact(t)
			provenancePath := filepath.Join(r.OutputDirectory, provenanceName)
			switch mutation {
			case "recipe":
				raw, err := os.ReadFile(e.RecipeFile)
				if err != nil {
					t.Fatal(err)
				}
				writeFixture(t, e.RecipeFile, string(raw)+" ")
			case "weights":
				writeFixture(t, filepath.Join(r.OutputDirectory, "adapter_model.safetensors"), "changed")
			case "adapter config":
				writeFixture(t, filepath.Join(r.OutputDirectory, "adapter_config.json"), "{}")
			case "tokenizer":
				writeFixture(t, filepath.Join(r.OutputDirectory, "tokenizer.json"), "changed")
			case "extra tokenizer":
				writeFixture(t, filepath.Join(r.OutputDirectory, "added_tokens.json"), "{}")
			case "model":
				writeFixture(t, filepath.Join(r.Model.Inventory.Root, "config.json"), "{}")
			case "runtime":
				writeFixture(t, filepath.Join(r.Runtime.Inventory.Root, "axolotl"), "changed")
			case "provenance":
				writeFixture(t, provenancePath, "{malformed")
			case "legacy", "null files", "bad invocation", "negative loss", "unknown key":
				raw, err := os.ReadFile(provenancePath)
				if err != nil {
					t.Fatal(err)
				}
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(raw, &fields); err != nil {
					t.Fatal(err)
				}
				switch mutation {
				case "legacy":
					fields["version"] = json.RawMessage(`1`)
				case "null files":
					fields["files"] = json.RawMessage(`null`)
				case "bad invocation":
					fields["invocationId"] = json.RawMessage(`"00000000000000000000000000000000"`)
				case "negative loss":
					fields["evalLoss"] = json.RawMessage(`-1`)
				case "unknown key":
					fields["extra"] = json.RawMessage(`true`)
				}
				updated, err := json.Marshal(fields)
				if err != nil {
					t.Fatal(err)
				}
				writeFixture(t, provenancePath, string(updated))
			}
			parent := filepath.Join(t.TempDir(), "private")
			if err := os.Mkdir(parent, 0700); err != nil {
				t.Fatal(err)
			}
			plan := filepath.Join(parent, "import.json")
			err := PrepareImport(e.RecipeFile, plan, "peft")
			if mutation != "none" {
				if err == nil {
					t.Fatal("accepted changed artifact identity")
				}
				if _, statErr := os.Stat(plan); !os.IsNotExist(statErr) {
					t.Fatal("published rejected import", statErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(plan)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(raw), `"status":"unqualified"`) || !strings.Contains(string(raw), `"servingQualified":false`) || strings.Contains(string(raw), e.Job.Token) {
				t.Fatal("import plan made an unsupported or sensitive claim")
			}
			if PrepareImport(e.RecipeFile, plan, "peft") == nil {
				t.Fatal("replaced existing plan")
			}
		})
	}
}

func TestExportRejectsMissingRedirectedOrUnsafeFiles(t *testing.T) {
	for _, state := range []string{"missing config", "missing tokenizer", "linked", "empty", "budget", "remote base", "wrong format", "tokenizer redirect", "pickle", "sharded", "chat templates", "malformed", "null"} {
		t.Run(state, func(t *testing.T) {
			r, _ := completedArtifact(t)
			config := filepath.Join(r.OutputDirectory, "adapter_config.json")
			tokenizer := filepath.Join(r.OutputDirectory, "tokenizer.json")
			switch state {
			case "missing config":
				if err := os.Remove(config); err != nil {
					t.Fatal(err)
				}
			case "missing tokenizer":
				if err := os.Remove(tokenizer); err != nil {
					t.Fatal(err)
				}
			case "linked":
				if err := os.Remove(tokenizer); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(config, tokenizer); err != nil {
					t.Fatal(err)
				}
			case "empty":
				writeFixture(t, tokenizer, "")
			case "budget":
				r.Budget.ExportBytes = 1
			case "remote base":
				writeFixture(t, config, `{"peft_type":"LORA","task_type":"CAUSAL_LM","base_model_name_or_path":"remote"}`)
			case "wrong format":
				writeFixture(t, config, `{"peft_type":"PROMPT_TUNING"}`)
			case "tokenizer redirect":
				writeFixture(t, filepath.Join(r.OutputDirectory, "tokenizer_config.json"), `{"tokenizer_file":"outside.json"}`)
			case "pickle":
				writeFixture(t, filepath.Join(r.OutputDirectory, "adapter_model.bin"), "pickle")
			case "sharded":
				writeFixture(t, filepath.Join(r.OutputDirectory, "adapter_model.safetensors.index.json"), "{}")
			case "chat templates":
				if err := os.Mkdir(filepath.Join(r.OutputDirectory, "chat_templates"), 0700); err != nil {
					t.Fatal(err)
				}
			case "malformed":
				writeFixture(t, config, "{broken")
			case "null":
				writeFixture(t, config, "null")
			}
			if _, err := artifactFiles(r); err == nil {
				t.Fatal("accepted unsafe artifact")
			}
		})
	}
}

func TestImportPreparationRejectsUnsupportedTargetsAndDestinations(t *testing.T) {
	r, e := completedArtifact(t)
	for _, target := range []string{"", "vllm", "ollama", "llama.cpp", "gguf"} {
		if PrepareImport(e.RecipeFile, filepath.Join(t.TempDir(), "plan"), target) == nil {
			t.Fatal("accepted unsupported target", target)
		}
	}
	for _, path := range []string{e.RecipeFile, "relative", filepath.Join(r.OutputDirectory, "plan"), filepath.Join(r.Model.Inventory.Root, "plan"), filepath.Join(r.Runtime.Inventory.Root, "plan"), r.Train.Path} {
		if PrepareImport(e.RecipeFile, path, "peft") == nil {
			t.Fatal("accepted overlapping destination")
		}
	}
	if PrepareImport("/missing/recipe.json", filepath.Join(t.TempDir(), "plan"), "peft") == nil {
		t.Fatal("accepted missing recipe")
	}
	writeFixture(t, e.RecipeFile, "{}")
	if PrepareImport(e.RecipeFile, filepath.Join(t.TempDir(), "plan"), "peft") == nil {
		t.Fatal("accepted invalid recipe")
	}
}

func TestExportBindsAndChecksTheSavedProcessor(t *testing.T) {
	// The export fixture holds the real processor_config.json of the locked
	// transformers (see executionArtifactScript).
	r, _ := completedArtifact(t)
	files, err := artifactFiles(r)
	if err != nil || files[processorConfigName] == "" {
		t.Fatalf("saved processor not bound: %v %v", files, err)
	}
	split := func(t *testing.T, r Recipe) {
		if err := os.Remove(filepath.Join(r.OutputDirectory, processorConfigName)); err != nil {
			t.Fatal(err)
		}
		writeFixture(t, filepath.Join(r.OutputDirectory, imageProcessorName), qwenImageProcessor)
		writeFixture(t, filepath.Join(r.OutputDirectory, videoProcessorName), qwenVideoProcessor)
	}
	r, _ = completedArtifact(t)
	split(t, r)
	if _, err := artifactFiles(r); err != nil {
		t.Fatal("split real processor files rejected:", err)
	}
	for name, change := range map[string]func(*testing.T, Recipe){
		"auto_map": func(t *testing.T, r Recipe) {
			writeFixture(t, filepath.Join(r.OutputDirectory, processorConfigName), strings.Replace(qwenSavedProcessor, "{", `{"auto_map":{},`, 1))
		},
		"image_processor_type": func(t *testing.T, r Recipe) {
			split(t, r)
			writeFixture(t, filepath.Join(r.OutputDirectory, imageProcessorName), strings.Replace(qwenImageProcessor, "Qwen2VLImageProcessorFast", "OtherImageProcessor", 1))
		},
		"processor_class": func(t *testing.T, r Recipe) {
			split(t, r)
			writeFixture(t, filepath.Join(r.OutputDirectory, videoProcessorName), strings.Replace(qwenVideoProcessor, `"Qwen3VLProcessor"`, `"OtherProcessor"`, 1))
		},
		"complete processor": func(t *testing.T, r Recipe) {
			split(t, r)
			if err := os.Remove(filepath.Join(r.OutputDirectory, videoProcessorName)); err != nil {
				t.Fatal(err)
			}
		},
		"complete processor ": func(t *testing.T, r Recipe) {
			if err := os.Remove(filepath.Join(r.OutputDirectory, processorConfigName)); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			r, _ := completedArtifact(t)
			change(t, r)
			if _, err := artifactFiles(r); err == nil || !strings.Contains(err.Error(), strings.TrimSpace(name)) {
				t.Fatalf("unsupported saved processor accepted or unnamed: %v", err)
			}
		})
	}
}

package llmtraining

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
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
	// The processor and model configurations change to content that still
	// passes the export checks, so only the reload binding can reject them.
	saved := map[string]string{processorConfigName: savedProcessorFixture, "config.json": testdataPath("smoke-output/saved_config.json")}
	for _, name := range []string{"adapter_config.json", "tokenizer_config.json", "tokenizer.json", "added_tokens.json", processorConfigName, "config.json"} {
		t.Run(name, func(t *testing.T) {
			r, e, jobPath := executionFixture(t, executionScript)
			script := executionScript + "printf x > '" + filepath.Join(r.OutputDirectory, "adapter_model.safetensors") + "'\n"
			target := filepath.Join(r.OutputDirectory, name)
			if source, ok := saved[name]; ok {
				script += "if [ \"$5\" = train ]; then cp '" + source + "' '" + target + "'; fi\n"
				script += "if [ \"$5\" = reload ]; then printf ' ' >> '" + target + "'; fi\n"
			} else {
				script += "if [ \"$5\" = reload ]; then printf '{}' > '" + target + "'; fi\n"
			}
			if name == "tokenizer_config.json" || name == "tokenizer.json" {
				script = strings.ReplaceAll(script, "then printf '{}'", "then printf '{\"changed\":true}'")
			}
			updateExecutionScript(t, r, &e, jobPath, script)
			err := RunJob(context.Background(), jobPath, executionInvocation, executionGPU)
			if err == nil {
				t.Fatal("accepted changed export")
			}
			if _, ok := saved[name]; ok && !strings.Contains(err.Error(), "changed during reload") {
				t.Fatalf("error = %v, want the reload binding", err)
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
	rejectedNames := map[string]string{"pickle": "adapter_model.bin", "sharded": "adapter_model.safetensors.index.json", "chat templates": "chat_templates",
		"legacy chat template": "chat_template.json", "additional chat templates": "additional_chat_templates"}
	for _, state := range []string{"missing config", "missing tokenizer", "linked", "empty", "budget", "remote base", "wrong format", "tokenizer redirect", "pickle", "sharded", "chat templates", "legacy chat template", "additional chat templates", "malformed", "null"} {
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
			case "legacy chat template":
				writeFixture(t, filepath.Join(r.OutputDirectory, "chat_template.json"), `{"chat_template":"x"}`)
			case "additional chat templates":
				if err := os.Mkdir(filepath.Join(r.OutputDirectory, "additional_chat_templates"), 0700); err != nil {
					t.Fatal(err)
				}
				writeFixture(t, filepath.Join(r.OutputDirectory, "additional_chat_templates", "x.jinja"), "x")
			case "malformed":
				writeFixture(t, config, "{broken")
			case "null":
				writeFixture(t, config, "null")
			}
			_, err := artifactFiles(r, exportHooks{})
			if err == nil {
				t.Fatal("accepted unsafe artifact")
			}
			if name, rejected := rejectedNames[state]; rejected && !strings.HasSuffix(err.Error(), "unsupported training export layout: "+name) {
				t.Fatalf("rejected export layout without its name: %v", err)
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

// splitProcessorArtifact completes a job whose training saves the split
// processor files of the pinned snapshot instead of processor_config.json.
func splitProcessorArtifact(t *testing.T) (Recipe, jobEnvelope) {
	t.Helper()
	r, e, jobPath := executionFixture(t, executionScript)
	script := executionScript + "printf x > '" + filepath.Join(r.OutputDirectory, "adapter_model.safetensors") + "'\n" +
		"if [ \"$5\" = train ]; then rm '" + filepath.Join(r.OutputDirectory, processorConfigName) + "'\n" +
		"cp '" + testdataPath("qwen3.8-processor/"+imageProcessorName) + "' '" + filepath.Join(r.OutputDirectory, imageProcessorName) + "'\n" +
		"cp '" + testdataPath("qwen3.8-processor/"+videoProcessorName) + "' '" + filepath.Join(r.OutputDirectory, videoProcessorName) + "'\nfi\n"
	updateExecutionScript(t, r, &e, jobPath, script)
	if err := RunJob(context.Background(), jobPath, executionInvocation, executionGPU); err != nil {
		t.Fatal(err)
	}
	return r, e
}

func TestExportBindsAndChecksTheSavedProcessor(t *testing.T) {
	// The export fixture holds the real processor_config.json of the locked
	// transformers (see executionArtifactScript).
	r, _ := completedArtifact(t)
	files, err := artifactFiles(r, exportHooks{})
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
	r, e := splitProcessorArtifact(t)
	provenance, err := os.ReadFile(filepath.Join(r.OutputDirectory, provenanceName))
	if err != nil {
		t.Fatal(err)
	}
	var bound Provenance
	if err := json.Unmarshal(provenance, &bound); err != nil {
		t.Fatal(err)
	}
	if bound.Files[imageProcessorName] == "" || bound.Files[videoProcessorName] == "" || bound.Files[processorConfigName] != "" {
		t.Fatalf("split processor files not bound in the provenance: %v", bound.Files)
	}
	plans := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(plans, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := PrepareImport(e.RecipeFile, filepath.Join(plans, "bound.json"), "peft"); err != nil {
		t.Fatal("import rejected the bound split processor files:", err)
	}
	writeFixture(t, filepath.Join(r.OutputDirectory, imageProcessorName), strings.Replace(qwenImageProcessor, "\n", " ", 1))
	if err := PrepareImport(e.RecipeFile, filepath.Join(plans, "changed.json"), "peft"); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatal("import accepted a changed split processor file:", err)
	}
	for _, c := range []struct {
		name, want string
		change     func(*testing.T, Recipe)
	}{
		{"combined with auto_map", "top level contains auto_map", func(t *testing.T, r Recipe) {
			writeFixture(t, filepath.Join(r.OutputDirectory, processorConfigName), strings.Replace(qwenSavedProcessor, "{", `{"auto_map":{},`, 1))
		}},
		{"split with other image processor", "image_processor_type must be", func(t *testing.T, r Recipe) {
			split(t, r)
			writeFixture(t, filepath.Join(r.OutputDirectory, imageProcessorName), strings.Replace(qwenImageProcessor, "Qwen2VLImageProcessorFast", "OtherImageProcessor", 1))
		}},
		{"split with other processor class", "processor_class must be", func(t *testing.T, r Recipe) {
			split(t, r)
			writeFixture(t, filepath.Join(r.OutputDirectory, videoProcessorName), strings.Replace(qwenVideoProcessor, `"Qwen3VLProcessor"`, `"OtherProcessor"`, 1))
		}},
		{"split without video processor", "preprocessor_config.json without video_preprocessor_config.json", func(t *testing.T, r Recipe) {
			split(t, r)
			if err := os.Remove(filepath.Join(r.OutputDirectory, videoProcessorName)); err != nil {
				t.Fatal(err)
			}
		}},
		{"combined with only the image processor", "preprocessor_config.json without video_preprocessor_config.json", func(t *testing.T, r Recipe) {
			writeFixture(t, filepath.Join(r.OutputDirectory, imageProcessorName), qwenImageProcessor)
		}},
		{"combined with other processor class", "processor_config.json: processor_class must be Qwen3VLProcessor", func(t *testing.T, r Recipe) {
			writeFixture(t, filepath.Join(r.OutputDirectory, processorConfigName), strings.Replace(qwenSavedProcessor, `"processor_class": "Qwen3VLProcessor"`, `"processor_class": "OtherProcessor"`, 1))
		}},
		{"combined with top-level file path", "processor_config.json: top level file names must name inventoried files", func(t *testing.T, r Recipe) {
			writeFixture(t, filepath.Join(r.OutputDirectory, processorConfigName), strings.Replace(qwenSavedProcessor, "{", `{"vocab_file":"missing.json",`, 1))
		}},
		{"combined with top-level file redirect", "processor_config.json: top level contains transformers_weights", func(t *testing.T, r Recipe) {
			writeFixture(t, filepath.Join(r.OutputDirectory, processorConfigName), strings.Replace(qwenSavedProcessor, "{", `{"transformers_weights":"x",`, 1))
		}},
		{"no processor files", "requires a complete processor", func(t *testing.T, r Recipe) {
			if err := os.Remove(filepath.Join(r.OutputDirectory, processorConfigName)); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, _ := completedArtifact(t)
			c.change(t, r)
			if _, err := artifactFiles(r, exportHooks{}); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("unsupported saved processor accepted or unnamed: %v", err)
			}
		})
	}
}

// withQuantization adds a quantization_config object to a model config.
func withQuantization(config, quantization string) string {
	return strings.TrimSuffix(strings.TrimSpace(config), "}") + `,"quantization_config":` + quantization + `}`
}

// The saved model config.json comes from the CPU smoke, which compares the
// key structure of its real output with this file. qlora/quantization_config.json
// is what the locked transformers serializes for the bitsandbytes NF4 settings
// that Axolotl requests for QLoRA.
var (
	smokeSavedConfig         = testdataText("smoke-output/saved_config.json")
	qloraQuantizationFixture = testdataText("qlora/quantization_config.json")
)

func TestExportModelConfigPassesTheModelChecks(t *testing.T) {
	for _, tc := range []struct{ name, adapter, config, want string }{
		{"saved model configuration", "lora", smokeSavedConfig, ""},
		{"real model configuration", "lora", testdataText("qwen3.8-27b-config/config.json"), ""},
		{"qlora quantization", "qlora", withQuantization(smokeSavedConfig, qloraQuantizationFixture), ""},
		{"quantization without qlora", "lora", withQuantization(smokeSavedConfig, qloraQuantizationFixture), "quantization_config"},
		{"other quantization", "qlora", withQuantization(smokeSavedConfig, strings.Replace(qloraQuantizationFixture, `"nf4"`, `"fp4"`, 1)), "quantization_config field bnb_4bit_quant_type differs"},
		{"missing quantization field", "qlora", withQuantization(smokeSavedConfig, strings.Replace(qloraQuantizationFixture, `"quant_method": "bitsandbytes"`, `"quant_methods": "bitsandbytes"`, 1)), "quantization_config field quant_method differs"},
		{"null quantization", "qlora", withQuantization(smokeSavedConfig, "null"), "quantization_config must be an object"},
		{"extra quantization field", "qlora", withQuantization(smokeSavedConfig, strings.Replace(qloraQuantizationFixture, "{", `{"extra\nfield":1,`, 1)), "quantization_config has an extra field"},
		{"nested quantization", "qlora", strings.Replace(smokeSavedConfig, `"text_config": {`, `"text_config": {"quantization_config":`+qloraQuantizationFixture+`,`, 1), "quantization_config only at the top level"},
		{"nested and top-level quantization", "qlora", withQuantization(strings.Replace(smokeSavedConfig, `"vision_config": {`, `"vision_config": {"extra":[{"quantization_config":{}}],`, 1), qloraQuantizationFixture), "quantization_config only at the top level"},
		{"nested remote code", "lora", `{` + qwenModelFields + `,"vision_config":{"auto_map":{}}}`, "training export config.json vision_config contains auto_map"},
		{"sub-config file path", "lora", `{"model_type":"qwen3_5","architectures":["Qwen3_5ForConditionalGeneration"],"text_config":{"model_type":"qwen3_5_text","tokenizer_file":"missing.json"}}`, "text_config file names"},
		{"other architecture", "lora", `{"model_type":"llama"}`, "image-text architecture"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := completedArtifact(t)
			r.Adapter = tc.adapter
			r.Budget.ExportBytes = 1 << 20
			writeFixture(t, filepath.Join(r.OutputDirectory, "config.json"), tc.config)
			_, err := artifactFiles(r, exportHooks{})
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "training export config.json") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestExportRequiresTheSavedModelConfig(t *testing.T) {
	r, _ := completedArtifact(t)
	files, err := artifactFiles(r, exportHooks{})
	if err != nil || files[modelConfigName] == "" {
		t.Fatalf("saved config.json not bound: %v %v", files, err)
	}
	if err := os.Remove(filepath.Join(r.OutputDirectory, modelConfigName)); err != nil {
		t.Fatal(err)
	}
	if _, err := artifactFiles(r, exportHooks{}); err == nil || !strings.Contains(err.Error(), "training export requires config.json") {
		t.Fatalf("error = %v", err)
	}
}

func TestExportRejectsAnOversizedConfigurationBeforeBufferingIt(t *testing.T) {
	t.Parallel()
	r, _ := completedArtifact(t)
	r.Budget.ExportBytes = 64 << 20
	// Valid JSON above the 1 MiB configuration limit.
	writeFixture(t, filepath.Join(r.OutputDirectory, modelConfigName), strings.TrimSuffix(strings.TrimSpace(smokeSavedConfig), "}")+`,"pad":"`+strings.Repeat("x", 2<<20)+`"}`)
	// The export rejects it from its size, before it reads and buffers it.
	hashed := false
	hooks := exportHooks{hashed: func(name string) { hashed = hashed || name == modelConfigName }}
	if _, err := artifactFiles(r, hooks); err == nil || err.Error() != "training export config.json is larger than 1 MiB" {
		t.Fatalf("error = %v", err)
	}
	if hashed {
		t.Fatal("oversized config.json was read before it was rejected")
	}
}

func TestExportValidatesTheBytesItHashed(t *testing.T) {
	t.Parallel()
	for _, name := range slices.Sorted(maps.Keys(exportJSONNames)) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r, _ := completedArtifact(t)
			if name == imageProcessorName || name == videoProcessorName {
				r, _ = splitProcessorArtifact(t)
			}
			r.Budget.ExportBytes = 1 << 20
			// After the file is hashed, replace it on disk with content that
			// fails validation. The export must validate the bytes it hashed.
			hooks := exportHooks{hashed: func(hashed string) {
				if hashed == name {
					writeFixture(t, filepath.Join(r.OutputDirectory, name), `{`)
				}
			}}
			if _, err := artifactFiles(r, hooks); err != nil {
				t.Fatal("validation read the file again:", err)
			}
		})
	}
}

func TestExportRejectsAConfigurationThatGrowsWhileHashed(t *testing.T) {
	t.Parallel()
	r, _ := completedArtifact(t)
	r.Budget.ExportBytes = 64 << 20
	path := filepath.Join(r.OutputDirectory, modelConfigName)
	// A valid configuration of exactly the limit passes the size check, then
	// grows before the copy.
	base := strings.TrimSuffix(strings.TrimSpace(smokeSavedConfig), "}")
	padding := configurationBytes - len(base) - len(`,"pad":""}`)
	writeFixture(t, path, base+`,"pad":"`+strings.Repeat("x", padding)+`"}`)
	if _, err := artifactFiles(r, exportHooks{}); err != nil {
		t.Fatal("configuration of exactly the limit rejected:", err)
	}
	if info, err := os.Stat(path); err != nil || info.Size() != configurationBytes {
		t.Fatalf("fixture size = %v %v", info, err)
	}
	hooks := exportHooks{statted: func(statted string) {
		if statted == path {
			f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if _, err := f.WriteString(" grown"); err != nil {
				t.Fatal(err)
			}
		}
	}}
	if _, err := artifactFiles(r, hooks); err == nil || !strings.Contains(err.Error(), "training export config.json") {
		t.Fatalf("grown configuration accepted or unnamed: %v", err)

	}
}

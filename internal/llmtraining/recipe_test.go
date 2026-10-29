package llmtraining

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func writeFixture(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(content))
	return hex.EncodeToString(digest[:])
}

func fixture(t *testing.T) Recipe {
	t.Helper()
	root := t.TempDir()
	inventory := func(name string, files map[string]string) Inventory {
		dir := filepath.Join(root, name)
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		i := Inventory{Root: dir, Files: map[string]string{}}
		for name, content := range files {
			i.Files[name] = writeFixture(t, filepath.Join(dir, name), content)
		}
		return i
	}
	r := Recipe{Version: 1, Model: Snapshot{Revision: strings.Repeat("a", 40), Inventory: inventory("model", map[string]string{"config.json": `{"model_type":"llama"}`, "model.safetensors": "fixture bytes, not a qualified model"})}, Tokenizer: Snapshot{Revision: strings.Repeat("b", 40), Inventory: inventory("tokenizer", map[string]string{"tokenizer_config.json": `{}`, "tokenizer.json": `{}`})}, Runtime: Runtime{Entrypoint: "axolotl", Inventory: inventory("runtime", map[string]string{"axolotl": "#!/bin/sh\nexit 99\n", "entry_points.txt": "[axolotl.plugins]\nexample = unapproved:Plugin\n"})}, OutputDirectory: filepath.Join(root, "output"), Adapter: "qlora", Rank: 8, Alpha: 16, SequenceLength: 128, MicroBatchSize: 1, GradientAccumulation: 1, MaxSteps: 2, LearningRate: 0.0002, Seed: 42, Budget: Budget{RAMBytes: 1, WeightsBytes: 1, CacheBytes: 1, CheckpointBytes: 1, RetainedCheckpoints: 2, ExportBytes: 1, ReserveBytes: 1}}
	if err := os.Mkdir(r.OutputDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	r.Train.Path = filepath.Join(root, "train.jsonl")
	r.Train.SHA256 = writeFixture(t, r.Train.Path, "{\"text\":\"training example\"}\n")
	r.HeldOut.Path = filepath.Join(root, "heldout.jsonl")
	r.HeldOut.SHA256 = writeFixture(t, r.HeldOut.Path, "{\"text\":\"held out example\"}\n")
	return r
}

func TestSpecialFilesNeverBlockPreparation(t *testing.T) {
	r := fixture(t)
	fifo := filepath.Join(filepath.Dir(r.OutputDirectory), "fifo")
	if err := unix.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := openRegular(fifo); err == nil {
		t.Fatal("accepted FIFO")
	}
	if _, err := openRegular(r.OutputDirectory); err == nil {
		t.Fatal("accepted directory")
	}
	if _, err := openRegular(fifo + "missing"); err == nil {
		t.Fatal("accepted missing file")
	}
	if err := Prepare(fifo, filepath.Join(filepath.Dir(fifo), "plan.json")); err == nil {
		t.Fatal("accepted FIFO recipe")
	}
	r.Train.Path = fifo
	if _, err := Compile(r, math.MaxUint64, math.MaxUint64); err == nil {
		t.Fatal("accepted FIFO dataset")
	}
	if _, _, err := available(fifo + "missing"); err == nil {
		t.Fatal("accepted missing storage")
	}
	if err := inspectConfig(fifo, false, nil); err == nil {
		t.Fatal("accepted FIFO model config")
	}
	if err := matchFile(fifo, strings.Repeat("a", 64), 100); err == nil {
		t.Fatal("accepted FIFO digest input")
	}
}

func TestCompileDeterministicCandidate(t *testing.T) {
	r := fixture(t)
	first, err := Compile(r, math.MaxUint64, math.MaxUint64)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Compile(r, math.MaxUint64, math.MaxUint64)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(first)
	b, _ := json.Marshal(second)
	if string(a) != string(b) {
		t.Fatal("candidate is not deterministic")
	}
	if first.Status != "unqualified" || first.EstimatedStorageBytes != 7 || first.TrainRows != 1 || first.HeldOutRows != 1 {
		t.Fatalf("unexpected plan: %+v", first)
	}
	c := first.Config
	if !c.Load4Bit || c.Load8Bit || !c.BF16 || c.FP16 || c.TrustRemoteCode || c.AutoResume || c.SaveOnlyModel || c.SamplePacking || len(c.Plugins) != 0 {
		t.Fatalf("unsafe defaults: %+v", c)
	}
	if c.UseWandB || c.UseMLFlow || c.UseTensorBoard || c.UseComet || c.UseTrackio || c.UseOTelMetrics || c.HFUseAuthToken || c.HubModelID != nil || c.PushDatasetToHub != nil || c.WandBMode != "disabled" {
		t.Fatal("reporting or upload configuration enabled")
	}
	if c.TestDatasets[0].Split != "train" || c.TestDatasets[0].Path != r.HeldOut.Path || c.Datasets[0].Field != "text" || c.DatasetPreparedPath != filepath.Join(r.OutputDirectory, "cache") {
		t.Fatalf("wrong dataset mapping: %+v", c)
	}
	if strings.Contains(string(a), "training example") || strings.Contains(string(a), "unapproved:Plugin") {
		t.Fatal("plan exposed file contents")
	}
	r.Adapter = "lora"
	lora, err := Compile(r, math.MaxUint64, math.MaxUint64)
	if err != nil || lora.Config.Load4Bit || lora.RecipeSHA256 == first.RecipeSHA256 {
		t.Fatal("LoRA compilation or provenance failed", err)
	}
}

func TestCompileModelFamilyUsesDecodedString(t *testing.T) {
	for _, tc := range []struct {
		name, config string
		accepted     bool
	}{
		{"escaped llama", `{"model_type":"llam\u0061"}`, true},
		{"escaped unknown", `{"model_type":"unkn\u006fwn"}`, false},
		{"case sensitive", `{"model_type":"Llama"}`, false},
		{"missing", `{}`, false},
		{"null", `{"model_type":null}`, false},
		{"number", `{"model_type":1}`, false},
		{"boolean", `{"model_type":true}`, false},
		{"object", `{"model_type":{}}`, false},
		{"array", `{"model_type":["llama"]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := fixture(t)
			r.Model.Inventory.Files["config.json"] = writeFixture(t, filepath.Join(r.Model.Inventory.Root, "config.json"), tc.config)
			_, err := Compile(r, math.MaxUint64, math.MaxUint64)
			if (err == nil) != tc.accepted {
				t.Fatalf("model family accepted = %v, want %v: %v", err == nil, tc.accepted, err)
			}
		})
	}
}

func TestReadRecipeRequiresCanonicalKeys(t *testing.T) {
	r := fixture(t)
	// Inventory file names are operator-selected paths, not recipe field names.
	r.Model.Inventory.Files["RANK"] = strings.Repeat("a", 64)
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, old, replacement string
		accepted               bool
	}{
		{"canonical", `"rank":8`, `"rank":8`, true},
		{"escaped canonical", `"rank":8`, `"r\u0061nk":8`, true},
		{"explicit zero seed", `"seed":42`, `"seed":0`, true},
		{"omitted seed", `"seed":42,`, ``, false},
		{"null seed", `"seed":42`, `"seed":null`, false},
		{"omitted nested field", `"ramBytes":1,`, ``, false},
		{"null nested field", `"ramBytes":1`, `"ramBytes":null`, false},
		{"top alias", `"rank":8`, `"RANK":8`, false},
		{"top override", `"rank":8`, `"rank":8,"RANK":16`, false},
		{"budget alias", `"budget":`, `"BUDGET":`, false},
		{"nested budget alias", `"ramBytes":1`, `"RAMBYTES":1`, false},
		{"snapshot alias", `"revision":`, `"REVISION":`, false},
		{"inventory alias", `"inventory":`, `"INVENTORY":`, false},
		{"inventory root alias", `"root":`, `"ROOT":`, false},
		{"inventory files alias", `"files":`, `"FILES":`, false},
		{"dataset alias", `"sha256":`, `"SHA256":`, false},
		{"runtime alias", `"entrypoint":`, `"ENTRYPOINT":`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := strings.Replace(string(raw), tc.old, tc.replacement, 1)
			_, err := ReadRecipe(strings.NewReader(input))
			if (err == nil) != tc.accepted {
				t.Fatalf("recipe accepted = %v, want %v: %v", err == nil, tc.accepted, err)
			}
			if err != nil && (strings.Contains(err.Error(), r.Model.Inventory.Root) || (tc.replacement != "" && strings.Contains(err.Error(), tc.replacement))) {
				t.Fatal("recipe error disclosed input")
			}
		})
	}
}

func TestCompileRejectsInvalidAssetConfigUTF8(t *testing.T) {
	for _, asset := range []string{"model", "tokenizer"} {
		t.Run(asset, func(t *testing.T) {
			r := fixture(t)
			inventory, name := &r.Model.Inventory, "config.json"
			if asset == "tokenizer" {
				inventory, name = &r.Tokenizer.Inventory, "tokenizer_config.json"
			}
			invalid := "{\"model_type\":\"llama\",\"private\":\"secret-\xff\"}"
			inventory.Files[name] = writeFixture(t, filepath.Join(inventory.Root, name), invalid)
			if _, err := Compile(r, math.MaxUint64, math.MaxUint64); err == nil || strings.Contains(err.Error(), "secret-") || strings.Contains(err.Error(), inventory.Root) {
				t.Fatal("invalid asset UTF-8 accepted or disclosed", err)
			}
		})
	}
}

func TestInventoryBoundsAllVisitedEntries(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	i := Inventory{Root: root, Files: map[string]string{"nested/file.txt": writeFixture(t, filepath.Join(root, "nested", "file.txt"), "fixture")}}
	if err := i.Inspect(); err != nil {
		t.Fatal("normal nested inventory rejected", err)
	}
	// The root is excluded; the nested directory and file consume two entries.
	for n := 0; n < 65536-2; n++ {
		if err := os.Mkdir(filepath.Join(root, "empty"+strconv.Itoa(n)), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := i.Inspect(); err != nil {
		t.Fatal("inventory at visited-entry limit rejected", err)
	}
	if err := os.Mkdir(filepath.Join(root, "one-too-many"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := i.Inspect(); err == nil || strings.Contains(err.Error(), root) {
		t.Fatal("excess empty directories accepted or disclosed", err)
	}
	if err := os.Mkdir(filepath.Join(root, "wide-overflow"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := i.Inspect(); err == nil {
		t.Fatal("excess entries in one directory accepted")
	}
}

func TestBoundedDirectoryEntries(t *testing.T) {
	root := t.TempDir()
	if entries, err := boundedDirectoryEntries(root, 0); err != nil || len(entries) != 0 {
		t.Fatal("empty directory rejected", err)
	}
	for _, name := range []string{"a", "b"} {
		if err := os.Mkdir(filepath.Join(root, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, limit := range []int{0, 1} {
		if entries, err := boundedDirectoryEntries(root, limit); err == nil || entries != nil || strings.Contains(err.Error(), root) {
			t.Fatal("directory overflow accepted or disclosed", err)
		}
	}
	if entries, err := boundedDirectoryEntries(root, 2); err != nil || len(entries) != 2 {
		t.Fatal("directory at limit rejected", err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	if _, err := boundedDirectoryEntries(link, 2); err == nil {
		t.Fatal("directory link followed")
	}
}

func TestRejectInvalidRecipe(t *testing.T) {
	for name, change := range map[string]func(*Recipe){
		"version": func(r *Recipe) { r.Version = 2 }, "adapter": func(r *Recipe) { r.Adapter = "remote" },
		"rank": func(r *Recipe) { r.Rank = 65 }, "alpha": func(r *Recipe) { r.Alpha = 0 }, "sequence": func(r *Recipe) { r.SequenceLength = 8192 },
		"batch": func(r *Recipe) { r.MicroBatchSize = 5 }, "accumulation": func(r *Recipe) { r.GradientAccumulation = 0 }, "steps": func(r *Recipe) { r.MaxSteps = 0 }, "seed": func(r *Recipe) { r.Seed = -1 },
		"rate": func(r *Recipe) { r.LearningRate = math.NaN() }, "revision": func(r *Recipe) { r.Model.Revision = "main" },
		"output": func(r *Recipe) { r.OutputDirectory = "relative" }, "dataset": func(r *Recipe) { r.Train.SHA256 = "bad" }, "same dataset": func(r *Recipe) { r.HeldOut.Path = r.Train.Path },
		"inventory": func(r *Recipe) { r.Runtime.Inventory.Files = map[string]string{} }, "root": func(r *Recipe) { r.Runtime.Inventory.Root = "relative" },
		"traversal": func(r *Recipe) { r.Runtime.Inventory.Files["../outside"] = strings.Repeat("a", 64) }, "digest": func(r *Recipe) { r.Runtime.Inventory.Files["axolotl"] = "bad" },
		"entrypoint": func(r *Recipe) { r.Runtime.Entrypoint = "missing" }, "ram": func(r *Recipe) { r.Budget.RAMBytes = 0 }, "retained": func(r *Recipe) { r.Budget.RetainedCheckpoints = 9 },
		"sum overflow": func(r *Recipe) { r.Budget.CacheBytes = math.MaxUint64 }, "checkpoint overflow": func(r *Recipe) { r.Budget.CheckpointBytes = math.MaxUint64 },
	} {
		t.Run(name, func(t *testing.T) {
			r := fixture(t)
			change(&r)
			if _, err := Compile(r, math.MaxUint64, math.MaxUint64); err == nil {
				t.Fatal("accepted invalid recipe")
			}
		})
	}
}

func TestReadRecipeStrictAndPrivate(t *testing.T) {
	r := fixture(t)
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ReadRecipe(strings.NewReader(string(raw))); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`{"secret-token":1}`, `{"version":1,"version":1}`, `null`, `{} {}`, strings.Repeat("x", (8<<20)+1)} {
		if _, err := ReadRecipe(strings.NewReader(raw)); err == nil || strings.Contains(err.Error(), "secret-token") {
			t.Fatalf("unsafe or missing error: %v", err)
		}
	}
}

func TestInputsRejectChangesAndUnsupportedAssets(t *testing.T) {
	for name, change := range map[string]func(*testing.T, *Recipe){
		"mutated": func(t *testing.T, r *Recipe) {
			writeFixture(t, filepath.Join(r.Runtime.Inventory.Root, "axolotl"), "changed")
		},
		"unlisted plugin": func(t *testing.T, r *Recipe) {
			writeFixture(t, filepath.Join(r.Runtime.Inventory.Root, "plugin.py"), "secret")
		},
		"missing": func(t *testing.T, r *Recipe) {
			if err := os.Remove(filepath.Join(r.Runtime.Inventory.Root, "entry_points.txt")); err != nil {
				t.Fatal(err)
			}
		},
		"symlink": func(t *testing.T, r *Recipe) {
			if err := os.Symlink(r.Train.Path, filepath.Join(r.Runtime.Inventory.Root, "link")); err != nil {
				t.Fatal(err)
			}
		},
		"GGUF": func(t *testing.T, r *Recipe) {
			r.Model.Inventory.Files["model.gguf"] = writeFixture(t, filepath.Join(r.Model.Inventory.Root, "model.gguf"), "not training weights")
		},
		"no weights": func(t *testing.T, r *Recipe) {
			delete(r.Model.Inventory.Files, "model.safetensors")
			if err := os.Remove(filepath.Join(r.Model.Inventory.Root, "model.safetensors")); err != nil {
				t.Fatal(err)
			}
		},
		"missing config": func(t *testing.T, r *Recipe) {
			delete(r.Tokenizer.Inventory.Files, "tokenizer_config.json")
			if err := os.Remove(filepath.Join(r.Tokenizer.Inventory.Root, "tokenizer_config.json")); err != nil {
				t.Fatal(err)
			}
		},
		"prequantized": func(t *testing.T, r *Recipe) {
			r.Model.Inventory.Files["config.json"] = writeFixture(t, filepath.Join(r.Model.Inventory.Root, "config.json"), `{"model_type":"llama","quantization_config":{}}`)
		},
		"remote code": func(t *testing.T, r *Recipe) {
			r.Model.Inventory.Files["config.json"] = writeFixture(t, filepath.Join(r.Model.Inventory.Root, "config.json"), `{"model_type":"llama","auto_map":{}}`)
		},
		"wrong family": func(t *testing.T, r *Recipe) {
			r.Model.Inventory.Files["config.json"] = writeFixture(t, filepath.Join(r.Model.Inventory.Root, "config.json"), `{"model_type":"unknown"}`)
		},
		"malformed config": func(t *testing.T, r *Recipe) {
			r.Tokenizer.Inventory.Files["tokenizer_config.json"] = writeFixture(t, filepath.Join(r.Tokenizer.Inventory.Root, "tokenizer_config.json"), `null`)
		},
		"nonempty output": func(t *testing.T, r *Recipe) {
			writeFixture(t, filepath.Join(r.OutputDirectory, "previous"), "do not overwrite")
		},
		"output overlaps": func(t *testing.T, r *Recipe) { r.OutputDirectory = r.Model.Inventory.Root },
		"missing path":    func(t *testing.T, r *Recipe) { r.OutputDirectory = filepath.Join(r.OutputDirectory, "missing") },
	} {
		t.Run(name, func(t *testing.T) {
			r := fixture(t)
			change(t, &r)
			if _, err := Compile(r, math.MaxUint64, math.MaxUint64); err == nil {
				t.Fatal("accepted invalid input")
			}
		})
	}
}

func TestDatasetsRejectLeakageAndInvalidRecords(t *testing.T) {
	for _, content := range []string{"", `{"text":""}`, `{"text":"secret","metadata":"secret"}`, `{"text":"x","text":"y"}`, `{"text":"training example"}`, `{"text":"training \u0065xample"}`, strings.Repeat("x", 64<<10), strings.Repeat("{\"text\":\"heldout\"}\n", 50001)} {
		r := fixture(t)
		r.HeldOut.SHA256 = writeFixture(t, r.HeldOut.Path, content)
		if _, err := Compile(r, math.MaxUint64, math.MaxUint64); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatal("unsafe or missing error", err)
		}
	}
	r := fixture(t)
	writeFixture(t, r.Train.Path, `{"text":"changed"}`)
	if _, err := Compile(r, math.MaxUint64, math.MaxUint64); err == nil {
		t.Fatal("accepted changed dataset")
	}
	if _, err := Compile(fixture(t), 0, math.MaxUint64); err == nil {
		t.Fatal("accepted insufficient RAM")
	}
	if _, err := Compile(fixture(t), math.MaxUint64, 6); err == nil {
		t.Fatal("accepted insufficient storage")
	}
}

func TestDatasetRequiresExactLowercaseTextKey(t *testing.T) {
	for _, content := range []string{
		`{"Text":"heldout"}`,
		`{"TEXT":"heldout"}`,
		`{"text":"training example","Text":"apparently distinct"}`,
		`{"Text":"apparently distinct","text":"training example"}`,
	} {
		t.Run(content, func(t *testing.T) {
			r := fixture(t)
			r.HeldOut.SHA256 = writeFixture(t, r.HeldOut.Path, content)
			if _, err := Compile(r, math.MaxUint64, math.MaxUint64); err == nil || err.Error() != "candidate dataset requires one bounded text object per line" {
				t.Fatal("accepted ambiguous dataset keys or disclosed content", err)
			}
		})
	}
}

func TestMalformedUTF8IsRejectedWithoutDisclosure(t *testing.T) {
	r := fixture(t)
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	malformed := strings.Replace(string(raw), r.Model.Inventory.Root, "/secret\xff", 1)
	if _, err := ReadRecipe(strings.NewReader(malformed)); err == nil || err.Error() != "invalid candidate recipe JSON" {
		t.Fatal("invalid recipe UTF-8 was accepted or disclosed", err)
	}
	r.HeldOut.SHA256 = writeFixture(t, r.HeldOut.Path, "{\"text\":\"secret\xff\"}\n")
	if _, err := Compile(r, math.MaxUint64, math.MaxUint64); err == nil || err.Error() != "candidate dataset requires one bounded text object per line" {
		t.Fatal("invalid dataset UTF-8 was accepted or disclosed", err)
	}
}

func TestPreparePrivateNoOverwriteOrExecution(t *testing.T) {
	r := fixture(t)
	root := filepath.Dir(r.OutputDirectory)
	path := filepath.Join(root, "recipe.json")
	plan := filepath.Join(root, "plan.json")
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, path, string(raw))
	if err := Prepare(path, plan); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(plan)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(plan)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("plan permissions", info, err)
	}
	if err := Prepare(path, plan); err == nil {
		t.Fatal("overwrote prior plan")
	}
	if current, err := os.ReadFile(plan); err != nil || string(current) != string(original) {
		t.Fatal("changed prior plan", err)
	}
	if leftovers, err := filepath.Glob(filepath.Join(root, ".candidate-plan-*")); err != nil || len(leftovers) != 0 {
		t.Fatal("publication left temporary plans", leftovers, err)
	}
	for _, pair := range [][2]string{{path, path}, {"relative", plan}, {path, filepath.Join(root, "missing", "plan")}, {path, filepath.Join(r.OutputDirectory, "plan")}, {path, filepath.Join(r.Runtime.Inventory.Root, "plan")}} {
		if err := Prepare(pair[0], pair[1]); err == nil {
			t.Fatal("accepted unsafe destination")
		}
	}
	writeFixture(t, path, `{"secret":123}`)
	if err := Prepare(path, filepath.Join(root, "new-plan")); err == nil {
		t.Fatal("accepted invalid recipe")
	}
	if _, err := os.Stat(filepath.Join(root, "new-plan")); !os.IsNotExist(err) {
		t.Fatal("wrote rejected candidate")
	}
}

func TestPrepareWriteFailureAllowsRetry(t *testing.T) {
	// File-size limits affect the whole process, so inject the real short write
	// in an isolated test process, without constraining other package tests.
	if os.Getenv("LLMTRAINING_WRITE_FAILURE_HELPER") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestPrepareWriteFailureAllowsRetry$")
		cmd.Env = append(os.Environ(), "LLMTRAINING_WRITE_FAILURE_HELPER=1")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("write failure subprocess: %v\n%s", err, output)
		}
		return
	}
	r := fixture(t)
	root := filepath.Dir(r.OutputDirectory)
	path, plan := filepath.Join(root, "recipe.json"), filepath.Join(root, "plan.json")
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, path, string(raw))
	var original unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_FSIZE, &original); err != nil {
		t.Fatal(err)
	}
	signal.Ignore(unix.SIGXFSZ)
	limited := original
	limited.Cur = 1
	if err := unix.Setrlimit(unix.RLIMIT_FSIZE, &limited); err != nil {
		t.Fatal(err)
	}
	prepareErr := Prepare(path, plan)
	if err := unix.Setrlimit(unix.RLIMIT_FSIZE, &original); err != nil {
		t.Fatal(err)
	}
	if prepareErr == nil {
		t.Fatal("accepted partial plan write")
	}
	if strings.Contains(prepareErr.Error(), root) || strings.Contains(prepareErr.Error(), "training example") {
		t.Fatal("write failure disclosed private input")
	}
	if _, err := os.Lstat(plan); !os.IsNotExist(err) {
		t.Fatal("failed write left final plan", err)
	}
	if leftovers, err := filepath.Glob(filepath.Join(root, ".candidate-plan-*")); err != nil || len(leftovers) != 0 {
		t.Fatal("failed write left temporary plans", leftovers, err)
	}
	if err := Prepare(path, plan); err != nil {
		t.Fatal("retry failed", err)
	}
	var result Plan
	contents, err := os.ReadFile(plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(contents, &result); err != nil || result.Status != "unqualified" {
		t.Fatal("retry did not publish a complete candidate plan", err)
	}
}

func TestPublishPlanDirectorySyncFailureAllowsRetry(t *testing.T) {
	root := t.TempDir()
	plan := filepath.Join(root, "plan.json")
	raw := []byte(`{"status":"unqualified"}`)
	called := false
	err := publishPlanWithDirectorySync(plan, raw, func(directory string) error {
		called = true
		contents, err := os.ReadFile(plan)
		if err != nil || string(contents) != string(raw) || directory != root {
			t.Fatal("sync did not follow complete publication", err)
		}
		return errors.New("injected sync failure at " + directory)
	})
	if !called || err == nil || strings.Contains(err.Error(), root) {
		t.Fatal("sync failure accepted or disclosed private path", err)
	}
	if _, err := os.Lstat(plan); !os.IsNotExist(err) {
		t.Fatal("sync failure left final plan", err)
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatal("sync failure left temporary plans", err)
	}
	if err := publishPlan(plan, raw); err != nil {
		t.Fatal("publication retry failed", err)
	}
	contents, err := os.ReadFile(plan)
	if err != nil || string(contents) != string(raw) {
		t.Fatal("retry did not publish complete plan", err)
	}
}

func TestPublishPlanRollbackPreservesOtherDestinations(t *testing.T) {
	for _, existing := range []bool{true, false} {
		t.Run(strconv.FormatBool(existing), func(t *testing.T) {
			root := t.TempDir()
			plan := filepath.Join(root, "plan.json")
			if existing {
				writeFixture(t, plan, "other publication")
			}
			called := false
			err := publishPlanWithDirectorySync(plan, []byte("candidate"), func(directory string) error {
				called = true
				replacement := filepath.Join(directory, "replacement")
				writeFixture(t, replacement, "other publication")
				if err := os.Rename(replacement, plan); err != nil {
					t.Fatal(err)
				}
				return errors.New("injected sync failure at " + directory)
			})
			if err == nil || called == existing || strings.Contains(err.Error(), root) {
				t.Fatal("publication failure accepted or disclosed private path", err)
			}
			contents, readErr := os.ReadFile(plan)
			if readErr != nil || string(contents) != "other publication" {
				t.Fatal("publication failure removed another destination", readErr)
			}
			if entries, err := os.ReadDir(root); err != nil || len(entries) != 1 {
				t.Fatal("publication failure left temporary plans", err)
			}
		})
	}
}

func TestPublishPlanTemporaryCleanupFailureRollsBack(t *testing.T) {
	root := t.TempDir()
	plan := filepath.Join(root, "plan.json")
	err := publishPlanWithDirectorySync(plan, []byte("candidate"), func(directory string) error {
		staged, err := filepath.Glob(filepath.Join(directory, ".candidate-plan-*"))
		if err != nil || len(staged) != 1 {
			t.Fatal("missing staging file", err)
		}
		// A nonempty directory supplies a harmless, deterministic cleanup
		// failure even when these tests run as root.
		if err := os.Rename(staged[0], filepath.Join(directory, "saved-staging")); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(staged[0], 0700); err != nil {
			t.Fatal(err)
		}
		writeFixture(t, filepath.Join(staged[0], "blocker"), "fixture")
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "temporary file cleanup failed") || strings.Contains(err.Error(), root) {
		t.Fatal("cleanup failure accepted or disclosed private path", err)
	}
	if _, err := os.Lstat(plan); !os.IsNotExist(err) {
		t.Fatal("cleanup failure left final publication", err)
	}
}

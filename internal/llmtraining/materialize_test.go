package llmtraining

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func materializeFixture(t *testing.T) (MaterializeRequest, string, string) {
	t.Helper()
	r := fixture(t)
	if err := os.Mkdir(filepath.Join(r.Runtime.Inventory.Root, "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(r.Runtime.Inventory.Root, "bin/python3"), "not a Python executable; must never run")
	request := MaterializeRequest{Version: 1, Preset: "smoke-lora", Model: LocalSnapshot{Root: r.Model.Inventory.Root, Revision: r.Model.Revision}, Tokenizer: LocalSnapshot{Root: r.Tokenizer.Inventory.Root, Revision: r.Tokenizer.Revision}, RuntimeRoot: r.Runtime.Inventory.Root, Train: r.Train.Path, HeldOut: r.HeldOut.Path, OutputDirectory: r.OutputDirectory, Budget: r.Budget}
	root := filepath.Dir(r.OutputDirectory)
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	return request, filepath.Join(root, "inputs.json"), filepath.Join(root, "recipe.json")
}

func writeMaterializeInput(t *testing.T, path string, request MaterializeRequest) {
	t.Helper()
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, path, string(raw))
}

func TestMaterializePublishesUsablePrivateRecipeWithoutExecutingRuntime(t *testing.T) {
	for _, preset := range []string{"smoke-lora", "representative-qlora"} {
		t.Run(preset, func(t *testing.T) {
			request, input, output := materializeFixture(t)
			request.Preset = preset
			writeMaterializeInput(t, input, request)
			if err := Materialize(input, output); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(output)
			if err != nil {
				t.Fatal(err)
			}
			r, err := ReadRecipe(bytes.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			if r.Runtime.Entrypoint != "bin/python3" || len(r.Runtime.Inventory.Files) != 3 || r.Seed != 42 || r.LearningRate != 0.0002 || r.MicroBatchSize != 1 {
				t.Fatal("materialized recipe lost fixed execution parameters")
			}
			if preset == "smoke-lora" && (r.Adapter != "lora" || r.Rank != 8 || r.Alpha != 16 || r.SequenceLength != 128 || r.GradientAccumulation != 1 || r.MaxSteps != 2) {
				t.Fatal("wrong smoke parameters")
			}
			if preset == "representative-qlora" && (r.Adapter != "qlora" || r.Rank != 16 || r.Alpha != 32 || r.SequenceLength != 2048 || r.GradientAccumulation != 4 || r.MaxSteps != 100) {
				t.Fatal("wrong representative parameters")
			}
			if r.Model.Revision != request.Model.Revision || r.Tokenizer.Revision != request.Tokenizer.Revision || !reflect.DeepEqual(r.Budget, request.Budget) || strings.Contains(string(raw), "training example") || strings.Contains(string(raw), "not a Python executable") {
				t.Fatal("input identity changed or content leaked")
			}
			if info, err := os.Stat(output); err != nil || info.Mode().Perm() != 0600 {
				t.Fatal("recipe is not private", err)
			}
			if err := Prepare(output, filepath.Join(filepath.Dir(output), "plan.json")); err != nil {
				t.Fatal("materialized recipe cannot prepare", err)
			}
			if err := Materialize(input, output); err == nil {
				t.Fatal("overwrote existing recipe")
			}
			writeFixture(t, request.Train, `{"text":"changed"}`)
			if err := Prepare(output, filepath.Join(filepath.Dir(output), "changed-plan.json")); err == nil {
				t.Fatal("materialized recipe did not bind dataset bytes")
			}
		})
	}
}

func TestMaterializeRejectsAmbiguousOrIncompleteInputBeforePublication(t *testing.T) {
	request, input, output := materializeFixture(t)
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	for _, altered := range []string{
		strings.Replace(string(raw), `"version":1`, `"version":1,"version":1`, 1),
		strings.Replace(string(raw), `"version":1`, `"Version":1`, 1),
		strings.Replace(string(raw), `"version":1,`, "", 1),
		strings.Replace(string(raw), `"version":1`, `"version":null`, 1),
		strings.Replace(string(raw), `"version":1`, `"version":1,"plugin":"SECRET"`, 1),
		string(raw) + `{}`,
		strings.Repeat(" ", 65537),
		"\xff",
	} {
		writeFixture(t, input, altered)
		if err := Materialize(input, output); err == nil || strings.Contains(err.Error(), "SECRET") {
			t.Fatal("accepted invalid materialization input or exposed input", err)
		}
	}
	if _, err := os.Lstat(output); !os.IsNotExist(err) {
		t.Fatal("invalid input published a recipe", err)
	}
}

func TestMaterializeRejectsInvalidInputsAndUnsafeDestinations(t *testing.T) {
	for name, alter := range map[string]func(*MaterializeRequest, *string, *string){
		"version":                func(r *MaterializeRequest, _, _ *string) { r.Version = 2 },
		"preset":                 func(r *MaterializeRequest, _, _ *string) { r.Preset = "custom" },
		"revision":               func(r *MaterializeRequest, _, _ *string) { r.Model.Revision = "main" },
		"relative-root":          func(r *MaterializeRequest, _, _ *string) { r.RuntimeRoot = "relative" },
		"zero-budget":            func(r *MaterializeRequest, _, _ *string) { r.Budget.RAMBytes = 0 },
		"output-overlaps-model":  func(r *MaterializeRequest, _, output *string) { *output = filepath.Join(r.Model.Root, "recipe.json") },
		"input-overlaps-runtime": func(r *MaterializeRequest, input, _ *string) { *input = filepath.Join(r.RuntimeRoot, "inputs.json") },
		"same-file":              func(_ *MaterializeRequest, input, output *string) { *output = *input },
		"same-split":             func(r *MaterializeRequest, _, _ *string) { r.HeldOut = r.Train },
		"missing-root":           func(r *MaterializeRequest, _, _ *string) { r.Model.Root += "-missing" },
		"missing-output":         func(r *MaterializeRequest, _, _ *string) { r.OutputDirectory += "-missing" },
	} {
		t.Run(name, func(t *testing.T) {
			request, input, output := materializeFixture(t)
			alter(&request, &input, &output)
			writeMaterializeInput(t, input, request)
			if err := Materialize(input, output); err == nil {
				t.Fatal("accepted invalid input")
			}
		})
	}
	request, input, output := materializeFixture(t)
	writeMaterializeInput(t, input, request)
	if err := os.Chmod(filepath.Dir(output), 0755); err != nil {
		t.Fatal(err)
	}
	if err := Materialize(input, output); err == nil {
		t.Fatal("accepted a nonprivate publication directory")
	}
}

func TestMaterializeRejectsLinksSpecialFilesAndOversizedDatasets(t *testing.T) {
	for _, kind := range []string{"symlink", "fifo", "oversized", "changed-config", "missing-entrypoint", "heldout-overlap"} {
		t.Run(kind, func(t *testing.T) {
			request, input, output := materializeFixture(t)
			switch kind {
			case "symlink":
				if err := os.Symlink(request.Train, filepath.Join(request.Model.Root, "linked.json")); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				if err := unix.Mkfifo(filepath.Join(request.RuntimeRoot, "fifo"), 0600); err != nil {
					t.Fatal(err)
				}
			case "oversized":
				if err := os.Truncate(request.Train, (64<<20)+1); err != nil {
					t.Fatal(err)
				}
			case "changed-config":
				writeFixture(t, filepath.Join(request.Model.Root, "config.json"), `{"model_type":"unsupported"}`)
			case "missing-entrypoint":
				if err := os.Remove(filepath.Join(request.RuntimeRoot, "bin/python3")); err != nil {
					t.Fatal(err)
				}
			case "heldout-overlap":
				writeFixture(t, request.HeldOut, "{\"text\":\"training example\"}\n")
			}
			writeMaterializeInput(t, input, request)
			if err := Materialize(input, output); err == nil {
				t.Fatal("accepted unsupported input")
			}
			if _, err := os.Stat(output); !os.IsNotExist(err) {
				t.Fatal("published invalid recipe", err)
			}
		})
	}
}

func TestMaterializeInventoryLimitsAndDigestErrors(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, filepath.Join(root, "entry"), "bytes")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	full := make(map[string]string, 32768)
	for i := range 32768 {
		full[string(rune(i))] = "digest"
	}
	if _, err := inventoryLocalEntries(root, root, entries, full); err == nil {
		t.Fatal("accepted overfull inventory")
	}
	if _, err := localDigest(filepath.Join(root, "entry"), 4); err == nil {
		t.Fatal("accepted oversized file")
	}
	if _, err := localDigest(root, 1024); err == nil {
		t.Fatal("accepted directory as dataset")
	}
	if _, err := inventoryLocalFiles(filepath.Join(root, "entry")); err == nil {
		t.Fatal("accepted file as inventory")
	}
}

func TestFileDigestNeedsACleanAbsolutePath(t *testing.T) {
	root := t.TempDir()
	want := writeFixture(t, filepath.Join(root, "one.safetensors"), "shard")
	if digest, err := FileDigest(context.Background(), filepath.Join(root, "one.safetensors")); err != nil || digest != want {
		t.Fatal("file digest differs from the fixture digest", err)
	}
	for _, path := range []string{"relative", root + "/", "/"} {
		if _, err := FileDigest(context.Background(), path); err == nil {
			t.Fatalf("file %q was accepted", path)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := FileDigest(ctx, filepath.Join(root, "one.safetensors")); !errors.Is(err, context.Canceled) {
		t.Fatal("a canceled digest must stop", err)
	}
}

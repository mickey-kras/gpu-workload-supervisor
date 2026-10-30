package llmtraining

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompileBindsShardIndexToInventory(t *testing.T) {
	for _, tc := range []struct {
		name, index string
		accepted    bool
	}{
		{"valid", `{"metadata":{"total_size":42},"weight_map":{"a":"one.safetensors","b":"nested/two.safetensors","c":"one.safetensors"}}`, true},
		{"traversal", `{"weight_map":{"a":"../outside.safetensors"}}`, false},
		{"absolute", `{"weight_map":{"a":"/private/outside.safetensors"}}`, false},
		{"unclean", `{"weight_map":{"a":"nested/../one.safetensors"}}`, false},
		{"backslash", `{"weight_map":{"a":"..\\outside.safetensors"}}`, false},
		{"missing shard", `{"weight_map":{"a":"missing.safetensors"}}`, false},
		{"wrong extension", `{"weight_map":{"a":"config.json"}}`, false},
		{"empty path", `{"weight_map":{"a":""}}`, false},
		{"null path", `{"weight_map":{"a":null}}`, false},
		{"number path", `{"weight_map":{"a":1}}`, false},
		{"empty map", `{"weight_map":{}}`, false},
		{"null map", `{"weight_map":null}`, false},
		{"array map", `{"weight_map":[]}`, false},
		{"missing map", `{}`, false},
		{"case alias", `{"WEIGHT_MAP":{"a":"one.safetensors"}}`, false},
		{"duplicate key", `{"weight_map":{"a":"../outside.safetensors","a":"one.safetensors"}}`, false},
		{"trailing value", `{"weight_map":{"a":"one.safetensors"}}{}`, false},
		{"malformed", `{"weight_map":`, false},
		{"null index", `null`, false},
		{"invalid UTF8", "{\"weight_map\":{\"a\":\"\xff.safetensors\"}}", false},
		{"oversized", strings.Repeat(" ", 1<<20) + `{"weight_map":{"a":"one.safetensors"}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := fixture(t)
			inventory := &r.Model.Inventory
			delete(inventory.Files, "model.safetensors")
			if err := os.Remove(filepath.Join(inventory.Root, "model.safetensors")); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(inventory.Root, "nested"), 0700); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"one.safetensors", "nested/two.safetensors"} {
				inventory.Files[name] = writeFixture(t, filepath.Join(inventory.Root, name), "fixture shard")
			}
			inventory.Files["model.safetensors.index.json"] = writeFixture(t, filepath.Join(inventory.Root, "model.safetensors.index.json"), tc.index)
			_, err := Compile(r, math.MaxUint64, math.MaxUint64)
			if (err == nil) != tc.accepted {
				t.Fatalf("shard index accepted = %v, want %v: %v", err == nil, tc.accepted, err)
			}
			if err != nil && (strings.Contains(err.Error(), inventory.Root) || strings.Contains(err.Error(), "outside.safetensors")) {
				t.Fatal("shard index error disclosed a private path")
			}
		})
	}
}

func TestCompileRejectsWeightFilenameOverrides(t *testing.T) {
	for _, weights := range []string{`"../outside.safetensors"`, `"one.safetensors"`} {
		t.Run(weights, func(t *testing.T) {
			r := fixture(t)
			inventory := &r.Model.Inventory
			inventory.Files["config.json"] = writeFixture(t, filepath.Join(inventory.Root, "config.json"), `{"model_type":"qwen3_5","architectures":["Qwen3_5ForConditionalGeneration"],"text_config":{"model_type":"qwen3_5_text"},"transformers_weights":"other.safetensors.index.json"}`)
			inventory.Files["other.safetensors.index.json"] = writeFixture(t, filepath.Join(inventory.Root, "other.safetensors.index.json"), `{"weight_map":{"a":`+weights+`}}`)
			inventory.Files["one.safetensors"] = writeFixture(t, filepath.Join(inventory.Root, "one.safetensors"), "fixture shard")
			if _, err := Compile(r, math.MaxUint64, math.MaxUint64); err == nil {
				t.Fatal("accepted alternate weight index or returned unrelated error", err)
			}
		})
	}
}

func TestCompileRejectsModelConfigurationIndirection(t *testing.T) {
	for _, tc := range []struct {
		name, config string
	}{
		{"alternate config", `{"model_type":"qwen3_5","architectures":["Qwen3_5ForConditionalGeneration"],"text_config":{"model_type":"qwen3_5_text"},"configuration_files":["config.5.20.1.json"]}`},
		{"attribute alias", `{"model_type":"qwen3_5","architectures":["Qwen3_5ForConditionalGeneration"],"text_config":{"model_type":"qwen3_5_text"},"attribute_map":{"transformers_weights":"selected_weights"},"selected_weights":"other.safetensors.index.json"}`},
		{"state injection", `{"model_type":"qwen3_5","architectures":["Qwen3_5ForConditionalGeneration"],"text_config":{"model_type":"qwen3_5_text"},"__dict__":{"transformers_weights":"other.safetensors.index.json"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := fixture(t)
			inventory := &r.Model.Inventory
			inventory.Files["config.json"] = writeFixture(t, filepath.Join(inventory.Root, "config.json"), tc.config)
			inventory.Files["config.5.20.1.json"] = writeFixture(t, filepath.Join(inventory.Root, "config.5.20.1.json"), `{"model_type":"qwen3_5","architectures":["Qwen3_5ForConditionalGeneration"],"text_config":{"model_type":"qwen3_5_text"},"transformers_weights":"other.safetensors.index.json"}`)
			inventory.Files["other.safetensors.index.json"] = writeFixture(t, filepath.Join(inventory.Root, "other.safetensors.index.json"), `{"weight_map":{"a":"model.safetensors"}}`)
			if _, err := Compile(r, math.MaxUint64, math.MaxUint64); err == nil || !strings.Contains(err.Error(), "model configuration indirection is unsupported") {
				t.Fatal("accepted indirect model configuration or returned unrelated error", err)
			}
		})
	}
}

func TestCompileValidatesAlternateShardIndexes(t *testing.T) {
	for _, tc := range []struct {
		name, shard string
		accepted    bool
	}{
		{"escaping", "../outside.safetensors", false},
		{"inventoried", "model.safetensors", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := fixture(t)
			inventory := &r.Model.Inventory
			inventory.Files["other.safetensors.index.json"] = writeFixture(t, filepath.Join(inventory.Root, "other.safetensors.index.json"), `{"weight_map":{"a":"`+tc.shard+`"}}`)
			_, err := Compile(r, math.MaxUint64, math.MaxUint64)
			if (err == nil) != tc.accepted {
				t.Fatalf("alternate shard index accepted = %v, want %v: %v", err == nil, tc.accepted, err)
			}
		})
	}
}

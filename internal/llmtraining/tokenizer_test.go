package llmtraining

import (
	"math"
	"path/filepath"
	"testing"
)

func TestCompileRejectsTokenizerFilenameOverrides(t *testing.T) {
	for _, tc := range []struct {
		name, config string
		accepted     bool
	}{
		{"default filename", `{}`, true},
		{"traversal", `{"fast_tokenizer_files":["../tokenizer.5.0.0.json"]}`, false},
		{"absolute", `{"fast_tokenizer_files":["/private/tokenizer.5.0.0.json"]}`, false},
		{"inventoried version", `{"fast_tokenizer_files":["tokenizer.5.0.0.json"]}`, false},
		{"empty list", `{"fast_tokenizer_files":[]}`, false},
		{"null", `{"fast_tokenizer_files":null}`, false},
		{"wrong type", `{"fast_tokenizer_files":1}`, false},
		{"escaped key", `{"fast_tokenizer_\u0066iles":["tokenizer.5.0.0.json"]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := fixture(t)
			inventory := &r.Tokenizer.Inventory
			inventory.Files["tokenizer_config.json"] = writeFixture(t, filepath.Join(inventory.Root, "tokenizer_config.json"), tc.config)
			inventory.Files["tokenizer.5.0.0.json"] = writeFixture(t, filepath.Join(inventory.Root, "tokenizer.5.0.0.json"), `{}`)
			_, err := Compile(r, math.MaxUint64, math.MaxUint64)
			if (err == nil) != tc.accepted {
				t.Fatalf("tokenizer override accepted = %v, want %v: %v", err == nil, tc.accepted, err)
			}
			if err != nil && err.Error() != "candidate tokenizer filename overrides are unsupported" {
				t.Fatal("tokenizer override returned unrelated error or disclosed private input", err)
			}
		})
	}
}

func TestCompileAppliesTheSharedLoaderKeys(t *testing.T) {
	cases := map[string]string{}
	for _, key := range RemoteCodeKeys {
		cases[key] = "candidate remote-code mappings are unsupported"
	}
	for _, key := range ModelRedirectKeys {
		cases[key] = "candidate model configuration indirection is unsupported"
	}
	for _, key := range FileRedirectKeys {
		cases[key] = "candidate tokenizer filename overrides are unsupported"
	}
	for key, want := range cases {
		t.Run(key, func(t *testing.T) {
			r := fixture(t)
			inventory := &r.Tokenizer.Inventory
			inventory.Files["tokenizer_config.json"] = writeFixture(t, filepath.Join(inventory.Root, "tokenizer_config.json"), `{"`+key+`":{}}`)
			if _, err := Compile(r, math.MaxUint64, math.MaxUint64); err == nil || err.Error() != want {
				t.Fatalf("key %s: error = %v, want %q", key, err, want)
			}
		})
	}
	for _, key := range FilePathKeys {
		for name, value := range map[string]string{"absolute": `"/private/vocab.json"`, "unbound": `"absent.json"`, "number": `1`} {
			t.Run(key+" "+name, func(t *testing.T) {
				r := fixture(t)
				inventory := &r.Tokenizer.Inventory
				inventory.Files["tokenizer_config.json"] = writeFixture(t, filepath.Join(inventory.Root, "tokenizer_config.json"), `{"`+key+`":`+value+`}`)
				if _, err := Compile(r, math.MaxUint64, math.MaxUint64); err == nil || err.Error() != "candidate tokenizer file names must name inventoried files" {
					t.Fatalf("key %s %s: error = %v", key, name, err)
				}
			})
		}
		t.Run(key+" bound", func(t *testing.T) {
			r := fixture(t)
			inventory := &r.Tokenizer.Inventory
			inventory.Files["tokenizer_config.json"] = writeFixture(t, filepath.Join(inventory.Root, "tokenizer_config.json"), `{"`+key+`":"tokenizer_config.json"}`)
			if _, err := Compile(r, math.MaxUint64, math.MaxUint64); err != nil {
				t.Fatalf("key %s with a bound file name: %v", key, err)
			}
		})
	}
}

package llmtraining

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestModelConfigLoaderKeysApplyInSubConfigs(t *testing.T) {
	base := `"model_type":"qwen3_5","architectures":["Qwen3_5ForConditionalGeneration"]`
	for _, tc := range []struct{ name, config, want string }{
		{"text config model redirect", `{` + base + `,"text_config":{"model_type":"qwen3_5_text","attribute_map":{}}}`, "text_config contains attribute_map, which the loader key lists reject"},
		{"deep remote code", `{` + base + `,"text_config":{"model_type":"qwen3_5_text","rope_parameters":{"auto_map":{}}}}`, "text_config contains auto_map"},
		{"remote code in a list", `{` + base + `,"text_config":{"model_type":"qwen3_5_text"},"extra":[{"custom_pipelines":{}}]}`, "top level contains custom_pipelines"},
		{"vision config weight redirect", `{` + base + `,"text_config":{"model_type":"qwen3_5_text"},"vision_config":{"transformers_weights":"x"}}`, "vision_config contains transformers_weights"},
		{"text config file path", `{` + base + `,"text_config":{"model_type":"qwen3_5_text","vocab_file":"missing.json"}}`, "text_config file names must name inventoried files"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := compileWithFile(t, true, "config.json", tc.config); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

// The model configurations of both pinned presets are stored verbatim:
//
//	qwen3.5-0.8b-config/config.json  Qwen/Qwen3.5-0.8B, pin 2fc06364715b967f1860aea9cf38778875588b17,
//	                                 sha256 b90b86f35c8e6925ef74ee04d0e758f0a845c83a42089ad82bbaa948de9b4204
//	qwen3.8-27b-config/config.json   Qwen/Qwen3.8-27B, pin 1d4bf0f2ff6012fd82039f2fa52739d0dd7c60c0,
//	                                 sha256 191e0af232104ed8b65258cf3fb2b842e288008baca7633c11b82a1ac7203aab
//
// They were read from the main branch on 2026-11-02, and their sizes match
// the Hub listing. To recheck, download config.json at each pin and compare
// sha256sum.
func TestCompileAcceptsThePinnedModelConfigurations(t *testing.T) {
	for _, name := range []string{"qwen3.5-0.8b-config/config.json", "qwen3.8-27b-config/config.json"} {
		t.Run(name, func(t *testing.T) {
			if err := compileWithFile(t, true, "config.json", testdataText(name)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestModelSnapshotErrorNamesTheFile(t *testing.T) {
	if err := compileWithFile(t, true, "tokenizer.json", `{}`); err == nil || !strings.Contains(err.Error(), "must not contain tokenizer.json") {
		t.Fatalf("error = %v", err)
	}
	if err := compileWithFile(t, true, "nested/config.json", `{}`); err == nil || !strings.Contains(err.Error(), "must not contain a nested file,") {
		t.Fatalf("nested error = %v", err)
	}
	// Other inventory names are private: errors use a fixed phrase.
	for _, model := range []bool{true, false} {
		err := compileWithFile(t, model, "private-name.json", `{}`)
		if err == nil || !strings.Contains(err.Error(), "must not contain an unlisted file") || strings.Contains(err.Error(), "private-name") {
			t.Fatalf("model=%v error = %v", model, err)
		}
	}
	// Well-known public names are named, and the error lists the allowed files.
	err := compileWithFile(t, false, "adapter_config.json", `{}`)
	if err == nil || !strings.Contains(err.Error(), "tokenizer snapshot must not contain adapter_config.json, only tokenizer_config.json, tokenizer.json") {
		t.Fatalf("tokenizer error = %v", err)
	}
	if got := snapshotEntryName("chat_template.jinja"); got != "chat_template.jinja" {
		t.Fatalf("public name hidden: %q", got)
	}
}

func TestCandidateRejectsPrequantizedWeightsAtAnyDepth(t *testing.T) {
	for _, config := range []string{
		withQuantization(qwenModelConfig, qloraQuantizationFixture),
		strings.Replace(testdataText("qwen3.8-27b-config/config.json"), `"text_config": {`, `"text_config": {"quantization_config":{"quant_method":"fp8"},`, 1),
		strings.Replace(testdataText("qwen3.8-27b-config/config.json"), `"vision_config": {`, `"vision_config": {"extra":[{"quantization_config":{}}],`, 1),
	} {
		if err := compileWithFile(t, true, "config.json", config); err == nil || !strings.Contains(err.Error(), "unquantized") {
			t.Fatalf("error = %v", err)
		}
	}
}

// The verbatim upstream fixtures and the pins they come from. Each file must
// keep its recorded SHA-256. To recheck a changed value, download the file at
// the pin and compare sha256sum.
func TestVerbatimFixturesKeepTheirRecordedDigests(t *testing.T) {
	for _, tc := range []struct{ file, pin, digest string }{
		{"qwen3.5-0.8b-config/config.json", "Qwen/Qwen3.5-0.8B@2fc06364715b967f1860aea9cf38778875588b17", "b90b86f35c8e6925ef74ee04d0e758f0a845c83a42089ad82bbaa948de9b4204"},
		{"qwen3.8-27b-config/config.json", "Qwen/Qwen3.8-27B@1d4bf0f2ff6012fd82039f2fa52739d0dd7c60c0", "191e0af232104ed8b65258cf3fb2b842e288008baca7633c11b82a1ac7203aab"},
		{"qwen3.8-processor/preprocessor_config.json", "Qwen/Qwen3.8-27B@1d4bf0f2ff6012fd82039f2fa52739d0dd7c60c0", "27225450ac9c6529872ee1924fcb0962ff5634834f817040f444118116f4e516"},
		{"qwen3.8-processor/video_preprocessor_config.json", "Qwen/Qwen3.8-27B@1d4bf0f2ff6012fd82039f2fa52739d0dd7c60c0", "7768af27c1fafa9cc9011c1dc20067e03f8915e03b63504550e11d5066986d13"},
	} {
		digest := sha256.Sum256([]byte(testdataText(tc.file)))
		if got := hex.EncodeToString(digest[:]); got != tc.digest {
			t.Errorf("%s has sha256 %s, want %s from %s", tc.file, got, tc.digest, tc.pin)
		}
	}
}

func TestModelSnapshotHoldsOnlyModelFiles(t *testing.T) {
	for _, name := range []string{"preprocessor_config.json", "chat_template.json", "tokenizer_config.json", "nested/config.json"} {
		t.Run(name, func(t *testing.T) {
			if err := compileWithFile(t, true, name, `{}`); err == nil || !strings.Contains(err.Error(), "model snapshot") {
				t.Fatalf("error = %v", err)
			}
		})
	}
	for _, name := range []string{"generation_config.json", "model-00001-of-00002.safetensors", "nested/model.safetensors"} {
		t.Run(name, func(t *testing.T) {
			if err := compileWithFile(t, true, name, `{}`); err != nil {
				t.Fatal(err)
			}
		})
	}
	// A shard index is allowed only at the snapshot root.
	index := `{"weight_map":{"a":"model.safetensors"}}`
	if err := compileWithFile(t, true, "model.safetensors.index.json", index); err != nil {
		t.Fatal("root shard index rejected:", err)
	}
	if err := compileWithFile(t, true, "nested/model.safetensors.index.json", index); err == nil || !strings.Contains(err.Error(), "model snapshot must not contain a nested file") {
		t.Fatalf("nested shard index error = %v", err)
	}
}

func TestArchitectureReadsExactKeys(t *testing.T) {
	// Go's encoding/json matches struct fields without regard to case, but
	// transformers reads keys exactly.
	config := `{"model_type":"qwen3_5","architectures":["Qwen3_5ForConditionalGeneration"],"text_config":{"model_type":"llama","MODEL_TYPE":"qwen3_5_text"}}`
	if err := compileWithFile(t, true, "config.json", config); err == nil || !strings.Contains(err.Error(), "image-text architecture") {
		t.Fatalf("error = %v", err)
	}
}

func TestModelSubConfigsMustNotBeNull(t *testing.T) {
	for _, name := range modelSubConfigs {
		t.Run(name, func(t *testing.T) {
			config := `{"model_type":"qwen3_5","architectures":["Qwen3_5ForConditionalGeneration"],"text_config":{"model_type":"qwen3_5_text"},"vision_config":{}}`
			config = strings.Replace(config, `"`+name+`":{`, `"`+name+`":null,"unused":{`, 1)
			if err := compileWithFile(t, true, "config.json", config); err == nil || !strings.Contains(err.Error(), name+" must be an object") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestModelSubConfigsMustBeObjects(t *testing.T) {
	for _, name := range modelSubConfigs {
		t.Run(name, func(t *testing.T) {
			config := `{"model_type":"qwen3_5","architectures":["Qwen3_5ForConditionalGeneration"],"text_config":{"model_type":"qwen3_5_text"}}`
			config = strings.Replace(config, `"`+name+`":{"model_type":"qwen3_5_text"}`, `"`+name+`":"not an object"`, 1)
			if name == "vision_config" {
				config = strings.TrimSuffix(config, "}") + `,"vision_config":["not an object"]}`
			}
			if err := compileWithFile(t, true, "config.json", config); err == nil || !strings.Contains(err.Error(), name+" must be an object") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

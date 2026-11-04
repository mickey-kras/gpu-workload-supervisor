package llmtraining

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProcessorErrorsCarryNoFileKeys(t *testing.T) {
	parent := "bad\nkey" + strings.Repeat("k", 4096)
	content := strings.Replace(qwenSavedProcessor, `"image_processor": {`, `"image_processor": {"`+strings.ReplaceAll(parent, "\n", `\n`)+`":[{"auto_map":{}}],`, 1)
	err := compileWithFile(t, false, processorConfigName, content)
	if err == nil {
		t.Fatal("nested auto_map accepted")
	}
	message := err.Error()
	if strings.ContainsAny(message, "\n\r") || strings.Contains(message, "kkkk") || len(message) > 300 {
		t.Fatalf("error carries file content: %q", message)
	}
	if !strings.Contains(message, "image_processor contains auto_map") {
		t.Fatalf("error does not name the category and key: %q", message)
	}
}

func TestProcessorFilePathKeysNameInventoriedFiles(t *testing.T) {
	for _, tc := range []struct{ name, file, content, want string }{
		{"top level", imageProcessorName, strings.Replace(qwenImageProcessor, "{", `{"vocab_file":"missing.json",`, 1), "top level file names must name inventoried files"},
		{"nested", processorConfigName, strings.Replace(qwenSavedProcessor, `"video_processor": {`, `"video_processor": {"tokenizer_file":"missing.json",`, 1), "video_processor file names must name inventoried files"},
		{"inventoried", videoProcessorName, strings.Replace(qwenVideoProcessor, "{", `{"vocab_file":"tokenizer.json",`, 1), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := compileWithFile(t, false, tc.file, tc.content)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
	r, _ := completedArtifact(t)
	r.Budget.ExportBytes = 1 << 20
	writeFixture(t, filepath.Join(r.OutputDirectory, processorConfigName), strings.Replace(qwenSavedProcessor, `"image_processor": {`, `"image_processor": {"vocab_file":"missing.json",`, 1))
	if _, err := artifactFiles(r); err == nil || !strings.Contains(err.Error(), "training export processor configuration processor_config.json: image_processor file names") {
		t.Fatalf("export error = %v", err)
	}
}

func TestProcessorFilesRejectAudioTokenizerKeys(t *testing.T) {
	for _, key := range []string{"audio_tokenizer", "audio_tokenizer_class", "audio_tokenizer_name_or_path"} {
		t.Run(key, func(t *testing.T) {
			content := strings.Replace(qwenSavedProcessor, `"video_processor": {`, `"video_processor": {"extra":{"`+key+`":"x"},`, 1)
			if err := compileWithFile(t, false, processorConfigName, content); err == nil || !strings.Contains(err.Error(), "video_processor contains "+key) {
				t.Fatalf("error = %v", err)
			}
		})
	}
	r, _ := completedArtifact(t)
	writeFixture(t, filepath.Join(r.OutputDirectory, "audio_tokenizer_config.json"), `{}`)
	if _, err := artifactFiles(r); err == nil || !strings.Contains(err.Error(), "unsupported training export layout: audio_tokenizer_config.json") {
		t.Fatalf("export error = %v", err)
	}
}

func TestCandidateRejectsUnboundTemplateFiles(t *testing.T) {
	// The unbound-loader check names the template directory. The later
	// allowlist would only say "a nested file".
	for name, want := range map[string]string{
		"chat_template.json":               "must not contain chat_template.json",
		"chat_templates/x.json":            "must not contain chat_templates",
		"additional_chat_templates/x.json": "must not contain additional_chat_templates",
	} {
		t.Run(name, func(t *testing.T) {
			if err := compileWithFile(t, false, name, `{}`); err == nil || !strings.HasSuffix(err.Error(), want) {
				t.Fatalf("error = %v, want suffix %q", err, want)
			}
		})
	}
}

func TestTokenizerSnapshotHoldsOnlyTokenizerAndProcessorFiles(t *testing.T) {
	for _, name := range []string{"audio_tokenizer_config.json", "extra.json", "nested/tokenizer.json"} {
		t.Run(name, func(t *testing.T) {
			if err := compileWithFile(t, false, name, `{}`); err == nil || !strings.Contains(err.Error(), "tokenizer snapshot must not contain") {
				t.Fatalf("error = %v", err)
			}
		})
	}
	for _, name := range []string{"vocab.json", "merges.txt", "special_tokens_map.json", "added_tokens.json", "tokenizer.model", "vocab.txt"} {
		t.Run(name, func(t *testing.T) {
			if err := compileWithFile(t, false, name, `{}`); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestProcessorProblemReportsUndecodedValues(t *testing.T) {
	config := map[string]json.RawMessage{"broken": json.RawMessage(`{`)}
	if got := processorProblem(config, processorConfigName, nil); got != "configuration is invalid" {
		t.Fatalf("problem = %q", got)
	}
}

func TestExportTokenizerAndAdapterConfigsUseTheLoaderKeyLists(t *testing.T) {
	for _, tc := range []struct{ name, file, insert, want string }{
		{"tokenizer remote code", tokenizerConfigName, `"custom_pipelines":{},`, "training export tokenizer_config.json remote-code mappings are unsupported"},
		{"adapter audio tokenizer", adapterConfigName, `"audio_tokenizer_name_or_path":"x",`, "training export adapter_config.json model configuration indirection is unsupported"},
		{"tokenizer file path", tokenizerConfigName, `"vocab_file":"missing.json",`, "training export tokenizer_config.json tokenizer file names must name inventoried files"},
		{"adapter null mapping", adapterConfigName, `"auto_mapping":null,`, ""},
		{"tokenizer bound file path", tokenizerConfigName, `"tokenizer_file":"tokenizer.json",`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := completedArtifact(t)
			path := filepath.Join(r.OutputDirectory, tc.file)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			writeFixture(t, path, strings.Replace(strings.Replace(string(raw), "{", "{"+tc.insert, 1), ",}", "}", 1))
			_, err = artifactFiles(r)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || err.Error() != tc.want {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

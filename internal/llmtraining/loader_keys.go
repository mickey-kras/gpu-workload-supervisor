package llmtraining

import (
	"encoding/json"
	"strings"
)

// Keys in model and tokenizer JSON files that make a loader import code,
// load another model or configuration, or read files by name. The candidate
// inspection applies all four lists to the top level of config.json and
// tokenizer_config.json. The model checks apply the first three lists at any
// depth of every JSON file and FilePathKeys at the top level. The lists are
// not complete: loaders have other keys of this kind, and new loader versions
// can add more.
var (
	// RemoteCodeKeys make a loader import code.
	RemoteCodeKeys = []string{"auto_map", "auto_mapping", "custom_pipelines"}
	// ModelRedirectKeys make a loader load another model or change how it
	// reads the configuration. vLLM loads the verifier model that
	// speculators_config names, and the models that audio_model_id and
	// text_model_id name.
	ModelRedirectKeys = []string{"speculators_config", "configuration_files", "attribute_map", "__dict__", "audio_model_id", "text_model_id"}
	// FileRedirectKeys give other file names for weights or tokenizer files.
	FileRedirectKeys = []string{"fast_tokenizer_files", "transformers_weights"}
	// FilePathKeys name single tokenizer files, including the
	// vocab_files_names entries of the tokenizer classes in transformers.
	FilePathKeys = []string{
		"tokenizer_file", "vocab_file", "merges_file", "spm_file", "added_tokens_file",
		"source_spm", "target_spm", "vocab", "target_vocab_file", "tokenizer_config_file",
		"entity_vocab_file", "monolingual_vocab_file", "emoji_file", "word_shape_file",
		"word_pronunciation_file", "pretrained_vocab_file",
	}
)

// ValidFilePathValues needs each FilePathKeys member of object to be null or
// the bare name of a file in files.
func ValidFilePathValues(object map[string]json.RawMessage, files map[string]string) bool {
	for _, key := range FilePathKeys {
		value, present := object[key]
		if !present || string(value) == "null" {
			continue
		}
		var name string
		if json.Unmarshal(value, &name) != nil || strings.ContainsAny(name, `/\`) || files[name] == "" {
			return false
		}
	}
	return true
}

package llmtraining

// Keys in model and tokenizer JSON files that make a loader import code,
// load another model or configuration, or read files by name. The candidate
// inspection and the model checks share these lists. They are not complete:
// new loader versions can add keys of this kind.
var (
	// RemoteCodeKeys make a loader import code.
	RemoteCodeKeys = []string{"auto_map", "auto_mapping", "custom_pipelines"}
	// ModelRedirectKeys make a loader load another model or change how it
	// reads the configuration. vLLM loads the verifier model that
	// speculators_config names.
	ModelRedirectKeys = []string{"speculators_config", "configuration_files", "attribute_map", "__dict__"}
	// FileRedirectKeys give other file names for weights or tokenizer files.
	FileRedirectKeys = []string{"fast_tokenizer_files", "transformers_weights"}
	// FilePathKeys name single tokenizer files. A safe value is null or the
	// bare name of a bound file.
	FilePathKeys = []string{"tokenizer_file", "vocab_file", "merges_file", "spm_file", "added_tokens_file"}
)

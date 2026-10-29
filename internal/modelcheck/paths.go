package modelcheck

import (
	"encoding/json"
	"path"
	"regexp"
	"strings"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/llmtraining"
)

// textKeys hold token text, never file paths. Strings anywhere below
// chat_template are template text; the chat template check covers them.
// _name_or_path and name_or_path are labels: transformers and vLLM record
// them and do not load files from them.
var textKeys = map[string]bool{
	"content": true, "_name_or_path": true, "name_or_path": true,
	"additional_special_tokens": true, "bos_token": true, "eos_token": true, "unk_token": true,
	"pad_token": true, "sep_token": true, "cls_token": true, "mask_token": true, "extra_special_tokens": true,
}

// templateKey roots a subtree of template text.
const templateKey = "chat_template"

// fileNamePattern matches a bare name of a model, tokenizer or code file.
var fileNamePattern = regexp.MustCompile(`(?i)^[a-z0-9_.-]+\.(json|jinja|model|txt|spm|vocab|bpe|tiktoken|safetensors|bin|pt|pth|ckpt|pkl|py|msgpack|h5|onnx|gguf|npz|tflite)$`)

// checkPathValues closes the class of file path redirects. In every JSON
// file except token data, a string value at any depth must not be an
// absolute path, a home path, a relative path that starts with "./", a path
// with "..", or a backslash path. A bare file name must name a bound file
// that a loader reads. The known file path keys at the top level must be null
// or such a file name. Neither rule applies to tokenizer.json: the tokenizers
// library parses it as token data without file names.
func checkPathValues(name string, raw []byte, digests map[string]string) string {
	if name == "tokenizer.json" {
		return ""
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return ReasonInvalidJSON
	}
	loadable := map[string]string{}
	for file, digest := range digests {
		if !unloadedNames[file] {
			loadable[file] = digest
		}
	}
	if !safeValues("", false, value, loadable) {
		return ReasonFilePathRedirect
	}
	if object, ok := value.(map[string]any); ok {
		top := map[string]json.RawMessage{}
		for key, member := range object {
			encoded, _ := json.Marshal(member)
			top[key] = encoded
		}
		if !llmtraining.ValidFilePathValues(top, loadable) {
			return ReasonFilePathRedirect
		}
	}
	return ""
}

func safeValues(key string, template bool, value any, digests map[string]string) bool {
	template = template || key == templateKey
	switch v := value.(type) {
	case map[string]any:
		for member, child := range v {
			if !safeValues(member, template, child, digests) {
				return false
			}
		}
	case []any:
		for _, child := range v {
			if !safeValues(key, template, child, digests) {
				return false
			}
		}
	case string:
		return template || textKeys[key] || safePathValue(v, digests)
	}
	return true
}

func safePathValue(value string, digests map[string]string) bool {
	switch {
	case unloadedNames[value]:
		return false
	case strings.HasPrefix(value, "/"), strings.HasPrefix(value, "~"), strings.HasPrefix(value, "./"),
		strings.HasPrefix(value, `\`), strings.Contains(value, ".."):
		return false
	case fileNamePattern.MatchString(value):
		return digests[value] != ""
	case strings.Contains(value, "/") && fileNamePattern.MatchString(path.Base(value)):
		// A relative path to a file in a folder: the model has no folders.
		return false
	}
	return true
}

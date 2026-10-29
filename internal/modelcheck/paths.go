package modelcheck

import (
	"encoding/json"
	"path"
	"regexp"
	"strings"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/llmtraining"
)

// textKeys hold text or token strings, never file paths. The chat template
// check covers chat_template. _name_or_path and name_or_path hold a hub ID or
// the folder that saved the model; with the hub offline, no loader reads it.
var textKeys = map[string]bool{
	"chat_template": true, "content": true, "_name_or_path": true, "name_or_path": true,
	"additional_special_tokens": true, "bos_token": true, "eos_token": true, "unk_token": true,
	"pad_token": true, "sep_token": true, "cls_token": true, "mask_token": true, "extra_special_tokens": true,
}

// fileNamePattern matches a bare name of a model, tokenizer or code file.
var fileNamePattern = regexp.MustCompile(`(?i)^[a-z0-9_.-]+\.(json|jinja|model|txt|spm|vocab|bpe|tiktoken|safetensors|bin|pt|pth|ckpt|pkl|py|msgpack|h5|onnx|gguf|npz|tflite)$`)

// checkPathValues closes the class of file path redirects. In every JSON
// file except token data, a string value at any depth must not be an
// absolute path, a home path, a relative path that starts with "./", a path
// with "..", or a backslash path. A bare file name must name a bound file. The
// known file path keys at the top level must be null or a bound file name.
// tokenizer.json is token data that the tokenizers library parses without
// file names, so the rule does not apply to it.
func checkPathValues(name string, raw []byte, digests map[string]string) string {
	if name == "tokenizer.json" {
		return ""
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return ReasonInvalidJSON
	}
	if !safeValues("", value, digests) {
		return ReasonFilePathRedirect
	}
	if object, ok := value.(map[string]any); ok {
		top := map[string]json.RawMessage{}
		for key, member := range object {
			encoded, _ := json.Marshal(member)
			top[key] = encoded
		}
		if !llmtraining.ValidFilePathValues(top, digests) {
			return ReasonFilePathRedirect
		}
	}
	return ""
}

func safeValues(key string, value any, digests map[string]string) bool {
	switch v := value.(type) {
	case map[string]any:
		for member, child := range v {
			if !safeValues(member, child, digests) {
				return false
			}
		}
	case []any:
		for _, child := range v {
			if !safeValues(key, child, digests) {
				return false
			}
		}
	case string:
		return textKeys[key] || safePathValue(v, digests)
	}
	return true
}

func safePathValue(value string, digests map[string]string) bool {
	switch {
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

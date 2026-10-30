package modelcheck

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/llmtraining"
)

// textKeys hold token text or labels, never file paths or model IDs. Strings
// anywhere below chat_template are template text; the chat template check
// covers them. _name_or_path and name_or_path are labels: transformers and
// vLLM record them and do not load files or models from them.
var textKeys = map[string]bool{
	"content": true, "_name_or_path": true, "name_or_path": true,
	"additional_special_tokens": true, "bos_token": true, "eos_token": true, "unk_token": true,
	"pad_token": true, "sep_token": true, "cls_token": true, "mask_token": true, "extra_special_tokens": true,
}

// templateKey roots a subtree of template text.
const templateKey = "chat_template"

// tokenMapFiles are token maps: their keys are tokens. No key or value rule
// applies to them.
var tokenMapFiles = map[string]bool{"vocab.json": true, "added_tokens.json": true}

// tokenDataKeys hold token maps or lists inside a JSON file, for example
// model.vocab, model.merges and the special tokens of tokenizer.json. Below
// the top level, no key or value rule applies to their values.
var tokenDataKeys = map[string]bool{"vocab": true, "merges": true, "special_tokens": true, "added_tokens_decoder": true}

var (
	// fileNamePattern matches a bare name of a model, tokenizer or code file.
	fileNamePattern = regexp.MustCompile(`(?i)^[a-z0-9_.-]+\.(json|jinja|model|txt|spm|vocab|bpe|tiktoken|safetensors|bin|pt|pth|ckpt|pkl|py|msgpack|h5|onnx|gguf|npz|tflite)$`)
	// repoIDPattern matches the shape of a hub repository ID such as org/name.
	repoIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*/[A-Za-z0-9][A-Za-z0-9_.-]*$`)
)

// maxJSONDepth matches the nesting limit of encoding/json.
const maxJSONDepth = 10000

// errStop ends a scan that found its reason.
var errStop = errors.New("scan stopped")

// jsonScan reads one JSON value as a stream of tokens. It rejects duplicate
// keys and trailing data, and it applies the key and value rules without
// building a tree of the value.
type jsonScan struct {
	d          *json.Decoder
	digests    map[string]string
	keyRules   bool
	valueRules bool
	reason     string
}

// scanJSON gives the reason of the first problem in one JSON file, or "".
// The redirect key rule applies outside token data, also in tokenizer.json.
// The value rules do not apply to tokenizer.json, which the tokenizers
// library parses as token data without file names. No rule applies to the
// token maps vocab.json and added_tokens.json.
func scanJSON(name string, raw []byte, digests map[string]string) string {
	loadable := map[string]string{}
	for file, digest := range digests {
		if !unloadedNames[file] {
			loadable[file] = digest
		}
	}
	s := jsonScan{d: json.NewDecoder(bytes.NewReader(raw)), digests: loadable,
		keyRules: !tokenMapFiles[name], valueRules: !tokenMapFiles[name] && name != "tokenizer.json"}
	s.d.UseNumber()
	err := s.value(scope{}, 0)
	if errors.Is(err, errStop) {
		return s.reason
	}
	if err != nil {
		return ReasonInvalidJSON
	}
	if _, err := s.d.Token(); err != io.EOF {
		return ReasonInvalidJSON
	}
	return ""
}

// scope is the context of one value.
type scope struct {
	key      string
	template bool
	data     bool
	pathKey  bool
}

func (s *jsonScan) stop(reason string) error {
	s.reason = reason
	return errStop
}

func (s *jsonScan) value(at scope, depth int) error {
	token, err := s.d.Token()
	if err != nil {
		return err
	}
	switch v := token.(type) {
	case json.Delim:
		if at.pathKey && s.valueRules {
			return s.stop(ReasonFilePathRedirect)
		}
		if depth >= maxJSONDepth {
			return errors.New("json exceeds maximum nesting depth")
		}
		if v == '{' {
			return s.object(at, depth+1)
		}
		return s.array(at, depth+1)
	case string:
		if reason := s.stringReason(at, v); reason != "" {
			return s.stop(reason)
		}
	case nil:
	default:
		if at.pathKey && s.valueRules {
			return s.stop(ReasonFilePathRedirect)
		}
	}
	return nil
}

func (s *jsonScan) object(at scope, depth int) error {
	seen := map[string]bool{}
	for s.d.More() {
		token, err := s.d.Token()
		if err != nil {
			return err
		}
		key, ok := token.(string)
		if !ok || seen[key] {
			return errors.New("duplicate JSON key")
		}
		seen[key] = true
		child := scope{key: key, template: at.template || key == templateKey, data: at.data || (depth > 1 && tokenDataKeys[key])}
		child.pathKey = depth == 1 && slices.Contains(llmtraining.FilePathKeys, key)
		if reason := redirectKeys[key]; reason != "" && s.keyRules && !child.data {
			return s.stop(reason)
		}
		if err := s.value(child, depth); err != nil {
			return err
		}
	}
	_, err := s.d.Token()
	return err
}

func (s *jsonScan) array(at scope, depth int) error {
	for s.d.More() {
		if err := s.value(at, depth); err != nil {
			return err
		}
	}
	_, err := s.d.Token()
	return err
}

// stringReason applies the value rules to one string.
func (s *jsonScan) stringReason(at scope, value string) string {
	switch {
	case !s.valueRules:
		return ""
	case at.pathKey:
		if strings.ContainsAny(value, `/\`) || unloadedNames[value] || s.digests[value] == "" {
			return ReasonFilePathRedirect
		}
		return ""
	case at.template || at.data || textKeys[at.key]:
		return ""
	}
	return valueReason(value, s.digests)
}

// valueReason closes the class of redirects in string values. A value fails
// when it contains "..", starts with "/", "~", "./" or a backslash, names an
// unloaded or unbound file, names a file in a folder, or has the shape of a
// hub repository ID or a URL.
func valueReason(value string, digests map[string]string) string {
	switch {
	case unloadedNames[value],
		strings.HasPrefix(value, "/"), strings.HasPrefix(value, "~"), strings.HasPrefix(value, "./"),
		strings.HasPrefix(value, `\`), strings.Contains(value, ".."):
		return ReasonFilePathRedirect
	case fileNamePattern.MatchString(value):
		if digests[value] == "" {
			return ReasonFilePathRedirect
		}
		return ""
	case strings.Contains(value, "/") && fileNamePattern.MatchString(path.Base(value)):
		return ReasonFilePathRedirect
	case strings.Contains(value, "://"), repoIDPattern.MatchString(value):
		return ReasonModelRedirect
	}
	return ""
}

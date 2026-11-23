package modelcheck

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"strings"
)

// MaxTemplateBytes bounds the chat template text. The templates of common
// open models are below 32 KiB.
const MaxTemplateBytes = 256 << 10

// Reasons of the chat template check. They name the kind of problem, never
// a path or template text.
const (
	ReasonNoTemplate       = "no-chat-template"
	ReasonNamedTemplates   = "named-chat-templates"
	ReasonTemplateTooLarge = "chat-template-too-large"
	ReasonTemplateInvalid  = "chat-template-invalid"
	ReasonTemplateConflict = "chat-template-conflict"
)

const tokenizerConfigFile = "tokenizer_config.json"

// templateFiles hold the chat template, in the order that the loaders
// prefer them. JSON files hold it in the top-level key chat_template.
var templateFiles = []string{"chat_template.jinja", "chat_template.json", tokenizerConfigFile, "processor_config.json"}

// templateResult is the result of the chat template check: the pin of
// the template text and the first file that holds it.
type templateResult struct {
	pin  string
	file string
}

// checkTemplate reads the one chat template of the model and gives its pin,
// or the reason of the first problem. The check does not read the template
// structure: vLLM renders templates in a sandboxed Jinja environment.
func checkTemplate(ctx context.Context, root string, digests map[string]string) (templateResult, string, error) {
	text, file, reason, err := chatTemplate(ctx, root, digests)
	if err != nil || reason != "" {
		return templateResult{}, reason, err
	}
	if len(text) > MaxTemplateBytes {
		return templateResult{}, ReasonTemplateTooLarge, nil
	}
	return templateResult{pin: PinTemplate(text), file: file}, "", nil
}

// PinTemplate gives the SHA-256 of the template text as UTF-8 bytes. A
// file with the same text has the same SHA-256.
func PinTemplate(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// chatTemplate reads the one chat template of the model and gives the
// name of the first file that holds it. Each file that holds a template
// must hold the same text. The bytes that it reads must still match the
// inventory digests.
func chatTemplate(ctx context.Context, root string, digests map[string]string) (string, string, string, error) {
	var texts, names []string
	for _, name := range templateFiles {
		if digests[name] == "" {
			continue
		}
		limit, tooLarge := limitOf(name)
		raw, reason, err := readBound(ctx, filepath.Join(root, name), digests[name], limit, tooLarge)
		if err != nil || reason != "" {
			return "", "", reason, err
		}
		text, ok, reason := templateText(name, raw)
		if reason != "" {
			return "", "", reason, nil
		}
		if ok {
			texts, names = append(texts, text), append(names, name)
		}
	}
	if len(texts) == 0 {
		return "", "", ReasonNoTemplate, nil
	}
	for _, text := range texts[1:] {
		if text != texts[0] {
			return "", "", ReasonTemplateConflict, nil
		}
	}
	return texts[0], names[0], "", nil
}

// limitOf gives the read limit of a template or tokenizer file, and the
// reason of a larger file.
func limitOf(name string) (int64, string) {
	if strings.HasSuffix(name, ".jinja") {
		return MaxTemplateBytes, ReasonTemplateTooLarge
	}
	return MaxJSONFileBytes, ReasonJSONTooLarge
}

// templateText gives the template of one file and true, or false when the
// file holds none. chat_template.json must hold one.
func templateText(name string, raw []byte) (string, bool, string) {
	if strings.HasSuffix(name, ".jinja") {
		if len(raw) == 0 {
			return "", false, ReasonTemplateInvalid
		}
		return string(raw), true, ""
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return "", false, ReasonTemplateInvalid
	}
	value, ok := fields[templateKey]
	if !ok || string(value) == "null" {
		if name == "chat_template.json" {
			return "", false, ReasonTemplateInvalid
		}
		return "", false, ""
	}
	var text string
	if json.Unmarshal(value, &text) == nil {
		if text == "" {
			return "", false, ReasonTemplateInvalid
		}
		return text, true, ""
	}
	var list []json.RawMessage
	if json.Unmarshal(value, &list) == nil {
		return "", false, ReasonNamedTemplates
	}
	return "", false, ReasonTemplateInvalid
}

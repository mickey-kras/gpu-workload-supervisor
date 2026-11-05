package modelcheck

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

// MaxTemplateBytes bounds the chat template text. The templates of common
// open models are below 32 KiB.
const MaxTemplateBytes = 256 << 10

// Template bounds. maxTemplateDepth bounds the nesting of template blocks.
// maxTagTokens bounds the tokens of one tag.
const (
	maxTemplateDepth = 64
	maxTagTokens     = 2048
)

// Reasons of the chat template check. They name the kind of problem, never
// a path or template text.
const (
	ReasonNoTemplate         = "no-chat-template"
	ReasonNamedTemplates     = "named-chat-templates"
	ReasonTemplateTooLarge   = "chat-template-too-large"
	ReasonTemplateTooComplex = "chat-template-too-complex"
	ReasonTemplateInvalid    = "chat-template-invalid"
	ReasonTemplateConflict   = "chat-template-conflict"
	ReasonTemplateSyntax     = "chat-template-syntax"
	ReasonTemplateHiddenText = "chat-template-hidden-text"
	ReasonTemplateStatement  = "chat-template-statement"
	ReasonTemplateUnsafeName = "chat-template-unsafe-name"
	// ReasonTemplateUnknownSource marks a construct outside the supported
	// subset that can bring in text from another source.
	ReasonTemplateUnknownSource = "chat-template-unknown-source"
	// ReasonTemplateTooManyLiterals marks a review list above
	// maxReviewItems.
	ReasonTemplateTooManyLiterals = "chat-template-too-many-literals"
	// ReasonTemplateReviewTooLarge marks a review list above
	// maxReviewBytes as JSON.
	ReasonTemplateReviewTooLarge = "chat-template-review-too-large"
	// ReasonTemplateCheckFailed marks a failure of the checker itself, not
	// a problem in the template.
	ReasonTemplateCheckFailed = "chat-template-check-failed"
	// ReasonTemplateNeedsReview marks a template with literals that no
	// operator approved.
	ReasonTemplateNeedsReview = "chat-template-needs-review"
	// ReasonTemplateApprovalFailed marks an approver that failed, or that
	// gave an approval that is not valid or is for another template or
	// review list.
	ReasonTemplateApprovalFailed = "chat-template-approval-failed"
)

// templateFiles hold the chat template, in the order that the loaders
// prefer them. JSON files hold it in the top-level key chat_template.
var templateFiles = []string{"chat_template.jinja", "chat_template.json", "tokenizer_config.json", "processor_config.json"}

// templateResult is the result of the structural template check: the pin
// of the template text and the literals that an operator must read.
type templateResult struct {
	pin    string
	file   string
	review []ReviewItem
}

// checkTemplate runs the structural chat template check. It gives the pin
// and the review list, or the reason of the first problem.
func checkTemplate(ctx context.Context, root string, digests map[string]string) (templateResult, string, error) {
	text, file, reason, err := chatTemplate(ctx, root, digests)
	if err != nil || reason != "" {
		return templateResult{}, reason, err
	}
	tokens, reason, err := specialTokens(ctx, root, digests)
	if err != nil || reason != "" {
		return templateResult{}, reason, err
	}
	review, reason := scanTemplate(ctx, text, tokens.special, tokens.variables)
	if err := ctx.Err(); err != nil {
		return templateResult{}, "", err
	}
	if reason != "" {
		return templateResult{}, reason, nil
	}
	return templateResult{pin: PinTemplate(text), file: file, review: review}, "", nil
}

// scanHook runs at the start of each template scan. Tests replace it to
// check that a panic in the scan fails closed.
var scanHook = func() {}

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

// specialTokenFiles define the special and added tokens of the tokenizer.
var specialTokenFiles = []string{"tokenizer_config.json", "special_tokens_map.json", "tokenizer.json"}

// specialTokenKeys hold one special token, as a string or as an object
// with the key content.
var specialTokenKeys = []string{"bos_token", "eos_token", "unk_token", "pad_token", "sep_token", "cls_token", "mask_token"}

// extraTokensKey holds named special tokens, such as image_token. The
// loader gives each name to the template, like eos_token.
const extraTokensKey = "extra_special_tokens"

// tokenizerTokens are the special tokens of a tokenizer. special are the
// tokens that can pass a literal without review. variables are the values
// of the special token names, which the loader gives the template. Each
// special token name of the tokenizer has an entry, also without a value.
type tokenizerTokens struct {
	special   map[string]bool
	variables map[string][]string
}

// specialTokens reads the special tokens of the tokenizer that can pass a
// literal without review: the special token keys, additional_special_tokens
// and extra_special_tokens of tokenizer_config.json and
// special_tokens_map.json, and the added tokens with "special": true. It
// keeps only tokens with the shape of markup. A non-special added token is
// text like any other. Keys must match exactly. It also reads each value of
// the special token names.
func specialTokens(ctx context.Context, root string, digests map[string]string) (tokenizerTokens, string, error) {
	tokens := tokenizerTokens{special: map[string]bool{}, variables: map[string][]string{}}
	for _, name := range specialTokenFiles {
		if digests[name] == "" {
			continue
		}
		raw, reason, err := readBound(ctx, filepath.Join(root, name), digests[name], MaxJSONFileBytes, ReasonJSONTooLarge)
		if err != nil || reason != "" {
			return tokenizerTokens{}, reason, err
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil || fields == nil {
			return tokenizerTokens{}, ReasonInvalidJSON, nil
		}
		tokens.add(name, fields)
	}
	return tokens, "", nil
}

// markupToken matches a special token with the shape of markup: <|...|>,
// <...> or [...], without ASCII whitespace. In Go, \s matches only ASCII
// whitespace, so markup also checks for other spaces.
var markupToken = regexp.MustCompile(`^(?:<\|[^\s]+\|>|<[^\s<>]+>|\[[^\s\[\]]+\])$`)

// markup tells whether a special token has the shape of markup and no
// space of any script, such as a no-break space. A token with spaces can
// hold a readable instruction, so it does not pass without review.
func markup(token string) bool {
	return len(token) <= maxMarkupToken && markupToken.MatchString(token) &&
		!strings.ContainsFunc(token, func(r rune) bool { return unicode.IsSpace(r) || unicode.Is(unicode.Zs, r) })
}

// maxMarkupToken bounds the length of a special token that passes a
// literal without review.
const maxMarkupToken = 32

// add adds the tokens of one tokenizer file. tokenizer.json gives only
// its added tokens. tokenizer_config.json gives its special token names
// and the added tokens of added_tokens_decoder. special_tokens_map.json
// gives its special token names.
func (t tokenizerTokens) add(name string, fields map[string]json.RawMessage) {
	switch name {
	case "tokenizer.json":
		t.addEntries(listOf(fields["added_tokens"]))
	case "tokenizer_config.json":
		t.addNamed(fields)
		var decoder map[string]json.RawMessage
		_ = json.Unmarshal(fields["added_tokens_decoder"], &decoder)
		t.addEntries(slices.Collect(maps.Values(decoder)))
	case "special_tokens_map.json":
		t.addNamed(fields)
	}
}

// addNamed adds the special token names of a file and their values.
func (t tokenizerTokens) addNamed(fields map[string]json.RawMessage) {
	for key, values := range namedTokens(fields) {
		t.variables[key] = append(t.variables[key], tokenValues(values)...)
		for _, value := range values {
			t.addSpecial(tokenText(value))
		}
	}
}

// addEntries adds the added tokens that are marked special.
func (t tokenizerTokens) addEntries(entries []json.RawMessage) {
	for _, entry := range entries {
		if specialEntry(entry) {
			t.addSpecial(tokenText(entry))
		}
	}
}

// addSpecial adds a special token with the shape of markup.
func (t tokenizerTokens) addSpecial(token string) {
	if markup(token) {
		t.special[token] = true
	}
}

// namedTokens gives the values of each special token name of a tokenizer
// file: the special token keys, additional_special_tokens and
// extra_special_tokens. Each key of an extra_special_tokens object is also
// a name. A key without a value gives an empty list.
func namedTokens(fields map[string]json.RawMessage) map[string][]json.RawMessage {
	named := map[string][]json.RawMessage{}
	for _, key := range specialTokenKeys {
		if value, ok := fields[key]; ok {
			named[key] = []json.RawMessage{value}
		}
	}
	if value, ok := fields["additional_special_tokens"]; ok {
		named["additional_special_tokens"] = listOf(value)
	}
	extra, ok := fields[extraTokensKey]
	if !ok {
		return named
	}
	var object map[string]json.RawMessage
	var list []json.RawMessage
	switch {
	case json.Unmarshal(extra, &object) == nil && object != nil:
		// Sorted keys give the same order on each run, so the review
		// SHA-256 stays the same.
		for _, key := range slices.Sorted(maps.Keys(object)) {
			named[key] = append(named[key], object[key])
			named[extraTokensKey] = append(named[extraTokensKey], object[key])
		}
	case json.Unmarshal(extra, &list) == nil:
		named[extraTokensKey] = list
	default:
		named[extraTokensKey] = []json.RawMessage{extra}
	}
	return named
}

// tokenValues gives the text of each token value that is not null or
// empty. A value that is not a token gives its JSON text, so that it still
// needs review.
func tokenValues(values []json.RawMessage) []string {
	texts := []string{}
	for _, value := range values {
		if text := tokenText(value); text != "" {
			texts = append(texts, text)
			continue
		}
		if string(value) == "null" || string(value) == `""` {
			continue
		}
		var compact bytes.Buffer
		if json.Compact(&compact, value) == nil {
			texts = append(texts, compact.String())
		}
	}
	return texts
}

func listOf(raw json.RawMessage) []json.RawMessage {
	var list []json.RawMessage
	_ = json.Unmarshal(raw, &list)
	return list
}

// specialEntry tells whether an added token entry has "special": true.
func specialEntry(raw json.RawMessage) bool {
	var entry map[string]json.RawMessage
	return json.Unmarshal(raw, &entry) == nil && string(entry["special"]) == "true"
}

// tokenText gives the text of one token: a string, or an object with the
// exact key content. Else it gives "".
func tokenText(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) == nil && json.Unmarshal(object["content"], &text) == nil {
		return text
	}
	return ""
}

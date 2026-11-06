package modelcheck

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// roleWords are the message roles. A literal can hold them without review.
var roleWords = map[string]bool{"system": true, "user": true, "assistant": true, "tool": true, "ipython": true, "developer": true}

// autoPass tells whether a literal needs no review: it is made only of
// special tokens of the tokenizer, role words, and the ASCII characters of
// plainRune, or only of qwenTags and these ASCII characters. Each other
// character needs review, also a symbol, a mark or a letter of another
// script.
func (s *templateScan) autoPass(text string) bool {
	if qwenTagsOnly(text) {
		return true
	}
	for text != "" {
		if token := s.longestToken(text); token != "" {
			text = text[len(token):]
			continue
		}
		r, size := utf8.DecodeRuneInString(text)
		if plainRune(r) {
			text = text[size:]
			continue
		}
		word := leadingWord(text)
		if !roleWords[word] {
			return false
		}
		text = text[len(word):]
	}
	return true
}

// qwenTags are the tags of the Qwen3 templates. Their tokenizers do not
// mark them as special, but they are markup. A literal made only of these
// tags, ASCII whitespace and ASCII punctuation passes without review. The
// tags must match exactly. A similar tag, such as <THINK>, needs review.
var qwenTags = []string{"<think>", "</think>", "<tool_call>", "</tool_call>"}

// qwenTagsOnly tells whether text is made only of qwenTags and the ASCII
// characters of plainRune.
func qwenTagsOnly(text string) bool {
	for text != "" {
		if tag := leadingTag(text); tag != "" {
			text = text[len(tag):]
			continue
		}
		r, size := utf8.DecodeRuneInString(text)
		if !plainRune(r) {
			return false
		}
		text = text[size:]
	}
	return true
}

// leadingTag gives the tag of qwenTags at the start of text, or "".
func leadingTag(text string) string {
	for _, tag := range qwenTags {
		if strings.HasPrefix(text, tag) {
			return tag
		}
	}
	return ""
}

// longestToken gives the longest special token at the start of text. It
// looks up each prefix of at most maxMarkupToken bytes, so its cost does
// not depend on the number of special tokens. A longer token never passes
// a literal.
func (s *templateScan) longestToken(text string) string {
	for n := min(len(text), maxMarkupToken); n > 0; n-- {
		if s.special[text[:n]] {
			return text[:n]
		}
	}
	return ""
}

// plainRune tells whether r is ASCII whitespace (space, tab, line feed or
// carriage return) or ASCII punctuation: !"#$%&'()*+,-./:;<=>?@[\]^_`{|}~.
func plainRune(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r < utf8.RuneSelf && (unicode.IsPunct(r) || unicode.IsSymbol(r))
}

// leadingWord gives the run of letters and digits at the start of text.
func leadingWord(text string) string {
	for i, r := range text {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			return text[:i]
		}
	}
	return text
}

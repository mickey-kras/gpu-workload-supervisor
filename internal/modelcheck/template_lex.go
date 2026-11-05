package modelcheck

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// This file reads the text of a chat template as Jinja tokens. It never
// runs the template.

// hiddenText finds text that a reader does not see: invalid UTF-8 and the
// replacement character U+FFFD, which often marks text that did not decode,
// control characters other than tab, line feed and a carriage return
// before a line feed, format characters such as zero-width and
// bidirectional controls and tag characters, private use characters, line
// and paragraph separators, and variation selectors. On a terminal, the
// text after a bare carriage return writes over the text before it.
func hiddenText(text string) bool {
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		i += size
		switch {
		case r == '\t', r == '\n', r == '\r' && strings.HasPrefix(text[i:], "\n"):
		case r == utf8.RuneError, unicode.IsControl(r),
			unicode.In(r, unicode.Cf, unicode.Co, unicode.Zl, unicode.Zp, unicode.Variation_Selector):
			return true
		}
	}
	return false
}

// Template token kinds.
const (
	tokName = iota
	tokString
	tokNumber
	tokOperator
)

// templateToken is one token. pair is the index of the matching bracket
// for a bracket token. pos is the byte offset of the token in the
// template.
type templateToken struct {
	kind int
	text string
	pair int
	pos  int
}

func (t templateToken) is(kind int, text string) bool {
	return t.kind == kind && t.text == text
}

// nextTag finds the next "{{", "{%" or "{#" at or after pos.
func nextTag(text string, pos int) int {
	for {
		i := strings.IndexByte(text[pos:], '{')
		if i < 0 || pos+i+1 >= len(text) {
			return -1
		}
		pos += i
		if c := text[pos+1]; c == '{' || c == '%' || c == '#' {
			return pos
		}
		pos++
	}
}

// lexTag reads the tokens of one tag from pos up to its end delimiter. An
// end delimiter inside brackets does not end the tag, as in Jinja.
func lexTag(text string, pos int, block bool) ([]templateToken, int, string) {
	end := "}}"
	if block {
		end = "%}"
	}
	if pos < len(text) && (text[pos] == '-' || text[pos] == '+') {
		pos++
	}
	var tokens []templateToken
	var brackets []int
	for pos = skipSpace(text, pos); pos < len(text); pos = skipSpace(text, pos) {
		if next := tagEnd(text, pos, end); next > 0 && len(brackets) == 0 {
			return tokens, next, ""
		}
		token, next, reason := lexToken(text, pos, len(tokens) > 0 && tokens[len(tokens)-1].is(tokOperator, "."))
		if reason != "" {
			return nil, 0, reason
		}
		token.pos = pos
		if tokens = append(tokens, token); !balance(tokens, &brackets) {
			return nil, 0, ReasonTemplateSyntax
		}
		if pos = next; len(tokens) > maxTagTokens {
			return nil, 0, ReasonTemplateTooComplex
		}
	}
	return nil, 0, ReasonTemplateSyntax
}

// skipSpace gives the first position at or after pos that is not
// whitespace.
func skipSpace(text string, pos int) int {
	for pos < len(text) && strings.IndexByte(" \t\n\r", text[pos]) >= 0 {
		pos++
	}
	return pos
}

// tagEnd gives the position after the end delimiter at pos, with an
// optional "-" or "+" before it, or 0.
func tagEnd(text string, pos int, end string) int {
	if c := text[pos]; c == '-' || c == '+' {
		pos++
	}
	if strings.HasPrefix(text[pos:], end) {
		return pos + len(end)
	}
	return 0
}

// balance tracks the open brackets of a tag for its last token, and pairs
// each closing bracket with its open bracket. It fails on a bracket that
// does not match.
func balance(tokens []templateToken, brackets *[]int) bool {
	last := len(tokens) - 1
	token := tokens[last]
	if token.kind != tokOperator {
		return true
	}
	switch token.text {
	case "(", "[", "{":
		*brackets = append(*brackets, last)
	case ")", "]", "}":
		open := openBrackets[token.text]
		n := len(*brackets)
		if n == 0 || tokens[(*brackets)[n-1]].text != open {
			return false
		}
		tokens[(*brackets)[n-1]].pair, tokens[last].pair = last, (*brackets)[n-1]
		*brackets = (*brackets)[:n-1]
	}
	return true
}

// openBrackets give the open bracket of each closing bracket.
var openBrackets = map[string]string{")": "(", "]": "[", "}": "{"}

var operators = []string{"**", "//", "==", "!=", "<=", ">=", "+", "-", "*", "/", "%", "~", "<", ">", "=", "|", ".", ",", ":", ";", "(", ")", "[", "]", "{", "}"}

// Number patterns of Jinja. A float needs a digit after its "." and
// after its "e". After a "." token, Jinja reads no float: x.0.5 is the
// subscript 0 and then the subscript 5.
var (
	floatNumber   = regexp.MustCompile(`^(?i)(\d+_)*\d+((\.(\d+_)*\d+)?e[+\-]?(\d+_)*\d+|\.(\d+_)*\d+)`)
	integerNumber = regexp.MustCompile(`^(?i)(0b(_?[0-1])+|0o(_?[0-7])+|0x(_?[\da-f])+|[1-9](_?\d)*|0(_?0)*)`)
)

// lexToken reads the token at pos. afterDot tells whether the previous
// token is ".".
func lexToken(text string, pos int, afterDot bool) (templateToken, int, string) {
	c := text[pos]
	switch {
	case nameByte(c):
		end := scanWhile(text, pos, func(b byte) bool { return nameByte(b) || digit(b) })
		return templateToken{kind: tokName, text: text[pos:end]}, end, ""
	case digit(c):
		n := 0
		if !afterDot {
			n = len(floatNumber.FindString(text[pos:]))
		}
		if n == 0 {
			n = len(integerNumber.FindString(text[pos:]))
		}
		return templateToken{kind: tokNumber, text: text[pos : pos+n]}, pos + n, ""
	case c == '\'' || c == '"':
		return lexString(text, pos)
	}
	for _, op := range operators {
		if strings.HasPrefix(text[pos:], op) {
			return templateToken{kind: tokOperator, text: op}, pos + len(op), ""
		}
	}
	return templateToken{}, 0, ReasonTemplateSyntax
}

// nameByte tells whether b can start a name. Names are ASCII.
func nameByte(b byte) bool {
	return b == '_' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

func digit(b byte) bool {
	return b >= '0' && b <= '9'
}

// scanWhile gives the end of the run of bytes after pos that match.
func scanWhile(text string, pos int, match func(byte) bool) int {
	end := pos + 1
	for end < len(text) && match(text[end]) {
		end++
	}
	return end
}

// lexString reads one string literal and decodes its escapes like Jinja,
// which decodes them as Python unicode escapes. The token holds the
// decoded value, so an escape cannot hide text from the rules.
func lexString(text string, pos int) (templateToken, int, string) {
	quote := text[pos]
	var b strings.Builder
	for i := pos + 1; i < len(text); {
		c := text[i]
		switch {
		case c == quote:
			value := b.String()
			if hiddenText(value) {
				return templateToken{}, 0, ReasonTemplateHiddenText
			}
			return templateToken{kind: tokString, text: value}, i + 1, ""
		case c != '\\':
			b.WriteByte(c)
			i++
			continue
		}
		n, reason := unescape(&b, text[i:])
		if reason != "" {
			return templateToken{}, 0, reason
		}
		i += n
	}
	return templateToken{}, 0, ReasonTemplateSyntax
}

var simpleEscapes = map[byte]string{
	'\\': `\`, '\'': `'`, '"': `"`, 'n': "\n", 't': "\t", 'r': "\r",
	'a': "\a", 'b': "\b", 'f': "\f", 'v': "\v", '\n': "",
}

// escapeDigits give the number of hex digits of the \x, \u and \U escapes.
var escapeDigits = map[byte]int{'x': 2, 'u': 4, 'U': 8}

// unescape decodes the escape at the start of s and gives its length. A
// named escape (\N{...}) can give any character, so it is hidden text.
func unescape(b *strings.Builder, s string) (int, string) {
	if len(s) < 2 {
		return 0, ReasonTemplateSyntax
	}
	if v, ok := simpleEscapes[s[1]]; ok {
		b.WriteString(v)
		return 2, ""
	}
	digits := escapeDigits[s[1]]
	switch {
	case s[1] == 'N':
		return 0, ReasonTemplateHiddenText
	case digits > 0:
		if len(s) < 2+digits {
			return 0, ReasonTemplateSyntax
		}
		r, reason := codePoint(s[2:2+digits], 16)
		if reason != "" {
			return 0, reason
		}
		b.WriteRune(r)
		return 2 + digits, ""
	case s[1] >= '0' && s[1] <= '7':
		n := 2
		for n < 4 && n < len(s) && s[n] >= '0' && s[n] <= '7' {
			n++
		}
		r, reason := codePoint(s[1:n], 8)
		if reason != "" {
			return 0, reason
		}
		b.WriteRune(r)
		return n, ""
	case s[1] >= utf8.RuneSelf:
		// Jinja first writes each character that is not ASCII as a
		// backslash escape. The backslash before it escapes the backslash
		// of that escape, so the text is the escape itself.
		r, size := utf8.DecodeRuneInString(s[1:])
		b.WriteString(backslashEscape(r))
		return 1 + size, ""
	}
	b.WriteString(s[:2])
	return 2, ""
}

// backslashEscape gives the Python backslash escape of a character that is
// not ASCII: \xhh, \uhhhh or \Uhhhhhhhh.
func backslashEscape(r rune) string {
	switch {
	case r <= 0xff:
		return fmt.Sprintf(`\x%02x`, r)
	case r <= 0xffff:
		return fmt.Sprintf(`\u%04x`, r)
	}
	return fmt.Sprintf(`\U%08x`, r)
}

// codePoint parses the digits of an escape. A value that is not a valid
// number or is above unicode.MaxRune fails.
func codePoint(digits string, base int) (rune, string) {
	v, err := strconv.ParseUint(digits, base, 32)
	if err != nil || v > unicode.MaxRune {
		return 0, ReasonTemplateSyntax
	}
	return rune(v), ""
}

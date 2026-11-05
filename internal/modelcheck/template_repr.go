package modelcheck

import "strings"

// This file finds the constructs that write the Python text of a value
// that is not text, such as "<built-in method get of dict object>",
// "range(0, 3)", "<generator object ...>" or "True". This text is not a
// literal, request data or a special token, and subscripts, slices and
// filters can take letters out of it. So the subset rejects each of these
// constructs, with chat-template-unknown-source.

// pythonMethods are the method names of the Python values that a
// template can reach: str, list, tuple, dict, its views, int, float,
// Markup, a generator and the loop object. An attribute with one of these
// names that is not called gives the method, and the text of a method
// names it. A loop attribute such as loop.index is not a method.
var pythonMethods = map[string]bool{
	// str
	"capitalize": true, "casefold": true, "center": true, "count": true, "encode": true, "endswith": true,
	"expandtabs": true, "find": true, "format": true, "format_map": true, "index": true, "isalnum": true,
	"isalpha": true, "isascii": true, "isdecimal": true, "isdigit": true, "isidentifier": true, "islower": true,
	"isnumeric": true, "isprintable": true, "isspace": true, "istitle": true, "isupper": true, "join": true,
	"ljust": true, "lower": true, "lstrip": true, "maketrans": true, "partition": true, "removeprefix": true,
	"removesuffix": true, "replace": true, "rfind": true, "rindex": true, "rjust": true, "rpartition": true,
	"rsplit": true, "rstrip": true, "split": true, "splitlines": true, "startswith": true, "strip": true,
	"swapcase": true, "title": true, "translate": true, "upper": true, "zfill": true,
	// list, tuple, dict and dict views
	"append": true, "clear": true, "copy": true, "extend": true, "insert": true, "pop": true, "remove": true,
	"reverse": true, "sort": true, "fromkeys": true, "get": true, "items": true, "keys": true, "popitem": true,
	"setdefault": true, "update": true, "values": true, "isdisjoint": true,
	// int and float
	"as_integer_ratio": true, "bit_count": true, "bit_length": true, "conjugate": true, "from_bytes": true,
	"to_bytes": true, "is_integer": true, "fromhex": true, "hex": true,
	// Markup, a generator and the loop object
	"escape": true, "unescape": true, "striptags": true, "send": true, "throw": true, "close": true,
	"cycle": true, "changed": true,
}

// constants are the Jinja names of True, False and None.
var constants = map[string]bool{"true": true, "false": true, "none": true, "True": true, "False": true, "None": true}

// iteratorFilters give an iterator. Its text is "<generator object ...>".
var iteratorFilters = map[string]bool{
	"batch": true, "map": true, "select": true, "reject": true, "selectattr": true, "rejectattr": true,
	"slice": true, "items": true, "unique": true, "reverse": true,
}

// iteratorMethods give a dict view. Its text is "dict_items([...])".
var iteratorMethods = map[string]bool{"items": true, "keys": true, "values": true}

// consumerFilters turn an iterator into a list, a string of its items or
// one item.
var consumerFilters = map[string]bool{
	"list": true, "join": true, "first": true, "last": true, "length": true, "count": true,
	"sum": true, "min": true, "max": true, "sort": true,
}

// reprSource tells whether tokens[i] writes the Python text of a value that
// is not text. conditions marks the tokens of an inline if condition.
func (s *templateScan) reprSource(tokens []templateToken, i int, conditions []bool) bool {
	t, prev, next := tokens[i], tokenAt(tokens, i-1), tokenAt(tokens, i+1)
	switch {
	case t.kind != tokName || kwarg(tokens, i):
		return false
	case prev.is(tokOperator, "."):
		return attributeRepr(tokens, i)
	case prev.is(tokOperator, "|"):
		return iteratorFilters[t.text] && !consumed(tokens, i)
	case next.is(tokOperator, "(") && (t.text == "namespace" || t.text == "dict"):
		return !wholeSetValue(tokens, i, closing(tokens, i+1))
	case next.is(tokOperator, "(") && t.text == "range":
		return !membership(tokens, i-1) && !consumed(tokens, i)
	case s.namespaces[t.text]:
		return !next.is(tokOperator, ".") && !next.is(tokName, "is") && !(tokens[0].is(tokName, "set") && i == 1)
	case constants[t.text]:
		return !testName(tokens, i) && !quietConstant(tokens, i, conditions)
	}
	return false
}

// attributeRepr tells whether the attribute at tokens[i] gives a method
// that is not called, or a dict view that no filter consumes. A loop
// attribute, such as loop.index, is not a method.
func attributeRepr(tokens []templateToken, i int) bool {
	name, called := tokens[i].text, tokenAt(tokens, i+1).is(tokOperator, "(")
	loopAttribute := tokenAt(tokens, i-2).is(tokName, "loop") && !tokenAt(tokens, i-3).is(tokOperator, ".") && name != "cycle" && name != "changed"
	switch {
	case !called:
		return pythonMethods[name] && !loopAttribute
	case iteratorMethods[name]:
		return !consumed(tokens, i)
	}
	return false
}

// methodKey tells whether tokens[i] reads a key or an attribute that can
// name a method. For a string key, Jinja gives the attribute when the
// value has no such item, so x['title'] on a string gives a method. A
// filter attribute, as in map(attribute='items'), reads each part in the
// same way. tokens[i] is the "[" of a subscript or the "(" of a filter
// call.
func (s *templateScan) methodKey(tokens []templateToken, i int) bool {
	t := tokens[i]
	switch {
	case t.is(tokOperator, "[") && i > 0 && valueEnd(tokens[i-1]):
		return !s.plainKey(tokens, i+1, t.pair, false)
	case t.is(tokOperator, "(") && tokenAt(tokens, i-2).is(tokOperator, "|"):
		for _, value := range attributeValues(tokens, i) {
			if !s.plainKey(tokens, value[0], value[1], true) {
				return true
			}
		}
	}
	return false
}

// plainKey tells whether tokens[start:end] is a key that cannot name a
// method. It is a slice, one string literal that is not the name of a
// method, or a value of only request names, integer loop attributes,
// numbers and operators. In a dotted attribute path, each part counts.
func (s *templateScan) plainKey(tokens []templateToken, start, end int, dotted bool) bool {
	switch {
	case end-start == 1 && tokens[start].kind == tokString:
		return !methodName(tokens[start].text, dotted)
	case indexAtDepth(tokens[start:end], func(t templateToken) bool { return t.is(tokOperator, ":") }) >= 0:
		return true
	}
	for j := start; j < end; j++ {
		if !s.plainKeyToken(tokens, j) {
			return false
		}
	}
	return true
}

// plainKeyToken tells whether tokens[j] can be in a key that is not one
// string literal. A string, a call and a name that is not a request name
// can give the name of a method.
func (s *templateScan) plainKeyToken(tokens []templateToken, j int) bool {
	t, prev := tokens[j], tokenAt(tokens, j-1)
	switch {
	case t.kind == tokString:
		return false
	case t.kind != tokName, prev.is(tokOperator, "."), prev.is(tokOperator, "|"), keywords[t.text], constants[t.text], testName(tokens, j):
		return true
	case t.text == "loop":
		return loopIntegerAttribute(tokens, j)
	}
	return !tokenAt(tokens, j+1).is(tokOperator, "(") && s.requestName(t.text)
}

// methodName tells whether a key names a method. With dotted, each part of
// the path counts.
func methodName(key string, dotted bool) bool {
	if !dotted {
		return pythonMethods[key]
	}
	for part := range strings.SplitSeq(key, ".") {
		if pythonMethods[part] {
			return true
		}
	}
	return false
}

// consumed tells whether the iterator that tokens[i] gives can only be
// iterated or consumed: other iterator filters and then a filter of
// consumerFilters follow it, or it ends the iterable of a for statement,
// or it is in an if or elif statement. tokens[i] is a filter name, a
// method name or range.
func consumed(tokens []templateToken, i int) bool {
	if tokens[0].is(tokName, "if") || tokens[0].is(tokName, "elif") {
		return true
	}
	j := valueAfter(tokens, i)
	for tokenAt(tokens, j).is(tokOperator, "|") && tokenAt(tokens, j+1).kind == tokName {
		name := tokens[j+1].text
		switch {
		case consumerFilters[name]:
			return true
		case !iteratorFilters[name]:
			return false
		}
		j = valueAfter(tokens, j+1)
	}
	return loopIterable(tokens, i, j)
}

// valueAfter gives the index after the name at tokens[i] and its call
// brackets, if any.
func valueAfter(tokens []templateToken, i int) int {
	if tokenAt(tokens, i+1).is(tokOperator, "(") {
		return closing(tokens, i+1) + 1
	}
	return i + 1
}

// loopIterable tells whether tokens[i] is outside brackets in the iterable
// of a for statement, and tokens[end] ends the iterable: the end of the
// tag, if or recursive.
func loopIterable(tokens []templateToken, i, end int) bool {
	in := loopIn(tokens)
	if !tokens[0].is(tokName, "for") || in < 0 || i <= in || enclosing(tokens, i) >= 0 {
		return false
	}
	return end == len(tokens) || tokenAt(tokens, end).is(tokName, "if") || tokenAt(tokens, end).is(tokName, "recursive")
}

// wholeSetValue tells whether tokens[i:end+1] is the whole value of a set
// statement.
func wholeSetValue(tokens []templateToken, i, end int) bool {
	return tokens[0].is(tokName, "set") && i == assignAt(tokens)+1 && end == len(tokens)-1
}

// quietConstant tells whether the constant at tokens[i] cannot become
// text where it is: in an if or elif statement, in an inline if condition,
// as a direct operand of a comparison, as the argument of a test, or as
// the whole value of a set statement or a keyword argument. A value that
// set or a keyword argument gives can reach the output through its name.
// So can a comparison, which gives True or False. The part selection
// rule of template_parts.go stops a template from taking letters out of
// this text.
func quietConstant(tokens []templateToken, i int, conditions []bool) bool {
	switch {
	case tokens[0].is(tokName, "if"), tokens[0].is(tokName, "elif"), conditions[i]:
		return true
	case directOperand(tokens, i), testArgument(tokens, i), wholeSetValue(tokens, i, i):
		return true
	}
	next := tokenAt(tokens, i+1)
	return tokenAt(tokens, i-1).is(tokOperator, "=") && kwarg(tokens, i-2) && (next.is(tokOperator, ",") || next.is(tokOperator, ")"))
}

// inlineConditions marks the tokens of each inline if condition, as in
// x if c else y or the if of a for statement: the tokens after the if and
// before the else, at the same bracket depth.
func inlineConditions(tokens []templateToken) []bool {
	marks := make([]bool, len(tokens))
	levels := []bool{false}
	for i, t := range tokens {
		top := len(levels) - 1
		switch {
		case openBracket(t):
			levels = append(levels, false)
		case closeBracket(t) && top > 0:
			levels = levels[:top]
		case t.is(tokName, "if") && i > 0:
			levels[top] = true
		case t.is(tokName, "else"):
			levels[top] = false
		default:
			marks[i] = levels[top]
		}
	}
	return marks
}

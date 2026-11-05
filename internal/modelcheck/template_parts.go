package modelcheck

import (
	"strings"
	"unicode"
)

// This file checks part selection: a subscript, a slice, x.0, and the
// filters that select a part of a value. On a literal or on the Python
// text of a value, part selection can pick letters that spell a word that
// no operator reads, for example 'erongI'[::-1]. So the subset accepts
// part selection only on request data, with
// chat-template-unknown-source for each other value.
//
// A value is request data when it is a chain that starts at a request
// name and has only steps that keep parts of their input:
//
//   - an attribute, a subscript, a slice or x.0;
//   - a call of a method of partMethods, and get with one argument;
//   - a filter of partKeepers, and batch and slice with one argument.
//
// A request name is a name that the loader gives from the request, such
// as messages, or a name that set or for gives, when each assignment to
// it is request data or a literal without letters and digits.

// partFilters select a part of their input.
var partFilters = map[string]bool{"first": true, "last": true, "min": true, "max": true, "truncate": true, "slice": true, "batch": true}

// testFilters select items with a test. With a test name, they select a
// part of their input.
var testFilters = map[string]bool{"select": true, "reject": true}

// partMethods give parts of their value and add no other text.
var partMethods = map[string]bool{
	"split": true, "rsplit": true, "splitlines": true, "strip": true, "lstrip": true, "rstrip": true,
	"lower": true, "upper": true, "title": true, "capitalize": true, "items": true, "keys": true, "values": true, "get": true,
}

// partKeepers are the filters that give parts of their input and add no
// other text. string and tojson are not in the list: they give the Python
// text or the JSON text of a value.
var partKeepers = map[string]bool{
	"list": true, "trim": true, "lower": true, "upper": true, "title": true, "capitalize": true, "first": true,
	"last": true, "min": true, "max": true, "reverse": true, "sort": true, "unique": true, "select": true,
	"reject": true, "selectattr": true, "rejectattr": true, "items": true, "dictsort": true, "batch": true, "slice": true,
}

// fillArguments are the calls that add their second argument to the
// result: a default of get and a fill value of batch and slice.
var fillArguments = map[string]bool{"get": true, "batch": true, "slice": true}

// notRequest are names that the loader gives, but not from the request:
// loop, the special token names and the special names of a macro. The
// scan also never treats the special token names of the tokenizer as
// request names. It is built from specialTokenKeys and macroVariables, so
// a name added to one of them is never request data.
var notRequest = notRequestNames()

func notRequestNames() map[string]bool {
	names := map[string]bool{"loop": true, "additional_special_tokens": true, extraTokensKey: true}
	for _, key := range specialTokenKeys {
		names[key] = true
	}
	for name := range macroVariables {
		names[name] = true
	}
	return names
}

// assignment is one value that set, for or a macro argument gives to
// names. head is the name at the start of the chain of the value, or ""
// for a literal without letters and digits. request tells whether the
// value can be request data.
type assignment struct {
	targets []string
	head    string
	request bool
}

// requestNames finds the assigned names that hold only request data. It
// starts with all assigned names and removes each name with an
// assignment that is not request data, until no name changes. A name
// that depends on itself stays: each value that it gets comes from
// request data.
func (s *templateScan) requestNames(tags [][]templateToken) {
	var all []assignment
	for _, tokens := range tags {
		all = append(all, s.assignments(tokens)...)
	}
	s.request = map[string]bool{}
	byHead := map[string][]assignment{}
	for _, a := range all {
		for _, name := range a.targets {
			s.request[name] = true
		}
		byHead[a.head] = append(byHead[a.head], a)
	}
	var lost []string
	for _, a := range all {
		if !a.request || a.head != "" && !s.requestName(a.head) {
			lost = append(lost, s.drop(a.targets)...)
		}
	}
	for len(lost) > 0 {
		name := lost[len(lost)-1]
		lost = lost[:len(lost)-1]
		for _, a := range byHead[name] {
			lost = append(lost, s.drop(a.targets)...)
		}
	}
}

// drop marks names as not request data and gives the names that changed.
func (s *templateScan) drop(names []string) []string {
	var changed []string
	for _, name := range names {
		if s.request[name] {
			s.request[name] = false
			changed = append(changed, name)
		}
	}
	return changed
}

// requestName tells whether a name holds only request data. A special
// token name of the tokenizer, such as image_token of
// extra_special_tokens, is not request data.
func (s *templateScan) requestName(name string) bool {
	_, token := s.variables[name]
	if token || notRequest[name] || trustedFunctions[name] || s.macros[name] || s.namespaces[name] || constants[name] || keywords[name] {
		return false
	}
	request, assigned := s.request[name]
	return request || !assigned
}

// assignments gives the assignments of one tag: a set statement, a set
// block, a for statement or the arguments of a macro.
func (s *templateScan) assignments(tokens []templateToken) []assignment {
	if len(tokens) < 2 {
		return nil
	}
	rest := tokens[1:]
	switch tokens[0].text {
	case "set":
		eq := assignAt(rest)
		if eq < 0 {
			return []assignment{{targets: []string{rest[0].text}}}
		}
		return []assignment{s.valueOf(targetNames(rest[:eq]), tokens, eq+2, len(tokens))}
	case "for":
		in := loopIn(rest)
		if in < 0 {
			return nil
		}
		return []assignment{s.valueOf(targetNames(rest[:in]), tokens, in+2, loopEnd(rest, in)+1)}
	case "macro":
		var names []string
		for _, arg := range arguments(tokens, 2) {
			names = append(names, tokens[arg[0]].text)
		}
		return []assignment{{targets: names}}
	}
	return nil
}

// targetNames gives the names that an assignment target binds. A
// namespace attribute, as in ns.x, binds no name.
func targetNames(tokens []templateToken) []string {
	var names []string
	for k, t := range tokens {
		if t.kind == tokName && !tokenAt(tokens, k-1).is(tokOperator, ".") && !tokenAt(tokens, k+1).is(tokOperator, ".") {
			names = append(names, t.text)
		}
	}
	return names
}

// loopEnd gives the end of the iterable of a for statement: the if or
// recursive outside brackets after the in, or the end of the tag.
func loopEnd(rest []templateToken, in int) int {
	if end := indexAtDepth(rest[in+1:], func(t templateToken) bool { return t.is(tokName, "if") || t.is(tokName, "recursive") }); end >= 0 {
		return in + 1 + end
	}
	return len(rest)
}

// valueOf gives the assignment of the value tokens[start:end] to targets.
// The value must be one chain with request steps only, or one literal
// without letters and digits.
func (s *templateScan) valueOf(targets []string, tokens []templateToken, start, end int) assignment {
	a := assignment{targets: targets}
	switch {
	case end-start == 1 && tokens[start].kind == tokString:
		a.request = !hasWordRune(tokens[start].text)
	case end > start:
		head := chainStart(tokens, end-1)
		a.head, a.request = tokens[head].text, head == start && requestChain(tokens, head, end)
	}
	return a
}

// hasWordRune tells whether text has a letter or a digit.
func hasWordRune(text string) bool {
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

// partSource tells whether tokens[i] selects a part of a value that is not
// request data. tokens[i] is the "[" of a subscript or a slice, the "."
// of x.0, or the "|" of a filter of partFilters, of select or reject with
// a test name, or of a filter with an attribute that has an integer part.
func (s *templateScan) partSource(tokens []templateToken, i int) bool {
	if !partSelection(tokens, i) {
		return false
	}
	head := chainStart(tokens, i-1)
	return !requestChain(tokens, head, i) || !s.requestName(tokens[head].text)
}

// partSelection tells whether tokens[i] selects a part of a value. A
// subscript with one string key reads a key, not a part.
func partSelection(tokens []templateToken, i int) bool {
	t, next := tokens[i], tokenAt(tokens, i+1)
	switch {
	case i == 0, t.kind != tokOperator:
		return false
	case t.text == "[":
		return valueEnd(tokens[i-1]) && !(next.kind == tokString && tokenAt(tokens, i+2).is(tokOperator, "]"))
	case t.text == "." && tokenAt(tokens, i+2).is(tokOperator, "("):
		return next.text == "replace" || removalMethods[next.text] && len(arguments(tokens, i+2)) > 0
	case t.text == ".":
		return next.kind == tokNumber
	case t.text == "|" && tokenAt(tokens, i+2).is(tokOperator, "("):
		args := len(arguments(tokens, i+2))
		return partFilters[next.text] || testFilters[next.text] && args > 0 || indexAttribute(tokens, i+2) || removalFilters[next.text] && args > 0 || next.text == "replace"
	case t.text == "|":
		return partFilters[next.text]
	}
	return false
}

// removalMethods remove the characters of their argument from a value:
// strip, lstrip and rstrip from the ends, split and rsplit in the middle.
// Without an argument, they remove only whitespace. replace, as a method
// or a filter, always removes text.
var removalMethods = map[string]bool{"strip": true, "lstrip": true, "rstrip": true, "split": true, "rsplit": true}

// removalFilters remove the characters of their argument: trim with an
// argument removes them from the ends.
var removalFilters = map[string]bool{"trim": true}

// attributePositions give the position of the attribute argument of the
// filters that also take it without the keyword attribute.
var attributePositions = map[string]int{
	"selectattr": 0, "rejectattr": 0, "groupby": 0, "sum": 0, "unique": 1, "min": 1, "max": 1, "join": 1, "sort": 2,
}

// attributeValues gives the [start, end) ranges of the attribute values of
// the filter call at tokens[o]: the value of the keyword attribute, and
// the argument at the position of attributePositions.
func attributeValues(tokens []templateToken, o int) [][2]int {
	position, positional := attributePositions[tokens[o-1].text]
	var values [][2]int
	for n, arg := range arguments(tokens, o) {
		switch keyword := argumentOf(tokens, o, arg[0]).keyword; {
		case keyword == "attribute":
			values = append(values, [2]int{arg[0] + 2, arg[1]})
		case keyword == "" && positional && n == position:
			values = append(values, arg)
		}
	}
	return values
}

// indexAttribute tells whether the filter call at tokens[o] gives an
// attribute with an integer part, such as attribute='0' or 'x.1', or an
// attribute that is not one string literal. Jinja reads an integer part
// as a subscript.
func indexAttribute(tokens []templateToken, o int) bool {
	for _, value := range attributeValues(tokens, o) {
		if value[1]-value[0] != 1 || tokens[value[0]].kind != tokString || integerPart(tokens[value[0]].text) {
			return true
		}
	}
	return false
}

// integerPart tells whether a part of a dotted attribute path can be an
// integer. Jinja reads a part as an integer when str.isdigit() accepts it.
// That includes digits that are not ASCII, such as "٠" and "０". So a part
// with a character that is not ASCII also counts.
func integerPart(path string) bool {
	for part := range strings.SplitSeq(path, ".") {
		if part != "" && strings.Trim(part, "0123456789") == "" || !asciiText(part) {
			return true
		}
	}
	return false
}

// asciiText tells whether text has only ASCII characters.
func asciiText(text string) bool {
	for i := 0; i < len(text); i++ {
		if text[i] >= 0x80 {
			return false
		}
	}
	return true
}

// chainStart gives the index of the first token of the chain of
// attributes, subscripts, calls and filters that ends at tokens[end].
func chainStart(tokens []templateToken, end int) int {
	j := end
	for {
		t := tokens[j]
		if t.kind == tokOperator && (t.text == ")" || t.text == "]") {
			if !valueEnd(tokenAt(tokens, t.pair-1)) {
				return t.pair
			}
			j = t.pair - 1
			continue
		}
		if prev := tokenAt(tokens, j-1); j >= 2 && (prev.is(tokOperator, ".") || prev.is(tokOperator, "|")) {
			j -= 2
			continue
		}
		return j
	}
}

// requestChain tells whether tokens[head:end] is a name that is not
// called, and then only steps that keep parts of their input.
func requestChain(tokens []templateToken, head, end int) bool {
	if tokens[head].kind != tokName || tokenAt(tokens, head+1).is(tokOperator, "(") {
		return false
	}
	for j := head + 1; j < end; {
		next, ok := keptStep(tokens, j)
		if !ok {
			return false
		}
		j = next
	}
	return true
}

// keptStep checks the step at tokens[j] and gives the index after it. An
// attribute, a subscript and x.0 keep parts. A call must be of a method of
// partMethods, and a filter must be in partKeepers. get, batch and slice
// must have at most one argument.
func keptStep(tokens []templateToken, j int) (int, bool) {
	t, name := tokens[j], tokenAt(tokens, j+1)
	switch {
	case t.is(tokOperator, "["):
		return t.pair + 1, true
	case t.is(tokOperator, ".") && !tokenAt(tokens, j+2).is(tokOperator, "("):
		return j + 2, true
	case t.is(tokOperator, ".") && partMethods[name.text], t.is(tokOperator, "|") && partKeepers[name.text]:
		next := j + 2
		if tokenAt(tokens, next).is(tokOperator, "(") {
			if fillArguments[name.text] && len(arguments(tokens, next)) > 1 {
				return 0, false
			}
			next = tokens[next].pair + 1
		}
		return next, true
	}
	return 0, false
}

// loopSource tells whether the iterable of a for statement in tokens is
// not one of these: a request chain on a request name, a call of range,
// or a list, dict or tuple literal. A loop over other values, such as a
// string literal, gives parts of the value, as part selection does.
func (s *templateScan) loopSource(tokens []templateToken) bool {
	rest := tokens[1:]
	in := loopIn(rest)
	if !tokens[0].is(tokName, "for") || in < 0 {
		return false
	}
	return !s.wholeItems(tokens, in+2, loopEnd(rest, in)+1)
}

// loopFilters keep the items of their input whole, group them, as
// groupby does, or give one value for each item, as map does. On a value
// that gives whole items, they give whole items too.
var loopFilters = func() map[string]bool {
	filters := map[string]bool{"groupby": true, "map": true}
	for name := range partKeepers {
		filters[name] = true
	}
	return filters
}()

// wholeItems tells whether iterating tokens[start:end] gives whole values
// that the other rules check: request data, integers or the items of a
// literal collection. Filters of loopFilters can follow.
func (s *templateScan) wholeItems(tokens []templateToken, start, end int) bool {
	end = trimLoopFilters(tokens, start, end)
	switch {
	case start >= end:
		return false
	case indexAtDepth(tokens[start:end], func(t templateToken) bool { return t.is(tokOperator, ",") }) >= 0:
		return true
	case tokens[start].kind == tokOperator && closing(tokens, start) == end-1:
		return s.literalItems(tokens, start, end)
	case tokens[start].is(tokName, "range") && tokenAt(tokens, start+1).is(tokOperator, "(") && closing(tokens, start+1) == end-1:
		return true
	}
	return chainStart(tokens, end-1) == start && requestChain(tokens, start, end) && s.requestName(tokens[start].text)
}

// literalItems tells whether the bracket tokens[start:end] is a list, dict
// or tuple literal, or a value in round brackets that gives whole items.
func (s *templateScan) literalItems(tokens []templateToken, start, end int) bool {
	switch tokens[start].text {
	case "[", "{":
		return true
	case "(":
		return tokens[end-2].is(tokOperator, ",") || s.wholeItems(tokens, start+1, end-1)
	}
	return false
}

// trimLoopFilters gives the end of tokens[start:end] without the filters
// of loopFilters at its end.
func trimLoopFilters(tokens []templateToken, start, end int) int {
	for end-start > 2 {
		name := end - 1
		if tokens[name].is(tokOperator, ")") {
			name = tokens[name].pair - 1
		}
		if name-1 <= start || !tokens[name-1].is(tokOperator, "|") || !loopFilters[tokens[name].text] {
			return end
		}
		end = name - 1
	}
	return end
}

// reorderFilters change the order of the items of their input, or drop
// items. On a string, the items are letters.
var reorderFilters = map[string]bool{"reverse": true, "sort": true, "unique": true}

// reorderSource tells whether tokens[i] is the "|" of a filter of
// reorderFilters on a value that does not give whole items. For example,
// 'tool'|reverse|join gives "loot".
func (s *templateScan) reorderSource(tokens []templateToken, i int) bool {
	if i == 0 || !tokens[i].is(tokOperator, "|") || !reorderFilters[tokenAt(tokens, i+1).text] {
		return false
	}
	return !s.wholeItems(tokens, chainStart(tokens, i-1), i)
}

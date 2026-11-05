package modelcheck

import "slices"

// This file keeps the Python text of a boolean, None and a float out of
// the output. A comparison, a test, not and in give True or False. A float
// literal, /, ** with an exponent that is not an integer literal and the
// float filter give a float, and the text of a float can be "inf", "nan"
// or "1e+20". loop.first and loop.last give True or False. This text is
// not a literal, request data or a special token, and filters such as
// string, title and replace can change it into other words. So these
// values can only be where they cannot become text:
//
//   - in an if or elif statement, or in an inline if condition;
//   - in the value of set, in the iterable of for, or in a default of a
//     macro argument. Then the names that get the value hold a value that
//     is not text, and each use of these names follows the same rule.
//
// A constant, such as true, can also be a direct operand of a comparison,
// the argument of a test, or a keyword argument of a filter that only
// switches a behavior, see controlKeywords. Each other
// place fails with chat-template-unknown-source.

// boolFlows holds the names that can hold a value that is not text. A key
// is a name, "ns.x" for an attribute of a namespace, or "ns.*" when some
// attribute of ns can hold such a value.
type boolFlows struct {
	marked map[string]bool
	edges  map[string][]string
}

// nonTextNames finds the names that can hold a value that is not text. It
// marks the targets of each such value, and then the targets of each
// value that uses a marked name, until no name changes.
func (s *templateScan) nonTextNames(tags [][]templateToken) {
	s.macroArgs = map[string][]string{}
	for _, tokens := range tags {
		s.addMacroArgs(tokens)
	}
	flows := boolFlows{marked: map[string]bool{}, edges: map[string][]string{}}
	var seeds []string
	for _, tokens := range tags {
		seeds = append(seeds, s.addFlows(flows, tokens)...)
	}
	for len(seeds) > 0 {
		key := seeds[len(seeds)-1]
		seeds = append(seeds[:len(seeds)-1], flows.mark(key)...)
	}
	s.nonText = flows.marked
}

// addMacroArgs records the argument names of a macro statement.
func (s *templateScan) addMacroArgs(tokens []templateToken) {
	if !tokens[0].is(tokName, "macro") || !tokenAt(tokens, 2).is(tokOperator, "(") {
		return
	}
	for _, arg := range arguments(tokens, 2) {
		s.macroArgs[tokens[1].text] = append(s.macroArgs[tokens[1].text], tokens[arg[0]].text)
	}
}

// addFlows adds the flows of one tag: from each name that a value uses to
// the keys that get the value. It gives the keys that get a value from a
// source of nonTextSource.
func (s *templateScan) addFlows(flows boolFlows, tokens []templateToken) []string {
	var seeds []string
	conditions := inlineConditions(tokens)
	for i := range tokens {
		source, use := nonTextSource(tokens, i), nameUse(tokens, i)
		if !source && !use {
			continue
		}
		keys, _ := s.nonTextPlace(tokens, i, conditions)
		if source {
			seeds = append(seeds, keys...)
			continue
		}
		for _, from := range useKeys(tokens, i) {
			flows.edges[from] = append(flows.edges[from], keys...)
		}
	}
	return seeds
}

// mark marks key and gives the keys that its value flows to. An attribute
// key also marks "ns.*".
func (f boolFlows) mark(key string) []string {
	if f.marked[key] {
		return nil
	}
	f.marked[key] = true
	next := f.edges[key]
	if base, _, ok := cutAttribute(key); ok {
		next = append(next, base+".*")
	}
	return next
}

// cutAttribute splits "ns.x" into ns and x.
func cutAttribute(key string) (string, string, bool) {
	for i := 0; i < len(key); i++ {
		if key[i] == '.' {
			return key[:i], key[i+1:], key[i+1:] != "*"
		}
	}
	return key, "", false
}

// nonTextValue tells whether tokens[i] gives a value that is not text: a
// source of nonTextSource, or a use of a name that can hold such a value.
func (s *templateScan) nonTextValue(tokens []templateToken, i int) bool {
	if nonTextSource(tokens, i) {
		return true
	}
	if !nameUse(tokens, i) {
		return false
	}
	for _, key := range useKeys(tokens, i) {
		if s.nonText[key] {
			return true
		}
	}
	return false
}

// nonTextSource tells whether tokens[i] makes a boolean, None or a float.
func nonTextSource(tokens []templateToken, i int) bool {
	t, prev := tokens[i], tokenAt(tokens, i-1)
	switch t.kind {
	case tokNumber:
		return !prev.is(tokOperator, ".") && floatNumber.MatchString(t.text)
	case tokString:
		return t.text == "float" && mapFilterName(tokens, i)
	case tokOperator:
		switch t.text {
		case "==", "!=", "<", ">", "<=", ">=", "/":
			return true
		case "**":
			return !integerLiteral(tokenAt(tokens, i+1))
		}
		return false
	case tokName:
		return nonTextName(tokens, i)
	}
	return false
}

// boolMethods are the methods of trustedMethods that give True or False.
var boolMethods = map[string]bool{"startswith": true, "endswith": true}

// nonTextName tells whether the name at tokens[i] makes a boolean, None or
// a float: is, not, the in of a comparison, a constant, the float filter,
// startswith, endswith, loop.first and loop.last.
func nonTextName(tokens []templateToken, i int) bool {
	t, prev := tokens[i], tokenAt(tokens, i-1)
	switch {
	case t.text == "is", t.text == "not", membership(tokens, i):
		return true
	case prev.is(tokOperator, "|"):
		return t.text == "float"
	case prev.is(tokOperator, ".") && tokenAt(tokens, i+1).is(tokOperator, "("):
		return boolMethods[t.text]
	case prev.is(tokOperator, "."):
		return (t.text == "first" || t.text == "last") && tokenAt(tokens, i-2).is(tokName, "loop") && !tokenAt(tokens, i-3).is(tokOperator, ".")
	}
	return constants[t.text] && !testName(tokens, i)
}

// mapFilterName tells whether the string at tokens[i] is the filter name
// of a map call.
func mapFilterName(tokens []templateToken, i int) bool {
	o := enclosing(tokens, i)
	if o < 2 || !tokens[o].is(tokOperator, "(") || !tokens[o-1].is(tokName, "map") || !tokens[o-2].is(tokOperator, "|") {
		return false
	}
	arg := argumentOf(tokens, o, i)
	return arg.keyword == "" && arg.whole
}

// nameUse tells whether the name at tokens[i] reads a variable: it is not
// an attribute, a filter, a test, a keyword, a call or an assignment
// target.
func nameUse(tokens []templateToken, i int) bool {
	t, prev := tokens[i], tokenAt(tokens, i-1)
	switch {
	case t.kind != tokName, keywords[t.text], constants[t.text], prev.is(tokOperator, "."), prev.is(tokOperator, "|"):
		return false
	case kwarg(tokens, i), testName(tokens, i), tokenAt(tokens, i+1).is(tokOperator, "("):
		return false
	}
	return !assignTarget(tokens, i)
}

// assignTarget tells whether tokens[i] is in the target of set or for, or
// is the name of a macro or of a macro argument.
func assignTarget(tokens []templateToken, i int) bool {
	switch tokens[0].text {
	case "set":
		eq := assignAt(tokens)
		return eq < 0 || i < eq
	case "for":
		return i < loopIn(tokens)
	case "macro":
		arg := argumentOf(tokens, 2, i)
		return i == 1 || arg.index >= 0 && i == argumentStart(tokens, 2, arg.index)
	}
	return false
}

// argumentStart gives the index of the first token of argument n of the
// call that opens at tokens[o].
func argumentStart(tokens []templateToken, o, n int) int {
	return arguments(tokens, o)[n][0]
}

// useKeys gives the keys that a use of the name at tokens[i] reads: the
// name, and "ns.x" for an attribute or "ns.*" for the whole value.
func useKeys(tokens []templateToken, i int) []string {
	name := tokens[i].text
	if attr := tokenAt(tokens, i+2); tokenAt(tokens, i+1).is(tokOperator, ".") && attr.kind == tokName {
		return []string{name, name + "." + attr.text}
	}
	return []string{name, name + ".*"}
}

// nonTextPlace tells whether a value that is not text at tokens[i] cannot
// become text there. It gives the keys of the names that get the value.
func (s *templateScan) nonTextPlace(tokens []templateToken, i int, conditions []bool) ([]string, bool) {
	if tokens[0].is(tokName, "if") || tokens[0].is(tokName, "elif") || conditions[i] {
		return nil, true
	}
	if keys, ok := s.macroCallKeys(tokens, i); ok {
		return keys, true
	}
	switch {
	case tokens[0].is(tokName, "set"):
		return setKeys(tokens, i)
	case tokens[0].is(tokName, "for"):
		return loopKeys(tokens, i)
	case tokens[0].is(tokName, "macro"):
		return macroKeys(tokens, i)
	}
	return nil, false
}

// setKeys gives the keys that the value of a set statement at tokens[i]
// flows to. An attribute target gives "ns.x". A keyword argument of the
// namespace call that is the whole value gives "ns.x" for its keyword.
func setKeys(tokens []templateToken, i int) ([]string, bool) {
	eq := assignAt(tokens)
	if eq < 0 || i <= eq {
		return nil, false
	}
	target := tokens[1:eq]
	if len(target) == 3 && target[1].is(tokOperator, ".") {
		return []string{target[0].text + "." + target[2].text}, true
	}
	if len(target) == 1 && tokenAt(tokens, eq+1).is(tokName, "namespace") && tokenAt(tokens, eq+2).is(tokOperator, "(") && closing(tokens, eq+2) == len(tokens)-1 {
		if arg := argumentOf(tokens, eq+2, i); arg.keyword != "" {
			return []string{target[0].text + "." + arg.keyword}, true
		}
	}
	return targetNames(target), true
}

// loopKeys gives the names of a for statement when tokens[i] is in its
// iterable. loop.previtem and loop.nextitem give the items too. The keys
// do not tell one loop from another, so each loop.previtem and
// loop.nextitem of the template then follows the rule.
func loopKeys(tokens []templateToken, i int) ([]string, bool) {
	rest := tokens[1:]
	in := loopIn(rest)
	if in < 0 || i <= in+1 || i > loopEnd(rest, in) {
		return nil, false
	}
	return append(targetNames(rest[:in]), "loop.previtem", "loop.nextitem"), true
}

// macroCallKeys gives the macro argument that gets tokens[i] when
// tokens[i] is in an argument of a call of a macro of the template. A
// keyword that the macro does not declare, or a positional argument after
// the declared ones, gives no key.
func (s *templateScan) macroCallKeys(tokens []templateToken, i int) ([]string, bool) {
	if len(s.macros) == 0 {
		return nil, false
	}
	for o := enclosing(tokens, i); o > 0; o = enclosing(tokens, o) {
		callee, before := tokens[o-1], tokenAt(tokens, o-2)
		if !tokens[o].is(tokOperator, "(") || callee.kind != tokName || !s.macros[callee.text] || before.is(tokOperator, ".") || before.is(tokOperator, "|") {
			continue
		}
		names, arg := s.macroArgs[callee.text], argumentOf(tokens, o, i)
		switch {
		case arg.keyword != "" && slices.Contains(names, arg.keyword):
			return []string{arg.keyword}, true
		case arg.keyword == "" && arg.index >= 0 && arg.index < len(names):
			return []string{names[arg.index]}, true
		}
		return nil, false
	}
	return nil, false
}

// macroKeys gives the macro argument whose default holds tokens[i].
func macroKeys(tokens []templateToken, i int) ([]string, bool) {
	arg := argumentOf(tokens, 2, i)
	if !tokenAt(tokens, 2).is(tokOperator, "(") || arg.index < 0 {
		return nil, false
	}
	return []string{tokens[argumentStart(tokens, 2, arg.index)].text}, true
}

// quietNonText tells whether the value that is not text at tokens[i]
// cannot become text where it is.
func (s *templateScan) quietNonText(tokens []templateToken, i int, conditions []bool) bool {
	if _, ok := s.nonTextPlace(tokens, i, conditions); ok {
		return true
	}
	return constants[tokens[i].text] && (directOperand(tokens, i) || testArgument(tokens, i) || filterKeyword(tokens, i))
}

// controlKeywords are the keyword arguments of filters that only switch
// a behavior and never become a part of the result, such as boolean of
// default or reverse of sort. Others, such as default_value of default, d
// of join, start of sum and default of int, can be the result.
var controlKeywords = map[string]bool{
	"boolean": true, "killwords": true, "break_long_words": true, "break_on_hyphens": true, "first": true,
	"blank": true, "reverse": true, "case_sensitive": true, "indent": true, "ensure_ascii": true, "sort_keys": true,
}

// filterKeyword tells whether tokens[i] is the whole value of a keyword
// argument of controlKeywords of a filter.
func filterKeyword(tokens []templateToken, i int) bool {
	o := enclosing(tokens, i)
	if o < 2 || !tokens[o].is(tokOperator, "(") || !tokens[o-2].is(tokOperator, "|") {
		return false
	}
	arg := argumentOf(tokens, o, i)
	return controlKeywords[arg.keyword] && arg.whole
}

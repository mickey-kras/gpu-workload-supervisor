package modelcheck

import "regexp"

// This file decides which literals of a chat template need review. It
// fails closed: each literal needs review, except in a few positions where
// it cannot reach the output:
//
//   - a subscript key, x['k'];
//   - the first argument of get, x.get('k');
//   - a direct operand of ==, !=, in or not in, also an item of a list
//     that is such an operand, and the argument of a test;
//   - the attribute and test arguments of selectattr and rejectattr, and
//     the attribute of map and groupby;
//   - the message of raise_exception;
//   - a strftime_now format of only % directives, ASCII whitespace and
//     ASCII punctuation.
//
// The keyword names of dict and namespace and the attribute names that set
// gives to a namespace can reach the output, so they need review too.

// dateFormat matches a strftime format of only % directives, ASCII
// whitespace and ASCII punctuation, see plainRune.
var dateFormat = regexp.MustCompile("^(?:%[A-Za-z%]|[\t\n\r !-$&-/:-@\\[-`{-~])*$")

// operandStops can stand next to a direct operand of a comparison.
var operandStops = map[string]bool{"(": true, "[": true, ",": true, ")": true, "]": true, "=": true}

// operandKeywords can stand next to a direct operand of a comparison.
var operandKeywords = map[string]bool{"if": true, "elif": true, "else": true, "and": true, "or": true, "not": true}

// selectorFilters take an attribute or a test name as their first
// arguments. For map and groupby, only the attribute is quiet.
var selectorFilters = map[string]bool{"selectattr": true, "rejectattr": true}

// tagUses records the literals and the names of one tag that need review,
// and the values of the special token names that the tag uses.
func (s *templateScan) tagUses(tokens []templateToken) string {
	for k, t := range tokens {
		switch {
		case t.kind == tokString && !quiet(tokens, k):
			s.add(t.pos, t.text)
		case t.kind == tokName && outputName(tokens, k):
			s.add(t.pos, t.text)
		case t.kind == tokName && !tokenAt(tokens, k-1).is(tokOperator, ".") && !kwarg(tokens, k):
			if reason := s.variableUses(t); reason != "" {
				return reason
			}
		}
	}
	return ""
}

// maxSpecialValue bounds the value of a special token name that a template
// uses.
const maxSpecialValue = 256

// specialUses are the values of one special token name that need review,
// or the reason that the name fails.
type specialUses struct {
	values []string
	reason string
}

// variableUses records the values of the special token name t, such as
// eos_token. The loader gives the template these values. A value passes
// without review by the rule of a literal: it must be a special token with
// the shape of markup. A name can have many values, so the scan checks
// them once for each name.
func (s *templateScan) variableUses(t templateToken) string {
	uses, ok := s.specials[t.text]
	if !ok {
		uses = s.specialValues(s.variables[t.text])
		s.specials[t.text] = uses
	}
	for _, value := range uses.values {
		// add keeps no more items here too, but the break also stops the
		// loop, so the work stays bounded for a name with many values.
		if s.count > maxReviewItems {
			break
		}
		s.add(t.pos, value)
	}
	return uses.reason
}

// specialValues gives the values that need review, or the reason of the
// first value that fails.
func (s *templateScan) specialValues(values []string) specialUses {
	var uses specialUses
	for _, value := range values {
		switch {
		case hiddenText(value):
			return specialUses{reason: ReasonTemplateHiddenText}
		case len(value) > maxSpecialValue:
			return specialUses{reason: ReasonTemplateTooComplex}
		case value != "" && !s.autoPass(value):
			uses.values = append(uses.values, value)
		}
	}
	return uses
}

// reviewItemBytes is the size as JSON of a review item with an empty text
// and a line of one digit, with the comma before it.
const reviewItemBytes = len(`,{"line":1,"text":""}`)

// add records a literal that can reach the output, if it needs review. It
// keeps count and a lower bound of the size as JSON of all such literals.
// When count or size goes over maxReviewItems or maxReviewBytes, it keeps
// no more items, and scanTemplate fails: the scan uses bounded memory.
func (s *templateScan) add(pos int, text string) {
	if text == "" || s.count > maxReviewItems || s.autoPass(text) {
		return
	}
	s.count++
	if s.size > maxReviewBytes {
		return
	}
	s.size += reviewItemBytes + len(text)
	s.review = append(s.review, ReviewItem{Line: s.line(pos), Text: text})
}

// outputName tells whether the name at tokens[k] can reach the output as
// text: a keyword name of dict or namespace, or an attribute that set gives
// to a namespace.
func outputName(tokens []templateToken, k int) bool {
	callee := kwargCallee(tokens, k)
	return setAttribute(tokens, k) || callee == "dict" || callee == "namespace"
}

// setAttribute tells whether the name at tokens[k] is an attribute that
// set gives to a namespace.
func setAttribute(tokens []templateToken, k int) bool {
	return tokens[0].is(tokName, "set") && k > 1 && tokens[k-1].is(tokOperator, ".") && k < assignAt(tokens)
}

// kwargCallee gives the name of the function when tokens[k] is the name of
// a keyword argument of a function call, or "".
func kwargCallee(tokens []templateToken, k int) string {
	if !kwarg(tokens, k) {
		return ""
	}
	o := enclosing(tokens, k)
	if o < 1 || !tokens[o].is(tokOperator, "(") || tokens[o-1].kind != tokName || tokenAt(tokens, o-2).is(tokOperator, ".") {
		return ""
	}
	return tokens[o-1].text
}

// quiet tells whether the literal at tokens[k] is in a position where it
// cannot reach the output.
func quiet(tokens []templateToken, k int) bool {
	return subscriptKey(tokens, k) || directOperand(tokens, k) || testArgument(tokens, k) || quietArgument(tokens, k)
}

// subscriptKey tells whether tokens[k] is the whole key of a subscript.
func subscriptKey(tokens []templateToken, k int) bool {
	return tokenAt(tokens, k-1).is(tokOperator, "[") && tokenAt(tokens, k+1).is(tokOperator, "]") && valueEnd(tokenAt(tokens, k-2))
}

// directOperand tells whether tokens[k] is a whole operand of ==, !=, in
// or not in, or a whole item of a literal list that is such an operand.
func directOperand(tokens []templateToken, k int) bool {
	next, prev := tokenAt(tokens, k+1), tokenAt(tokens, k-1)
	switch {
	case comparedBy(tokens, k+1) && standsAlone(prev, -1):
		return true
	case (prev.is(tokOperator, "==") || prev.is(tokOperator, "!=") || membership(tokens, k-1)) && standsAlone(next, 1):
		return true
	}
	o := enclosing(tokens, k)
	if o < 0 || !tokens[o].is(tokOperator, "[") || valueEnd(tokenAt(tokens, o-1)) || !wholeArgument(tokens, o, k) {
		return false
	}
	end := closing(tokens, o)
	return membership(tokens, o-1) && standsAlone(tokenAt(tokens, end+1), 1) ||
		comparedBy(tokens, end+1) && standsAlone(tokenAt(tokens, o-1), -1)
}

// comparedBy tells whether tokens[j] starts ==, !=, in or not in.
func comparedBy(tokens []templateToken, j int) bool {
	t := tokenAt(tokens, j)
	return t.is(tokOperator, "==") || t.is(tokOperator, "!=") || membership(tokens, j) ||
		t.is(tokName, "not") && tokenAt(tokens, j+1).is(tokName, "in")
}

// membership tells whether tokens[j] is the in operator of a comparison.
// The first in of a for statement outside brackets starts the iterable,
// and the loop writes the items of the iterable.
func membership(tokens []templateToken, j int) bool {
	return tokenAt(tokens, j).is(tokName, "in") && (!tokens[0].is(tokName, "for") || j != loopIn(tokens))
}

// loopIn gives the index of the in of a for statement, or -1.
func loopIn(tokens []templateToken) int {
	return indexAtDepth(tokens, func(t templateToken) bool { return t.is(tokName, "in") })
}

// standsAlone tells whether t can stand next to a direct operand: no
// operator that binds tighter than a comparison, such as ~, + or |.
func standsAlone(t templateToken, dir int) bool {
	switch {
	case t.kind < 0:
		return true
	case t.kind == tokOperator:
		return operandStops[t.text] && (dir < 0 && t.text != ")" && t.text != "]" || dir > 0 && t.text != "(" && t.text != "[")
	case t.kind == tokName:
		return operandKeywords[t.text] || dir < 0 && t.text == "in"
	}
	return false
}

// testArgument tells whether tokens[k] is a whole argument of a test:
// x is equalto('k') or x is sameas 'k'.
func testArgument(tokens []templateToken, k int) bool {
	if testName(tokens, k-1) {
		return standsAlone(tokenAt(tokens, k+1), 1)
	}
	o := enclosing(tokens, k)
	return o > 0 && tokens[o].is(tokOperator, "(") && testName(tokens, o-1) && wholeArgument(tokens, o, k)
}

// testName tells whether tokens[j] is the name of a test, after is or
// is not.
func testName(tokens []templateToken, j int) bool {
	prev := tokenAt(tokens, j-1)
	return tokenAt(tokens, j).kind == tokName && (prev.is(tokName, "is") || prev.is(tokName, "not") && tokenAt(tokens, j-2).is(tokName, "is"))
}

// quietArgument tells whether tokens[k] is a quiet argument of a call: the
// first argument of get, the arguments of selectattr and rejectattr, the
// attribute of map and groupby, the message of raise_exception and a date
// format of strftime_now.
func quietArgument(tokens []templateToken, k int) bool {
	o := enclosing(tokens, k)
	if o < 1 || !tokens[o].is(tokOperator, "(") {
		return false
	}
	callee, before := tokens[o-1].text, tokenAt(tokens, o-2)
	switch {
	case callee == "raise_exception" && !before.is(tokOperator, "."):
		return true
	case callee == "strftime_now" && !before.is(tokOperator, "."):
		return wholeArgument(tokens, o, k) && dateFormat.MatchString(tokens[k].text)
	}
	arg := argumentOf(tokens, o, k)
	switch {
	case callee == "get" && before.is(tokOperator, "."):
		return arg.index == 0 && arg.whole
	case !before.is(tokOperator, "|") || !arg.whole:
		return false
	case selectorFilters[callee]:
		return arg.keyword == ""
	case callee == "map":
		return arg.keyword == "attribute"
	case callee == "groupby":
		return arg.keyword == "attribute" || arg.keyword == "" && arg.index == 0
	}
	return false
}

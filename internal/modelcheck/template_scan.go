package modelcheck

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
)

// This file checks a chat template without running it. vLLM runs the
// template in the sandboxed Jinja environment of transformers. The check
// accepts only the supported subset of Jinja in docs/MODEL-CHECKS.md and
// fails closed on each other construct. With this subset, the template can
// only write text from its own literals, from the request and from the
// special tokens of the tokenizer. Then the check lists each literal that
// an operator must read, because only a person can tell an instruction in
// a literal from markup.

// The loader gives these names: messages, tools, documents,
// add_generation_prompt, the special token names such as bos_token, loop,
// and the functions of trustedFunctions. Other names that the template does
// not assign come from the request, for example a chat template argument.
// unsafeName denies the names that write their own text, such as lipsum.

// trustedFunctions are the functions that a template can call, other than
// its own macros. strftime_now writes the date in a format from a literal.
var trustedFunctions = map[string]bool{"raise_exception": true, "strftime_now": true, "namespace": true, "range": true, "dict": true}

// trustedMethods are the methods that a template can call. They give
// parts of a value, a boolean or a number, and do not make other text.
// Methods such as encode, decode and to_bytes can make text from a number,
// so they are not in the list.
var trustedMethods = map[string]bool{
	"items": true, "keys": true, "values": true, "get": true, "strip": true, "lstrip": true, "rstrip": true,
	"split": true, "rsplit": true, "splitlines": true, "startswith": true, "endswith": true, "lower": true,
	"upper": true, "title": true, "capitalize": true, "replace": true, "join": true, "count": true,
}

// macroVariables are the special names of a macro. kwargs gives the
// keyword names of the call as text, varargs gives arguments that no
// declared argument names, and caller needs a call block.
var macroVariables = map[string]bool{"kwargs": true, "varargs": true, "caller": true}

// mapFilters are the filters that map can name. map gives no other
// arguments to them, and none of them calls another filter or a test by
// name.
var mapFilters = map[string]bool{
	"trim": true, "lower": true, "upper": true, "title": true, "capitalize": true, "string": true,
	"int": true, "float": true, "length": true, "tojson": true, "e": true, "escape": true,
}

// mapKeywords are the keyword arguments of map.
var mapKeywords = map[string]bool{"attribute": true, "default": true}

// trustedFilters change values that the template already has. They do not
// bring in other text. Filters that write their own text, such as urlize,
// filesizeformat, pprint, xmlattr and format, are not in the list.
// striptags is not in the list because it decodes HTML entities, for
// example "&#73;" to "I". Thus it can make letters from numbers.
var trustedFilters = map[string]bool{
	"abs": true, "batch": true, "capitalize": true, "center": true, "count": true, "d": true, "default": true,
	"dictsort": true, "e": true, "escape": true, "first": true, "float": true, "forceescape": true, "groupby": true,
	"indent": true, "int": true, "items": true, "join": true, "last": true, "length": true, "list": true, "lower": true,
	"map": true, "max": true, "min": true, "reject": true, "rejectattr": true, "replace": true, "reverse": true,
	"round": true, "safe": true, "select": true, "selectattr": true, "slice": true, "sort": true, "string": true,
	"sum": true, "title": true, "tojson": true, "trim": true, "truncate": true, "unique": true,
	"upper": true, "urlencode": true, "wordcount": true, "wordwrap": true,
}

// trustedTests are the Jinja tests that the subset supports. A test gives
// a boolean, not text. docs/MODEL-CHECKS.md lists the same tests.
var trustedTests = map[string]bool{
	"boolean": true, "callable": true, "defined": true, "divisibleby": true, "eq": true, "equalto": true,
	"escaped": true, "even": true, "false": true, "filter": true, "float": true, "ge": true, "gt": true,
	"in": true, "integer": true, "iterable": true, "le": true, "lower": true, "lt": true, "mapping": true,
	"ne": true, "none": true, "number": true, "odd": true, "sameas": true, "sequence": true, "string": true,
	"test": true, "true": true, "undefined": true, "upper": true,
}

// deniedNames give access to Python objects, write their own text or make
// the output nondeterministic. unsafeName also denies other names.
var deniedNames = map[string]bool{
	"self": true, "cycler": true, "joiner": true, "lipsum": true, "attr": true,
	"format": true, "format_map": true, "mro": true, "random": true,
}

// statementNames start a statement tag, and a "(" can follow them.
var statementNames = map[string]bool{"if": true, "elif": true, "for": true, "set": true, "macro": true}

// blockStatements open a block. Their end statement is "end" + the name.
var blockStatements = map[string]bool{"if": true, "for": true, "macro": true, "generation": true}

// maxReviewItems bounds the items of the review list. maxReviewBytes
// bounds the review list as JSON, so that a result with the list stays
// well below MaxRecordBytes.
const (
	maxReviewItems = 4096
	maxReviewBytes = 1 << 20
)

// ReviewItem is one literal of a chat template that an operator must read:
// its line in the template and its text.
type ReviewItem struct {
	Line int    `json:"line"`
	Text string `json:"text"`
}

// templateScan holds the state of one template scan. The scan stops
// between tags when ctx ends.
type templateScan struct {
	ctx    context.Context
	stack  []string
	macros map[string]bool
	// namespaces are the names that set gives a namespace. A template can
	// use them only with an attribute.
	namespaces map[string]bool
	// special are the special and added tokens of the tokenizer.
	special map[string]bool
	// variables are the values of the special token names.
	variables map[string][]string
	lines     []int
	// request tells, for each name that the template assigns, whether
	// it holds only request data. See template_parts.go.
	request map[string]bool
	// nonText are the names that can hold a boolean, None or a float.
	// See template_bool.go.
	nonText map[string]bool
	// macroArgs are the argument names of each macro of the template.
	macroArgs map[string][]string
	// review are the literals and names that can reach the output and
	// need review, in source order. count and size bound it, see add.
	review      []ReviewItem
	count, size int
	// specials keep, for each special token name, the values that need
	// review, or the reason that the name fails. See variableUses.
	specials map[string]specialUses
}

// scanTemplate checks one chat template without running it. special
// are the special tokens of the tokenizer, and variables are the values of
// the special token names, such as eos_token. It gives the review list in
// source order, or the reason of the first problem. A panic on hostile
// input fails closed, with the reason of a checker failure. The scan stops
// between tags when ctx ends. The caller then reads ctx.Err().
func scanTemplate(ctx context.Context, text string, special map[string]bool, variables map[string][]string) ([]ReviewItem, string) {
	return failClosed(func() ([]ReviewItem, string) {
		scanHook()
		if len(text) > MaxTemplateBytes {
			return nil, ReasonTemplateTooLarge
		}
		if hiddenText(text) {
			return nil, ReasonTemplateHiddenText
		}
		s := newScan(text, special, variables)
		s.ctx = ctx
		if reason := s.read(text); reason != "" {
			return nil, reason
		}
		if s.count > maxReviewItems {
			return nil, ReasonTemplateTooManyLiterals
		}
		if raw, err := json.Marshal(s.review); err != nil || len(raw) > maxReviewBytes {
			return nil, ReasonTemplateReviewTooLarge
		}
		return s.review, ""
	})
}

// failClosed runs scan. A panic gives the reason of a checker failure, so
// that hostile input cannot stop the checker or pass.
func failClosed(scan func() ([]ReviewItem, string)) (review []ReviewItem, reason string) {
	defer func() {
		if recover() != nil {
			review, reason = nil, ReasonTemplateCheckFailed
		}
	}()
	return scan()
}

func newScan(text string, special map[string]bool, variables map[string][]string) *templateScan {
	return &templateScan{ctx: context.Background(), macros: map[string]bool{}, namespaces: map[string]bool{}, specials: map[string]specialUses{}, special: special, variables: variables, lines: lineStarts(text)}
}

// lineStarts gives the byte offset of the start of each line.
func lineStarts(text string) []int {
	starts := []int{0}
	for i := 0; i < len(text); i++ {
		if text[i] == '\n' {
			starts = append(starts, i+1)
		}
	}
	return starts
}

// line gives the line number of a byte offset.
func (s *templateScan) line(pos int) int {
	n, _ := slices.BinarySearch(s.lines, pos+1)
	return n
}

// read lexes the tags, checks their structure and their names, and lists
// the literals in source order: the text outside tags and the string
// literals in tags.
func (s *templateScan) read(text string) string {
	var tags [][]templateToken
	pos := 0
	for {
		i := nextTag(text, pos)
		end := i
		if i < 0 {
			end = len(text)
		}
		s.add(pos, text[pos:end])
		if i < 0 {
			break
		}
		tokens, next, reason := s.readTag(text, i)
		if reason != "" {
			return reason
		}
		if tokens != nil {
			tags = append(tags, tokens)
		}
		pos = next
	}
	if len(s.stack) > 0 {
		return ReasonTemplateSyntax
	}
	s.requestNames(tags)
	s.nonTextNames(tags)
	for _, tokens := range tags {
		if reason := s.sources(tokens); reason != "" {
			return reason
		}
	}
	return ""
}

// readTag reads the tag or the comment at text[i]. It checks the structure
// of a tag and lists its string literals. A comment gives no tokens. It
// stops when the context of the scan ends.
func (s *templateScan) readTag(text string, i int) ([]templateToken, int, string) {
	if s.ctx.Err() != nil {
		return nil, 0, ReasonTemplateCheckFailed
	}
	if text[i+1] == '#' {
		end := strings.Index(text[i+2:], "#}")
		if end < 0 {
			return nil, 0, ReasonTemplateSyntax
		}
		return nil, i + 2 + end + 2, ""
	}
	block := text[i+1] == '%'
	tokens, next, reason := lexTag(text, i+2, block)
	if reason == "" {
		reason = s.tag(block, tokens)
	}
	if reason != "" {
		return nil, 0, reason
	}
	if reason := s.tagUses(tokens); reason != "" {
		return nil, 0, reason
	}
	return tokens, next, ""
}

// sources checks that each name of a tag brings in no text from another
// source: a filter, a test, a method and a called function must be
// trusted. A called name must be a trusted function or a macro of the
// template. A call of a value that is not a name, such as x['decode'](),
// fails. It stops when the context of the scan ends.
func (s *templateScan) sources(tokens []templateToken) string {
	if s.ctx.Err() != nil {
		return ReasonTemplateCheckFailed
	}
	if s.loopSource(tokens) {
		return ReasonTemplateUnknownSource
	}
	conditions := inlineConditions(tokens)
	for i, t := range tokens {
		switch {
		case t.kind == tokName && (!s.trustedName(tokens, i) || s.macroReference(tokens, i) || s.reprSource(tokens, i, conditions)),
			computedCall(tokens, i), s.partSource(tokens, i), s.reorderSource(tokens, i), s.methodKey(tokens, i), !namedArgumentsResolved(tokens, i), outsideSubset(tokens, i),
			s.nonTextValue(tokens, i) && !s.quietNonText(tokens, i, conditions):
			return ReasonTemplateUnknownSource
		case t.kind == tokString:
			if reason := namedSource(tokens, i); reason != "" {
				return reason
			}
		}
	}
	return ""
}

// namedTestArgs give the argument that names a test, for filters that call
// a test by name. mapArguments checks the filter that map names.
var namedTestArgs = map[string]int{"select": 0, "reject": 0, "selectattr": 1, "rejectattr": 1}

// namedTestCall tells whether tokens[o] is the "(" of a call of select,
// reject, selectattr or rejectattr as a filter. It gives the index of the
// argument that names the test.
func namedTestCall(tokens []templateToken, o int) (int, bool) {
	if o < 2 || !tokens[o].is(tokOperator, "(") || !tokens[o-2].is(tokOperator, "|") {
		return 0, false
	}
	index, ok := namedTestArgs[tokens[o-1].text]
	return index, ok
}

// namedSource checks a string that names a test for select, reject,
// selectattr or rejectattr. The named test must be trusted and not denied.
func namedSource(tokens []templateToken, k int) string {
	o := enclosing(tokens, k)
	index, ok := namedTestCall(tokens, o)
	if !ok {
		return ""
	}
	name, arg := tokens[k].text, argumentOf(tokens, o, k)
	if arg.keyword != "" || !arg.whole || arg.index != index {
		return ""
	}
	switch {
	case deniedNames[name]:
		return ReasonTemplateUnsafeName
	case !trustedTests[name]:
		return ReasonTemplateUnknownSource
	}
	return ""
}

// trustedName tells whether the name at tokens[i] is trusted where it is:
// a filter, a test, a called function or any other name.
func (s *templateScan) trustedName(tokens []templateToken, i int) bool {
	t, prev, next := tokens[i], tokenAt(tokens, i-1), tokenAt(tokens, i+1)
	switch {
	case prev.is(tokOperator, "|"):
		return trustedFilters[t.text]
	case t.is(tokName, "not") && prev.is(tokName, "is"):
		return true
	case testName(tokens, i):
		return trustedTests[t.text]
	case prev.is(tokOperator, "."):
		return !next.is(tokOperator, "(") || trustedMethods[t.text]
	case macroVariables[t.text]:
		return false
	case i == 0 && statementNames[t.text], i == 1 && tokens[0].is(tokName, "macro"):
		return true
	case next.is(tokOperator, "(") && !keywords[t.text]:
		return trustedFunctions[t.text] || s.macros[t.text]
	}
	return true
}

// macroReference tells whether tokens[i] uses a macro of the template other
// than in a call or its definition. A macro has attributes such as
// arguments and name that give its identifiers as text, and a stored macro
// can replace a trusted method or function. A macro argument or a keyword
// argument with the name of a macro would give that name another value,
// which a call of the name then calls. So these fail too.
func (s *templateScan) macroReference(tokens []templateToken, i int) bool {
	switch {
	case !s.macros[tokens[i].text], tokenAt(tokens, i-1).is(tokOperator, "."), tokenAt(tokens, i+1).is(tokOperator, "("):
		return false
	}
	return i != 1 || !tokens[0].is(tokName, "macro")
}

// namedArgumentsResolved tells whether a call of select, reject,
// selectattr or rejectattr at tokens[o] gives the test name as one string
// literal, so that namedSource can check it. An expression, a name or an
// unpacked argument fails.
func namedArgumentsResolved(tokens []templateToken, o int) bool {
	index, ok := namedTestCall(tokens, o)
	if !ok {
		return true
	}
	for n, arg := range arguments(tokens, o) {
		switch first := tokens[arg[0]]; {
		case first.is(tokOperator, "*"), first.is(tokOperator, "**"):
			return false
		case n == index && !kwarg(tokens, arg[0]):
			return arg[1]-arg[0] == 1 && first.kind == tokString
		}
	}
	return true
}

// outsideSubset tells whether tokens[i] starts a construct outside the
// supported subset of Jinja: a "%" that can format numbers as text, an
// unpacked argument, or a map call with other arguments than one listed
// filter or the attribute and default keywords.
func outsideSubset(tokens []templateToken, i int) bool {
	t, prev := tokens[i], tokenAt(tokens, i-1)
	switch {
	case t.is(tokOperator, "%"):
		return !moduloOperands(tokens, i)
	case t.is(tokOperator, "*") || t.is(tokOperator, "**"):
		return prev.is(tokOperator, "(") || prev.is(tokOperator, ",") && tokenAt(tokens, enclosing(tokens, i)).is(tokOperator, "(")
	case t.is(tokOperator, "(") && prev.is(tokName, "map") && tokenAt(tokens, i-2).is(tokOperator, "|"):
		return !mapArguments(tokens, i)
	}
	return false
}

// integerLoopAttributes are the loop attributes that hold an integer.
// Others, such as loop.previtem, can hold text.
var integerLoopAttributes = map[string]bool{
	"index": true, "index0": true, "revindex": true, "revindex0": true, "length": true, "depth": true, "depth0": true,
}

// tightOperators bind as strongly as "%" or more strongly. Before the left
// operand of a "%", they make that operand a part of a larger operand, as
// in '%c' * 1 % 65, which Jinja reads as ('%c' * 1) % 65.
var tightOperators = map[string]bool{"*": true, "/": true, "//": true, "%": true, "**": true, ".": true, "|": true}

// moduloOperands tells whether the "%" at tokens[i] is an integer modulo.
// The whole left operand must be an integer or an integer loop attribute,
// such as loop.index0, with an optional sign. The right operand must be an
// integer.
func moduloOperands(tokens []templateToken, i int) bool {
	right, after := tokenAt(tokens, i+1), tokenAt(tokens, i+2)
	if !integerLiteral(right) || after.is(tokOperator, ".") || after.is(tokOperator, "[") || after.is(tokOperator, "(") {
		return false
	}
	start := i - 1
	switch left := tokenAt(tokens, start); {
	case integerLiteral(left):
	case loopIntegerAttribute(tokens, i-3):
		start = i - 3
	default:
		return false
	}
	for sign := tokenAt(tokens, start-1); (sign.is(tokOperator, "-") || sign.is(tokOperator, "+")) && !valueEnd(tokenAt(tokens, start-2)); sign = tokenAt(tokens, start-1) {
		start--
	}
	before := tokenAt(tokens, start-1)
	return before.kind != tokOperator || !tightOperators[before.text]
}

// integerLiteral tells whether t is an integer literal.
func integerLiteral(t templateToken) bool {
	return t.kind == tokNumber && integerNumber.FindString(t.text) == t.text
}

// mapArguments tells whether the map call at tokens[o] gives only the
// attribute and default keywords, or only one listed filter name.
func mapArguments(tokens []templateToken, o int) bool {
	args := arguments(tokens, o)
	if len(args) == 1 && args[0][1]-args[0][0] == 1 && tokens[args[0][0]].kind == tokString {
		return mapFilters[tokens[args[0][0]].text]
	}
	for _, arg := range args {
		if !kwarg(tokens, arg[0]) || !mapKeywords[tokens[arg[0]].text] {
			return false
		}
	}
	return len(args) > 0
}

// computedCall tells whether tokens[i] calls a value that is not a name:
// a "(" after a string, a subscript or another call.
func computedCall(tokens []templateToken, i int) bool {
	prev := tokenAt(tokens, i-1)
	return tokens[i].is(tokOperator, "(") && (prev.kind == tokString || prev.is(tokOperator, "]") || prev.is(tokOperator, ")"))
}

// tag applies the rules to one tag.
func (s *templateScan) tag(block bool, tokens []templateToken) string {
	if len(tokens) == 0 {
		return ReasonTemplateSyntax
	}
	if reason := unsafeName(tokens); reason != "" {
		return reason
	}
	if rebindsTrusted(tokens) || shadowsMethod(tokens) {
		return ReasonTemplateUnsafeName
	}
	if !block {
		return ""
	}
	if tokens[0].kind != tokName {
		return ReasonTemplateSyntax
	}
	keyword, rest := tokens[0].text, tokens[1:]
	switch {
	case blockStatements[keyword]:
		if reason := s.open(keyword); reason != "" {
			return reason
		}
		return s.statement(keyword, rest)
	case keyword == "set":
		return s.set(rest)
	case strings.HasPrefix(keyword, "end"):
		return s.close(keyword[3:], rest)
	case keyword == "elif" || keyword == "else":
		return s.branch(keyword, rest)
	case keyword == "break" || keyword == "continue":
		if len(rest) == 0 && slices.Contains(s.stack, "for") {
			return ""
		}
		return ReasonTemplateSyntax
	}
	return ReasonTemplateStatement
}

// open starts a block.
func (s *templateScan) open(block string) string {
	if len(s.stack) >= maxTemplateDepth {
		return ReasonTemplateTooComplex
	}
	s.stack = append(s.stack, block)
	return ""
}

// close ends the innermost block, which must be of the same kind. An if or
// for block after its else has the suffix elseSuffix on the stack.
func (s *templateScan) close(block string, rest []templateToken) string {
	if len(s.stack) == 0 || strings.TrimSuffix(s.top(), elseSuffix) != block || len(rest) > 0 {
		return ReasonTemplateSyntax
	}
	s.stack = s.stack[:len(s.stack)-1]
	return ""
}

func (s *templateScan) top() string {
	if len(s.stack) == 0 {
		return ""
	}
	return s.stack[len(s.stack)-1]
}

// elseSuffix marks a block on the stack after its else.
const elseSuffix = " else"

// branch checks elif with a condition in an if block, and else without a
// condition in an if or a for block. Neither can follow an else.
func (s *templateScan) branch(keyword string, rest []templateToken) string {
	top := s.top()
	switch {
	case keyword == "elif" && top == "if" && len(rest) > 0:
		return ""
	case keyword == "else" && (top == "if" || top == "for") && len(rest) == 0:
		s.stack[len(s.stack)-1] = top + elseSuffix
		return ""
	}
	return ReasonTemplateSyntax
}

func (s *templateScan) statement(keyword string, rest []templateToken) string {
	switch keyword {
	case "for":
		if loopIn(rest) < 1 {
			return ReasonTemplateSyntax
		}
	case "macro":
		if len(rest) < 2 || rest[0].kind != tokName || !rest[1].is(tokOperator, "(") {
			return ReasonTemplateSyntax
		}
		s.macros[rest[0].text] = true
	}
	return ""
}

// set checks an assignment or a block assignment. It records a name that
// gets a namespace.
func (s *templateScan) set(rest []templateToken) string {
	eq := assignAt(rest)
	switch {
	case eq == 0 || len(rest) == 0:
		return ReasonTemplateSyntax
	case eq < 0:
		return s.open("set")
	case eq == 1 && tokenAt(rest, 2).is(tokName, "namespace"):
		s.namespaces[rest[0].text] = true
	}
	return ""
}

// assignAt gives the index of the "=" of an assignment outside brackets,
// or -1.
func assignAt(tokens []templateToken) int {
	return indexAtDepth(tokens, func(t templateToken) bool { return t.is(tokOperator, "=") })
}

// keywords are the Jinja operator names.
var keywords = map[string]bool{"in": true, "and": true, "or": true, "not": true, "if": true, "else": true, "is": true}

// unsafeName finds a name or a key that reaches Python objects: an
// attribute that starts with "_", a name or a string with "__", or a denied
// name.
func unsafeName(tokens []templateToken) string {
	for i, t := range tokens {
		attribute := i > 0 && tokens[i-1].is(tokOperator, ".")
		if t.kind == tokName && (attribute && strings.HasPrefix(t.text, "_") || deniedNames[t.text]) ||
			strings.Contains(t.text, "__") && t.kind != tokOperator {
			return ReasonTemplateUnsafeName
		}
	}
	return ""
}

// rebindsTrusted tells whether a tag can give a trusted function name or
// loop another value: a macro, a macro argument, a set target or a for
// target with that name. The checks trust these names, for example the
// check of quiet arguments and the integer modulo of a loop attribute, so
// a template must not rebind them. A tag can use a function name only in a
// call, and loop only before ".", as in loop.index0. A tag can also use
// both before a test such as "is defined", or as a keyword argument of a
// call.
func rebindsTrusted(tokens []templateToken) bool {
	macro := tokens[0].is(tokName, "macro")
	for i, t := range tokens {
		isLoop := t.text == "loop"
		if t.kind != tokName || !trustedFunctions[t.text] && !isLoop || tokenAt(tokens, i-1).is(tokOperator, ".") {
			continue
		}
		use := "("
		if isLoop {
			use = "."
		}
		next := tokenAt(tokens, i+1)
		switch {
		case macro && (i == 1 || !next.is(tokOperator, use)):
			return true
		case next.is(tokOperator, use), next.is(tokName, "is"), kwarg(tokens, i) && callArgument(tokens, i):
		default:
			return true
		}
	}
	return false
}

// callArgument tells whether tokens[i] is directly in the brackets of a
// call.
func callArgument(tokens []templateToken, i int) bool {
	o := enclosing(tokens, i)
	return o > 0 && tokens[o].is(tokOperator, "(") && tokens[o-1].kind == tokName && !keywords[tokens[o-1].text]
}

// shadowsMethod tells whether a tag gives a namespace an attribute with the
// name of a trusted method, such as namespace(get=x) or set ns.get = x.
// The attribute would replace the method, and the checks trust the method.
func shadowsMethod(tokens []templateToken) bool {
	for k, t := range tokens {
		if t.kind == tokName && trustedMethods[t.text] && (setAttribute(tokens, k) || kwargCallee(tokens, k) == "namespace") {
			return true
		}
	}
	return false
}

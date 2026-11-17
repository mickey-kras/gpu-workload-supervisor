package modelcheck

// This file holds helpers that find brackets, arguments and positions in
// the tokens of one tag.

// argument describes the argument of a call that holds a token: its
// position, its keyword or "", and whether the token is all of the
// argument, or all of the value of a keyword argument. index is -1 when no
// argument holds the token.
type argument struct {
	index   int
	keyword string
	whole   bool
}

// argumentOf gives the argument that holds tokens[k] in the call that opens
// at tokens[o].
func argumentOf(tokens []templateToken, o, k int) argument {
	for n, arg := range arguments(tokens, o) {
		if k < arg[0] || k >= arg[1] {
			continue
		}
		start, keyword := arg[0], ""
		if arg[1]-arg[0] > 2 && kwarg(tokens, arg[0]) {
			start, keyword = arg[0]+2, tokens[arg[0]].text
		}
		return argument{index: n, keyword: keyword, whole: start == k && arg[1] == k+1}
	}
	return argument{index: -1}
}

// wholeArgument tells whether tokens[k] is all of its argument, or all of
// the value of a keyword argument, in the call that opens at tokens[o].
func wholeArgument(tokens []templateToken, o, k int) bool {
	return argumentOf(tokens, o, k).whole
}

// enclosing gives the index of the innermost open bracket before tokens[k]
// that does not close before it, or -1.
func enclosing(tokens []templateToken, k int) int {
	depth := 0
	for j := k - 1; j >= 0; j-- {
		switch t := tokens[j]; {
		case closeBracket(t):
			depth++
		case openBracket(t):
			if depth == 0 {
				return j
			}
			depth--
		}
	}
	return -1
}

// valueEnd tells whether t can end a value, so that a "[" after it is a
// subscript and not a list.
func valueEnd(t templateToken) bool {
	switch t.kind {
	case tokString, tokNumber:
		return true
	case tokName:
		return !keywords[t.text]
	}
	return t.text == ")" || t.text == "]" || t.text == "}"
}

// arguments gives the [start, end) ranges of the arguments in the brackets
// that open at tokens[open].
func arguments(tokens []templateToken, open int) [][2]int {
	end := closing(tokens, open)
	var args [][2]int
	start, depth := open+1, 0
	for i := open + 1; i < end; i++ {
		t := tokens[i]
		switch {
		case openBracket(t):
			depth++
		case closeBracket(t):
			depth--
		case t.is(tokOperator, ",") && depth == 0:
			args, start = append(args, [2]int{start, i}), i+1
		}
	}
	if start < end {
		args = append(args, [2]int{start, end})
	}
	return args
}

// kwarg tells whether the name at tokens[i] is a keyword argument name.
func kwarg(tokens []templateToken, i int) bool {
	return i+1 < len(tokens) && tokens[i+1].is(tokOperator, "=")
}

// closing gives the index of the bracket that closes tokens[open], or
// len(tokens) when tokens[open] is not an open bracket.
func closing(tokens []templateToken, open int) int {
	if t := tokens[open]; openBracket(t) && t.pair > open {
		return t.pair
	}
	return len(tokens)
}

// indexAtDepth gives the index of the first token outside brackets that
// matches, or -1.
func indexAtDepth(tokens []templateToken, match func(templateToken) bool) int {
	depth := 0
	for i, t := range tokens {
		switch {
		case depth == 0 && match(t):
			return i
		case openBracket(t):
			depth++
		case closeBracket(t):
			depth--
		}
	}
	return -1
}

// openBracket tells whether t is "(", "[" or "{".
func openBracket(t templateToken) bool {
	return t.kind == tokOperator && (t.text == "(" || t.text == "[" || t.text == "{")
}

// closeBracket tells whether t is ")", "]" or "}".
func closeBracket(t templateToken) bool {
	return t.kind == tokOperator && openBrackets[t.text] != ""
}

// loopIntegerAttribute tells whether tokens[start:start+3] is an integer
// attribute of loop, such as loop.index0, that is not an attribute of
// another value and has no attribute, subscript or call after it.
func loopIntegerAttribute(tokens []templateToken, start int) bool {
	after := tokenAt(tokens, start+3)
	return tokenAt(tokens, start).is(tokName, "loop") && !tokenAt(tokens, start-1).is(tokOperator, ".") &&
		tokenAt(tokens, start+1).is(tokOperator, ".") && integerLoopAttributes[tokenAt(tokens, start+2).text] &&
		!after.is(tokOperator, ".") && !after.is(tokOperator, "[") && !after.is(tokOperator, "(")
}

func tokenAt(tokens []templateToken, i int) templateToken {
	if i < 0 || i >= len(tokens) {
		return templateToken{kind: -1}
	}
	return tokens[i]
}

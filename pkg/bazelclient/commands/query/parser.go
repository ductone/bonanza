package query

import (
	"fmt"
	"strconv"
	"strings"
)

type tokenKind int

const (
	tokenWord tokenKind = iota
	tokenPunct
	tokenEOF
)

type token struct {
	kind tokenKind
	text string
	// quoted records that the word was written inside quotes, which
	// keeps a quoted "deps" from being read as the function name and a
	// quoted "+" from being read as an operator.
	quoted bool
}

// tokenize splits a query expression into words and punctuation.
//
// A word is any run of characters that is not whitespace, a parenthesis
// or a comma. That deliberately keeps "//pkg/...", "^//(cmd|pkg)(/|:)"
// and "go_test rule" (once quoted) in one piece, and it is why the
// operators are recognised by the parser rather than here: "+" is an
// operator between two expressions but an ordinary character inside a
// label such as "//pkg:a+b".
func tokenize(s string) []token {
	var tokens []token
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '(' || c == ')' || c == ',':
			tokens = append(tokens, token{kind: tokenPunct, text: string(c)})
			i++
		case c == '\'' || c == '"':
			quote := c
			j := i + 1
			for j < len(s) && s[j] != quote {
				j++
			}
			if j < len(s) {
				tokens = append(tokens, token{kind: tokenWord, text: s[i+1 : j], quoted: true})
				i = j + 1
			} else {
				// Unterminated quote. Take the rest verbatim and
				// let the parser report where it went wrong.
				tokens = append(tokens, token{kind: tokenWord, text: s[i+1:], quoted: true})
				i = len(s)
			}
		default:
			j := i
			for j < len(s) && !strings.ContainsRune(" \t\n\r(),", rune(s[j])) {
				j++
			}
			tokens = append(tokens, token{kind: tokenWord, text: s[i:j]})
			i = j
		}
	}
	return append(tokens, token{kind: tokenEOF})
}

type parser struct {
	tokens []token
	pos    int
}

func (p *parser) peek() token { return p.tokens[p.pos] }

func (p *parser) atEnd() bool { return p.peek().kind == tokenEOF }

func (p *parser) next() token {
	t := p.tokens[p.pos]
	if t.kind != tokenEOF {
		p.pos++
	}
	return t
}

func (p *parser) expectPunct(text string) error {
	t := p.peek()
	if t.kind != tokenPunct || t.text != text {
		return fmt.Errorf("expected %#v, got %#v", text, describe(t))
	}
	p.next()
	return nil
}

func describe(t token) string {
	if t.kind == tokenEOF {
		return "end of expression"
	}
	return t.text
}

// operatorFor recognises an unquoted word or punctuation as a set
// operator. A quoted token is never an operator: filter("-", x) filters
// on a hyphen.
func operatorFor(t token) (SetOperator, bool) {
	if t.quoted {
		return 0, false
	}
	switch {
	case t.kind == tokenWord && t.text == "union", t.kind == tokenWord && t.text == "+":
		return SetOperatorUnion, true
	case t.kind == tokenWord && t.text == "except", t.kind == tokenWord && t.text == "-":
		return SetOperatorExcept, true
	case t.kind == tokenWord && t.text == "intersect", t.kind == tokenWord && t.text == "^":
		return SetOperatorIntersect, true
	}
	return 0, false
}

// parseExpression parses a sequence of primaries joined by set
// operators. All three operators share one precedence level and
// associate to the left, matching Bazel.
func (p *parser) parseExpression() (Expression, error) {
	left, err := p.parsePrimary()
	if err != nil {
		return nil, err
	}
	for {
		operator, ok := operatorFor(p.peek())
		if !ok {
			return left, nil
		}
		p.next()
		right, err := p.parsePrimary()
		if err != nil {
			return nil, err
		}
		left = BinaryExpression{Operator: operator, Left: left, Right: right}
	}
}

// knownFunctions are the functions this subset implements. A call to
// anything else is reported by name rather than being taken for a target
// pattern, so an unimplemented function is a visible error instead of an
// empty result.
var knownFunctions = map[string]struct{}{
	"deps": {}, "rdeps": {}, "kind": {}, "filter": {}, "attr": {}, "set": {},
}

func (p *parser) parsePrimary() (Expression, error) {
	t := p.peek()
	switch {
	case t.kind == tokenEOF:
		return nil, fmt.Errorf("unexpected end of expression")
	case t.kind == tokenPunct && t.text == "(":
		p.next()
		e, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		if err := p.expectPunct(")"); err != nil {
			return nil, err
		}
		return e, nil
	case t.kind == tokenPunct:
		return nil, fmt.Errorf("unexpected %#v", t.text)
	}

	// A word followed by "(" is a function call; otherwise it is a
	// target pattern.
	if !t.quoted && p.tokens[p.pos+1].kind == tokenPunct && p.tokens[p.pos+1].text == "(" {
		name := t.text
		if _, ok := knownFunctions[name]; !ok {
			return nil, fmt.Errorf("function %#v is not supported by this implementation of \"bazel query\"", name)
		}
		p.next()
		p.next()
		return p.parseCall(name)
	}
	p.next()
	return PatternExpression{Pattern: t.text}, nil
}

// parseCall parses the arguments of a call whose name and opening
// parenthesis have already been consumed.
func (p *parser) parseCall(name string) (Expression, error) {
	if name == "set" {
		// set() takes whitespace-separated patterns rather than a
		// comma-separated argument list.
		var patterns []string
		for {
			t := p.peek()
			if t.kind == tokenPunct && t.text == ")" {
				p.next()
				return SetExpression{Patterns: patterns}, nil
			}
			if t.kind != tokenWord {
				return nil, fmt.Errorf("expected a target pattern in set(), got %#v", describe(t))
			}
			p.next()
			patterns = append(patterns, t.text)
		}
	}

	args, err := p.parseArguments()
	if err != nil {
		return nil, fmt.Errorf("%s(): %w", name, err)
	}
	switch name {
	case "deps":
		if len(args) != 1 && len(args) != 2 {
			return nil, fmt.Errorf("deps() takes one or two arguments, got %d", len(args))
		}
		universe, err := args[0].expression(name, 1)
		if err != nil {
			return nil, err
		}
		depth, err := optionalDepth(name, args, 1)
		if err != nil {
			return nil, err
		}
		return DepsExpression{Universe: universe, Depth: depth}, nil
	case "rdeps":
		if len(args) != 2 && len(args) != 3 {
			return nil, fmt.Errorf("rdeps() takes two or three arguments, got %d", len(args))
		}
		universe, err := args[0].expression(name, 1)
		if err != nil {
			return nil, err
		}
		targets, err := args[1].expression(name, 2)
		if err != nil {
			return nil, err
		}
		depth, err := optionalDepth(name, args, 2)
		if err != nil {
			return nil, err
		}
		return RdepsExpression{Universe: universe, Targets: targets, Depth: depth}, nil
	case "kind", "filter":
		if len(args) != 2 {
			return nil, fmt.Errorf("%s() takes two arguments, got %d", name, len(args))
		}
		pattern, err := args[0].word(name, 1)
		if err != nil {
			return nil, err
		}
		targets, err := args[1].expression(name, 2)
		if err != nil {
			return nil, err
		}
		if name == "kind" {
			return KindExpression{Pattern: pattern, Targets: targets}, nil
		}
		return FilterExpression{Pattern: pattern, Targets: targets}, nil
	case "attr":
		if len(args) != 3 {
			return nil, fmt.Errorf("attr() takes three arguments, got %d", len(args))
		}
		attrName, err := args[0].word(name, 1)
		if err != nil {
			return nil, err
		}
		pattern, err := args[1].word(name, 2)
		if err != nil {
			return nil, err
		}
		targets, err := args[2].expression(name, 3)
		if err != nil {
			return nil, err
		}
		return AttrExpression{Name: attrName, Pattern: pattern, Targets: targets}, nil
	}
	return nil, fmt.Errorf("function %#v is not supported", name)
}

// argument is one comma-separated argument. An argument that is a single
// word is usable either as a word (a regular expression, an attribute
// name, a depth) or as a target pattern, and which one it is depends on
// the function, so the decision is deferred to the caller.
type argument struct {
	expr  Expression
	text  string
	isRaw bool
}

func (a argument) word(function string, position int) (string, error) {
	if !a.isRaw {
		return "", fmt.Errorf("%s(): argument %d must be a word, not an expression", function, position)
	}
	return a.text, nil
}

func (a argument) expression(function string, position int) (Expression, error) {
	if a.isRaw {
		return PatternExpression{Pattern: a.text}, nil
	}
	return a.expr, nil
}

func optionalDepth(function string, args []argument, after int) (int, error) {
	if len(args) <= after {
		return -1, nil
	}
	text, err := args[after].word(function, after+1)
	if err != nil {
		return 0, err
	}
	depth, err := strconv.Atoi(text)
	if err != nil {
		return 0, fmt.Errorf("%s(): depth %#v is not an integer", function, text)
	}
	if depth < 0 {
		return 0, fmt.Errorf("%s(): depth %d is negative", function, depth)
	}
	return depth, nil
}

func (p *parser) parseArguments() ([]argument, error) {
	var args []argument
	if t := p.peek(); t.kind == tokenPunct && t.text == ")" {
		p.next()
		return args, nil
	}
	for {
		// A lone word argument keeps its text, so the caller can use
		// it as a regular expression or a depth rather than as a
		// target pattern. Only a word that is immediately followed by
		// "," or ")" qualifies; anything else is a full expression.
		parsed := false
		if t := p.peek(); t.kind == tokenWord {
			nextT := p.tokens[p.pos+1]
			if nextT.kind == tokenPunct && (nextT.text == "," || nextT.text == ")") {
				p.next()
				args = append(args, argument{text: t.text, isRaw: true})
				parsed = true
			}
		}
		if !parsed {
			e, err := p.parseExpression()
			if err != nil {
				return nil, err
			}
			args = append(args, argument{expr: e})
		}

		t := p.peek()
		switch {
		case t.kind == tokenPunct && t.text == ",":
			p.next()
		case t.kind == tokenPunct && t.text == ")":
			p.next()
			return args, nil
		default:
			return nil, fmt.Errorf("expected \",\" or \")\", got %#v", describe(t))
		}
	}
}

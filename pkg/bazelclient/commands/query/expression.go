package query

import (
	"fmt"
	"strings"
)

// Expression is a parsed Bazel query expression.
//
// Only the subset of the query language that callers of "bazel query"
// in build tooling actually use is represented: target patterns, set(),
// the three set operators, and the deps(), rdeps(), kind(), filter() and
// attr() functions. Anything outside it is rejected at parse time with
// the name of the construct, rather than being silently treated as a
// target pattern -- a query that quietly matches nothing is worse than
// one that refuses to run, because the caller cannot tell the two apart
// from an empty result.
type Expression interface {
	isExpression()
}

// PatternExpression is a bare target pattern, such as //pkg/... or
// //pkg:target.
type PatternExpression struct {
	Pattern string
}

// SetExpression is set(a b c), an explicit list of target patterns.
// Unlike a bare pattern, the arguments of set() are separated by
// whitespace rather than commas.
type SetExpression struct {
	Patterns []string
}

// SetOperator is one of the three binary set operations.
type SetOperator int

const (
	// SetOperatorUnion is "union", or "+".
	SetOperatorUnion SetOperator = iota
	// SetOperatorExcept is "except", or "-".
	SetOperatorExcept
	// SetOperatorIntersect is "intersect", or "^".
	SetOperatorIntersect
)

func (o SetOperator) String() string {
	switch o {
	case SetOperatorUnion:
		return "union"
	case SetOperatorExcept:
		return "except"
	case SetOperatorIntersect:
		return "intersect"
	default:
		return fmt.Sprintf("SetOperator(%d)", int(o))
	}
}

// BinaryExpression combines two expressions with a set operator. All
// three operators share one precedence level and associate to the left,
// as they do in Bazel.
type BinaryExpression struct {
	Operator SetOperator
	Left     Expression
	Right    Expression
}

// DepsExpression is deps(x) or deps(x, depth): the targets reachable
// from x, optionally bounded by depth. A negative Depth means unbounded.
type DepsExpression struct {
	Universe Expression
	Depth    int
}

// RdepsExpression is rdeps(universe, x) or rdeps(universe, x, depth):
// the targets within universe that can reach x.
type RdepsExpression struct {
	Universe Expression
	Targets  Expression
	Depth    int
}

// KindExpression is kind(pattern, x): the targets in x whose rule kind
// matches the pattern, which is an unanchored regular expression.
type KindExpression struct {
	Pattern string
	Targets Expression
}

// FilterExpression is filter(pattern, x): the targets in x whose label
// matches the pattern, which is an unanchored regular expression.
type FilterExpression struct {
	Pattern string
	Targets Expression
}

// AttrExpression is attr(name, pattern, x): the targets in x that have
// an attribute with the given name whose value matches the pattern.
type AttrExpression struct {
	Name    string
	Pattern string
	Targets Expression
}

func (PatternExpression) isExpression() {}
func (SetExpression) isExpression()     {}
func (BinaryExpression) isExpression()  {}
func (DepsExpression) isExpression()    {}
func (RdepsExpression) isExpression()   {}
func (KindExpression) isExpression()    {}
func (FilterExpression) isExpression()  {}
func (AttrExpression) isExpression()    {}

// ParseExpression parses a Bazel query expression.
//
// Bazel accepts the expression either as a single argument or split
// across several, joining them with spaces, so callers pass the
// arguments unmodified.
func ParseExpression(arguments []string) (Expression, error) {
	p := &parser{tokens: tokenize(strings.Join(arguments, " "))}
	e, err := p.parseExpression()
	if err != nil {
		return nil, err
	}
	if !p.atEnd() {
		return nil, fmt.Errorf("unexpected %#v after the end of the expression", p.peek().text)
	}
	return e, nil
}

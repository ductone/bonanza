package query_test

import (
	"testing"

	"bonanza.build/pkg/bazelclient/commands/query"

	"github.com/stretchr/testify/require"
)

func TestParseExpressionPatterns(t *testing.T) {
	t.Run("BarePattern", func(t *testing.T) {
		e, err := query.ParseExpression([]string{"//pkg/..."})
		require.NoError(t, err)
		require.Equal(t, query.PatternExpression{Pattern: "//pkg/..."}, e)
	})

	t.Run("PatternWithTarget", func(t *testing.T) {
		e, err := query.ParseExpression([]string{"//pkg/foo:foo.go"})
		require.NoError(t, err)
		require.Equal(t, query.PatternExpression{Pattern: "//pkg/foo:foo.go"}, e)
	})

	t.Run("Set", func(t *testing.T) {
		// set() separates its patterns with whitespace, not commas.
		e, err := query.ParseExpression([]string{"set(//pkg/foo:foo.go //cmd/bar:bar.go )"})
		require.NoError(t, err)
		require.Equal(t, query.SetExpression{
			Patterns: []string{"//pkg/foo:foo.go", "//cmd/bar:bar.go"},
		}, e)
	})

	t.Run("EmptySet", func(t *testing.T) {
		e, err := query.ParseExpression([]string{"set()"})
		require.NoError(t, err)
		require.Equal(t, query.SetExpression{}, e)
	})

	t.Run("ArgumentsAreJoined", func(t *testing.T) {
		// Bazel accepts the expression split across arguments.
		e, err := query.ParseExpression([]string{"//pkg/...", "union", "//cmd/..."})
		require.NoError(t, err)
		require.Equal(t, query.BinaryExpression{
			Operator: query.SetOperatorUnion,
			Left:     query.PatternExpression{Pattern: "//pkg/..."},
			Right:    query.PatternExpression{Pattern: "//cmd/..."},
		}, e)
	})
}

func TestParseExpressionOperators(t *testing.T) {
	for _, tc := range []struct {
		name     string
		text     string
		operator query.SetOperator
	}{
		{"UnionWord", "//a union //b", query.SetOperatorUnion},
		{"UnionSymbol", "//a + //b", query.SetOperatorUnion},
		{"ExceptWord", "//a except //b", query.SetOperatorExcept},
		{"ExceptSymbol", "//a - //b", query.SetOperatorExcept},
		{"IntersectWord", "//a intersect //b", query.SetOperatorIntersect},
		{"IntersectSymbol", "//a ^ //b", query.SetOperatorIntersect},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, err := query.ParseExpression([]string{tc.text})
			require.NoError(t, err)
			require.Equal(t, query.BinaryExpression{
				Operator: tc.operator,
				Left:     query.PatternExpression{Pattern: "//a"},
				Right:    query.PatternExpression{Pattern: "//b"},
			}, e)
		})
	}

	t.Run("LeftAssociative", func(t *testing.T) {
		// All three operators share one precedence level, so
		// "a + b - c" is "(a + b) - c" and not "a + (b - c)".
		e, err := query.ParseExpression([]string{"//a + //b - //c"})
		require.NoError(t, err)
		require.Equal(t, query.BinaryExpression{
			Operator: query.SetOperatorExcept,
			Left: query.BinaryExpression{
				Operator: query.SetOperatorUnion,
				Left:     query.PatternExpression{Pattern: "//a"},
				Right:    query.PatternExpression{Pattern: "//b"},
			},
			Right: query.PatternExpression{Pattern: "//c"},
		}, e)
	})

	t.Run("ParenthesesOverrideAssociativity", func(t *testing.T) {
		e, err := query.ParseExpression([]string{"//a + (//b - //c)"})
		require.NoError(t, err)
		require.Equal(t, query.BinaryExpression{
			Operator: query.SetOperatorUnion,
			Left:     query.PatternExpression{Pattern: "//a"},
			Right: query.BinaryExpression{
				Operator: query.SetOperatorExcept,
				Left:     query.PatternExpression{Pattern: "//b"},
				Right:    query.PatternExpression{Pattern: "//c"},
			},
		}, e)
	})

	t.Run("HyphenInsideLabelIsNotAnOperator", func(t *testing.T) {
		// A label may contain a hyphen. Only a hyphen that stands
		// alone between two expressions is the "except" operator.
		e, err := query.ParseExpression([]string{"//cmd/be-conductor:be-conductor"})
		require.NoError(t, err)
		require.Equal(t, query.PatternExpression{Pattern: "//cmd/be-conductor:be-conductor"}, e)
	})

	t.Run("QuotedOperatorIsAWord", func(t *testing.T) {
		e, err := query.ParseExpression([]string{`filter("-", //a)`})
		require.NoError(t, err)
		require.Equal(t, query.FilterExpression{
			Pattern: "-",
			Targets: query.PatternExpression{Pattern: "//a"},
		}, e)
	})
}

func TestParseExpressionFunctions(t *testing.T) {
	t.Run("Kind", func(t *testing.T) {
		e, err := query.ParseExpression([]string{`kind("go_test rule", //pkg/...)`})
		require.NoError(t, err)
		require.Equal(t, query.KindExpression{
			Pattern: "go_test rule",
			Targets: query.PatternExpression{Pattern: "//pkg/..."},
		}, e)
	})

	t.Run("Filter", func(t *testing.T) {
		e, err := query.ParseExpression([]string{`filter("^//(cmd|pkg)(/|:)", //...)`})
		require.NoError(t, err)
		require.Equal(t, query.FilterExpression{
			Pattern: "^//(cmd|pkg)(/|:)",
			Targets: query.PatternExpression{Pattern: "//..."},
		}, e)
	})

	t.Run("Attr", func(t *testing.T) {
		e, err := query.ParseExpression([]string{`attr("tags", "manual", //pkg/...)`})
		require.NoError(t, err)
		require.Equal(t, query.AttrExpression{
			Name:    "tags",
			Pattern: "manual",
			Targets: query.PatternExpression{Pattern: "//pkg/..."},
		}, e)
	})

	t.Run("DepsWithoutDepth", func(t *testing.T) {
		e, err := query.ParseExpression([]string{"deps(//pkg/foo)"})
		require.NoError(t, err)
		require.Equal(t, query.DepsExpression{
			Universe: query.PatternExpression{Pattern: "//pkg/foo"},
			Depth:    -1,
		}, e)
	})

	t.Run("DepsWithDepth", func(t *testing.T) {
		e, err := query.ParseExpression([]string{"deps(//pkg/foo, 2)"})
		require.NoError(t, err)
		require.Equal(t, query.DepsExpression{
			Universe: query.PatternExpression{Pattern: "//pkg/foo"},
			Depth:    2,
		}, e)
	})

	t.Run("Rdeps", func(t *testing.T) {
		e, err := query.ParseExpression([]string{"rdeps(//pkg/..., //pkg/foo:foo.go)"})
		require.NoError(t, err)
		require.Equal(t, query.RdepsExpression{
			Universe: query.PatternExpression{Pattern: "//pkg/..."},
			Targets:  query.PatternExpression{Pattern: "//pkg/foo:foo.go"},
			Depth:    -1,
		}, e)
	})

	t.Run("RdepsWithDepth", func(t *testing.T) {
		e, err := query.ParseExpression([]string{"rdeps(//pkg/..., //pkg/foo:foo.go, 1)"})
		require.NoError(t, err)
		require.Equal(t, query.RdepsExpression{
			Universe: query.PatternExpression{Pattern: "//pkg/..."},
			Targets:  query.PatternExpression{Pattern: "//pkg/foo:foo.go"},
			Depth:    1,
		}, e)
	})
}

// The expressions below are the ones ductone/c1's affected-target
// selector (ci/bazel-affected.sh) actually emits. They are the reason
// this subset was chosen, so they are asserted verbatim.
func TestParseExpressionRealSelectorQueries(t *testing.T) {
	t.Run("SourceOwnership", func(t *testing.T) {
		e, err := query.ParseExpression([]string{`kind("source file", set(//pkg/foo:* ))`})
		require.NoError(t, err)
		require.Equal(t, query.KindExpression{
			Pattern: "source file",
			Targets: query.SetExpression{Patterns: []string{"//pkg/foo:*"}},
		}, e)
	})

	t.Run("BackendBuildScope", func(t *testing.T) {
		e, err := query.ParseExpression([]string{
			`kind("go_(binary|library) rule", filter("^//(cmd|pkg)(/|:)", rdeps(//cmd/... + //pkg/..., set(//pkg/foo:foo.go )))) except attr("tags", "squire-only", //cmd/... + //pkg/...)`,
		})
		require.NoError(t, err)

		universe := query.BinaryExpression{
			Operator: query.SetOperatorUnion,
			Left:     query.PatternExpression{Pattern: "//cmd/..."},
			Right:    query.PatternExpression{Pattern: "//pkg/..."},
		}
		require.Equal(t, query.BinaryExpression{
			Operator: query.SetOperatorExcept,
			Left: query.KindExpression{
				Pattern: "go_(binary|library) rule",
				Targets: query.FilterExpression{
					Pattern: "^//(cmd|pkg)(/|:)",
					Targets: query.RdepsExpression{
						Universe: universe,
						Targets:  query.SetExpression{Patterns: []string{"//pkg/foo:foo.go"}},
						Depth:    -1,
					},
				},
			},
			Right: query.AttrExpression{
				Name:    "tags",
				Pattern: "squire-only",
				Targets: universe,
			},
		}, e)
	})

	t.Run("UnitScope", func(t *testing.T) {
		e, err := query.ParseExpression([]string{
			`kind("go_test rule", //cmd/... + //pkg/... + //dev/...) except filter("^//tests(/|:)", //cmd/...)`,
		})
		require.NoError(t, err)
		require.IsType(t, query.BinaryExpression{}, e)
	})
}

func TestParseExpressionErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
	}{
		// An unimplemented function must be named, not silently taken
		// for a target pattern: an empty result and "this function
		// does nothing" are indistinguishable to the caller.
		{"UnsupportedFunction", "somepath(//a, //b)"},
		{"UnsupportedFunctionTests", "tests(//a)"},
		{"TrailingGarbage", "//a //b"},
		{"UnclosedParenthesis", "(//a"},
		{"MissingArgument", "kind()"},
		{"TooFewArguments", "kind(//a)"},
		{"TooManyArguments", "kind(a, b, c)"},
		{"AttrTooFew", `attr("tags", //a)`},
		{"RdepsTooFew", "rdeps(//a)"},
		{"NegativeDepth", "deps(//a, -1)"},
		{"NonIntegerDepth", "deps(//a, two)"},
		{"EmptyExpression", ""},
		{"DanglingOperator", "//a union"},
		{"ExpressionWhereWordExpected", "kind(//a + //b, //c)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := query.ParseExpression([]string{tc.text})
			require.Error(t, err)
		})
	}
}

func TestEncodeRoundTrip(t *testing.T) {
	// Every expression the parser can produce must encode, or a query
	// that parses would fail at the boundary instead of at parse time.
	for _, text := range []string{
		"//pkg/...",
		"set(//a //b)",
		"//a + //b",
		"//a - //b",
		"//a ^ //b",
		"deps(//a)",
		"deps(//a, 3)",
		"rdeps(//a, //b)",
		"rdeps(//a, //b, 2)",
		`kind("go_test rule", //a)`,
		`filter("^//pkg", //a)`,
		`attr("tags", "manual", //a)`,
		`kind("go_(binary|library) rule", filter("^//(cmd|pkg)(/|:)", rdeps(//cmd/... + //pkg/..., set(//pkg/foo:foo.go )))) except attr("tags", "squire-only", //cmd/... + //pkg/...)`,
	} {
		t.Run(text, func(t *testing.T) {
			e, err := query.ParseExpression([]string{text})
			require.NoError(t, err)
			encoded, err := query.Encode(e)
			require.NoError(t, err)
			require.NotNil(t, encoded.Expression)
		})
	}
}

package analysis

import (
	"testing"

	model_core "bonanza.build/pkg/model/core"
	model_starlark_pb "bonanza.build/pkg/proto/model/starlark"
)

func TestTestTagFilters(t *testing.T) {
	for _, tc := range []struct {
		name    string
		tags    []string
		filters []string
		want    bool
		invalid bool
	}{
		{name: "negative exclusions", tags: []string{"integration"}, filters: []string{"-integration"}},
		{name: "positive disjunction", tags: []string{"small"}, filters: []string{"medium", "small"}, want: true},
		{name: "missing positive", tags: []string{"large"}, filters: []string{"small", "medium"}},
		{name: "negative overrides positive", tags: []string{"small", "integration"}, filters: []string{"small", "-integration"}},
		{name: "unfiltered", tags: []string{"integration"}, want: true},
		{name: "invalid empty exclusion", filters: []string{"-"}, invalid: true},
		{name: "invalid after excluded tag", tags: []string{"integration"}, filters: []string{"-integration", "-"}, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateTestTagFilters(tc.filters)
			if (err != nil) != tc.invalid {
				t.Fatalf("validateTestTagFilters(%v) = %v, want invalid=%v", tc.filters, err, tc.invalid)
			}
			if !tc.invalid {
				got := testTagsMatch(tc.tags, tc.filters)
				if got != tc.want {
					t.Fatalf("testTagsMatch(%v, %v) = %v, want %v", tc.tags, tc.filters, got, tc.want)
				}
			}
		})
	}
}

func TestUnsupportedTestExecutionTags(t *testing.T) {
	if got := unsupportedTestTag([]string{"manual", "no-cache"}); got != "no-cache" {
		t.Fatalf("uncached test would be scheduled on cached worker: got %q", got)
	}
	if got := unsupportedTestTag([]string{"manual", "no-remote-exec"}); got != "no-remote-exec" {
		t.Fatalf("local-only test would be remotely scheduled: got %q", got)
	}
	if got := unsupportedTestTag([]string{"manual"}); got != "" {
		t.Fatalf("ordinary manual test should remain runnable: got %q", got)
	}
}

func TestTestShardCount(t *testing.T) {
	definition := model_core.NewSimpleMessage[int](&model_starlark_pb.Rule_Definition{Attrs: []*model_starlark_pb.NamedAttr{{
		Name: "shard_count",
		Attr: &model_starlark_pb.Attr{Default: &model_starlark_pb.Value{Kind: &model_starlark_pb.Value_Int{Int: &model_starlark_pb.Int{Negative: true, AbsoluteValue: []byte{1}}}}},
	}}})
	for _, tc := range []struct {
		name    string
		value   *model_starlark_pb.Int
		want    uint32
		invalid bool
	}{
		{name: "default unsharded", want: 1},
		{name: "explicit two shards", value: &model_starlark_pb.Int{AbsoluteValue: []byte{2}}, want: 2},
		{name: "zero shards", value: &model_starlark_pb.Int{}, invalid: true},
		{name: "too many shards", value: &model_starlark_pb.Int{AbsoluteValue: []byte{51}}, invalid: true},
		{name: "negative shard count", value: &model_starlark_pb.Int{Negative: true, AbsoluteValue: []byte{2}}, invalid: true},
		{name: "huge shard count", value: &model_starlark_pb.Int{AbsoluteValue: []byte{1, 0}}, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value := &model_starlark_pb.Value{Kind: &model_starlark_pb.Value_None{}}
			if tc.value != nil {
				value = &model_starlark_pb.Value{Kind: &model_starlark_pb.Value_Int{Int: tc.value}}
			}
			target := model_core.NewSimpleMessage[int](&model_starlark_pb.RuleTarget{PublicAttrValues: []*model_starlark_pb.RuleTarget_PublicAttrValue{{
				ValueParts: []*model_starlark_pb.Select_Group{{NoMatch: &model_starlark_pb.Select_Group_NoMatchValue{NoMatchValue: value}}},
			}}})
			count, err := testShardCount(target, definition)
			if (err != nil) != tc.invalid || count != tc.want {
				t.Fatalf("shard count = %d, %v; want %d, invalid=%v", count, err, tc.want, tc.invalid)
			}
		})
	}
}

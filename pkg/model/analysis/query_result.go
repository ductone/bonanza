package analysis

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"bonanza.build/pkg/label"
	model_core "bonanza.build/pkg/model/core"
	"bonanza.build/pkg/model/evaluation"
	model_analysis_pb "bonanza.build/pkg/proto/model/analysis"
	model_starlark_pb "bonanza.build/pkg/proto/model/starlark"
	"bonanza.build/pkg/storage/object"
)

// ComputeQueryResultValue evaluates a "bazel query" expression.
//
// Evaluation happens here rather than on the client because the walks
// the language describes are over the whole loading-phase graph: an
// rdeps() visits every target in its universe, which for a repository of
// any size is not something a client can drive over the network.
//
// The set operations are computed over canonical label strings. Every
// function that needs more than a label -- kind(), attr() -- fetches the
// targets it needs in one pass before filtering, so a query costs a
// bounded number of evaluation rounds rather than one per graph level.
func (c *baseComputer[TReference, TMetadata]) ComputeQueryResultValue(ctx context.Context, key *model_analysis_pb.QueryResult_Key, e QueryResultEnvironment[TReference, TMetadata]) (PatchedQueryResultValue[TMetadata], error) {
	buildSpecification := e.GetBuildSpecificationValue(&model_analysis_pb.BuildSpecification_Key{})
	if !buildSpecification.IsSet() {
		return PatchedQueryResultValue[TMetadata]{}, evaluation.ErrMissingDependency
	}
	rootModule, err := label.NewModule(buildSpecification.Message.RootModuleName)
	if err != nil {
		return PatchedQueryResultValue[TMetadata]{}, fmt.Errorf("invalid root module name %#v: %w", buildSpecification.Message.RootModuleName, err)
	}

	ev := &queryEvaluator[TReference, TMetadata]{
		context:       ctx,
		computer:      c,
		environment:   e,
		rootRepo:      rootModule.ToModuleInstance(nil).GetBareCanonicalRepo(),
		labelResolver: newLabelResolver(e),
	}
	labels, err := ev.evaluate(key.Expression)
	if err != nil {
		return PatchedQueryResultValue[TMetadata]{}, err
	}
	if ev.missingDependencies {
		return PatchedQueryResultValue[TMetadata]{}, evaluation.ErrMissingDependency
	}

	// --output=label_kind wants the kind alongside the label, and
	// --output=label does not, but the difference is one string per
	// target and computing it here keeps the client from having to make
	// a second round trip per result.
	sorted := make([]string, 0, len(labels))
	for l := range labels {
		sorted = append(sorted, l)
	}
	sort.Strings(sorted)

	targets := make([]*model_analysis_pb.QueryResult_Value_Target, 0, len(sorted))
	for _, l := range sorted {
		kind, ok := ev.targetKind(l)
		if !ok {
			continue
		}
		targets = append(targets, &model_analysis_pb.QueryResult_Value_Target{
			Label: l,
			Kind:  kind,
		})
	}
	if ev.missingDependencies {
		return PatchedQueryResultValue[TMetadata]{}, evaluation.ErrMissingDependency
	}
	return model_core.NewSimplePatchedMessage[TMetadata](&model_analysis_pb.QueryResult_Value{
		Targets: targets,
	}), nil
}

type queryEvaluator[TReference object.BasicReference, TMetadata model_core.ReferenceMetadata] struct {
	context       context.Context
	computer      *baseComputer[TReference, TMetadata]
	environment   QueryResultEnvironment[TReference, TMetadata]
	rootRepo      label.CanonicalRepo
	labelResolver label.Resolver

	// missingDependencies records that at least one value the walk
	// needed was not available yet. Evaluation continues so that the
	// rest of the missing values are requested in the same round; the
	// caller turns this into ErrMissingDependency once.
	missingDependencies bool
}

func (ev *queryEvaluator[TReference, TMetadata]) evaluate(expression *model_analysis_pb.QueryExpression) (map[string]struct{}, error) {
	if expression == nil {
		return nil, fmt.Errorf("empty query expression")
	}
	switch x := expression.Expression.(type) {
	case *model_analysis_pb.QueryExpression_Pattern:
		return ev.expandPatterns([]string{x.Pattern})
	case *model_analysis_pb.QueryExpression_Set_:
		return ev.expandPatterns(x.Set.Patterns)
	case *model_analysis_pb.QueryExpression_Binary_:
		return ev.evaluateBinary(x.Binary)
	case *model_analysis_pb.QueryExpression_Deps_:
		return ev.evaluateDeps(x.Deps)
	case *model_analysis_pb.QueryExpression_Rdeps_:
		return ev.evaluateRdeps(x.Rdeps)
	case *model_analysis_pb.QueryExpression_Kind_:
		return ev.evaluateKind(x.Kind)
	case *model_analysis_pb.QueryExpression_Filter_:
		return ev.evaluateFilter(x.Filter)
	case *model_analysis_pb.QueryExpression_Attr_:
		return ev.evaluateAttr(x.Attr)
	}
	return nil, fmt.Errorf("unsupported query expression")
}

func (ev *queryEvaluator[TReference, TMetadata]) evaluateBinary(binary *model_analysis_pb.QueryExpression_Binary) (map[string]struct{}, error) {
	left, err := ev.evaluate(binary.Left)
	if err != nil {
		return nil, err
	}
	right, err := ev.evaluate(binary.Right)
	if err != nil {
		return nil, err
	}
	out := map[string]struct{}{}
	switch binary.Operator {
	case model_analysis_pb.QueryExpression_Binary_UNION:
		for l := range left {
			out[l] = struct{}{}
		}
		for l := range right {
			out[l] = struct{}{}
		}
	case model_analysis_pb.QueryExpression_Binary_EXCEPT:
		for l := range left {
			if _, ok := right[l]; !ok {
				out[l] = struct{}{}
			}
		}
	case model_analysis_pb.QueryExpression_Binary_INTERSECT:
		for l := range left {
			if _, ok := right[l]; ok {
				out[l] = struct{}{}
			}
		}
	default:
		return nil, fmt.Errorf("unsupported set operator %d", binary.Operator)
	}
	return out, nil
}

// expandPatterns turns apparent target patterns into the set of targets
// they match. Targets tagged "manual" are included, which is how query
// differs from build: a query asks what exists, not what a wildcard
// would build.
func (ev *queryEvaluator[TReference, TMetadata]) expandPatterns(patterns []string) (map[string]struct{}, error) {
	out := map[string]struct{}{}
	for _, pattern := range patterns {
		apparent, err := label.NewApparentTargetPattern(pattern)
		if err != nil {
			return nil, fmt.Errorf("invalid target pattern %#v: %w", pattern, err)
		}
		canonical, err := label.Canonicalize(ev.labelResolver, ev.rootRepo, apparent)
		if err != nil {
			return nil, err
		}
		var iterErr error
		for targetLabel := range ev.computer.expandCanonicalTargetPattern(
			ev.context,
			ev.environment,
			canonical,
			/* includeManualTargets = */ true,
			&iterErr,
		) {
			out[targetLabel.String()] = struct{}{}
		}
		if iterErr != nil {
			if !isMissingDependency(iterErr) {
				return nil, fmt.Errorf("failed to expand target pattern %#v: %w", pattern, iterErr)
			}
			ev.missingDependencies = true
		}
	}
	return out, nil
}

// dependenciesOf fetches the loading-phase dependencies of a set of
// targets in one round, so a graph walk costs one evaluation round per
// level rather than one per target.
func (ev *queryEvaluator[TReference, TMetadata]) dependenciesOf(labels map[string]struct{}) map[string][]string {
	out := make(map[string][]string, len(labels))
	for l := range labels {
		value := ev.environment.GetTargetDependenciesValue(&model_analysis_pb.TargetDependencies_Key{Label: l})
		if !value.IsSet() {
			ev.missingDependencies = true
			continue
		}
		out[l] = value.Message.Labels
	}
	return out
}

func (ev *queryEvaluator[TReference, TMetadata]) evaluateDeps(deps *model_analysis_pb.QueryExpression_Deps) (map[string]struct{}, error) {
	universe, err := ev.evaluate(deps.Universe)
	if err != nil {
		return nil, err
	}
	// Forward reachability. The frontier is expanded a level at a time,
	// and each level is fetched in one round.
	reached := map[string]struct{}{}
	frontier := universe
	for depth := 0; len(frontier) > 0 && (deps.Depth < 0 || depth <= int(deps.Depth)); depth++ {
		next := map[string]struct{}{}
		edges := ev.dependenciesOf(frontier)
		for l := range frontier {
			reached[l] = struct{}{}
		}
		if deps.Depth >= 0 && depth == int(deps.Depth) {
			break
		}
		for _, targets := range edges {
			for _, t := range targets {
				if _, ok := reached[t]; !ok {
					next[t] = struct{}{}
				}
			}
		}
		frontier = next
	}
	return reached, nil
}

func (ev *queryEvaluator[TReference, TMetadata]) evaluateRdeps(rdeps *model_analysis_pb.QueryExpression_Rdeps) (map[string]struct{}, error) {
	universe, err := ev.evaluate(rdeps.Universe)
	if err != nil {
		return nil, err
	}
	targets, err := ev.evaluate(rdeps.Targets)
	if err != nil {
		return nil, err
	}

	// The whole universe is fetched in one round and reversed in
	// memory. rdeps() is bounded by the universe, so an edge leaving it
	// cannot bring a target back in and is dropped here.
	edges := ev.dependenciesOf(universe)
	if ev.missingDependencies {
		return nil, nil
	}
	reverse := make(map[string][]string, len(edges))
	for from, tos := range edges {
		for _, to := range tos {
			reverse[to] = append(reverse[to], from)
		}
	}

	reached := map[string]struct{}{}
	frontier := map[string]struct{}{}
	for t := range targets {
		// A target outside the universe is still a valid starting
		// point: rdeps(//pkg/..., //some:file) asks which targets in
		// //pkg/... reach that file, and the file itself need not be
		// in //pkg/....
		frontier[t] = struct{}{}
		if _, ok := universe[t]; ok {
			reached[t] = struct{}{}
		}
	}
	for depth := 0; len(frontier) > 0 && (rdeps.Depth < 0 || depth < int(rdeps.Depth)); depth++ {
		next := map[string]struct{}{}
		for l := range frontier {
			for _, from := range reverse[l] {
				if _, ok := reached[from]; !ok {
					reached[from] = struct{}{}
					next[from] = struct{}{}
				}
			}
		}
		frontier = next
	}
	return reached, nil
}

func (ev *queryEvaluator[TReference, TMetadata]) evaluateFilter(filter *model_analysis_pb.QueryExpression_Filter) (map[string]struct{}, error) {
	targets, err := ev.evaluate(filter.Targets)
	if err != nil {
		return nil, err
	}
	re, err := regexp.Compile(filter.Pattern)
	if err != nil {
		return nil, fmt.Errorf("filter(): invalid regular expression %#v: %w", filter.Pattern, err)
	}
	out := map[string]struct{}{}
	for l := range targets {
		if re.MatchString(l) {
			out[l] = struct{}{}
		}
	}
	return out, nil
}

func (ev *queryEvaluator[TReference, TMetadata]) evaluateKind(kind *model_analysis_pb.QueryExpression_Kind) (map[string]struct{}, error) {
	targets, err := ev.evaluate(kind.Targets)
	if err != nil {
		return nil, err
	}
	re, err := regexp.Compile(kind.Pattern)
	if err != nil {
		return nil, fmt.Errorf("kind(): invalid regular expression %#v: %w", kind.Pattern, err)
	}
	out := map[string]struct{}{}
	for l := range targets {
		k, ok := ev.targetKind(l)
		if !ok {
			continue
		}
		if re.MatchString(k) {
			out[l] = struct{}{}
		}
	}
	return out, nil
}

// targetKind reports a target's kind in the form
// "--output=label_kind" prints: "go_library rule", "source file".
func (ev *queryEvaluator[TReference, TMetadata]) targetKind(targetLabel string) (string, bool) {
	targetValue := ev.environment.GetTargetValue(&model_analysis_pb.Target_Key{Label: targetLabel})
	if !targetValue.IsSet() {
		ev.missingDependencies = true
		return "", false
	}
	switch definition := targetValue.Message.Definition.GetKind().(type) {
	case *model_starlark_pb.Target_Definition_RuleTarget:
		ruleIdentifier := definition.RuleTarget.RuleIdentifier
		if i := strings.LastIndexByte(ruleIdentifier, '%'); i >= 0 {
			ruleIdentifier = ruleIdentifier[i+1:]
		}
		if ruleIdentifier == "" {
			// An anonymous rule, as testing.analysis_test()
			// synthesizes.
			ruleIdentifier = "analysis_test"
		}
		return ruleIdentifier + " rule", true
	case *model_starlark_pb.Target_Definition_SourceFileTarget:
		return "source file", true
	case *model_starlark_pb.Target_Definition_PredeclaredOutputFileTarget:
		return "generated file", true
	case *model_starlark_pb.Target_Definition_Alias:
		return "alias rule", true
	case *model_starlark_pb.Target_Definition_PackageGroup:
		return "package_group rule", true
	}
	return "", false
}

func (ev *queryEvaluator[TReference, TMetadata]) evaluateAttr(attr *model_analysis_pb.QueryExpression_Attr) (map[string]struct{}, error) {
	targets, err := ev.evaluate(attr.Targets)
	if err != nil {
		return nil, err
	}
	re, err := regexp.Compile(attr.Pattern)
	if err != nil {
		return nil, fmt.Errorf("attr(): invalid regular expression %#v: %w", attr.Pattern, err)
	}
	out := map[string]struct{}{}
	for l := range targets {
		matched, ok := ev.targetAttrMatches(l, attr.Name, re)
		if !ok {
			continue
		}
		if matched {
			out[l] = struct{}{}
		}
	}
	return out, nil
}

// targetAttrMatches reports whether a target's named attribute has a
// value matching the pattern.
//
// public_attr_values is positional against the rule's public attributes,
// so the rule definition is what gives the position a name. Every branch
// of a select() is considered, on the same terms as TargetDependencies:
// the loading phase has no configuration with which to pick one.
func (ev *queryEvaluator[TReference, TMetadata]) targetAttrMatches(targetLabel, attrName string, re *regexp.Regexp) (bool, bool) {
	targetValue := ev.environment.GetTargetValue(&model_analysis_pb.Target_Key{Label: targetLabel})
	if !targetValue.IsSet() {
		ev.missingDependencies = true
		return false, false
	}
	definition, ok := targetValue.Message.Definition.GetKind().(*model_starlark_pb.Target_Definition_RuleTarget)
	if !ok {
		return false, true
	}
	ruleIdentifier := definition.RuleTarget.RuleIdentifier
	if ruleIdentifier == "" {
		return false, true
	}
	ruleValue := ev.environment.GetCompiledBzlFileGlobalValue(&model_analysis_pb.CompiledBzlFileGlobal_Key{
		Identifier: ruleIdentifier,
	})
	if !ruleValue.IsSet() {
		ev.missingDependencies = true
		return false, false
	}
	ruleKind, ok := ruleValue.Message.Global.GetKind().(*model_starlark_pb.Value_Rule)
	if !ok {
		return false, true
	}
	ruleDefinition, ok := ruleKind.Rule.Kind.(*model_starlark_pb.Rule_Definition_)
	if !ok {
		return false, true
	}

	// Public attributes are the ones whose names do not start with an
	// underscore, in declaration order, which is the order
	// public_attr_values follows.
	index := -1
	position := 0
	for _, namedAttr := range ruleDefinition.Definition.Attrs {
		if strings.HasPrefix(namedAttr.Name, "_") {
			continue
		}
		if namedAttr.Name == attrName {
			index = position
			break
		}
		position++
	}
	if index < 0 || index >= len(definition.RuleTarget.PublicAttrValues) {
		return false, true
	}

	matched := false
	for _, group := range definition.RuleTarget.PublicAttrValues[index].ValueParts {
		if selectGroupMatches(group, re) {
			matched = true
			break
		}
	}
	return matched, true
}

func selectGroupMatches(group *model_starlark_pb.Select_Group, re *regexp.Regexp) bool {
	if group == nil {
		return false
	}
	for _, condition := range group.Conditions {
		if valueMatches(condition.Value, re) {
			return true
		}
	}
	if noMatch, ok := group.NoMatch.(*model_starlark_pb.Select_Group_NoMatchValue); ok {
		return valueMatches(noMatch.NoMatchValue, re)
	}
	return false
}

// valueMatches matches the scalar forms an attribute value takes. A list
// whose elements live in an external object is not followed: doing so
// needs the list reader, and an attribute large enough to be stored
// externally is not one a caller filters on. It reports no match rather
// than guessing.
func valueMatches(value *model_starlark_pb.Value, re *regexp.Regexp) bool {
	if value == nil {
		return false
	}
	switch kind := value.Kind.(type) {
	case *model_starlark_pb.Value_Str:
		return re.MatchString(kind.Str)
	case *model_starlark_pb.Value_Label:
		return re.MatchString(kind.Label)
	case *model_starlark_pb.Value_List:
		for _, element := range kind.List.Elements {
			if leaf, ok := element.Level.(*model_starlark_pb.List_Element_Leaf); ok {
				if valueMatches(leaf.Leaf, re) {
					return true
				}
			}
		}
	}
	return false
}

func isMissingDependency(err error) bool {
	return err != nil && strings.Contains(err.Error(), evaluation.ErrMissingDependency.Error())
}

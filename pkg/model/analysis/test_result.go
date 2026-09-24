package analysis

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"bonanza.build/pkg/label"
	model_core "bonanza.build/pkg/model/core"
	"bonanza.build/pkg/model/evaluation"
	model_analysis_pb "bonanza.build/pkg/proto/model/analysis"
)

// validateTestTagFilters rejects malformed filters even if no targets match.
func validateTestTagFilters(filters []string) error {
	for _, filter := range filters {
		tag := strings.TrimPrefix(filter, "-")
		if tag == "" || strings.ContainsAny(tag, ", \t\n") {
			return fmt.Errorf("invalid test tag filter %q", filter)
		}
	}
	return nil
}

// testTagsMatch implements Bazel's positive-OR, negative-AND tag selection.
func testTagsMatch(tags, filters []string) bool {
	positive, matchedPositive := false, false
	for _, filter := range filters {
		negative := strings.HasPrefix(filter, "-")
		tag := strings.TrimPrefix(filter, "-")
		match := slices.Contains(tags, tag)
		if negative && match {
			return false
		}
		if !negative {
			positive = true
			matchedPositive = matchedPositive || match
		}
	}
	return !positive || matchedPositive
}

func (c *baseComputer[TReference, TMetadata]) ComputeTestResultValue(ctx context.Context, key *model_analysis_pb.TestResult_Key, e TestResultEnvironment[TReference, TMetadata]) (PatchedTestResultValue[TMetadata], error) {
	if err := validateTestTagFilters(key.TestTagFilters); err != nil {
		return PatchedTestResultValue[TMetadata]{}, err
	}
	buildSpecificationMessage := e.GetBuildSpecificationValue(&model_analysis_pb.BuildSpecification_Key{})
	if !buildSpecificationMessage.IsSet() {
		return PatchedTestResultValue[TMetadata]{}, evaluation.ErrMissingDependency
	}
	rootModuleName := buildSpecificationMessage.Message.RootModuleName
	rootModule, err := label.NewModule(rootModuleName)
	if err != nil {
		return PatchedTestResultValue[TMetadata]{}, fmt.Errorf("invalid root module name %#v: %w", rootModuleName, err)
	}
	rootRepo := rootModule.ToModuleInstance(nil).GetBareCanonicalRepo()
	rootPackage := rootRepo.GetRootPackage()
	thread := c.newStarlarkThread(ctx, e, buildSpecificationMessage.Message.BuiltinsModuleNames)
	labelResolver := newLabelResolver(e)
	type testCandidate struct {
		target        label.CanonicalLabel
		from          label.CanonicalPackage
		suiteMember   bool
		packageMember bool
	}
	missingDependencies := false
	patchedTests := model_core.NewPatchedMessage(
		[]*model_analysis_pb.TestResult_Value_Test(nil),
		model_core.NewReferenceMessagePatcher[TMetadata](),
	)

	for i, configuration := range key.Configurations {
		targetPlatformConfigurationReference, err := c.createInitialConfiguration(ctx, e, thread, rootPackage, configuration)
		if err != nil {
			if !errors.Is(err, evaluation.ErrMissingDependency) {
				return PatchedTestResultValue[TMetadata]{}, fmt.Errorf("failed to create initial configuration for configuration at index %d: %w", i, err)
			}
			missingDependencies = true
			continue
		}
		clonedConfigurationReference := model_core.Unpatch(e, targetPlatformConfigurationReference).Decay()
		testedLabels := map[string]struct{}{}
		visitedSuites := map[string]struct{}{}
		for _, targetPattern := range key.TargetPatterns {
			apparentTargetPattern, err := label.NewApparentTargetPattern(targetPattern)
			if err != nil {
				return PatchedTestResultValue[TMetadata]{}, fmt.Errorf("invalid target pattern %#v: %w", targetPattern, err)
			}
			canonicalTargetPattern, err := label.Canonicalize(labelResolver, rootRepo, apparentTargetPattern)
			if err != nil {
				return PatchedTestResultValue[TMetadata]{}, err
			}
			var iterErr error
			for targetLabel := range c.expandCanonicalTargetPattern(ctx, e, canonicalTargetPattern, false, &iterErr) {
				// Expanding a suite may discover more suites. Keep the queue
				// local to each matched target so errors retain context.
				queue := []testCandidate{{target: targetLabel, from: targetLabel.GetCanonicalPackage()}}
				for len(queue) != 0 {
					candidate := queue[0]
					queue = queue[1:]
					current := candidate.target
					visibleTarget := e.GetVisibleTargetValue(model_core.MustBuildPatchedMessage(func(patcher *model_core.ReferenceMessagePatcher[TMetadata]) *model_analysis_pb.VisibleTarget_Key {
						return &model_analysis_pb.VisibleTarget_Key{
							FromPackage:            candidate.from.String(),
							ToLabel:                current.String(),
							ConfigurationReference: model_core.Patch(e, clonedConfigurationReference).Merge(patcher),
						}
					}))
					if !visibleTarget.IsSet() {
						missingDependencies = true
						continue
					}
					visibleLabel := visibleTarget.Message.Label
					if _, done := testedLabels[visibleLabel]; done {
						continue
					}
					target := e.GetTargetValue(&model_analysis_pb.Target_Key{Label: visibleLabel})
					if !target.IsSet() {
						missingDependencies = true
						continue
					}
					ruleTarget := target.Message.Definition.GetRuleTarget()
					if ruleTarget == nil {
						if candidate.suiteMember {
							return PatchedTestResultValue[TMetadata]{}, fmt.Errorf("test suite member %q is not a test", visibleLabel)
						}
						testedLabels[visibleLabel] = struct{}{}
						continue
					}
					if ruleTarget.RuleIdentifier == testSuiteRuleIdentifier {
						// An empty suite includes test rules in its own package,
						// not other suites and their possibly cross-package tests.
						if candidate.packageMember {
							continue
						}
						suiteLabel, err := label.NewCanonicalLabel(visibleLabel)
						if err != nil {
							return PatchedTestResultValue[TMetadata]{}, err
						}
						if _, seen := visitedSuites[visibleLabel]; seen {
							continue
						}
						visitedSuites[visibleLabel] = struct{}{}
						definition, err := getRuleDefinition(e, visibleLabel)
						if err != nil {
							if errors.Is(err, evaluation.ErrMissingDependency) {
								missingDependencies = true
								continue
							}
							return PatchedTestResultValue[TMetadata]{}, err
						}
						tests, err := getTestRuleAttribute(model_core.Nested(target, ruleTarget), definition, "tests")
						if err != nil {
							return PatchedTestResultValue[TMetadata]{}, fmt.Errorf("test suite %#v: %w", visibleLabel, err)
						}
						members := map[string]struct{}{}
						if err := c.collectLabelsFromValue(ctx, tests, suiteLabel.GetCanonicalPackage(), members); err != nil {
							return PatchedTestResultValue[TMetadata]{}, fmt.Errorf("test suite %#v: %w", visibleLabel, err)
						}
						if len(members) == 0 {
							packagePattern, err := label.NewCanonicalTargetPattern(suiteLabel.GetCanonicalPackage().String() + ":all")
							if err != nil {
								return PatchedTestResultValue[TMetadata]{}, err
							}
							var packageErr error
							for member := range c.expandCanonicalTargetPattern(ctx, e, packagePattern, false, &packageErr) {
								if member.String() != visibleLabel {
									queue = append(queue, testCandidate{target: member, from: suiteLabel.GetCanonicalPackage(), packageMember: true})
								}
							}
							if packageErr != nil {
								if !errors.Is(packageErr, evaluation.ErrMissingDependency) {
									return PatchedTestResultValue[TMetadata]{}, packageErr
								}
								missingDependencies = true
							}
						} else {
							for _, member := range slices.Sorted(maps.Keys(members)) {
								memberLabel, err := label.NewCanonicalLabel(member)
								if err != nil {
									return PatchedTestResultValue[TMetadata]{}, fmt.Errorf("invalid test suite member %q in %q: %w", member, visibleLabel, err)
								}
								queue = append(queue, testCandidate{target: memberLabel, from: suiteLabel.GetCanonicalPackage(), suiteMember: true})
							}
						}
						continue
					}
					definition, err := getRuleDefinition(e, visibleLabel)
					if err != nil {
						if errors.Is(err, evaluation.ErrMissingDependency) {
							missingDependencies = true
							continue
						}
						return PatchedTestResultValue[TMetadata]{}, err
					}
					if !definition.IsSet() || !definition.Message.Test {
						if candidate.suiteMember {
							return PatchedTestResultValue[TMetadata]{}, fmt.Errorf("test suite member %q is not a test", visibleLabel)
						}
						testedLabels[visibleLabel] = struct{}{}
						continue
					}
					if !testTagsMatch(ruleTarget.Tags, key.TestTagFilters) {
						testedLabels[visibleLabel] = struct{}{}
						continue
					}
					count, err := testShardCount(model_core.Nested(target, ruleTarget), definition)
					if err != nil {
						return PatchedTestResultValue[TMetadata]{}, fmt.Errorf("test %#v: %w", visibleLabel, err)
					}
					for shard := uint32(0); shard < count; shard++ {
						result := e.GetTargetTestResultValue(model_core.MustBuildPatchedMessage(func(patcher *model_core.ReferenceMessagePatcher[TMetadata]) *model_analysis_pb.TargetTestResult_Key {
							return &model_analysis_pb.TargetTestResult_Key{
								Label:                  visibleLabel,
								ConfigurationReference: model_core.Patch(e, clonedConfigurationReference).Merge(patcher),
								TestFilter:             key.TestFilter,
								ShardIndex:             shard,
								ShardCount:             count,
							}
						}))
						if !result.IsSet() {
							missingDependencies = true
							continue
						}
						outputsReference := model_core.Patch(e, model_core.Nested(result, result.Message.OutputsReference))
						patchedTests.Message = append(patchedTests.Message, &model_analysis_pb.TestResult_Value_Test{
							Label:            visibleLabel,
							Status:           result.Message.Status,
							ExitCode:         result.Message.ExitCode,
							OutputsReference: outputsReference.Merge(patchedTests.Patcher),
							ShardIndex:       shard,
							ShardCount:       count,
						})
					}
					testedLabels[visibleLabel] = struct{}{}
				}
			}
			if iterErr != nil {
				if !errors.Is(iterErr, evaluation.ErrMissingDependency) {
					return PatchedTestResultValue[TMetadata]{}, fmt.Errorf("failed to iterate target pattern %#v: %w", targetPattern, iterErr)
				}
				missingDependencies = true
			}
		}
	}
	if missingDependencies {
		return PatchedTestResultValue[TMetadata]{}, evaluation.ErrMissingDependency
	}
	return model_core.NewPatchedMessage(
		&model_analysis_pb.TestResult_Value{Tests: patchedTests.Message},
		patchedTests.Patcher,
	), nil
}

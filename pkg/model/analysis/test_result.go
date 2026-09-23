package analysis

import (
	"context"
	"errors"
	"fmt"

	"bonanza.build/pkg/label"
	model_core "bonanza.build/pkg/model/core"
	"bonanza.build/pkg/model/evaluation"
	model_analysis_pb "bonanza.build/pkg/proto/model/analysis"
)

func (c *baseComputer[TReference, TMetadata]) ComputeTestResultValue(ctx context.Context, key *model_analysis_pb.TestResult_Key, e TestResultEnvironment[TReference, TMetadata]) (PatchedTestResultValue[TMetadata], error) {
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
	missingDependencies := false
	labelResolver := newLabelResolver(e)

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
			for canonicalTargetLabel := range c.expandCanonicalTargetPattern(
				ctx,
				e,
				canonicalTargetPattern,
				/* includeManualTargets = */ false,
				&iterErr,
			) {
				visibleTargetValue := e.GetVisibleTargetValue(
					model_core.MustBuildPatchedMessage(func(patcher *model_core.ReferenceMessagePatcher[TMetadata]) *model_analysis_pb.VisibleTarget_Key {
						return &model_analysis_pb.VisibleTarget_Key{
							FromPackage:            canonicalTargetLabel.GetCanonicalPackage().String(),
							ToLabel:                canonicalTargetLabel.String(),
							ConfigurationReference: model_core.Patch(e, clonedConfigurationReference).Merge(patcher),
						}
					}),
				)
				if !visibleTargetValue.IsSet() {
					missingDependencies = true
					continue
				}
				visibleLabel := visibleTargetValue.Message.Label
				if _, ok := testedLabels[visibleLabel]; ok {
					continue
				}

				// Skip everything that is not a test rule.
				// Target patterns commonly match a mixture
				// of tests and other targets, and only the
				// tests should be run.
				ruleDefinition, err := getRuleDefinition(e, visibleLabel)
				if err != nil {
					if !errors.Is(err, evaluation.ErrMissingDependency) {
						return PatchedTestResultValue[TMetadata]{}, err
					}
					missingDependencies = true
					continue
				}
				if !ruleDefinition.IsSet() || !ruleDefinition.Message.Test {
					testedLabels[visibleLabel] = struct{}{}
					continue
				}

				testResultValue := e.GetTargetTestResultValue(
					model_core.MustBuildPatchedMessage(func(patcher *model_core.ReferenceMessagePatcher[TMetadata]) *model_analysis_pb.TargetTestResult_Key {
						return &model_analysis_pb.TargetTestResult_Key{
							Label:                  visibleLabel,
							ConfigurationReference: model_core.Patch(e, clonedConfigurationReference).Merge(patcher),
							TestFilter:             key.TestFilter,
						}
					}),
				)
				if !testResultValue.IsSet() {
					missingDependencies = true
					continue
				}
				testedLabels[visibleLabel] = struct{}{}

				outputsReference := model_core.Patch(e, model_core.Nested(testResultValue, testResultValue.Message.OutputsReference))
				patchedTests.Message = append(patchedTests.Message, &model_analysis_pb.TestResult_Value_Test{
					Label:            visibleLabel,
					Status:           testResultValue.Message.Status,
					ExitCode:         testResultValue.Message.ExitCode,
					OutputsReference: outputsReference.Merge(patchedTests.Patcher),
				})
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
		&model_analysis_pb.TestResult_Value{
			Tests: patchedTests.Message,
		},
		patchedTests.Patcher,
	), nil
}

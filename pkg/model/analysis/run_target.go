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

func (c *baseComputer[TReference, TMetadata]) ComputeRunTargetValue(ctx context.Context, key *model_analysis_pb.RunTarget_Key, e RunTargetEnvironment[TReference, TMetadata]) (PatchedRunTargetValue[TMetadata], error) {
	buildSpecificationMessage := e.GetBuildSpecificationValue(&model_analysis_pb.BuildSpecification_Key{})
	if !buildSpecificationMessage.IsSet() {
		return PatchedRunTargetValue[TMetadata]{}, evaluation.ErrMissingDependency
	}
	rootModuleName := buildSpecificationMessage.Message.RootModuleName
	rootModule, err := label.NewModule(rootModuleName)
	if err != nil {
		return PatchedRunTargetValue[TMetadata]{}, fmt.Errorf("invalid root module name %#v: %w", rootModuleName, err)
	}
	rootRepo := rootModule.ToModuleInstance(nil).GetBareCanonicalRepo()

	apparentTargetPattern, err := label.NewApparentTargetPattern(key.TargetPattern)
	if err != nil {
		return PatchedRunTargetValue[TMetadata]{}, fmt.Errorf("invalid target pattern %#v: %w", key.TargetPattern, err)
	}
	canonicalTargetPattern, err := label.Canonicalize(newLabelResolver(e), rootRepo, apparentTargetPattern)
	if err != nil {
		return PatchedRunTargetValue[TMetadata]{}, err
	}

	// "bazel run" can only launch a single executable, so refuse to
	// proceed if the target pattern is ambiguous.
	var canonicalTargetLabel label.CanonicalLabel
	gotCanonicalTargetLabel := false
	var errIter error
	for l := range c.expandCanonicalTargetPattern(
		ctx,
		e,
		canonicalTargetPattern,
		/* includeManualTargets = */ true,
		&errIter,
	) {
		if gotCanonicalTargetLabel {
			return PatchedRunTargetValue[TMetadata]{}, fmt.Errorf("target pattern %#v expands to more than one target", key.TargetPattern)
		}
		canonicalTargetLabel = l
		gotCanonicalTargetLabel = true
	}
	if errIter != nil {
		return PatchedRunTargetValue[TMetadata]{}, errIter
	}
	if !gotCanonicalTargetLabel {
		return PatchedRunTargetValue[TMetadata]{}, fmt.Errorf("target pattern %#v does not expand to any targets", key.TargetPattern)
	}

	// Construct the same configuration that BuildResult would use,
	// so that the target that is launched is the very same one that
	// the build materialized.
	thread := c.newStarlarkThread(ctx, e, buildSpecificationMessage.Message.BuiltinsModuleNames)
	configurationReference, err := c.createInitialConfiguration(ctx, e, thread, rootRepo.GetRootPackage(), key.Configuration)
	if err != nil {
		return PatchedRunTargetValue[TMetadata]{}, err
	}
	clonedConfigurationReference := model_core.Unpatch(e, configurationReference).Decay()

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
		return PatchedRunTargetValue[TMetadata]{}, evaluation.ErrMissingDependency
	}

	targetCompletionValue := e.GetTargetCompletionValue(
		model_core.MustBuildPatchedMessage(func(patcher *model_core.ReferenceMessagePatcher[TMetadata]) *model_analysis_pb.TargetCompletion_Key {
			return &model_analysis_pb.TargetCompletion_Key{
				Label:                  visibleTargetValue.Message.Label,
				ConfigurationReference: model_core.Patch(e, clonedConfigurationReference).Merge(patcher),
			}
		}),
	)
	if !targetCompletionValue.IsSet() {
		return PatchedRunTargetValue[TMetadata]{}, evaluation.ErrMissingDependency
	}
	if targetCompletionValue.Message.ExecutablePath == "" {
		return PatchedRunTargetValue[TMetadata]{}, errors.New("target does not provide an executable")
	}

	runfilesDirectory := model_core.Patch(e, model_core.Nested(targetCompletionValue, targetCompletionValue.Message.RunfilesDirectory))
	return model_core.NewPatchedMessage(
		&model_analysis_pb.RunTarget_Value{
			ExecutablePath:    targetCompletionValue.Message.ExecutablePath,
			RunfilesDirectory: runfilesDirectory.Message,
			WorkspaceName:     rootRepo.String(),
		},
		runfilesDirectory.Patcher,
	), nil
}

package analysis

import (
	"context"

	model_core "bonanza.build/pkg/model/core"
	"bonanza.build/pkg/model/evaluation"
	model_analysis_pb "bonanza.build/pkg/proto/model/analysis"
)

func (baseComputer[TReference, TMetadata]) ComputeCompletedActionResultValue(ctx context.Context, key model_core.Message[*model_analysis_pb.CompletedActionResult_Key, TReference], e CompletedActionResultEnvironment[TReference, TMetadata]) (PatchedCompletedActionResultValue[TMetadata], error) {
	patchedExecuteRequest := model_core.Patch(e, model_core.Nested(key, key.Message.ExecuteRequest))
	actionResult := e.GetActionResultValue(
		model_core.NewPatchedMessage(
			&model_analysis_pb.ActionResult_Key{
				ExecuteRequest: patchedExecuteRequest.Message,
			},
			patchedExecuteRequest.Patcher,
		),
	)
	if !actionResult.IsSet() {
		return PatchedCompletedActionResultValue[TMetadata]{}, evaluation.ErrMissingDependency
	}

	patchedOutputsReference := model_core.Patch(e, model_core.Nested(actionResult, actionResult.Message.OutputsReference))
	return model_core.NewPatchedMessage(
		&model_analysis_pb.CompletedActionResult_Value{
			ExitCode:         actionResult.Message.ExitCode,
			OutputsReference: patchedOutputsReference.Message,
		},
		patchedOutputsReference.Patcher,
	), nil
}

// CompletedActionResultEnvironmentForTesting is an instance of
// CompletedActionResultEnvironment that is used by tests.
type CompletedActionResultEnvironmentForTesting CompletedActionResultEnvironment[model_core.CreatedObjectTree, model_core.CreatedObjectTree]

package analysis_test

import (
	"context"
	"encoding"
	"testing"

	model_core "bonanza.build/pkg/model/core"
	"bonanza.build/pkg/model/evaluation"
	model_analysis_pb "bonanza.build/pkg/proto/model/analysis"
	model_command_pb "bonanza.build/pkg/proto/model/command"
	model_filesystem_pb "bonanza.build/pkg/proto/model/filesystem"

	"github.com/stretchr/testify/require"

	"go.uber.org/mock/gomock"
	"google.golang.org/protobuf/types/known/durationpb"
)

func TestComputeCompletedActionResultValue(t *testing.T) {
	ctrl, ctx := gomock.WithContext(context.Background(), t)
	bct := newBaseComputerTester(ctrl)

	actionObject := newObject(func(patcher *model_core.ReferenceMessagePatcher[model_core.CreatedObjectTree]) encoding.BinaryMarshaler {
		return model_core.NewProtoBinaryMarshaler(&model_command_pb.Action{
			InputRootReference: &model_filesystem_pb.DirectoryReference{DirectoriesCount: 1},
		})
	})
	outputsObject := newObject(func(patcher *model_core.ReferenceMessagePatcher[model_core.CreatedObjectTree]) encoding.BinaryMarshaler {
		return model_core.NewProtoBinaryMarshaler(&model_command_pb.Outputs{
			Stdout: &model_filesystem_pb.FileContents{TotalSizeBytes: 5},
		})
	})
	newKey := func(patcher *model_core.ReferenceMessagePatcher[model_core.CreatedObjectTree]) *model_analysis_pb.CompletedActionResult_Key {
		return &model_analysis_pb.CompletedActionResult_Key{
			ExecuteRequest: &model_analysis_pb.ExecuteRequest{
				PlatformPkixPublicKey: []byte{0x1b, 0x51, 0x66, 0x2f},
				ActionReference:       attachObject(patcher, actionObject),
				ExecutionTimeout:      &durationpb.Duration{Seconds: 900},
			},
		}
	}

	// The exit code and the outputs of the action are passed through
	// unchanged, no matter what the action terminated with. A test
	// that fails is a result the user asked for, and one that has to
	// remain cacheable so that unchanged tests are not re-run.
	for name, exitCode := range map[string]int64{
		"Success": 0,
		"Failure": 42,
	} {
		t.Run(name, func(t *testing.T) {
			e := NewMockCompletedActionResultEnvironmentForTesting(ctrl)
			e.EXPECT().CaptureExistingObject(gomock.Any()).
				DoAndReturn(func(reference model_core.CreatedObjectTree) model_core.CreatedObjectTree {
					return reference
				}).
				AnyTimes()
			e.EXPECT().GetActionResultValue(
				eqPatchedMessage(func(patcher *model_core.ReferenceMessagePatcher[model_core.CreatedObjectTree]) *model_analysis_pb.ActionResult_Key {
					return &model_analysis_pb.ActionResult_Key{
						ExecuteRequest: newKey(patcher).ExecuteRequest,
					}
				}),
			).Return(newMessage(func(patcher *model_core.ReferenceMessagePatcher[model_core.CreatedObjectTree]) *model_analysis_pb.ActionResult_Value {
				return &model_analysis_pb.ActionResult_Value{
					ExitCode:         exitCode,
					OutputsReference: attachObject(patcher, outputsObject),
				}
			}))

			value, err := bct.computer.ComputeCompletedActionResultValue(
				ctx,
				newMessage(newKey),
				e,
			)
			require.NoError(t, err)
			requireEqualPatchedMessage(
				t,
				func(patcher *model_core.ReferenceMessagePatcher[model_core.CreatedObjectTree]) *model_analysis_pb.CompletedActionResult_Value {
					return &model_analysis_pb.CompletedActionResult_Value{
						ExitCode:         exitCode,
						OutputsReference: attachObject(patcher, outputsObject),
					}
				},
				value,
			)
		})
	}

	t.Run("MissingActionResult", func(t *testing.T) {
		e := NewMockCompletedActionResultEnvironmentForTesting(ctrl)
		e.EXPECT().CaptureExistingObject(gomock.Any()).
			DoAndReturn(func(reference model_core.CreatedObjectTree) model_core.CreatedObjectTree {
				return reference
			}).
			AnyTimes()
		e.EXPECT().GetActionResultValue(gomock.Any()).
			Return(model_core.Message[*model_analysis_pb.ActionResult_Value, model_core.CreatedObjectTree]{})

		_, err := bct.computer.ComputeCompletedActionResultValue(ctx, newMessage(newKey), e)
		require.ErrorIs(t, err, evaluation.ErrMissingDependency)
	})
}

package analysis_test

import (
	"testing"

	model_analysis "bonanza.build/pkg/model/analysis"
	model_core "bonanza.build/pkg/model/core"
	model_filesystem "bonanza.build/pkg/model/filesystem"
	model_parser "bonanza.build/pkg/model/parser"
	model_analysis_pb "bonanza.build/pkg/proto/model/analysis"
	model_filesystem_pb "bonanza.build/pkg/proto/model/filesystem"
	model_starlark_pb "bonanza.build/pkg/proto/model/starlark"
	"bonanza.build/pkg/storage/object"

	"github.com/buildbarn/bb-storage/pkg/testutil"
	"github.com/buildbarn/bb-storage/pkg/util"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

func TestTargetCompletionOutputGroups(t *testing.T) {
	ctrl, ctx := gomock.WithContext(t.Context(), t)
	bct := newBaseComputerTester(ctrl)
	e := NewMockTargetCompletionEnvironmentForTesting(ctrl)
	directoryParameters := util.Must(model_filesystem.NewDirectoryCreationParametersFromProto(
		&model_filesystem_pb.DirectoryCreationParameters{
			Access:                    &model_filesystem_pb.DirectoryAccessParameters{},
			DirectoryMaximumSizeBytes: 1 << 16,
		}, object.SHA256V1ReferenceFormat,
	))
	e.EXPECT().GetDirectoryCreationParametersObjectValue(testutil.EqProto(t, &model_analysis_pb.DirectoryCreationParametersObject_Key{})).
		Return(directoryParameters, true)
	e.EXPECT().GetDirectoryReadersValue(testutil.EqProto(t, &model_analysis_pb.DirectoryReaders_Key{})).
		Return(&model_analysis.DirectoryReaders[model_core.CreatedObjectTree]{
			DirectoryContents: model_parser.LookupParsedObjectReader(
				bct.parsedObjectPoolIngester,
				model_parser.NewProtoObjectParser[model_core.CreatedObjectTree, model_filesystem_pb.DirectoryContents](),
			),
		}, true)

	manifest := &model_starlark_pb.File{Label: "@@main+//:manifest.json"}
	provider := &model_analysis_pb.TargetProvider_Value{
		Fields: &model_starlark_pb.Struct_Fields{
			Keys: []string{"mtree"},
			Values: []*model_starlark_pb.List_Element{{Level: &model_starlark_pb.List_Element_Leaf{
				Leaf: &model_starlark_pb.Value{Kind: &model_starlark_pb.Value_Depset{
					Depset: &model_starlark_pb.Depset{Elements: []*model_starlark_pb.List_Element{{Level: &model_starlark_pb.List_Element_Leaf{
						Leaf: &model_starlark_pb.Value{Kind: &model_starlark_pb.Value_File{File: manifest}},
					}}}},
				}},
			}}},
		},
	}
	e.EXPECT().GetTargetProviderValue(eqPatchedMessage(func(*model_core.ReferenceMessagePatcher[model_core.CreatedObjectTree]) *model_analysis_pb.TargetProvider_Key {
		return &model_analysis_pb.TargetProvider_Key{
			Label: "@@main+//:image", ProviderIdentifier: "@@builtins_core+//:exports.bzl%OutputGroupInfo",
		}
	})).Return(model_core.NewSimpleMessage[model_core.CreatedObjectTree](provider))
	e.EXPECT().GetFileRootValue(eqPatchedMessage(func(*model_core.ReferenceMessagePatcher[model_core.CreatedObjectTree]) *model_analysis_pb.FileRoot_Key {
		return &model_analysis_pb.FileRoot_Key{File: manifest, DirectoryLayout: model_analysis_pb.DirectoryLayout_INPUT_ROOT}
	})).Return(model_core.NewSimpleMessage[model_core.CreatedObjectTree](&model_analysis_pb.FileRoot_Value{
		RootDirectory: singleChildDirectoryContents("external", singleChildDirectoryContents("main+", &model_filesystem_pb.DirectoryContents{
			Leaves: &model_filesystem_pb.DirectoryContents_LeavesInline{LeavesInline: &model_filesystem_pb.Leaves{
				Files: []*model_filesystem_pb.FileNode{{Name: "manifest.json", Properties: &model_filesystem_pb.FileProperties{}}},
			}},
		})),
	}))

	completion, err := bct.computer.ComputeTargetCompletionValue(ctx,
		model_core.NewSimpleMessage[model_core.CreatedObjectTree](&model_analysis_pb.TargetCompletion_Key{
			Label: "@@main+//:image", OutputGroups: []string{"mtree"},
		}), e)
	require.NoError(t, err)
	requireEqualPatchedMessage(t, func(*model_core.ReferenceMessagePatcher[model_core.CreatedObjectTree]) *model_analysis_pb.TargetCompletion_Value {
		return &model_analysis_pb.TargetCompletion_Value{
			OutputRoot: singleChildDirectoryContents("external", singleChildDirectoryContents("main+", &model_filesystem_pb.DirectoryContents{
				Leaves: &model_filesystem_pb.DirectoryContents_LeavesInline{LeavesInline: &model_filesystem_pb.Leaves{
					Files: []*model_filesystem_pb.FileNode{{Name: "manifest.json", Properties: &model_filesystem_pb.FileProperties{}}},
				}},
			})),
		}
	}, completion)
	completion.Discard()
}

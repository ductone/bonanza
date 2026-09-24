package analysis_test

import (
	"context"
	"strings"
	"testing"

	model_analysis "bonanza.build/pkg/model/analysis"
	model_core "bonanza.build/pkg/model/core"
	model_filesystem "bonanza.build/pkg/model/filesystem"
	model_parser "bonanza.build/pkg/model/parser"
	model_analysis_pb "bonanza.build/pkg/proto/model/analysis"
	model_filesystem_pb "bonanza.build/pkg/proto/model/filesystem"
	model_starlark_pb "bonanza.build/pkg/proto/model/starlark"
	object_pb "bonanza.build/pkg/proto/storage/object"
	"bonanza.build/pkg/storage/object"

	"github.com/buildbarn/bb-storage/pkg/testutil"
	"github.com/buildbarn/bb-storage/pkg/util"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

func runfilesSymlinkEntry(name, fileLabel string) *model_starlark_pb.List_Element {
	return &model_starlark_pb.List_Element{
		Level: &model_starlark_pb.List_Element_Leaf{
			Leaf: &model_starlark_pb.Value{
				Kind: &model_starlark_pb.Value_Struct{
					Struct: &model_starlark_pb.Struct{
						Fields: &model_starlark_pb.Struct_Fields{
							Keys: []string{"path", "target_file"},
							Values: []*model_starlark_pb.List_Element{
								{Level: &model_starlark_pb.List_Element_Leaf{Leaf: &model_starlark_pb.Value{Kind: &model_starlark_pb.Value_Str{Str: name}}}},
								{Level: &model_starlark_pb.List_Element_Leaf{Leaf: &model_starlark_pb.Value{Kind: &model_starlark_pb.Value_File{File: &model_starlark_pb.File{Label: fileLabel}}}}},
							},
						},
					},
				},
			},
		},
	}
}

func fileTree(relativePath string) *model_filesystem_pb.DirectoryContents {
	parts := strings.Split(relativePath, "/")
	contents := &model_filesystem_pb.DirectoryContents{
		Leaves: &model_filesystem_pb.DirectoryContents_LeavesInline{
			LeavesInline: &model_filesystem_pb.Leaves{
				Files: []*model_filesystem_pb.FileNode{{
					Name: parts[len(parts)-1],
					Properties: &model_filesystem_pb.FileProperties{
						IsExecutable: parts[len(parts)-1] == "entry.mjs",
					},
				}},
			},
		},
	}
	for i := len(parts) - 2; i >= 0; i-- {
		contents = singleChildDirectoryContents(parts[i], contents)
	}
	return contents
}

func TestToolRunfilesSymlinksInActionInput(t *testing.T) {
	ctrl, ctx := gomock.WithContext(t.Context(), t)
	bct := newBaseComputerTester(ctrl)
	e := NewMockTargetActionInputRootEnvironmentForTesting(ctrl)

	key := &model_analysis_pb.TargetActionInputRoot_Key{Id: &model_analysis_pb.TargetActionId{Label: "@@app+//frontend:build"}}
	e.EXPECT().GetTargetActionValue(gomock.Any()).Return(model_core.NewSimpleMessage[model_core.CreatedObjectTree](&model_analysis_pb.TargetAction_Value{
		Definition: &model_analysis_pb.TargetActionDefinition{
			InitialOutputDirectory: &model_filesystem_pb.Directory{
				Contents: &model_filesystem_pb.Directory_ContentsInline{
					ContentsInline: &model_filesystem_pb.DirectoryContents{Leaves: emptyLeaves},
				},
			},
			Tools: []*model_analysis_pb.FilesToRunProvider{{
				Level: &model_analysis_pb.FilesToRunProvider_Leaf_{Leaf: &model_analysis_pb.FilesToRunProvider_Leaf{
					Executable: &model_starlark_pb.File{Label: "@@app+//tools:runner"},
					RunfilesSymlinks: []*model_starlark_pb.List_Element{
						runfilesSymlinkEntry("lib/entry.mjs", "@@app+//frontend:entry.mjs"),
					},
					RunfilesRootSymlinks: []*model_starlark_pb.List_Element{
						runfilesSymlinkEntry("manifest.json", "@@app+//frontend:manifest.json"),
					},
				}},
			}},
		},
	}))

	parameters := util.Must(model_filesystem.NewDirectoryCreationParametersFromProto(
		&model_filesystem_pb.DirectoryCreationParameters{
			Access:                    &model_filesystem_pb.DirectoryAccessParameters{},
			DirectoryMaximumSizeBytes: 1 << 16,
		},
		util.Must(object.NewReferenceFormat(object_pb.ReferenceFormat_SHA256_V1)),
	))
	e.EXPECT().GetDirectoryCreationParametersObjectValue(testutil.EqProto(t, &model_analysis_pb.DirectoryCreationParametersObject_Key{})).Return(parameters, true)
	e.EXPECT().GetDirectoryReadersValue(testutil.EqProto(t, &model_analysis_pb.DirectoryReaders_Key{})).Return(&model_analysis.DirectoryReaders[model_core.CreatedObjectTree]{
		DirectoryContents: model_parser.LookupParsedObjectReader(
			bct.parsedObjectPoolIngester,
			model_parser.NewProtoObjectParser[model_core.CreatedObjectTree, model_filesystem_pb.DirectoryContents](),
		),
	}, true)
	e.EXPECT().GetFileCreationParametersValue(testutil.EqProto(t, &model_analysis_pb.FileCreationParameters_Key{})).Return(model_core.NewSimpleMessage[model_core.CreatedObjectTree](&model_analysis_pb.FileCreationParameters_Value{}))
	e.EXPECT().GetFileRootValue(gomock.Any()).DoAndReturn(func(key model_core.PatchedMessage[*model_analysis_pb.FileRoot_Key, model_core.CreatedObjectTree]) model_core.Message[*model_analysis_pb.FileRoot_Value, model_core.CreatedObjectTree] {
		file := key.Message.File
		require.NotNil(t, file)
		var relativePath string
		switch file.Label {
		case "@@app+//tools:runner":
			require.Equal(t, model_analysis_pb.DirectoryLayout_INPUT_ROOT, key.Message.DirectoryLayout)
			relativePath = "external/app+/tools/runner"
		case "@@app+//frontend:entry.mjs", "@@app+//frontend:manifest.json":
			require.Equal(t, model_analysis_pb.DirectoryLayout_RUNFILES, key.Message.DirectoryLayout)
			relativePath = "app+/frontend/" + file.Label[len("@@app+//frontend:"):]
		default:
			t.Fatalf("unexpected file: %s", file.Label)
		}
		return model_core.NewSimpleMessage[model_core.CreatedObjectTree](&model_analysis_pb.FileRoot_Value{
			RootDirectory: fileTree(relativePath),
		})
	}).Times(3)
	e.EXPECT().CaptureCreatedObject(gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, created model_core.CreatedObject[model_core.CreatedObjectTree]) (model_core.CreatedObjectTree, error) {
		return model_core.CreatedObjectTree(created), nil
	}).AnyTimes()

	result, err := bct.computer.ComputeTargetActionInputRootValue(ctx, model_core.NewSimpleMessage[model_core.CreatedObjectTree](key), e)
	require.NoError(t, err)
	// Inspect the Merkle input root, not just whether the analysis succeeded:
	// these are the paths consumed by the JavaScript launcher on the worker.
	rootValue, metadata := result.SortAndSetReferences()
	rootMessage := model_core.NewMessage(rootValue.Message, object.OutgoingReferencesList[model_core.CreatedObjectTree](metadata))
	reader := model_parser.LookupParsedObjectReader(
		bct.parsedObjectPoolIngester,
		model_parser.NewProtoObjectParser[model_core.CreatedObjectTree, model_filesystem_pb.DirectoryContents](),
	)
	contents, err := model_parser.Dereference(ctx, reader, model_core.Nested(rootMessage, rootMessage.Message.InputRootReference.Reference))
	require.NoError(t, err)
	for _, check := range []struct {
		path       string
		executable bool
	}{
		{"external/app+/tools/runner.runfiles/_main/lib/entry.mjs", true},
		{"external/app+/tools/runner.runfiles/manifest.json", false},
	} {
		parts := strings.Split(check.path, "/")
		directory := contents
		for _, component := range parts[:len(parts)-1] {
			var next *model_filesystem_pb.Directory
			for _, child := range directory.Message.Directories {
				if child.Name == component {
					next = child.Directory
					break
				}
			}
			require.NotNil(t, next, "missing directory %s in %s", component, check.path)
			directory, err = model_filesystem.DirectoryGetContents(ctx, reader, model_core.Nested(directory, next))
			require.NoError(t, err)
		}
		leaves, err := model_filesystem.DirectoryGetLeaves(ctx, nil, directory)
		require.NoError(t, err)
		var found *model_filesystem_pb.FileProperties
		for _, file := range leaves.Message.Files {
			if file.Name == parts[len(parts)-1] {
				found = file.Properties
			}
		}
		require.NotNil(t, found, "missing runfile %s", check.path)
		require.Equal(t, check.executable, found.IsExecutable)
	}
}

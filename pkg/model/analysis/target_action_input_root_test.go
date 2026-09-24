package analysis_test

import (
	"context"
	"strings"
	"testing"

	model_analysis "bonanza.build/pkg/model/analysis"
	model_core "bonanza.build/pkg/model/core"
	model_encoding "bonanza.build/pkg/model/encoding"
	model_filesystem "bonanza.build/pkg/model/filesystem"
	model_parser "bonanza.build/pkg/model/parser"
	model_analysis_pb "bonanza.build/pkg/proto/model/analysis"
	model_command_pb "bonanza.build/pkg/proto/model/command"
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
	e.EXPECT().GetRootModuleValue(testutil.EqProto(t, &model_analysis_pb.RootModule_Key{})).Return(model_core.NewSimpleMessage[model_core.CreatedObjectTree](&model_analysis_pb.RootModule_Value{RootModuleName: "app"})).AnyTimes()

	key := &model_analysis_pb.TargetActionInputRoot_Key{Id: &model_analysis_pb.TargetActionId{Label: "@@app+//frontend:build"}}
	e.EXPECT().GetTargetActionValue(gomock.Any()).Return(model_core.NewSimpleMessage[model_core.CreatedObjectTree](&model_analysis_pb.TargetAction_Value{
		Definition: &model_analysis_pb.TargetActionDefinition{
			InitialOutputDirectory: &model_filesystem_pb.Directory{
				Contents: &model_filesystem_pb.Directory_ContentsInline{
					ContentsInline: singleChildDirectoryContents("dist", &model_filesystem_pb.DirectoryContents{Leaves: emptyLeaves}),
				},
			},
			Inputs: []*model_starlark_pb.List_Element{
				{Level: &model_starlark_pb.List_Element_Leaf{Leaf: &model_starlark_pb.Value{Kind: &model_starlark_pb.Value_File{
					File: &model_starlark_pb.File{Label: "@@app+//frontend:package.json"},
				}}}},
				{Level: &model_starlark_pb.List_Element_Leaf{Leaf: &model_starlark_pb.Value{Kind: &model_starlark_pb.Value_File{
					File: &model_starlark_pb.File{Label: "@@app+//frontend:node_modules/dependency.txt"},
				}}}},
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
			relativePath = "tools/runner"
		case "@@app+//frontend:package.json":
			require.Equal(t, model_analysis_pb.DirectoryLayout_INPUT_ROOT, key.Message.DirectoryLayout)
			relativePath = "frontend/package.json"
		case "@@app+//frontend:node_modules/dependency.txt":
			require.Equal(t, model_analysis_pb.DirectoryLayout_INPUT_ROOT, key.Message.DirectoryLayout)
			relativePath = "bazel-out/none/bin/frontend/node_modules/dependency.txt"
		case "@@app+//frontend:entry.mjs", "@@app+//frontend:manifest.json":
			require.Equal(t, model_analysis_pb.DirectoryLayout_RUNFILES, key.Message.DirectoryLayout)
			relativePath = "_main/frontend/" + file.Label[len("@@app+//frontend:"):]
		default:
			t.Fatalf("unexpected file: %s", file.Label)
		}
		return model_core.NewSimpleMessage[model_core.CreatedObjectTree](&model_analysis_pb.FileRoot_Value{
			RootDirectory: fileTree(relativePath),
		})
	}).Times(5)
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
		{"frontend/package.json", false},
		{"bazel-out/none/bin/frontend/node_modules/dependency.txt", false},
		{"tools/runner.runfiles/_main/lib/entry.mjs", true},
		{"tools/runner.runfiles/manifest.json", false},
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
		require.NotNil(t, found, "missing action input %s", check.path)
		require.Equal(t, check.executable, found.IsExecutable)
	}
	// The package output directory is rooted at Bazel's bindir; the tool's
	// runfiles stay next to its executable, not next to the output package.
	outputDirectory := contents
	for _, component := range []string{"bazel-out", "none", "bin", "frontend"} {
		var next *model_filesystem_pb.Directory
		for _, child := range outputDirectory.Message.Directories {
			if child.Name == component {
				next = child.Directory
				break
			}
		}
		require.NotNil(t, next, "missing output directory component %s", component)
		outputDirectory, err = model_filesystem.DirectoryGetContents(ctx, reader, model_core.Nested(outputDirectory, next))
		require.NoError(t, err)
	}
}

// C1's bazel/frontend/build_runner.mjs derives the execroot by removing
// BAZEL_BINDIR and the package name from the action's package cwd.
func TestFrontendActionCommandLayout(t *testing.T) {
	for _, tc := range []struct {
		name       string
		label      string
		outputPath []string
	}{
		{"MainWorkspace", "@@app+//frontend:build", []string{"bazel-out", "none", "bin", "frontend", "dist"}},
		{"ExternalRepository", "@@thirdparty+//frontend:build", []string{"bazel-out", "none", "bin", "external", "thirdparty+", "frontend", "dist"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctrl, ctx := gomock.WithContext(t.Context(), t)
			bct := newBaseComputerTester(ctrl)
			e := NewMockTargetActionCommandEnvironmentForTesting(ctrl)
			encoder := model_encoding.NewLZWCompressingDeterministicBinaryEncoder(1 << 20)
			e.EXPECT().GetTargetActionValue(gomock.Any()).Return(model_core.NewSimpleMessage[model_core.CreatedObjectTree](&model_analysis_pb.TargetAction_Value{
				Definition: &model_analysis_pb.TargetActionDefinition{
					OutputPathPattern: &model_command_pb.PathPattern{
						Children: &model_command_pb.PathPattern_ChildrenInline{
							ChildrenInline: &model_command_pb.PathPattern_Children{
								Children: []*model_command_pb.PathPattern_Child{{Name: "dist", Pattern: &model_command_pb.PathPattern{}}},
							},
						},
					},
					Env: []*model_command_pb.EnvironmentVariableList_Element{{
						Level: &model_command_pb.EnvironmentVariableList_Element_Leaf_{
							Leaf: &model_command_pb.EnvironmentVariableList_Element_Leaf{Name: "NODE_ENV", Value: "production"},
						},
					}},
				},
			}))
			e.EXPECT().GetActionEncoderObjectValue(gomock.Any()).Return(encoder, true)
			e.EXPECT().GetActionReadersValue(gomock.Any()).Return(&model_analysis.ActionReaders[model_core.CreatedObjectTree]{}, true)
			e.EXPECT().GetBuiltinsModuleNamesValue(gomock.Any()).Return(model_core.NewSimpleMessage[model_core.CreatedObjectTree](&model_analysis_pb.BuiltinsModuleNames_Value{}))
			e.EXPECT().GetDirectoryCreationParametersValue(gomock.Any()).Return(model_core.NewSimpleMessage[model_core.CreatedObjectTree](&model_analysis_pb.DirectoryCreationParameters_Value{
				DirectoryCreationParameters: &model_filesystem_pb.DirectoryCreationParameters{},
			}))
			e.EXPECT().GetFileCreationParametersValue(gomock.Any()).Return(model_core.NewSimpleMessage[model_core.CreatedObjectTree](&model_analysis_pb.FileCreationParameters_Value{
				FileCreationParameters: &model_filesystem_pb.FileCreationParameters{},
			}))
			e.EXPECT().GetDirectoryReadersValue(gomock.Any()).Return(&model_analysis.DirectoryReaders[model_core.CreatedObjectTree]{}, true)
			e.EXPECT().GetBuildSpecificationValue(gomock.Any()).Return(model_core.NewSimpleMessage[model_core.CreatedObjectTree](&model_analysis_pb.BuildSpecification_Value{}))
			e.EXPECT().GetRootModuleValue(gomock.Any()).Return(model_core.NewSimpleMessage[model_core.CreatedObjectTree](&model_analysis_pb.RootModule_Value{RootModuleName: "app"})).AnyTimes()
			e.EXPECT().CaptureCreatedObject(gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, created model_core.CreatedObject[model_core.CreatedObjectTree]) (model_core.CreatedObjectTree, error) {
				return model_core.CreatedObjectTree(created), nil
			}).AnyTimes()

			result, err := bct.computer.ComputeTargetActionCommandValue(ctx, model_core.NewSimpleMessage[model_core.CreatedObjectTree](&model_analysis_pb.TargetActionCommand_Key{
				Id: &model_analysis_pb.TargetActionId{Label: tc.label},
			}), e)
			require.NoError(t, err)
			resultValue, metadata := result.SortAndSetReferences()
			resultMessage := model_core.NewMessage(resultValue.Message, object.OutgoingReferencesList[model_core.CreatedObjectTree](metadata))
			command, err := model_parser.Dereference(ctx, model_parser.LookupParsedObjectReader(
				bct.parsedObjectPoolIngester,
				model_parser.NewChainedObjectParser(
					model_parser.NewEncodedObjectParser[model_core.CreatedObjectTree](encoder),
					model_parser.NewProtoObjectParser[model_core.CreatedObjectTree, model_command_pb.Command](),
				),
			), model_core.Nested(resultMessage, resultMessage.Message.CommandReference))
			require.NoError(t, err)
			require.Equal(t, ".", command.Message.WorkingDirectory)
			env := map[string]string{}
			for _, variable := range command.Message.EnvironmentVariables {
				env[variable.GetLeaf().Name] = variable.GetLeaf().Value
			}
			require.Equal(t, "production", env["NODE_ENV"])
			require.Equal(t, "bazel-out/none/bin", env["BAZEL_BINDIR"])
			pattern := command.Message.OutputPathPattern
			for _, component := range tc.outputPath {
				children := pattern.GetChildrenInline().GetChildren()
				require.Len(t, children, 1)
				require.Equal(t, component, children[0].Name)
				pattern = children[0].Pattern
			}
			require.Nil(t, pattern.Children)
		})
	}
}

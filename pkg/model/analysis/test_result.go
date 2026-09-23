package analysis

import (
	"context"
	"encoding"
	"errors"
	"fmt"

	"bonanza.build/pkg/label"
	model_core "bonanza.build/pkg/model/core"
	"bonanza.build/pkg/model/evaluation"
	model_parser "bonanza.build/pkg/model/parser"
	model_starlark "bonanza.build/pkg/model/starlark"
	model_analysis_pb "bonanza.build/pkg/proto/model/analysis"
	model_command_pb "bonanza.build/pkg/proto/model/command"
	model_starlark_pb "bonanza.build/pkg/proto/model/starlark"

	"google.golang.org/protobuf/types/known/durationpb"
)

// getRunfilesDepsetElements reads one of the runfiles depsets a
// FilesToRunProvider struct carries and returns its elements, which is
// the shape FilesToRunProvider.Leaf stores them in.
func getRunfilesDepsetElements[TReference any](
	ctx context.Context,
	listReader model_parser.MessageObjectReader[TReference, []*model_starlark_pb.List_Element],
	fields model_core.Message[*model_starlark_pb.Struct_Fields, TReference],
	name string,
) (model_core.Message[[]*model_starlark_pb.List_Element, TReference], error) {
	value, err := model_starlark.GetStructFieldValue(ctx, listReader, fields, name)
	if err != nil {
		return model_core.Message[[]*model_starlark_pb.List_Element, TReference]{}, fmt.Errorf("failed to obtain %s: %w", name, err)
	}
	depset, ok := value.Message.Kind.(*model_starlark_pb.Value_Depset)
	if !ok {
		return model_core.Message[[]*model_starlark_pb.List_Element, TReference]{}, fmt.Errorf("%s field of FilesToRunProvider is not a depset", name)
	}
	return model_core.Nested(value, depset.Depset.Elements), nil
}

func (c *baseComputer[TReference, TMetadata]) ComputeTestResultValue(
	ctx context.Context,
	key model_core.Message[*model_analysis_pb.TestResult_Key, TReference],
	e TestResultEnvironment[TReference, TMetadata],
) (PatchedTestResultValue[TMetadata], error) {
	targetLabelStr := key.Message.Label
	targetLabel, err := label.NewCanonicalLabel(targetLabelStr)
	if err != nil {
		return PatchedTestResultValue[TMetadata]{}, fmt.Errorf("invalid target label %#v: %w", targetLabelStr, err)
	}

	// The executable to run, and the runfiles that have to sit beside
	// it, both come from DefaultInfo.files_to_run.
	defaultInfo, err := getProviderFromConfiguredTarget(
		e,
		targetLabelStr,
		model_core.Patch(e, model_core.Nested(key, key.Message.ConfigurationReference)),
		defaultInfoProviderIdentifier,
	)
	if err != nil {
		return PatchedTestResultValue[TMetadata]{}, err
	}
	filesToRunValue, err := model_starlark.GetStructFieldValue(ctx, c.valueReaders.List, defaultInfo, "files_to_run")
	if err != nil {
		return PatchedTestResultValue[TMetadata]{}, fmt.Errorf("failed to obtain files_to_run of target %#v: %w", targetLabelStr, err)
	}
	filesToRunStruct, ok := filesToRunValue.Message.Kind.(*model_starlark_pb.Value_Struct)
	if !ok {
		return PatchedTestResultValue[TMetadata]{}, errors.New("files_to_run field of DefaultInfo provider is not a FilesToRunProvider")
	}
	filesToRunFields := model_core.Nested(filesToRunValue, filesToRunStruct.Struct.GetFields())

	executableValue, err := model_starlark.GetStructFieldValue(ctx, c.valueReaders.List, filesToRunFields, "executable")
	if err != nil {
		return PatchedTestResultValue[TMetadata]{}, fmt.Errorf("failed to obtain executable of target %#v: %w", targetLabelStr, err)
	}
	executableFile, ok := executableValue.Message.Kind.(*model_starlark_pb.Value_File)
	if !ok {
		// A rule that is a test rule but yields no executable is a
		// rule-authoring error, not a test failure.
		return PatchedTestResultValue[TMetadata]{}, fmt.Errorf("target %#v does not provide an executable to run", targetLabelStr)
	}
	executable := model_core.Nested(executableValue, executableFile.File)

	runfilesFiles, err := getRunfilesDepsetElements(ctx, c.valueReaders.List, filesToRunFields, "_runfiles_files")
	if err != nil {
		return PatchedTestResultValue[TMetadata]{}, err
	}

	// The execution platform. Tests declare no exec requirements of
	// their own here yet, so this resolves the same way a target with an
	// empty exec group does: the first registered platform that is
	// compatible with the current configuration.
	//
	// A test carrying exec_compatible_with is therefore not yet honoured.
	// Doing so needs the rule's test exec group, which lives on the rule
	// definition rather than on anything reachable from DefaultInfo.
	patchedConfigurationReference := model_core.Patch(e, model_core.Nested(key, key.Message.ConfigurationReference))
	resolvedToolchains := e.GetResolvedToolchainsValue(
		model_core.NewPatchedMessage(
			&model_analysis_pb.ResolvedToolchains_Key{
				ConfigurationReference: patchedConfigurationReference.Message,
			},
			patchedConfigurationReference.Patcher,
		),
	)
	actionEncoder, gotActionEncoder := e.GetActionEncoderObjectValue(&model_analysis_pb.ActionEncoderObject_Key{})
	directoryCreationParameters, gotDirectoryCreationParameters := e.GetDirectoryCreationParametersObjectValue(&model_analysis_pb.DirectoryCreationParametersObject_Key{})
	directoryCreationParametersMessage := e.GetDirectoryCreationParametersValue(&model_analysis_pb.DirectoryCreationParameters_Key{})
	directoryReaders, gotDirectoryReaders := e.GetDirectoryReadersValue(&model_analysis_pb.DirectoryReaders_Key{})
	fileCreationParametersMessage := e.GetFileCreationParametersValue(&model_analysis_pb.FileCreationParameters_Key{})
	if !resolvedToolchains.IsSet() ||
		!gotActionEncoder ||
		!gotDirectoryCreationParameters ||
		!directoryCreationParametersMessage.IsSet() ||
		!gotDirectoryReaders ||
		!fileCreationParametersMessage.IsSet() {
		return PatchedTestResultValue[TMetadata]{}, evaluation.ErrMissingDependency
	}

	// Synthesize an action definition that runs the executable with its
	// runfiles. This is deliberately not registered as an action on the
	// configured target: target.actions is exposed to aspects, so an
	// action that exists only because someone ran "bazel test" would
	// change what every aspect observes about the target.
	patchedDefinition, err := model_core.BuildPatchedMessage(func(patcher *model_core.ReferenceMessagePatcher[TMetadata]) (*model_analysis_pb.TargetActionDefinition, error) {
		patchedExecutable := model_core.Patch(e, executable)
		patchedRunfilesFiles := model_core.PatchList(e, runfilesFiles)
		return &model_analysis_pb.TargetActionDefinition{
			Tools: []*model_analysis_pb.FilesToRunProvider{{
				Level: &model_analysis_pb.FilesToRunProvider_Leaf_{
					Leaf: &model_analysis_pb.FilesToRunProvider_Leaf{
						Executable:    patchedExecutable.Merge(patcher),
						RunfilesFiles: patchedRunfilesFiles.Merge(patcher),
					},
				},
			}},
			PlatformPkixPublicKey: resolvedToolchains.Message.PlatformPkixPublicKey,
		}, nil
	})
	if err != nil {
		return PatchedTestResultValue[TMetadata]{}, fmt.Errorf("failed to create test action definition: %w", err)
	}
	actionDefinition := model_core.Unpatch(e, patchedDefinition).Decay()

	inputRoot, err := c.buildActionInputRoot(
		ctx,
		e,
		targetLabel,
		model_core.Nested(key, key.Message.ConfigurationReference),
		actionDefinition,
		directoryCreationParameters,
		directoryReaders,
	)
	if err != nil {
		return PatchedTestResultValue[TMetadata]{}, fmt.Errorf("failed to create input root for test %#v: %w", targetLabelStr, err)
	}

	// The command is a single argument: the executable, at the path the
	// input root placed it at. Tests take no arguments from the rule, so
	// none of the Args template expansion a declared action needs
	// applies here.
	executablePath, err := model_starlark.FileGetInputRootPath(executable, nil)
	if err != nil {
		return PatchedTestResultValue[TMetadata]{}, fmt.Errorf("failed to get path of test executable: %w", err)
	}
	patchedCommand := model_core.MustBuildPatchedMessage(func(patcher *model_core.ReferenceMessagePatcher[TMetadata]) *model_command_pb.Command {
		return &model_command_pb.Command{
			Arguments: []*model_command_pb.ArgumentList_Element{{
				Level: &model_command_pb.ArgumentList_Element_Leaf{
					Leaf: executablePath,
				},
			}},
			DirectoryCreationParameters: directoryCreationParametersMessage.Message.DirectoryCreationParameters,
			FileCreationParameters:      fileCreationParametersMessage.Message.FileCreationParameters,
		}
	})
	createdCommand, err := model_core.MarshalAndEncodeDeterministic(
		model_core.ProtoToBinaryMarshaler(patchedCommand),
		c.referenceFormat,
		actionEncoder,
	)
	if err != nil {
		return PatchedTestResultValue[TMetadata]{}, fmt.Errorf("failed to create test command: %w", err)
	}

	createdAction, err := model_core.MarshalAndEncodeDeterministic(
		model_core.MustBuildPatchedMessage(func(patcher *model_core.ReferenceMessagePatcher[TMetadata]) encoding.BinaryMarshaler {
			commandReference, err := patcher.CaptureAndAddDecodableReference(ctx, createdCommand, e)
			if err != nil {
				panic(err)
			}
			inputRootReference := model_core.Patch(e, model_core.Unpatch(e, inputRoot).Decay())
			patcher.Merge(inputRootReference.Patcher)
			return model_core.NewProtoBinaryMarshaler(&model_command_pb.Action{
				CommandReference:   commandReference,
				InputRootReference: inputRootReference.Message.InputRootReference,
			})
		}),
		c.referenceFormat,
		actionEncoder,
	)
	if err != nil {
		return PatchedTestResultValue[TMetadata]{}, fmt.Errorf("failed to create test action: %w", err)
	}

	// SuccessfulActionResult turns a non-zero exit into an error, which
	// is what "bazel test" wants: a failing test has to fail the
	// invocation rather than be reported as a value someone might cache
	// and ignore.
	actionResultKey, err := model_core.BuildPatchedMessage(func(patcher *model_core.ReferenceMessagePatcher[TMetadata]) (*model_analysis_pb.SuccessfulActionResult_Key, error) {
		actionReference, err := patcher.CaptureAndAddDecodableReference(ctx, createdAction, e)
		if err != nil {
			return nil, err
		}
		return &model_analysis_pb.SuccessfulActionResult_Key{
			ExecuteRequest: &model_analysis_pb.ExecuteRequest{
				ExecutionTimeout:      &durationpb.Duration{Seconds: 900},
				ActionReference:       actionReference,
				PlatformPkixPublicKey: resolvedToolchains.Message.PlatformPkixPublicKey,
			},
		}, nil
	})
	if err != nil {
		return PatchedTestResultValue[TMetadata]{}, fmt.Errorf("failed to create test action result key: %w", err)
	}
	if actionResult := e.GetSuccessfulActionResultValue(actionResultKey); !actionResult.IsSet() {
		return PatchedTestResultValue[TMetadata]{}, evaluation.ErrMissingDependency
	}
	return model_core.NewSimplePatchedMessage[TMetadata](&model_analysis_pb.TestResult_Value{}), nil
}

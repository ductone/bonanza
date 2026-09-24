package analysis

import (
	"context"
	"encoding"
	"errors"
	"fmt"
	"sort"
	"strings"

	"bonanza.build/pkg/label"
	model_core "bonanza.build/pkg/model/core"
	"bonanza.build/pkg/model/evaluation"
	model_filesystem "bonanza.build/pkg/model/filesystem"
	model_parser "bonanza.build/pkg/model/parser"
	model_starlark "bonanza.build/pkg/model/starlark"
	model_analysis_pb "bonanza.build/pkg/proto/model/analysis"
	model_command_pb "bonanza.build/pkg/proto/model/command"
	model_core_pb "bonanza.build/pkg/proto/model/core"
	model_starlark_pb "bonanza.build/pkg/proto/model/starlark"

	"github.com/buildbarn/bb-storage/pkg/filesystem/path"
	"github.com/buildbarn/bb-storage/pkg/util"

	"google.golang.org/protobuf/types/known/durationpb"
)

const testSuiteRuleIdentifier = "@@builtins_core+//:exports.bzl%test_suite"

// testExecGroupName is the name of the execution group in which the test
// action of a target runs. rule(test = True) declares it implicitly,
// inheriting the constraints of the default execution group.
const testExecGroupName = "test"

// getTestRuleAttribute returns a non-configurable rule attribute. Test suite
// membership and shard counts are loading-phase properties, not select()-able
// test action settings.
func getTestRuleAttribute[TReference any](
	target model_core.Message[*model_starlark_pb.RuleTarget, TReference],
	definition model_core.Message[*model_starlark_pb.Rule_Definition, TReference],
	name string,
) (model_core.Message[*model_starlark_pb.Value, TReference], error) {
	publicIndex := 0
	for _, attr := range definition.Message.Attrs {
		if strings.HasPrefix(attr.Name, "_") {
			continue
		}
		if publicIndex >= len(target.Message.PublicAttrValues) {
			return model_core.Message[*model_starlark_pb.Value, TReference]{}, fmt.Errorf("rule has fewer values than public attributes")
		}
		value := target.Message.PublicAttrValues[publicIndex]
		publicIndex++
		if attr.Name != name {
			continue
		}
		if len(value.ValueParts) != 1 || len(value.ValueParts[0].Conditions) != 0 {
			return model_core.Message[*model_starlark_pb.Value, TReference]{}, fmt.Errorf("test attribute %q must not be configurable", name)
		}
		group := value.ValueParts[0]
		if noMatch, ok := group.NoMatch.(*model_starlark_pb.Select_Group_NoMatchValue); ok {
			if _, none := noMatch.NoMatchValue.Kind.(*model_starlark_pb.Value_None); !none {
				return model_core.Nested(target, noMatch.NoMatchValue), nil
			}
		}
		return model_core.Nested(definition, attr.Attr.Default), nil
	}
	return model_core.Message[*model_starlark_pb.Value, TReference]{}, fmt.Errorf("rule has no %q attribute", name)
}

func testShardCount[TReference any](
	target model_core.Message[*model_starlark_pb.RuleTarget, TReference],
	definition model_core.Message[*model_starlark_pb.Rule_Definition, TReference],
) (uint32, error) {
	value, err := getTestRuleAttribute(target, definition, "shard_count")
	if err != nil {
		return 0, err
	}
	encoded, ok := value.Message.Kind.(*model_starlark_pb.Value_Int)
	if !ok || encoded.Int == nil {
		return 0, errors.New("shard_count is not an integer")
	}
	absolute := encoded.Int.AbsoluteValue
	if len(absolute) > 1 {
		return 0, errors.New("shard_count must be -1 or between 1 and 50")
	}
	var count uint32
	if len(absolute) == 1 {
		count = uint32(absolute[0])
	}
	if encoded.Int.Negative {
		if count != 1 {
			return 0, errors.New("shard_count must be -1 or between 1 and 50")
		}
		return 1, nil
	}
	if count == 0 || count > 50 {
		return 0, fmt.Errorf("shard_count %d must be between 1 and 50", count)
	}
	return count, nil
}

func unsupportedTestTag(tags []string) string {
	for _, tag := range tags {
		switch tag {
		case "exclusive", "external", "local", "no-cache", "no-remote", "no-remote-cache", "no-remote-exec", "no-sandbox":
			return tag
		}
	}
	return ""
}

// TODO: Derive the timeout from the "timeout" and "size" attributes that
// rule(test = True) adds implicitly, instead of applying one value to
// every test.
const testExecutionTimeoutSeconds = 900

type getRuleDefinitionEnvironment[TReference any] interface {
	GetCompiledBzlFileGlobalValue(*model_analysis_pb.CompiledBzlFileGlobal_Key) model_core.Message[*model_analysis_pb.CompiledBzlFileGlobal_Value, TReference]
	GetTargetValue(*model_analysis_pb.Target_Key) model_core.Message[*model_analysis_pb.Target_Value, TReference]
}

// getRuleDefinition obtains the definition of the rule that a target
// uses. Targets that are not rule targets (source files, package groups)
// yield an unset message rather than an error, as callers use this to
// distinguish test targets from everything else.
func getRuleDefinition[TReference any](
	e getRuleDefinitionEnvironment[TReference],
	targetLabel string,
) (model_core.Message[*model_starlark_pb.Rule_Definition, TReference], error) {
	var noDefinition model_core.Message[*model_starlark_pb.Rule_Definition, TReference]
	targetValue := e.GetTargetValue(&model_analysis_pb.Target_Key{Label: targetLabel})
	if !targetValue.IsSet() {
		return noDefinition, evaluation.ErrMissingDependency
	}
	ruleTarget, ok := targetValue.Message.Definition.GetKind().(*model_starlark_pb.Target_Definition_RuleTarget)
	if !ok {
		return noDefinition, nil
	}

	if ruleIdentifier := ruleTarget.RuleTarget.RuleIdentifier; ruleIdentifier != "" {
		ruleValue := e.GetCompiledBzlFileGlobalValue(&model_analysis_pb.CompiledBzlFileGlobal_Key{
			Identifier: ruleIdentifier,
		})
		if !ruleValue.IsSet() {
			return noDefinition, evaluation.ErrMissingDependency
		}
		v, ok := ruleValue.Message.Global.GetKind().(*model_starlark_pb.Value_Rule)
		if !ok {
			return noDefinition, fmt.Errorf("%#v is not a rule", ruleIdentifier)
		}
		d, ok := v.Rule.Kind.(*model_starlark_pb.Rule_Definition_)
		if !ok {
			return noDefinition, fmt.Errorf("%#v is not a rule definition", ruleIdentifier)
		}
		return model_core.Nested(ruleValue, d.Definition), nil
	}

	// Anonymous rule of which the definition is embedded into the
	// target, as created by testing.analysis_test().
	if d := ruleTarget.RuleTarget.RuleDefinition; d != nil {
		return model_core.Nested(targetValue, d), nil
	}
	return noDefinition, errors.New("rule target has neither a rule identifier, nor an inline rule definition")
}

// getTestExecutionPlatform resolves the execution platform of the "test"
// execution group of a test target, which is where its test binary runs.
func (c *baseComputer[TReference, TMetadata]) getTestExecutionPlatform(
	ctx context.Context,
	e TargetTestResultEnvironment[TReference, TMetadata],
	targetLabel label.CanonicalLabel,
	ruleDefinition model_core.Message[*model_starlark_pb.Rule_Definition, TReference],
	ruleTarget model_core.Message[*model_starlark_pb.RuleTarget, TReference],
	configurationReference model_core.Message[*model_core_pb.DecodableReference, TReference],
) ([]byte, error) {
	execGroups := ruleDefinition.Message.ExecGroups
	index, ok := sort.Find(
		len(execGroups),
		func(i int) int { return strings.Compare(testExecGroupName, execGroups[i].Name) },
	)
	if !ok {
		return nil, fmt.Errorf("rule of target %#v does not have a %#v exec group", targetLabel.String(), testExecGroupName)
	}
	execGroupDefinition := execGroups[index].ExecGroup
	if execGroupDefinition == nil {
		return nil, fmt.Errorf("missing definition of exec group %#v", testExecGroupName)
	}

	execCompatibleWith, err := c.constraintValuesToConstraints(
		ctx,
		e,
		targetLabel.GetCanonicalPackage(),
		append(append([]string(nil), execGroupDefinition.ExecCompatibleWith...), ruleTarget.Message.ExecCompatibleWith...),
	)
	if err != nil {
		return nil, fmt.Errorf("invalid constraint values for exec group %#v: %w", testExecGroupName, err)
	}

	patchedConfigurationReference := model_core.Patch(e, configurationReference)
	resolvedToolchains := e.GetResolvedToolchainsValue(
		model_core.NewPatchedMessage(
			&model_analysis_pb.ResolvedToolchains_Key{
				ExecCompatibleWith:     execCompatibleWith,
				ConfigurationReference: patchedConfigurationReference.Message,
				Toolchains:             execGroupDefinition.Toolchains,
			},
			patchedConfigurationReference.Patcher,
		),
	)
	if !resolvedToolchains.IsSet() {
		return nil, evaluation.ErrMissingDependency
	}
	return resolvedToolchains.Message.PlatformPkixPublicKey, nil
}

func (c *baseComputer[TReference, TMetadata]) ComputeTargetTestResultValue(ctx context.Context, key model_core.Message[*model_analysis_pb.TargetTestResult_Key, TReference], e TargetTestResultEnvironment[TReference, TMetadata]) (PatchedTargetTestResultValue[TMetadata], error) {
	targetLabel, err := label.NewCanonicalLabel(key.Message.Label)
	if err != nil {
		return PatchedTargetTestResultValue[TMetadata]{}, fmt.Errorf("invalid target label %#v: %w", key.Message.Label, err)
	}
	configurationReference := model_core.Nested(key, key.Message.ConfigurationReference)
	if key.Message.ShardCount == 0 || key.Message.ShardIndex >= key.Message.ShardCount {
		return PatchedTargetTestResultValue[TMetadata]{}, fmt.Errorf("invalid test shard %d/%d for %#v", key.Message.ShardIndex, key.Message.ShardCount, key.Message.Label)
	}

	actionEncoder, gotActionEncoder := e.GetActionEncoderObjectValue(&model_analysis_pb.ActionEncoderObject_Key{})
	directoryCreationParameters, gotDirectoryCreationParameters := e.GetDirectoryCreationParametersObjectValue(&model_analysis_pb.DirectoryCreationParametersObject_Key{})
	directoryCreationParametersMessage := e.GetDirectoryCreationParametersValue(&model_analysis_pb.DirectoryCreationParameters_Key{})
	directoryReaders, gotDirectoryReaders := e.GetDirectoryReadersValue(&model_analysis_pb.DirectoryReaders_Key{})
	fileCreationParametersMessage := e.GetFileCreationParametersValue(&model_analysis_pb.FileCreationParameters_Key{})

	ruleDefinition, err := getRuleDefinition(e, key.Message.Label)
	if err != nil {
		return PatchedTargetTestResultValue[TMetadata]{}, err
	}
	if !ruleDefinition.IsSet() || !ruleDefinition.Message.Test {
		return PatchedTargetTestResultValue[TMetadata]{}, fmt.Errorf("target %#v is not a test target", key.Message.Label)
	}
	targetValue := e.GetTargetValue(&model_analysis_pb.Target_Key{Label: key.Message.Label})
	if !targetValue.IsSet() {
		return PatchedTargetTestResultValue[TMetadata]{}, evaluation.ErrMissingDependency
	}
	ruleTarget := model_core.Nested(targetValue, targetValue.Message.Definition.GetRuleTarget())
	if tag := unsupportedTestTag(ruleTarget.Message.Tags); tag != "" {
		return PatchedTargetTestResultValue[TMetadata]{}, fmt.Errorf("test %#v requires %q execution, which cannot be run on the cached remote test worker", key.Message.Label, tag)
	}
	local, err := getTestRuleAttribute(ruleTarget, ruleDefinition, "local")
	if err != nil {
		return PatchedTargetTestResultValue[TMetadata]{}, fmt.Errorf("test %#v: %w", key.Message.Label, err)
	}
	if value, ok := local.Message.Kind.(*model_starlark_pb.Value_Bool); !ok {
		return PatchedTargetTestResultValue[TMetadata]{}, fmt.Errorf("test %#v: local attribute is not a boolean", key.Message.Label)
	} else if value.Bool {
		return PatchedTargetTestResultValue[TMetadata]{}, fmt.Errorf("test %#v requires local execution, which cannot be run on the cached remote test worker", key.Message.Label)
	}
	count, err := testShardCount(ruleTarget, ruleDefinition)
	if err != nil {
		return PatchedTargetTestResultValue[TMetadata]{}, fmt.Errorf("test %#v: %w", key.Message.Label, err)
	}
	if count != key.Message.ShardCount {
		return PatchedTargetTestResultValue[TMetadata]{}, fmt.Errorf("test %#v requests %d shards, expected %d", key.Message.Label, key.Message.ShardCount, count)
	}

	defaultInfo, err := getProviderFromConfiguredTarget(
		e,
		key.Message.Label,
		model_core.Patch(e, configurationReference),
		defaultInfoProviderIdentifier,
	)
	if err != nil {
		return PatchedTargetTestResultValue[TMetadata]{}, err
	}

	if !gotActionEncoder ||
		!gotDirectoryCreationParameters ||
		!directoryCreationParametersMessage.IsSet() ||
		!gotDirectoryReaders ||
		!fileCreationParametersMessage.IsSet() {
		return PatchedTargetTestResultValue[TMetadata]{}, evaluation.ErrMissingDependency
	}

	platformPkixPublicKey, err := c.getTestExecutionPlatform(ctx, e, targetLabel, ruleDefinition, ruleTarget, configurationReference)
	if err != nil {
		return PatchedTargetTestResultValue[TMetadata]{}, fmt.Errorf("test %#v: %w", key.Message.Label, err)
	}

	// Extract the test binary and its runfiles from the FilesToRun
	// provider that DefaultInfo carries. Bazel's test action runs
	// that binary; unlike the actions a rule registers itself, it is
	// not part of the target's action graph.
	filesToRunValue, err := model_starlark.GetStructFieldValue(ctx, c.valueReaders.List, defaultInfo, "files_to_run")
	if err != nil {
		return PatchedTargetTestResultValue[TMetadata]{}, err
	}
	filesToRunStruct, ok := filesToRunValue.Message.Kind.(*model_starlark_pb.Value_Struct)
	if !ok {
		return PatchedTargetTestResultValue[TMetadata]{}, errors.New("\"files_to_run\" field of DefaultInfo provider is not a struct")
	}
	filesToRun := model_core.Nested(filesToRunValue, filesToRunStruct.Struct.Fields)

	executableValue, err := model_starlark.GetStructFieldValue(ctx, c.valueReaders.List, filesToRun, "executable")
	if err != nil {
		return PatchedTargetTestResultValue[TMetadata]{}, err
	}
	executableFile, ok := executableValue.Message.Kind.(*model_starlark_pb.Value_File)
	if !ok {
		return PatchedTargetTestResultValue[TMetadata]{}, fmt.Errorf("test target %#v does not have an executable", key.Message.Label)
	}
	executable := model_core.Nested(executableValue, executableFile.File)
	rootModule := e.GetRootModuleValue(&model_analysis_pb.RootModule_Key{})
	if !rootModule.IsSet() {
		return PatchedTargetTestResultValue[TMetadata]{}, evaluation.ErrMissingDependency
	}
	executablePath, err := model_starlark.FileGetInputRootPath(executable, nil, rootModule.Message.RootModuleName)
	if err != nil {
		return PatchedTargetTestResultValue[TMetadata]{}, fmt.Errorf("failed to get path of test executable: %w", err)
	}

	runfilesFilesValue, err := model_starlark.GetStructFieldValue(ctx, c.valueReaders.List, filesToRun, "_runfiles_files")
	if err != nil {
		return PatchedTargetTestResultValue[TMetadata]{}, err
	}
	runfilesFilesDepset, ok := runfilesFilesValue.Message.Kind.(*model_starlark_pb.Value_Depset)
	if !ok {
		return PatchedTargetTestResultValue[TMetadata]{}, errors.New("\"_runfiles_files\" field of FilesToRunProvider is not a depset")
	}

	// Assemble the input root: the test binary, with its runfiles
	// directory next to it.
	var rootDirectory changeTrackingDirectory[TReference, TMetadata]
	loadOptions := &changeTrackingDirectoryLoadOptions[TReference]{
		context:                 ctx,
		directoryContentsReader: directoryReaders.DirectoryContents,
		leavesReader:            directoryReaders.Leaves,
	}
	if err := addFileToChangeTrackingDirectory(
		e,
		executable,
		&rootDirectory,
		loadOptions,
		model_analysis_pb.DirectoryLayout_INPUT_ROOT,
	); err != nil {
		return PatchedTargetTestResultValue[TMetadata]{}, fmt.Errorf("failed to add test executable to input root: %w", err)
	}

	runfilesDirectoryPath := executablePath + ".runfiles"
	runfilesDirectoryResolver := changeTrackingDirectoryNewDirectoryResolver[TReference, TMetadata]{
		loadOptions: loadOptions,
		stack:       util.NewNonEmptyStack(&rootDirectory),
	}
	if err := path.Resolve(path.UNIXFormat.NewParser(runfilesDirectoryPath), &runfilesDirectoryResolver); err != nil {
		return PatchedTargetTestResultValue[TMetadata]{}, fmt.Errorf("failed to create runfiles directory %#v: %w", runfilesDirectoryPath, err)
	}
	runfilesDirectory := runfilesDirectoryResolver.stack.Peek()
	if err := addFilesToChangeTrackingDirectory(
		e,
		model_core.Nested(runfilesFilesValue, runfilesFilesDepset.Depset.Elements),
		runfilesDirectory,
		loadOptions,
		model_analysis_pb.DirectoryLayout_RUNFILES,
	); err != nil {
		return PatchedTargetTestResultValue[TMetadata]{}, fmt.Errorf("failed to add runfiles to input root: %w", err)
	}
	if _, err := runfilesDirectory.getOrCreateDirectory(componentMainWorkspaceName); err != nil {
		return PatchedTargetTestResultValue[TMetadata]{}, fmt.Errorf("failed to create runfiles directory of the main workspace: %w", err)
	}

	// TODO: Set TEST_TMPDIR. Doing so requires the worker to provide
	// a writable scratch directory, which input roots currently do
	// not offer.
	environment := map[string]string{
		"RUNFILES_DIR":    runfilesDirectoryPath,
		"TEST_SRCDIR":     runfilesDirectoryPath,
		"TEST_TARGET":     targetLabel.String(),
		"TEST_WORKSPACE":  componentMainWorkspaceName.String(),
		"XML_OUTPUT_FILE": "test.xml",
	}
	if count > 1 {
		environment["TEST_TOTAL_SHARDS"] = fmt.Sprint(count)
		environment["TEST_SHARD_INDEX"] = fmt.Sprint(key.Message.ShardIndex)
		environment["TEST_SHARD_STATUS_FILE"] = "test.shard.status"
	}
	if testFilter := key.Message.TestFilter; testFilter != "" {
		environment["TESTBRIDGE_TEST_ONLY"] = testFilter
	}
	referenceFormat := c.referenceFormat
	environmentVariableList, _, err := convertDictToEnvironmentVariableList(
		ctx,
		environment,
		actionEncoder,
		referenceFormat,
		e,
	)
	if err != nil {
		return PatchedTargetTestResultValue[TMetadata]{}, err
	}
	// Capture structured XML when the test binary writes XML_OUTPUT_FILE.
	// For sharded tests the status file proves that the test framework
	// actually honored TEST_SHARD_INDEX instead of executing every case in
	// every shard. PathPattern children must be sorted by name.
	outputFiles := []*model_command_pb.PathPattern_Child{}
	if count > 1 {
		outputFiles = append(outputFiles, &model_command_pb.PathPattern_Child{
			Name:    "test.shard.status",
			Pattern: &model_command_pb.PathPattern{},
		})
	}
	outputFiles = append(outputFiles, &model_command_pb.PathPattern_Child{
		Name:    "test.xml",
		Pattern: &model_command_pb.PathPattern{},
	})
	createdCommand, err := model_core.MarshalAndEncodeDeterministic(
		model_core.NewPatchedMessage(
			model_core.NewProtoBinaryMarshaler(&model_command_pb.Command{
				Arguments: []*model_command_pb.ArgumentList_Element{{
					Level: &model_command_pb.ArgumentList_Element_Leaf{
						Leaf: executablePath,
					},
				}},
				EnvironmentVariables:        environmentVariableList.Message,
				DirectoryCreationParameters: directoryCreationParametersMessage.Message.DirectoryCreationParameters,
				FileCreationParameters:      fileCreationParametersMessage.Message.FileCreationParameters,
				WorkingDirectory:            (*path.Trace)(nil).GetUNIXString(),
				OutputPathPattern: &model_command_pb.PathPattern{
					Children: &model_command_pb.PathPattern_ChildrenInline{
						ChildrenInline: &model_command_pb.PathPattern_Children{Children: outputFiles},
					},
				},
			}),
			environmentVariableList.Patcher,
		),
		referenceFormat,
		actionEncoder,
	)
	if err != nil {
		return PatchedTargetTestResultValue[TMetadata]{}, fmt.Errorf("failed to create command: %w", err)
	}

	inputRootReference, err := c.createMerkleTreeFromChangeTrackingDirectory(
		ctx,
		e,
		&rootDirectory,
		directoryCreationParameters,
		directoryReaders,
		/* fileCreationParameters = */ nil,
		/* patchedFiles = */ nil,
	)
	if err != nil {
		return PatchedTargetTestResultValue[TMetadata]{}, fmt.Errorf("failed to create Merkle tree of input root: %w", err)
	}

	action, err := model_core.BuildPatchedMessage(func(patcher *model_core.ReferenceMessagePatcher[TMetadata]) (encoding.BinaryMarshaler, error) {
		patcher.Merge(inputRootReference.Patcher)
		commandReference, err := patcher.CaptureAndAddDecodableReference(ctx, createdCommand, e)
		if err != nil {
			return nil, err
		}
		return model_core.NewProtoBinaryMarshaler(&model_command_pb.Action{
			CommandReference:   commandReference,
			InputRootReference: inputRootReference.Message,
		}), nil
	})
	if err != nil {
		return PatchedTargetTestResultValue[TMetadata]{}, fmt.Errorf("failed to create action: %w", err)
	}
	createdAction, err := model_core.MarshalAndEncodeDeterministic(action, referenceFormat, actionEncoder)
	if err != nil {
		return PatchedTargetTestResultValue[TMetadata]{}, fmt.Errorf("failed to encode action: %w", err)
	}

	// A test that fails is a result, not a build failure, so this
	// requests CompletedActionResult instead of the
	// SuccessfulActionResult that build actions use.
	actionResultKey, err := model_core.BuildPatchedMessage(func(patcher *model_core.ReferenceMessagePatcher[TMetadata]) (*model_analysis_pb.CompletedActionResult_Key, error) {
		actionReference, err := patcher.CaptureAndAddDecodableReference(ctx, createdAction, e)
		if err != nil {
			return nil, err
		}
		return &model_analysis_pb.CompletedActionResult_Key{
			ExecuteRequest: &model_analysis_pb.ExecuteRequest{
				PlatformPkixPublicKey: platformPkixPublicKey,
				ActionReference:       actionReference,
				ExecutionTimeout:      &durationpb.Duration{Seconds: testExecutionTimeoutSeconds},
			},
		}, nil
	})
	if err != nil {
		return PatchedTargetTestResultValue[TMetadata]{}, fmt.Errorf("failed to create action result key: %w", err)
	}
	actionResult := e.GetCompletedActionResultValue(actionResultKey)
	if !actionResult.IsSet() {
		return PatchedTargetTestResultValue[TMetadata]{}, evaluation.ErrMissingDependency
	}
	if count > 1 && actionResult.Message.ExitCode == 0 {
		outputs, err := model_parser.MaybeDereference(ctx, directoryReaders.CommandOutputs, model_core.Nested(actionResult, actionResult.Message.OutputsReference))
		if err != nil {
			return PatchedTargetTestResultValue[TMetadata]{}, fmt.Errorf("test %#v shard %d/%d: cannot read outputs: %w", key.Message.Label, key.Message.ShardIndex, count, err)
		}
		if outputs.Message.OutputRoot == nil {
			return PatchedTargetTestResultValue[TMetadata]{}, fmt.Errorf("test %#v shard %d/%d did not write TEST_SHARD_STATUS_FILE", key.Message.Label, key.Message.ShardIndex, count)
		}
		leaves, err := model_filesystem.DirectoryGetLeaves(ctx, directoryReaders.Leaves, model_core.Nested(outputs, outputs.Message.OutputRoot))
		if err != nil {
			return PatchedTargetTestResultValue[TMetadata]{}, fmt.Errorf("test %#v shard %d/%d: cannot read output files: %w", key.Message.Label, key.Message.ShardIndex, count, err)
		}
		if _, found := sort.Find(len(leaves.Message.Files), func(i int) int {
			return strings.Compare("test.shard.status", leaves.Message.Files[i].Name)
		}); !found {
			return PatchedTargetTestResultValue[TMetadata]{}, fmt.Errorf("test %#v shard %d/%d did not write TEST_SHARD_STATUS_FILE", key.Message.Label, key.Message.ShardIndex, count)
		}
	}

	status := model_analysis_pb.TestStatus_TEST_STATUS_PASSED
	if actionResult.Message.ExitCode != 0 {
		status = model_analysis_pb.TestStatus_TEST_STATUS_FAILED
	}
	patchedOutputsReference := model_core.Patch(e, model_core.Nested(actionResult, actionResult.Message.OutputsReference))
	return model_core.NewPatchedMessage(
		&model_analysis_pb.TargetTestResult_Value{
			Status:           status,
			ExitCode:         actionResult.Message.ExitCode,
			ShardIndex:       key.Message.ShardIndex,
			OutputsReference: patchedOutputsReference.Message,
		},
		patchedOutputsReference.Patcher,
	), nil
}

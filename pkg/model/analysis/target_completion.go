package analysis

import (
	"context"
	"errors"
	"fmt"
	"slices"

	model_core "bonanza.build/pkg/model/core"
	"bonanza.build/pkg/model/evaluation"
	model_filesystem "bonanza.build/pkg/model/filesystem"
	model_parser "bonanza.build/pkg/model/parser"
	model_starlark "bonanza.build/pkg/model/starlark"
	model_analysis_pb "bonanza.build/pkg/proto/model/analysis"
	model_starlark_pb "bonanza.build/pkg/proto/model/starlark"
	"bonanza.build/pkg/storage/object"

	"github.com/buildbarn/bb-storage/pkg/filesystem/path"
	"github.com/buildbarn/bb-storage/pkg/util"

	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/semaphore"
)

func (c *baseComputer[TReference, TMetadata]) ComputeTargetCompletionValue(ctx context.Context, key model_core.Message[*model_analysis_pb.TargetCompletion_Key, TReference], e TargetCompletionEnvironment[TReference, TMetadata]) (PatchedTargetCompletionValue[TMetadata], error) {
	directoryCreationParameters, gotDirectoryCreationParameters := e.GetDirectoryCreationParametersObjectValue(&model_analysis_pb.DirectoryCreationParametersObject_Key{})
	directoryReaders, gotDirectoryReaders := e.GetDirectoryReadersValue(&model_analysis_pb.DirectoryReaders_Key{})
	if !gotDirectoryCreationParameters || !gotDirectoryReaders {
		return PatchedTargetCompletionValue[TMetadata]{}, evaluation.ErrMissingDependency
	}
	loadOptions := &changeTrackingDirectoryLoadOptions[TReference]{
		context:                 ctx,
		directoryContentsReader: directoryReaders.DirectoryContents,
		leavesReader:            directoryReaders.Leaves,
	}
	var rootDirectory changeTrackingDirectory[TReference, TMetadata]
	var defaultInfo model_core.Message[*model_starlark_pb.Struct_Fields, TReference]
	var outputGroupInfo model_core.Message[*model_analysis_pb.TargetProvider_Value, TReference]
	outputGroupInfoLoaded := false
	groups := key.Message.OutputGroups
	if len(groups) == 0 {
		groups = []string{"default"}
	}
	for _, groupName := range groups {
		var fields model_core.Message[*model_starlark_pb.Struct_Fields, TReference]
		if groupName == "default" {
			var err error
			defaultInfo, err = getProviderFromConfiguredTarget(
				e, key.Message.Label,
				model_core.Patch(e, model_core.Nested(key, key.Message.ConfigurationReference)),
				defaultInfoProviderIdentifier,
			)
			if err != nil {
				return PatchedTargetCompletionValue[TMetadata]{}, err
			}
			fields = defaultInfo
			groupName = "files"
		} else {
			if !outputGroupInfoLoaded {
				outputGroupInfo = e.GetTargetProviderValue(model_core.MustBuildPatchedMessage(func(patcher *model_core.ReferenceMessagePatcher[TMetadata]) *model_analysis_pb.TargetProvider_Key {
					return &model_analysis_pb.TargetProvider_Key{
						Label:                  key.Message.Label,
						ConfigurationReference: model_core.Patch(e, model_core.Nested(key, key.Message.ConfigurationReference)).Merge(patcher),
						ProviderIdentifier:     "@@builtins_core+//:exports.bzl%OutputGroupInfo",
					}
				}))
				if !outputGroupInfo.IsSet() {
					return PatchedTargetCompletionValue[TMetadata]{}, evaluation.ErrMissingDependency
				}
				outputGroupInfoLoaded = true
			}
			if outputGroupInfo.Message.Fields == nil {
				continue
			}
			fields = model_core.Nested(outputGroupInfo, outputGroupInfo.Message.Fields)
			if !slices.Contains(fields.Message.Keys, groupName) {
				continue
			}
		}
		files, err := model_starlark.GetStructFieldValue(ctx, c.valueReaders.List, fields, groupName)
		if err != nil {
			return PatchedTargetCompletionValue[TMetadata]{}, fmt.Errorf("read output group %q: %w", groupName, err)
		}
		filesDepset, ok := files.Message.Kind.(*model_starlark_pb.Value_Depset)
		if !ok {
			return PatchedTargetCompletionValue[TMetadata]{}, fmt.Errorf("output group %q is not a depset", groupName)
		}
		if err := addFilesToChangeTrackingDirectory(
			e,
			model_core.Nested(files, filesDepset.Depset.Elements),
			&rootDirectory,
			loadOptions,
			model_analysis_pb.DirectoryLayout_INPUT_ROOT,
		); err != nil {
			return PatchedTargetCompletionValue[TMetadata]{}, fmt.Errorf("materialize output group %q: %w", groupName, err)
		}
	}

	// Only the default group materializes the executable and runfiles. An
	// image metadata query must not accidentally run a binary link action.
	var executablePath string
	var runfilesDirectory *changeTrackingDirectory[TReference, TMetadata]
	if defaultInfo.Message != nil {
		var err error
		executablePath, runfilesDirectory, err = c.getExecutableAndRunfiles(ctx, e, defaultInfo, &rootDirectory, loadOptions)
		if err != nil {
			return PatchedTargetCompletionValue[TMetadata]{}, err
		}
	}

	group, groupCtx := errgroup.WithContext(ctx)
	var createdRootDirectory model_filesystem.CreatedDirectory[TMetadata]
	group.Go(func() error {
		return model_filesystem.CreateDirectoryMerkleTree[TMetadata, TMetadata](
			groupCtx,
			semaphore.NewWeighted(1),
			group,
			directoryCreationParameters,
			&capturableChangeTrackingDirectory[TReference, TMetadata]{
				options: &capturableChangeTrackingDirectoryOptions[TReference, TMetadata]{
					context:                 ctx,
					directoryContentsReader: directoryReaders.DirectoryContents,
					objectCapturer:          e,
				},
				directory: &rootDirectory,
			},
			model_filesystem.NewSimpleDirectoryMerkleTreeCapturer[TMetadata](e),
			&createdRootDirectory,
		)
	})
	var createdRunfilesDirectory model_filesystem.CreatedDirectory[TMetadata]
	if runfilesDirectory != nil {
		group.Go(func() error {
			return model_filesystem.CreateDirectoryMerkleTree[TMetadata, TMetadata](
				groupCtx,
				semaphore.NewWeighted(1),
				group,
				directoryCreationParameters,
				&capturableChangeTrackingDirectory[TReference, TMetadata]{
					options: &capturableChangeTrackingDirectoryOptions[TReference, TMetadata]{
						context:                 ctx,
						directoryContentsReader: directoryReaders.DirectoryContents,
						objectCapturer:          e,
					},
					directory: runfilesDirectory,
				},
				model_filesystem.NewSimpleDirectoryMerkleTreeCapturer[TMetadata](e),
				&createdRunfilesDirectory,
			)
		})
	}
	if err := group.Wait(); err != nil {
		return PatchedTargetCompletionValue[TMetadata]{}, err
	}

	value := &model_analysis_pb.TargetCompletion_Value{
		OutputRoot:     createdRootDirectory.Message.Message,
		ExecutablePath: executablePath,
	}
	patcher := createdRootDirectory.Message.Patcher
	if runfilesDirectory != nil {
		value.RunfilesDirectory = createdRunfilesDirectory.Message.Message
		patcher.Merge(createdRunfilesDirectory.Message.Patcher)
	}
	return model_core.NewPatchedMessage(value, patcher), nil
}

// TargetCompletionEnvironmentForTesting lets the existing analysis test
// harness exercise non-default output groups without running a worker.
type TargetCompletionEnvironmentForTesting TargetCompletionEnvironment[model_core.CreatedObjectTree, model_core.CreatedObjectTree]

// getExecutableAndRunfiles extracts the FilesToRunProvider from the
// DefaultInfo provider of a target. If the target provides an
// executable, the executable is added to the output root that is
// provided, and a separate directory hierarchy containing its runfiles
// is returned.
func (c *baseComputer[TReference, TMetadata]) getExecutableAndRunfiles(
	ctx context.Context,
	e TargetCompletionEnvironment[TReference, TMetadata],
	defaultInfo model_core.Message[*model_starlark_pb.Struct_Fields, TReference],
	outputRoot *changeTrackingDirectory[TReference, TMetadata],
	loadOptions *changeTrackingDirectoryLoadOptions[TReference],
) (string, *changeTrackingDirectory[TReference, TMetadata], error) {
	filesToRunValue, err := model_starlark.GetStructFieldValue(ctx, c.valueReaders.List, defaultInfo, "files_to_run")
	if err != nil {
		return "", nil, fmt.Errorf("failed to obtain \"files_to_run\" field of DefaultInfo provider: %w", err)
	}
	filesToRunStruct, ok := filesToRunValue.Message.Kind.(*model_starlark_pb.Value_Struct)
	if !ok {
		return "", nil, errors.New("\"files_to_run\" field of DefaultInfo provider is not a struct")
	}
	filesToRun := model_core.Nested(filesToRunValue, filesToRunStruct.Struct.Fields)

	executableValue, err := model_starlark.GetStructFieldValue(ctx, c.valueReaders.List, filesToRun, "executable")
	if err != nil {
		return "", nil, fmt.Errorf("failed to obtain \"files_to_run.executable\" field of DefaultInfo provider: %w", err)
	}
	executableFile, ok := executableValue.Message.Kind.(*model_starlark_pb.Value_File)
	if !ok {
		// Targets that are not executable leave this field set
		// to None.
		return "", nil, nil
	}
	executable := model_core.Nested(executableValue, executableFile.File)

	// The executable is not guaranteed to be part of
	// DefaultInfo.files, even though running the target requires it
	// to be present in the output root.
	if err := addFileToChangeTrackingDirectory(
		e,
		executable,
		outputRoot,
		loadOptions,
		model_analysis_pb.DirectoryLayout_INPUT_ROOT,
	); err != nil {
		return "", nil, fmt.Errorf("failed to add executable to output root: %w", err)
	}
	executablePath, err := model_starlark.FileGetInputRootPath(executable, nil)
	if err != nil {
		return "", nil, fmt.Errorf("failed to get path of executable: %w", err)
	}

	runfilesDirectory := &changeTrackingDirectory[TReference, TMetadata]{}
	runfilesFiles, err := getDepsetElements(ctx, c.valueReaders.List, filesToRun, "_runfiles_files")
	if err != nil {
		return "", nil, err
	}
	if err := addFilesToChangeTrackingDirectory(
		e,
		runfilesFiles,
		runfilesDirectory,
		loadOptions,
		model_analysis_pb.DirectoryLayout_RUNFILES,
	); err != nil {
		return "", nil, fmt.Errorf("failed to add runfiles to runfiles directory: %w", err)
	}

	// Create a ctx.workspace_name == "_main" directory. This is
	// needed to make path lookups of the form
	// "${RUNFILES_DIR}/_main/../${path}" work.
	if _, err := runfilesDirectory.getOrCreateDirectory(componentMainWorkspaceName); err != nil {
		return "", nil, err
	}

	// Runfiles symbolic links are placed at a location that is
	// chosen by the rule, instead of the location that is implied by
	// the label of the file.
	for _, symlinks := range []struct {
		fieldName string
		prefix    string
	}{
		{fieldName: "_runfiles_symlinks", prefix: componentMainWorkspaceName.String() + "/"},
		{fieldName: "_runfiles_root_symlinks", prefix: ""},
	} {
		entries, err := getDepsetElements(ctx, c.valueReaders.List, filesToRun, symlinks.fieldName)
		if err != nil {
			return "", nil, err
		}
		if err := c.addRunfilesSymlinks(ctx, e, entries, runfilesDirectory, symlinks.prefix, loadOptions); err != nil {
			return "", nil, fmt.Errorf("failed to add entries of %#v to runfiles directory: %w", symlinks.fieldName, err)
		}
	}

	return executablePath, runfilesDirectory, nil
}

// addRunfilesSymlinks handles both the workspace-relative and root-relative
// entries of FilesToRunProvider. Tools need the same runfiles tree as targets
// built for `run`, including entries held in external depset nodes.
func (c *baseComputer[TReference, TMetadata]) addRunfilesSymlinks(
	ctx context.Context,
	e addFilesToChangeTrackingDirectoryEnvironment[TReference, TMetadata],
	entries model_core.Message[[]*model_starlark_pb.List_Element, TReference],
	runfilesDirectory *changeTrackingDirectory[TReference, TMetadata],
	prefix string,
	loadOptions *changeTrackingDirectoryLoadOptions[TReference],
) error {
	var errIter error
	for entry := range model_starlark.AllListLeafElements(ctx, c.valueReaders.List, entries, &errIter) {
		if err := c.addRunfilesSymlink(ctx, e, entry, runfilesDirectory, prefix, loadOptions); err != nil {
			return err
		}
	}
	return errIter
}

// addRunfilesSymlink adds a single SymlinkEntry contained in a
// FilesToRunProvider to a runfiles directory. As the file needs to be
// placed at a path that differs from the one implied by its label, the
// file is first resolved in a directory hierarchy of its own.
func (c *baseComputer[TReference, TMetadata]) addRunfilesSymlink(
	ctx context.Context,
	e addFilesToChangeTrackingDirectoryEnvironment[TReference, TMetadata],
	entry model_core.Message[*model_starlark_pb.Value, TReference],
	runfilesDirectory *changeTrackingDirectory[TReference, TMetadata],
	prefix string,
	loadOptions *changeTrackingDirectoryLoadOptions[TReference],
) error {
	entryStruct, ok := entry.Message.Kind.(*model_starlark_pb.Value_Struct)
	if !ok {
		return errors.New("entry is not a SymlinkEntry struct")
	}
	entryFields := model_core.Nested(entry, entryStruct.Struct.Fields)

	pathValue, err := model_starlark.GetStructFieldValue(ctx, c.valueReaders.List, entryFields, "path")
	if err != nil {
		return fmt.Errorf("failed to obtain \"path\" field: %w", err)
	}
	pathStr, ok := pathValue.Message.Kind.(*model_starlark_pb.Value_Str)
	if !ok {
		return errors.New("\"path\" field is not a string")
	}

	targetFileValue, err := model_starlark.GetStructFieldValue(ctx, c.valueReaders.List, entryFields, "target_file")
	if err != nil {
		return fmt.Errorf("failed to obtain \"target_file\" field: %w", err)
	}
	targetFile, ok := targetFileValue.Message.Kind.(*model_starlark_pb.Value_File)
	if !ok {
		return errors.New("\"target_file\" field is not a file")
	}
	file := model_core.Nested(targetFileValue, targetFile.File)

	var sourceDirectory changeTrackingDirectory[TReference, TMetadata]
	if err := addFileToChangeTrackingDirectory(
		e,
		file,
		&sourceDirectory,
		loadOptions,
		model_analysis_pb.DirectoryLayout_RUNFILES,
	); err != nil {
		return err
	}
	sourcePath, err := model_starlark.FileGetRunfilesPath(file)
	if err != nil {
		return err
	}
	sourceResolver := changeTrackingDirectoryExistingFileResolver[TReference, TMetadata]{
		loadOptions: loadOptions,
		stack:       util.NewNonEmptyStack(&sourceDirectory),
	}
	if err := path.Resolve(path.UNIXFormat.NewParser(sourcePath), &sourceResolver); err != nil {
		return fmt.Errorf("failed to resolve path %#v: %w", sourcePath, err)
	}
	resolvedFile, err := sourceResolver.getFile()
	if err != nil {
		return fmt.Errorf("failed to resolve path %#v: %w", sourcePath, err)
	}

	targetPath := prefix + pathStr.Str
	targetResolver := changeTrackingDirectoryNewFileResolver[TReference, TMetadata]{
		loadOptions: loadOptions,
		stack:       util.NewNonEmptyStack(runfilesDirectory),
	}
	if err := path.Resolve(path.UNIXFormat.NewParser(targetPath), &targetResolver); err != nil {
		return fmt.Errorf("failed to resolve path %#v: %w", targetPath, err)
	}
	if targetResolver.TerminalName == nil {
		return fmt.Errorf("path %#v does not resolve to a file", targetPath)
	}
	return targetResolver.stack.Peek().setFile(loadOptions, *targetResolver.TerminalName, resolvedFile)
}

// getDepsetElements extracts the list of elements of a depset that is
// stored in a field of a struct.
func getDepsetElements[TReference object.BasicReference](
	ctx context.Context,
	reader model_parser.MessageObjectReader[TReference, []*model_starlark_pb.List_Element],
	structFields model_core.Message[*model_starlark_pb.Struct_Fields, TReference],
	name string,
) (model_core.Message[[]*model_starlark_pb.List_Element, TReference], error) {
	value, err := model_starlark.GetStructFieldValue(ctx, reader, structFields, name)
	if err != nil {
		return model_core.Message[[]*model_starlark_pb.List_Element, TReference]{}, fmt.Errorf("failed to obtain %#v field: %w", name, err)
	}
	depset, ok := value.Message.Kind.(*model_starlark_pb.Value_Depset)
	if !ok {
		return model_core.Message[[]*model_starlark_pb.List_Element, TReference]{}, fmt.Errorf("%#v field is not a depset", name)
	}
	return model_core.Nested(value, depset.Depset.Elements), nil
}

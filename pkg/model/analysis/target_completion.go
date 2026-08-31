package analysis

import (
	"context"
	"errors"

	model_core "bonanza.build/pkg/model/core"
	"bonanza.build/pkg/model/evaluation"
	model_filesystem "bonanza.build/pkg/model/filesystem"
	model_starlark "bonanza.build/pkg/model/starlark"
	model_analysis_pb "bonanza.build/pkg/proto/model/analysis"
	model_starlark_pb "bonanza.build/pkg/proto/model/starlark"

	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/semaphore"
)

func (c *baseComputer[TReference, TMetadata]) ComputeTargetCompletionValue(ctx context.Context, key model_core.Message[*model_analysis_pb.TargetCompletion_Key, TReference], e TargetCompletionEnvironment[TReference, TMetadata]) (PatchedTargetCompletionValue[TMetadata], error) {
	// TODO: This should also respect --output_groups.
	directoryCreationParameters, gotDirectoryCreationParameters := e.GetDirectoryCreationParametersObjectValue(&model_analysis_pb.DirectoryCreationParametersObject_Key{})
	directoryReaders, gotDirectoryReaders := e.GetDirectoryReadersValue(&model_analysis_pb.DirectoryReaders_Key{})
	defaultInfo, err := getProviderFromConfiguredTarget(
		e,
		key.Message.Label,
		model_core.Patch(e, model_core.Nested(key, key.Message.ConfigurationReference)),
		defaultInfoProviderIdentifier,
	)
	if err != nil {
		return PatchedTargetCompletionValue[TMetadata]{}, err
	}
	if !gotDirectoryCreationParameters || !gotDirectoryReaders {
		return PatchedTargetCompletionValue[TMetadata]{}, evaluation.ErrMissingDependency
	}

	files, err := model_starlark.GetStructFieldValue(ctx, c.valueReaders.List, defaultInfo, "files")
	if err != nil {
		return PatchedTargetCompletionValue[TMetadata]{}, err
	}
	filesDepset, ok := files.Message.Kind.(*model_starlark_pb.Value_Depset)
	if !ok {
		return PatchedTargetCompletionValue[TMetadata]{}, errors.New("\"files\" field of DefaultInfo provider is not a depset")
	}

	var rootDirectory changeTrackingDirectory[TReference, TMetadata]
	if err := addFilesToChangeTrackingDirectory(
		e,
		model_core.Nested(files, filesDepset.Depset.Elements),
		&rootDirectory,
		&changeTrackingDirectoryLoadOptions[TReference]{
			context:                 ctx,
			directoryContentsReader: directoryReaders.DirectoryContents,
			leavesReader:            directoryReaders.Leaves,
		},
		model_analysis_pb.DirectoryLayout_INPUT_ROOT,
	); err != nil {
		return PatchedTargetCompletionValue[TMetadata]{}, err
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
	if err := group.Wait(); err != nil {
		return PatchedTargetCompletionValue[TMetadata]{}, err
	}

	return model_core.NewPatchedMessage(
		&model_analysis_pb.TargetCompletion_Value{
			OutputRoot: createdRootDirectory.Message.Message,
		},
		createdRootDirectory.Message.Patcher,
	), nil
}

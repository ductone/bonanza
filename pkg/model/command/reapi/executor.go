package reapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"

	model_command "bonanza.build/pkg/model/command"
	model_core "bonanza.build/pkg/model/core"
	"bonanza.build/pkg/model/core/btree"
	model_encoding "bonanza.build/pkg/model/encoding"
	model_executewithstorage "bonanza.build/pkg/model/executewithstorage"
	model_filesystem "bonanza.build/pkg/model/filesystem"
	model_parser "bonanza.build/pkg/model/parser"
	model_command_pb "bonanza.build/pkg/proto/model/command"
	model_core_pb "bonanza.build/pkg/proto/model/core"
	model_filesystem_pb "bonanza.build/pkg/proto/model/filesystem"
	remoteworker_pb "bonanza.build/pkg/proto/remoteworker"
	"bonanza.build/pkg/remoteworker"
	"bonanza.build/pkg/storage/dag"
	"bonanza.build/pkg/storage/object"
	object_namespacemapping "bonanza.build/pkg/storage/object/namespacemapping"

	remoteexecution "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	bb_filesystem "github.com/buildbarn/bb-storage/pkg/filesystem"
	"github.com/buildbarn/bb-storage/pkg/filesystem/path"

	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/semaphore"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
)

const (
	// QueueSmall is the C1 Buildbarn queue used for ordinary actions.
	QueueSmall = "small"
	// QueueLink is the C1 Buildbarn queue used for link actions.
	QueueLink = "link"
)

var commandActionObjectFormat = model_core.NewProtoObjectFormat(&model_command_pb.Action{})

// Configuration describes the fixed, C1-supported REv2 platform for a worker.
// Arbitrary platform properties are intentionally not supported: Buildbarn
// matches a platform exactly, and adding an unregistered property parks work.
type Configuration struct {
	InstanceName string
	Queue        string
	WorkerID     map[string]string
}

type executor struct {
	objectDownloader              object.Downloader[object.GlobalReference]
	objectStoreSemaphore          *semaphore.Weighted
	parsedObjectPool              *model_parser.ParsedObjectPool
	dagUploader                   dag.Uploader[object.InstanceName, object.GlobalReference]
	objectContentsWalkerSemaphore *semaphore.Weighted
	client                        Client
	instanceName                  string
	queue                         string
	workerID                      map[string]string
}

// NewExecutor creates a worker executor that translates supported Bonanza
// command actions to REv2. It does not create or require a FUSE mount.
func NewExecutor(
	objectDownloader object.Downloader[object.GlobalReference],
	objectStoreSemaphore *semaphore.Weighted,
	parsedObjectPool *model_parser.ParsedObjectPool,
	dagUploader dag.Uploader[object.InstanceName, object.GlobalReference],
	objectContentsWalkerSemaphore *semaphore.Weighted,
	client Client,
	configuration Configuration,
) (remoteworker.Executor[*model_executewithstorage.Action[object.GlobalReference], model_core.Decodable[object.LocalReference], model_core.Decodable[object.LocalReference]], error) {
	if client == nil {
		return nil, status.Error(codes.InvalidArgument, "no REAPI client configured")
	}
	if strings.HasPrefix(configuration.InstanceName, "/") || strings.HasSuffix(configuration.InstanceName, "/") || strings.Contains(configuration.InstanceName, "//") {
		return nil, status.Errorf(codes.InvalidArgument, "invalid REAPI instance name %q", configuration.InstanceName)
	}
	if configuration.InstanceName != "" {
		for _, component := range strings.Split(configuration.InstanceName, "/") {
			if _, ok := path.NewComponent(component); !ok {
				return nil, status.Errorf(codes.InvalidArgument, "invalid REAPI instance name %q", configuration.InstanceName)
			}
		}
	}
	if configuration.Queue != QueueSmall && configuration.Queue != QueueLink {
		return nil, status.Errorf(codes.InvalidArgument, "unsupported C1 REAPI queue %q", configuration.Queue)
	}
	if objectStoreSemaphore == nil {
		return nil, status.Error(codes.InvalidArgument, "no object-store concurrency semaphore configured")
	}
	if objectContentsWalkerSemaphore == nil {
		return nil, status.Error(codes.InvalidArgument, "no object-contents walker semaphore configured")
	}
	return &executor{
		objectDownloader:              objectDownloader,
		objectStoreSemaphore:          objectStoreSemaphore,
		parsedObjectPool:              parsedObjectPool,
		dagUploader:                   dagUploader,
		objectContentsWalkerSemaphore: objectContentsWalkerSemaphore,
		client:                        client,
		instanceName:                  configuration.InstanceName,
		queue:                         configuration.Queue,
		workerID:                      configuration.WorkerID,
	}, nil
}

// validateStatelessCommand rejects command features whose semantics require the
// repository-action execution boundary. REv2 does not preserve either writable
// input inodes or a stable absolute input-root path. More importantly, this
// worker has no identity-bound, isolated credential broker, so forwarding such
// an action would risk executing it with another worker's credentials.
//
// Call this before flattening the command environment or uploading any REv2
// input. Repository credentials must never enter a content-addressed object,
// Action, Command, result, or log.
func validateStatelessCommand(command *model_command_pb.Command) error {
	if command.GetNeedsWritableInputFiles() || command.GetStableInputRootPathUuid() != "" {
		return status.Error(codes.FailedPrecondition, "stateful repository action requires an isolated native worker with a verified identity-bound credential broker")
	}
	return nil
}

func (e *executor) CheckReadiness(ctx context.Context) error {
	if err := e.client.CheckReadiness(ctx); err != nil {
		return fmt.Errorf("REAPI backend is not ready: %w", err)
	}
	return nil
}

func (e *executor) Execute(
	ctx context.Context,
	action *model_executewithstorage.Action[object.GlobalReference],
	executionTimeout time.Duration,
	executionEvents chan<- model_core.Decodable[object.LocalReference],
) (model_core.Decodable[object.LocalReference], time.Duration, remoteworker_pb.CurrentState_Completed_Result, error) {
	var badReference model_core.Decodable[object.LocalReference]
	if action == nil {
		return badReference, 0, 0, status.Error(codes.InvalidArgument, "no Bonanza action provided")
	}
	if !proto.Equal(action.Format, commandActionObjectFormat) {
		return badReference, 0, 0, status.Error(codes.InvalidArgument, "this worker cannot execute actions of this type")
	}

	// This backend currently has no intermediate Bonanza execution event to
	// report. Keep the argument in the signature so scheduler cancellation and
	// completion behavior remain provided by remoteworker.Client.
	_ = executionEvents

	referenceFormat := action.Reference.Value.GetReferenceFormat()
	actionEncoder, err := model_encoding.NewDeterministicBinaryEncoderFromProto(
		action.Encoders,
		uint32(referenceFormat.GetMaximumObjectSizeBytes()),
	)
	if err != nil {
		return badReference, 0, 0, status.Error(codes.InvalidArgument, "invalid action encoders")
	}

	parsedObjectPoolIngester := model_parser.NewParsedObjectPoolIngester(
		e.parsedObjectPool,
		model_parser.NewDownloadingObjectReader(
			object_namespacemapping.NewNamespaceAddingDownloader(
				e.objectDownloader,
				action.Reference.Value.InstanceName,
			),
		),
	)

	var virtualExecutionDuration time.Duration
	result := model_core.MustBuildPatchedMessage(func(resultPatcher *model_core.ReferenceMessagePatcher[dag.ObjectContentsWalker]) *model_command_pb.Result {
		result := &model_command_pb.Result{WorkerId: e.workerID}
		setError := func(err error) {
			err = executionFailureStatus(err)
			if result.Status == nil {
				result.Status = status.Convert(err).Proto()
			}
		}

		actionMessage, command, fileCreationParameters, directoryCreationParameters, err := e.readAction(
			ctx,
			parsedObjectPoolIngester,
			action,
			actionEncoder,
		)
		if err != nil {
			setError(fmt.Errorf("read Bonanza command action: %w", err))
			return result
		}

		if err := validateStatelessCommand(command.Message); err != nil {
			setError(err)
			return result
		}
		if executionTimeout <= 0 {
			setError(status.Error(codes.DeadlineExceeded, "Bonanza scheduler provided a non-positive execution timeout"))
			return result
		}

		arguments, err := flattenArguments(ctx, parsedObjectPoolIngester, command, actionEncoder)
		if err != nil {
			setError(fmt.Errorf("decode Bonanza command arguments: %w", err))
			return result
		}
		if len(arguments) == 0 {
			setError(status.Error(codes.InvalidArgument, "Bonanza command has no arguments"))
			return result
		}
		environmentVariables, err := flattenEnvironmentVariables(ctx, parsedObjectPoolIngester, command, actionEncoder)
		if err != nil {
			setError(fmt.Errorf("decode Bonanza command environment: %w", err))
			return result
		}
		workingDirectory, workingDirectoryComponents, err := normalizeWorkingDirectory(command.Message.WorkingDirectory)
		if err != nil {
			setError(err)
			return result
		}
		outputPaths, outputPatternSet, err := collectOutputPaths(
			ctx,
			parsedObjectPoolIngester,
			command,
			actionEncoder,
			workingDirectoryComponents,
		)
		if err != nil {
			setError(err)
			return result
		}

		inputRootReference, err := model_core.FlattenDecodableReference(
			model_core.Nested(actionMessage, actionMessage.Message.InputRootReference.GetReference()),
		)
		if err != nil {
			setError(fmt.Errorf("decode Bonanza input-root reference: %w", err))
			return result
		}
		inputRootDigest, err := e.uploadInputDirectory(
			ctx,
			parsedObjectPoolIngester,
			inputRootReference,
			directoryCreationParameters,
			fileCreationParameters,
		)
		if err != nil {
			setError(fmt.Errorf("upload Bonanza input root to REAPI CAS: %w", err))
			return result
		}

		remoteCommand := &remoteexecution.Command{
			Arguments:             arguments,
			EnvironmentVariables:  environmentVariables,
			OutputPaths:           outputPaths,
			WorkingDirectory:      workingDirectory,
			OutputDirectoryFormat: remoteexecution.Command_TREE_ONLY,
		}
		commandDigest, err := e.uploadProto(ctx, remoteCommand)
		if err != nil {
			setError(fmt.Errorf("upload REAPI command: %w", err))
			return result
		}
		remoteAction := &remoteexecution.Action{
			CommandDigest:   commandDigest,
			InputRootDigest: inputRootDigest,
			Timeout:         durationpb.New(executionTimeout),
			Platform: &remoteexecution.Platform{Properties: []*remoteexecution.Platform_Property{{
				Name:  "c1.queue",
				Value: e.queue,
			}}},
		}
		actionDigest, err := e.uploadProto(ctx, remoteAction)
		if err != nil {
			setError(fmt.Errorf("upload REAPI action: %w", err))
			return result
		}

		response, executionDuration, executeErr := e.executeREAPI(ctx, actionDigest, executionTimeout)
		virtualExecutionDuration = executionDuration
		if executeErr != nil {
			setError(executionFailureStatus(fmt.Errorf("execute REAPI action: %w", executeErr)))
			return result
		}
		if response.Status != nil && response.Status.Code != int32(codes.OK) {
			result.Status = response.Status
			return result
		}
		if response.Result == nil {
			setError(status.Error(codes.Internal, "REAPI execution succeeded without an action result"))
			return result
		}
		if metadata := response.Result.ExecutionMetadata; metadata != nil && metadata.WorkerStartTimestamp != nil && metadata.WorkerCompletedTimestamp != nil {
			if err := metadata.WorkerStartTimestamp.CheckValid(); err == nil {
				if err := metadata.WorkerCompletedTimestamp.CheckValid(); err == nil {
					if completed := metadata.WorkerCompletedTimestamp.AsTime(); !completed.Before(metadata.WorkerStartTimestamp.AsTime()) {
						virtualExecutionDuration = completed.Sub(metadata.WorkerStartTimestamp.AsTime())
					}
				}
			}
		}
		result.ExitCode = int64(response.Result.ExitCode)

		outputs, outputsPatcher, err := e.importOutputs(
			ctx,
			response.Result,
			outputPatternSet,
			workingDirectoryComponents,
			fileCreationParameters,
			directoryCreationParameters,
		)
		if err != nil {
			setError(fmt.Errorf("import REAPI action outputs: %w", err))
			return result
		}
		if proto.Size(outputs) > 0 {
			createdOutputs, err := model_core.MarshalAndEncodeDeterministic(
				model_core.NewPatchedMessage(
					model_core.NewProtoBinaryMarshaler(outputs),
					outputsPatcher,
				),
				referenceFormat,
				directoryCreationParameters.GetEncoder(),
			)
			if err != nil {
				setError(fmt.Errorf("marshal Bonanza action outputs: %w", err))
				return result
			}
			outputsReference, err := resultPatcher.CaptureAndAddDecodableReference(
				ctx,
				createdOutputs,
				model_core.WalkableCreatedObjectCapturer,
			)
			if err != nil {
				setError(fmt.Errorf("capture Bonanza action outputs: %w", err))
				return result
			}
			result.OutputsReference = outputsReference
		}
		return result
	})

	createdResult, err := model_core.MarshalAndEncodeDeterministic(
		model_core.ProtoToBinaryMarshaler(result),
		referenceFormat,
		actionEncoder,
	)
	if err != nil {
		return badReference, 0, 0, fmt.Errorf("marshal Bonanza REAPI result: %w", err)
	}
	resultReference := createdResult.Value.GetLocalReference()
	if err := e.dagUploader.UploadDAG(
		ctx,
		action.Reference.Value.WithLocalReference(resultReference),
		dag.NewSimpleObjectContentsWalker(createdResult.Value.Contents, createdResult.Value.Metadata),
	); err != nil {
		return badReference, 0, 0, fmt.Errorf("upload Bonanza REAPI result: %w", err)
	}

	resultCode := remoteworker_pb.CurrentState_Completed_SUCCEEDED
	if grpcCode := codes.Code(result.Message.Status.GetCode()); grpcCode != codes.OK {
		if grpcCode == codes.DeadlineExceeded {
			resultCode = remoteworker_pb.CurrentState_Completed_TIMED_OUT
		} else {
			resultCode = remoteworker_pb.CurrentState_Completed_FAILED
		}
	} else if result.Message.ExitCode != 0 {
		resultCode = remoteworker_pb.CurrentState_Completed_FAILED
	}
	return model_core.CopyDecodable(createdResult, resultReference), virtualExecutionDuration, resultCode, nil
}

func (e *executor) executeREAPI(ctx context.Context, actionDigest *remoteexecution.Digest, executionTimeout time.Duration) (*remoteexecution.ExecuteResponse, time.Duration, error) {
	executionContext, cancelExecution := context.WithTimeout(ctx, executionTimeout)
	defer cancelExecution()

	startedExecution := time.Now()
	response, err := e.client.Execute(executionContext, &remoteexecution.ExecuteRequest{
		InstanceName:   e.instanceName,
		ActionDigest:   actionDigest,
		DigestFunction: remoteexecution.DigestFunction_SHA256,
	})
	return response, time.Since(startedExecution), err
}

func executionFailureStatus(err error) error {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, err.Error())
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, err.Error())
	default:
		return err
	}
}

func (e *executor) readAction(
	ctx context.Context,
	parsedObjectPoolIngester *model_parser.ParsedObjectPoolIngester[object.LocalReference],
	action *model_executewithstorage.Action[object.GlobalReference],
	actionEncoder model_encoding.DeterministicBinaryEncoder,
) (
	model_core.Message[*model_command_pb.Action, object.LocalReference],
	model_core.Message[*model_command_pb.Command, object.LocalReference],
	*model_filesystem.FileCreationParameters,
	*model_filesystem.DirectoryCreationParameters,
	error,
) {
	var badAction model_core.Message[*model_command_pb.Action, object.LocalReference]
	var badCommand model_core.Message[*model_command_pb.Command, object.LocalReference]
	actionReader := model_parser.LookupParsedObjectReader(
		parsedObjectPoolIngester,
		model_parser.NewChainedObjectParser(
			model_parser.NewEncodedObjectParser[object.LocalReference](actionEncoder),
			model_parser.NewProtoObjectParser[object.LocalReference, model_command_pb.Action](),
		),
	)
	actionMessage, err := actionReader.ReadObject(ctx, model_core.CopyDecodable(action.Reference, action.Reference.Value.GetLocalReference()))
	if err != nil {
		return badAction, badCommand, nil, nil, err
	}
	commandReader := model_parser.LookupParsedObjectReader(
		parsedObjectPoolIngester,
		model_parser.NewChainedObjectParser(
			model_parser.NewEncodedObjectParser[object.LocalReference](actionEncoder),
			model_parser.NewProtoObjectParser[object.LocalReference, model_command_pb.Command](),
		),
	)
	command, err := model_parser.Dereference(ctx, commandReader, model_core.Nested(actionMessage, actionMessage.Message.CommandReference))
	if err != nil {
		return badAction, badCommand, nil, nil, err
	}
	referenceFormat := action.Reference.Value.GetReferenceFormat()
	fileCreationParameters, err := model_filesystem.NewFileCreationParametersFromProto(command.Message.FileCreationParameters, referenceFormat)
	if err != nil {
		return badAction, badCommand, nil, nil, fmt.Errorf("invalid Bonanza file creation parameters: %w", err)
	}
	directoryCreationParameters, err := model_filesystem.NewDirectoryCreationParametersFromProto(command.Message.DirectoryCreationParameters, referenceFormat)
	if err != nil {
		return badAction, badCommand, nil, nil, fmt.Errorf("invalid Bonanza directory creation parameters: %w", err)
	}
	return actionMessage, command, fileCreationParameters, directoryCreationParameters, nil
}

func flattenArguments(
	ctx context.Context,
	parsedObjectPoolIngester *model_parser.ParsedObjectPoolIngester[object.LocalReference],
	command model_core.Message[*model_command_pb.Command, object.LocalReference],
	actionEncoder model_encoding.DeterministicBinaryEncoder,
) ([]string, error) {
	arguments := make([]string, 0, len(command.Message.Arguments))
	var errIter error
	for element := range btree.AllLeaves(
		ctx,
		model_parser.LookupParsedObjectReader(
			parsedObjectPoolIngester,
			model_parser.NewChainedObjectParser(
				model_parser.NewEncodedObjectParser[object.LocalReference](actionEncoder),
				model_parser.NewProtoListObjectParser[object.LocalReference, model_command_pb.ArgumentList_Element](),
			),
		),
		model_core.Nested(command, command.Message.Arguments),
		func(element model_core.Message[*model_command_pb.ArgumentList_Element, object.LocalReference]) (*model_core_pb.DecodableReference, error) {
			return element.Message.GetParent(), nil
		},
		&errIter,
	) {
		leaf, ok := element.Message.Level.(*model_command_pb.ArgumentList_Element_Leaf)
		if !ok {
			return nil, status.Error(codes.InvalidArgument, "invalid leaf element in Bonanza arguments")
		}
		arguments = append(arguments, leaf.Leaf)
	}
	if errIter != nil {
		return nil, errIter
	}
	return arguments, nil
}

func flattenEnvironmentVariables(
	ctx context.Context,
	parsedObjectPoolIngester *model_parser.ParsedObjectPoolIngester[object.LocalReference],
	command model_core.Message[*model_command_pb.Command, object.LocalReference],
	actionEncoder model_encoding.DeterministicBinaryEncoder,
) ([]*remoteexecution.Command_EnvironmentVariable, error) {
	entriesByName := map[string]string{}
	var errIter error
	for entry := range btree.AllLeaves(
		ctx,
		model_parser.LookupParsedObjectReader(
			parsedObjectPoolIngester,
			model_parser.NewChainedObjectParser(
				model_parser.NewEncodedObjectParser[object.LocalReference](actionEncoder),
				model_parser.NewProtoListObjectParser[object.LocalReference, model_command_pb.EnvironmentVariableList_Element](),
			),
		),
		model_core.Nested(command, command.Message.EnvironmentVariables),
		func(entry model_core.Message[*model_command_pb.EnvironmentVariableList_Element, object.LocalReference]) (*model_core_pb.DecodableReference, error) {
			return entry.Message.GetParent(), nil
		},
		&errIter,
	) {
		leaf, ok := entry.Message.Level.(*model_command_pb.EnvironmentVariableList_Element_Leaf_)
		if !ok {
			return nil, status.Error(codes.InvalidArgument, "invalid leaf entry in Bonanza environment variables")
		}
		if leaf.Leaf == nil || leaf.Leaf.Name == "" {
			return nil, status.Error(codes.InvalidArgument, "Bonanza environment variable has no name")
		}
		entriesByName[leaf.Leaf.Name] = leaf.Leaf.Value
	}
	if errIter != nil {
		return nil, errIter
	}
	names := make([]string, 0, len(entriesByName))
	for name := range entriesByName {
		names = append(names, name)
	}
	sort.Strings(names)
	entries := make([]*remoteexecution.Command_EnvironmentVariable, 0, len(names))
	for _, name := range names {
		entries = append(entries, &remoteexecution.Command_EnvironmentVariable{
			Name:  name,
			Value: entriesByName[name],
		})
	}
	return entries, nil
}

func normalizeWorkingDirectory(workingDirectory string) (string, []string, error) {
	if workingDirectory == "" || workingDirectory == "/" || workingDirectory == "." {
		return "", nil, nil
	}
	components := strings.Split(strings.TrimPrefix(workingDirectory, "/"), "/")
	if len(components) == 0 {
		return "", nil, nil
	}
	for _, component := range components {
		if _, ok := path.NewComponent(component); !ok {
			return "", nil, status.Errorf(codes.Unimplemented, "REAPI backend does not support Bonanza working directory %q", workingDirectory)
		}
	}
	return strings.Join(components, "/"), components, nil
}

func collectOutputPaths(
	ctx context.Context,
	parsedObjectPoolIngester *model_parser.ParsedObjectPoolIngester[object.LocalReference],
	command model_core.Message[*model_command_pb.Command, object.LocalReference],
	actionEncoder model_encoding.DeterministicBinaryEncoder,
	workingDirectoryComponents []string,
) ([]string, bool, error) {
	if command.Message.OutputPathPattern == nil {
		return nil, false, nil
	}
	pathPatternChildrenReader := model_parser.LookupParsedObjectReader(
		parsedObjectPoolIngester,
		model_parser.NewChainedObjectParser(
			model_parser.NewEncodedObjectParser[object.LocalReference](actionEncoder),
			model_parser.NewProtoObjectParser[object.LocalReference, model_command_pb.PathPattern_Children](),
		),
	)
	var outputPaths []string
	var visit func(model_core.Message[*model_command_pb.PathPattern, object.LocalReference], []string) error
	visit = func(pattern model_core.Message[*model_command_pb.PathPattern, object.LocalReference], prefix []string) error {
		children, err := model_command.PathPatternGetChildren(ctx, pathPatternChildrenReader, pattern)
		if err != nil {
			return err
		}
		if children.Message == nil {
			if len(prefix) < len(workingDirectoryComponents) {
				return status.Errorf(codes.Unimplemented, "REAPI backend cannot capture Bonanza output path %q outside the working directory", strings.Join(prefix, "/"))
			}
			for i, component := range workingDirectoryComponents {
				if prefix[i] != component {
					return status.Errorf(codes.Unimplemented, "REAPI backend cannot capture Bonanza output path %q outside the working directory", strings.Join(prefix, "/"))
				}
			}
			outputPaths = append(outputPaths, strings.Join(prefix[len(workingDirectoryComponents):], "/"))
			return nil
		}
		for _, child := range children.Message.Children {
			if child == nil || child.Pattern == nil {
				return status.Error(codes.InvalidArgument, "Bonanza output pattern contains an incomplete child")
			}
			if _, ok := path.NewComponent(child.Name); !ok {
				return status.Errorf(codes.InvalidArgument, "Bonanza output pattern contains invalid component %q", child.Name)
			}
			if err := visit(model_core.Nested(children, child.Pattern), append(prefix, child.Name)); err != nil {
				return err
			}
		}
		return nil
	}
	if err := visit(model_core.Nested(command, command.Message.OutputPathPattern), nil); err != nil {
		return nil, false, err
	}
	sort.Strings(outputPaths)
	for i := 1; i < len(outputPaths); i++ {
		if outputPaths[i-1] == outputPaths[i] {
			return nil, false, status.Errorf(codes.InvalidArgument, "Bonanza output pattern contains duplicate path %q", outputPaths[i])
		}
	}
	return outputPaths, true, nil
}

func (e *executor) uploadInputDirectory(
	ctx context.Context,
	parsedObjectPoolIngester *model_parser.ParsedObjectPoolIngester[object.LocalReference],
	inputRootReference model_core.Decodable[object.LocalReference],
	directoryCreationParameters *model_filesystem.DirectoryCreationParameters,
	fileCreationParameters *model_filesystem.FileCreationParameters,
) (*remoteexecution.Digest, error) {
	directoryReader := model_parser.LookupParsedObjectReader(
		parsedObjectPoolIngester,
		model_parser.NewChainedObjectParser(
			model_parser.NewEncodedObjectParser[object.LocalReference](directoryCreationParameters.GetEncoder()),
			model_parser.NewProtoObjectParser[object.LocalReference, model_filesystem_pb.DirectoryContents](),
		),
	)
	leavesReader := model_parser.LookupParsedObjectReader(
		parsedObjectPoolIngester,
		model_parser.NewChainedObjectParser(
			model_parser.NewEncodedObjectParser[object.LocalReference](directoryCreationParameters.GetEncoder()),
			model_parser.NewProtoObjectParser[object.LocalReference, model_filesystem_pb.Leaves](),
		),
	)
	fileReader := model_filesystem.NewFileReader(
		model_parser.LookupParsedObjectReader(
			parsedObjectPoolIngester,
			model_parser.NewChainedObjectParser(
				model_parser.NewEncodedObjectParser[object.LocalReference](fileCreationParameters.GetFileContentsListEncoder()),
				model_filesystem.NewFileContentsListObjectParser[object.LocalReference](),
			),
		),
		model_parser.LookupParsedObjectReader(
			parsedObjectPoolIngester,
			model_parser.NewChainedObjectParser(
				model_parser.NewEncodedObjectParser[object.LocalReference](fileCreationParameters.GetChunkEncoder()),
				model_parser.NewRawObjectParser[object.LocalReference](),
			),
		),
		e.objectStoreSemaphore,
	)
	rootDirectory, err := directoryReader.ReadObject(ctx, inputRootReference)
	if err != nil {
		return nil, err
	}
	return e.uploadDirectory(ctx, directoryReader, leavesReader, fileReader, rootDirectory)
}

func (e *executor) uploadDirectory(
	ctx context.Context,
	directoryReader model_parser.MessageObjectReader[object.LocalReference, *model_filesystem_pb.DirectoryContents],
	leavesReader model_parser.MessageObjectReader[object.LocalReference, *model_filesystem_pb.Leaves],
	fileReader *model_filesystem.FileReader[object.LocalReference],
	directory model_core.Message[*model_filesystem_pb.DirectoryContents, object.LocalReference],
) (*remoteexecution.Digest, error) {
	leaves, err := model_filesystem.DirectoryGetLeaves(ctx, leavesReader, directory)
	if err != nil {
		return nil, err
	}
	remoteDirectory := &remoteexecution.Directory{}
	seenNames := map[string]struct{}{}
	addName := func(name string) error {
		if _, ok := path.NewComponent(name); !ok {
			return status.Errorf(codes.InvalidArgument, "Bonanza input directory contains invalid component %q", name)
		}
		if _, ok := seenNames[name]; ok {
			return status.Errorf(codes.InvalidArgument, "Bonanza input directory contains duplicate component %q", name)
		}
		seenNames[name] = struct{}{}
		return nil
	}
	for _, file := range leaves.Message.Files {
		if file == nil || file.Properties == nil {
			return nil, status.Error(codes.InvalidArgument, "Bonanza input directory contains a file without properties")
		}
		if err := addName(file.Name); err != nil {
			return nil, err
		}
		fileContents, err := model_filesystem.NewFileContentsEntryFromProto(model_core.Nested(leaves, file.Properties.Contents))
		if err != nil {
			return nil, fmt.Errorf("decode Bonanza input file %q: %w", file.Name, err)
		}
		fileDigest, err := e.uploadFile(ctx, fileReader, fileContents)
		if err != nil {
			return nil, fmt.Errorf("upload Bonanza input file %q: %w", file.Name, err)
		}
		remoteDirectory.Files = append(remoteDirectory.Files, &remoteexecution.FileNode{
			Name:         file.Name,
			Digest:       fileDigest,
			IsExecutable: file.Properties.IsExecutable,
		})
	}
	for _, symlink := range leaves.Message.Symlinks {
		if symlink == nil {
			return nil, status.Error(codes.InvalidArgument, "Bonanza input directory contains a nil symlink")
		}
		if err := addName(symlink.Name); err != nil {
			return nil, err
		}
		if err := validateSymlinkTarget(symlink.Target); err != nil {
			return nil, fmt.Errorf("invalid Bonanza input symlink %q: %w", symlink.Name, err)
		}
		remoteDirectory.Symlinks = append(remoteDirectory.Symlinks, &remoteexecution.SymlinkNode{
			Name:   symlink.Name,
			Target: symlink.Target,
		})
	}
	for _, child := range directory.Message.Directories {
		if child == nil || child.Directory == nil {
			return nil, status.Error(codes.InvalidArgument, "Bonanza input directory contains an incomplete subdirectory")
		}
		if err := addName(child.Name); err != nil {
			return nil, err
		}
		childContents, err := model_filesystem.DirectoryGetContents(ctx, directoryReader, model_core.Nested(directory, child.Directory))
		if err != nil {
			return nil, fmt.Errorf("read Bonanza input subdirectory %q: %w", child.Name, err)
		}
		childDigest, err := e.uploadDirectory(ctx, directoryReader, leavesReader, fileReader, childContents)
		if err != nil {
			return nil, err
		}
		remoteDirectory.Directories = append(remoteDirectory.Directories, &remoteexecution.DirectoryNode{
			Name:   child.Name,
			Digest: childDigest,
		})
	}
	sort.Slice(remoteDirectory.Files, func(i, j int) bool { return remoteDirectory.Files[i].Name < remoteDirectory.Files[j].Name })
	sort.Slice(remoteDirectory.Symlinks, func(i, j int) bool { return remoteDirectory.Symlinks[i].Name < remoteDirectory.Symlinks[j].Name })
	sort.Slice(remoteDirectory.Directories, func(i, j int) bool { return remoteDirectory.Directories[i].Name < remoteDirectory.Directories[j].Name })
	return e.uploadProto(ctx, remoteDirectory)
}

func (e *executor) uploadFile(ctx context.Context, fileReader *model_filesystem.FileReader[object.LocalReference], fileContents model_filesystem.FileContentsEntry[object.LocalReference]) (*remoteexecution.Digest, error) {
	if fileContents.GetEndBytes() > math.MaxInt64 {
		return nil, status.Error(codes.ResourceExhausted, "Bonanza input file is too large for REAPI")
	}
	hasher := sha256.New()
	readSize, err := io.Copy(hasher, fileReader.FileOpenRead(ctx, fileContents, 0))
	if err != nil {
		return nil, err
	}
	if readSize != int64(fileContents.GetEndBytes()) {
		return nil, status.Errorf(codes.DataLoss, "Bonanza input file has %d bytes, expected %d", readSize, fileContents.GetEndBytes())
	}
	digest := &remoteexecution.Digest{
		Hash:      hex.EncodeToString(hasher.Sum(nil)),
		SizeBytes: readSize,
	}
	if err := e.client.UploadBlob(ctx, digest, fileReader.FileOpenRead(ctx, fileContents, 0)); err != nil {
		return nil, err
	}
	return digest, nil
}

func (e *executor) uploadProto(ctx context.Context, message proto.Message) (*remoteexecution.Digest, error) {
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(message)
	if err != nil {
		return nil, err
	}
	digest := newDigest(encoded)
	if err := e.client.UploadBlob(ctx, digest, bytes.NewReader(encoded)); err != nil {
		return nil, err
	}
	return digest, nil
}

func newDigest(contents []byte) *remoteexecution.Digest {
	hash := sha256.Sum256(contents)
	return &remoteexecution.Digest{
		Hash:      hex.EncodeToString(hash[:]),
		SizeBytes: int64(len(contents)),
	}
}

func (e *executor) importOutputs(
	ctx context.Context,
	actionResult *remoteexecution.ActionResult,
	outputPatternSet bool,
	workingDirectoryComponents []string,
	fileCreationParameters *model_filesystem.FileCreationParameters,
	directoryCreationParameters *model_filesystem.DirectoryCreationParameters,
) (*model_command_pb.Outputs, *model_core.ReferenceMessagePatcher[dag.ObjectContentsWalker], error) {
	outputs := &model_command_pb.Outputs{}
	outputsPatcher := model_core.NewReferenceMessagePatcher[dag.ObjectContentsWalker]()

	stdout, err := e.importOutputBlob(ctx, actionResult.StdoutRaw, actionResult.StdoutDigest, fileCreationParameters)
	if err != nil {
		return nil, nil, fmt.Errorf("import REAPI standard output: %w", err)
	}
	outputs.Stdout = stdout.Merge(outputsPatcher)
	stderr, err := e.importOutputBlob(ctx, actionResult.StderrRaw, actionResult.StderrDigest, fileCreationParameters)
	if err != nil {
		return nil, nil, fmt.Errorf("import REAPI standard error: %w", err)
	}
	outputs.Stderr = stderr.Merge(outputsPatcher)

	if !outputPatternSet {
		return outputs, outputsPatcher, nil
	}
	root := newOutputDirectory()
	for _, outputFile := range actionResult.OutputFiles {
		if outputFile == nil {
			return nil, nil, status.Error(codes.InvalidArgument, "REAPI action result contains a nil output file")
		}
		if outputFile.Digest == nil && outputFile.Contents == nil {
			return nil, nil, status.Errorf(codes.InvalidArgument, "REAPI output file %q has neither a digest nor inline contents", outputFile.Path)
		}
		contents, err := e.importOutputBlob(ctx, outputFile.Contents, outputFile.Digest, fileCreationParameters)
		if err != nil {
			return nil, nil, fmt.Errorf("import REAPI output file %q: %w", outputFile.Path, err)
		}
		outputPath, err := joinOutputPath(workingDirectoryComponents, outputFile.Path)
		if err != nil {
			return nil, nil, err
		}
		if err := root.addFile(outputPath, &outputFileNode{contents: contents, isExecutable: outputFile.IsExecutable}); err != nil {
			return nil, nil, err
		}
	}
	for _, outputDirectory := range actionResult.OutputDirectories {
		if outputDirectory == nil {
			return nil, nil, status.Error(codes.InvalidArgument, "REAPI action result contains a nil output directory")
		}
		if outputDirectory.TreeDigest == nil {
			return nil, nil, status.Errorf(codes.Unimplemented, "REAPI output directory %q is not encoded as a Tree", outputDirectory.Path)
		}
		if err := validateDigest(outputDirectory.TreeDigest); err != nil {
			return nil, nil, fmt.Errorf("invalid REAPI output directory digest for %q: %w", outputDirectory.Path, err)
		}
		directory, err := e.importOutputTree(ctx, outputDirectory.TreeDigest, fileCreationParameters)
		if err != nil {
			return nil, nil, fmt.Errorf("import REAPI output directory %q: %w", outputDirectory.Path, err)
		}
		outputPath, err := joinOutputPath(workingDirectoryComponents, outputDirectory.Path)
		if err != nil {
			return nil, nil, err
		}
		if err := root.addDirectory(outputPath, directory); err != nil {
			return nil, nil, err
		}
	}
	for _, symlinks := range [][]*remoteexecution.OutputSymlink{
		actionResult.OutputSymlinks,
		actionResult.OutputFileSymlinks,
		actionResult.OutputDirectorySymlinks,
	} {
		for _, outputSymlink := range symlinks {
			if outputSymlink == nil {
				return nil, nil, status.Error(codes.InvalidArgument, "REAPI action result contains a nil output symlink")
			}
			if err := validateSymlinkTarget(outputSymlink.Target); err != nil {
				return nil, nil, fmt.Errorf("invalid REAPI output symlink %q: %w", outputSymlink.Path, err)
			}
			outputPath, err := joinOutputPath(workingDirectoryComponents, outputSymlink.Path)
			if err != nil {
				return nil, nil, err
			}
			if err := root.addSymlink(outputPath, outputSymlink.Target); err != nil {
				return nil, nil, err
			}
		}
	}

	group, groupCtx := errgroup.WithContext(ctx)
	var createdRoot model_filesystem.CreatedDirectory[dag.ObjectContentsWalker]
	group.Go(func() error {
		return model_filesystem.CreateDirectoryMerkleTree(
			groupCtx,
			e.objectContentsWalkerSemaphore,
			group,
			directoryCreationParameters,
			root,
			model_filesystem.NewSimpleDirectoryMerkleTreeCapturer(model_core.WalkableCreatedObjectCapturer),
			&createdRoot,
		)
	})
	if err := group.Wait(); err != nil {
		return nil, nil, err
	}
	outputs.OutputRoot = createdRoot.Message.Merge(outputsPatcher)
	return outputs, outputsPatcher, nil
}

func (e *executor) importOutputBlob(
	ctx context.Context,
	inlineContents []byte,
	digest *remoteexecution.Digest,
	fileCreationParameters *model_filesystem.FileCreationParameters,
) (model_core.PatchedMessage[*model_filesystem_pb.FileContents, dag.ObjectContentsWalker], error) {
	if digest != nil {
		reader, err := e.client.OpenBlob(ctx, digest)
		if err != nil {
			return model_core.PatchedMessage[*model_filesystem_pb.FileContents, dag.ObjectContentsWalker]{}, err
		}
		defer reader.Close()
		return model_filesystem.CreateFileMerkleTree(
			ctx,
			fileCreationParameters,
			reader,
			model_filesystem.NewSimpleFileMerkleTreeCapturer(model_core.WalkableCreatedObjectCapturer),
		)
	}
	if inlineContents == nil {
		return model_core.NewSimplePatchedMessage[dag.ObjectContentsWalker]((*model_filesystem_pb.FileContents)(nil)), nil
	}
	return model_filesystem.CreateFileMerkleTree(
		ctx,
		fileCreationParameters,
		bytes.NewReader(inlineContents),
		model_filesystem.NewSimpleFileMerkleTreeCapturer(model_core.WalkableCreatedObjectCapturer),
	)
}

func (e *executor) importOutputTree(
	ctx context.Context,
	treeDigest *remoteexecution.Digest,
	fileCreationParameters *model_filesystem.FileCreationParameters,
) (*outputDirectory, error) {
	encodedTree, err := e.client.ReadBlob(ctx, treeDigest)
	if err != nil {
		return nil, err
	}
	var tree remoteexecution.Tree
	if err := proto.Unmarshal(encodedTree, &tree); err != nil {
		return nil, fmt.Errorf("unmarshal REAPI Tree: %w", err)
	}
	if tree.Root == nil {
		return nil, status.Error(codes.InvalidArgument, "REAPI Tree has no root directory")
	}
	children := make(map[string]*remoteexecution.Directory, len(tree.Children))
	for _, child := range tree.Children {
		if child == nil {
			return nil, status.Error(codes.InvalidArgument, "REAPI Tree has a nil child directory")
		}
		encodedChild, err := proto.MarshalOptions{Deterministic: true}.Marshal(child)
		if err != nil {
			return nil, fmt.Errorf("marshal REAPI Tree child: %w", err)
		}
		childDigest := newDigest(encodedChild)
		key := digestKey(childDigest)
		if _, ok := children[key]; ok {
			return nil, status.Errorf(codes.InvalidArgument, "REAPI Tree has duplicate child directory digest %s", key)
		}
		children[key] = child
	}
	return e.importOutputTreeDirectory(ctx, tree.Root, children, fileCreationParameters, map[string]bool{})
}

func (e *executor) importOutputTreeDirectory(
	ctx context.Context,
	directory *remoteexecution.Directory,
	children map[string]*remoteexecution.Directory,
	fileCreationParameters *model_filesystem.FileCreationParameters,
	active map[string]bool,
) (*outputDirectory, error) {
	output := newOutputDirectory()
	for _, file := range directory.Files {
		if file == nil {
			return nil, status.Error(codes.InvalidArgument, "REAPI output directory has a nil file")
		}
		if file.Digest == nil {
			return nil, status.Errorf(codes.InvalidArgument, "REAPI output tree file %q has no digest", file.Name)
		}
		contents, err := e.importOutputBlob(ctx, nil, file.Digest, fileCreationParameters)
		if err != nil {
			return nil, fmt.Errorf("import REAPI output tree file %q: %w", file.Name, err)
		}
		if err := output.addFile([]string{file.Name}, &outputFileNode{contents: contents, isExecutable: file.IsExecutable}); err != nil {
			return nil, err
		}
	}
	for _, symlink := range directory.Symlinks {
		if symlink == nil {
			return nil, status.Error(codes.InvalidArgument, "REAPI output directory has a nil symlink")
		}
		if err := validateSymlinkTarget(symlink.Target); err != nil {
			return nil, fmt.Errorf("invalid REAPI output tree symlink %q: %w", symlink.Name, err)
		}
		if err := output.addSymlink([]string{symlink.Name}, symlink.Target); err != nil {
			return nil, err
		}
	}
	for _, child := range directory.Directories {
		if child == nil || child.Digest == nil {
			return nil, status.Error(codes.InvalidArgument, "REAPI output directory has an incomplete child directory")
		}
		if err := validateDigest(child.Digest); err != nil {
			return nil, fmt.Errorf("invalid REAPI output tree directory digest for %q: %w", child.Name, err)
		}
		key := digestKey(child.Digest)
		if active[key] {
			return nil, status.Errorf(codes.InvalidArgument, "REAPI output Tree contains a directory cycle at %q", child.Name)
		}
		childDirectory, ok := children[key]
		if !ok {
			return nil, status.Errorf(codes.InvalidArgument, "REAPI output Tree is missing child directory %q", child.Name)
		}
		active[key] = true
		childOutput, err := e.importOutputTreeDirectory(ctx, childDirectory, children, fileCreationParameters, active)
		delete(active, key)
		if err != nil {
			return nil, err
		}
		if err := output.addDirectory([]string{child.Name}, childOutput); err != nil {
			return nil, err
		}
	}
	return output, nil
}

func joinOutputPath(workingDirectoryComponents []string, outputPath string) ([]string, error) {
	if outputPath == "" {
		return append([]string(nil), workingDirectoryComponents...), nil
	}
	if strings.HasPrefix(outputPath, "/") || strings.HasSuffix(outputPath, "/") {
		return nil, status.Errorf(codes.InvalidArgument, "REAPI output path %q is not a relative path", outputPath)
	}
	components := append([]string(nil), workingDirectoryComponents...)
	for _, component := range strings.Split(outputPath, "/") {
		if _, ok := path.NewComponent(component); !ok {
			return nil, status.Errorf(codes.InvalidArgument, "REAPI output path %q contains invalid component %q", outputPath, component)
		}
		components = append(components, component)
	}
	return components, nil
}

func validateSymlinkTarget(target string) error {
	if target == "" {
		return status.Error(codes.InvalidArgument, "symlink target is empty")
	}
	if strings.HasPrefix(target, "/") {
		return status.Errorf(codes.Unimplemented, "absolute symlink target %q is not supported by the REAPI backend", target)
	}
	if strings.Contains(target, "\\") {
		return status.Errorf(codes.Unimplemented, "symlink target %q uses a non-UNIX separator", target)
	}
	return nil
}

func digestKey(digest *remoteexecution.Digest) string {
	return digest.GetHash() + "/" + fmt.Sprintf("%d", digest.GetSizeBytes())
}

type outputFileNode struct {
	contents     model_core.PatchedMessage[*model_filesystem_pb.FileContents, dag.ObjectContentsWalker]
	isExecutable bool
}

type outputEntry struct {
	file      *outputFileNode
	directory *outputDirectory
	symlink   *string
}

type outputDirectory struct {
	entries map[string]*outputEntry
}

func newOutputDirectory() *outputDirectory {
	return &outputDirectory{entries: map[string]*outputEntry{}}
}

func (d *outputDirectory) entry(name string) (*outputEntry, error) {
	if _, ok := path.NewComponent(name); !ok {
		return nil, status.Errorf(codes.InvalidArgument, "REAPI output contains invalid component %q", name)
	}
	entry, ok := d.entries[name]
	if !ok {
		entry = &outputEntry{}
		d.entries[name] = entry
	}
	return entry, nil
}

func (d *outputDirectory) addFile(components []string, file *outputFileNode) error {
	parent, name, err := d.parentFor(components)
	if err != nil {
		return err
	}
	entry, err := parent.entry(name)
	if err != nil {
		return err
	}
	if entry.file != nil || entry.directory != nil || entry.symlink != nil {
		return status.Errorf(codes.InvalidArgument, "REAPI output path %q conflicts with another output", strings.Join(components, "/"))
	}
	entry.file = file
	return nil
}

func (d *outputDirectory) addSymlink(components []string, target string) error {
	parent, name, err := d.parentFor(components)
	if err != nil {
		return err
	}
	entry, err := parent.entry(name)
	if err != nil {
		return err
	}
	if entry.file != nil || entry.directory != nil || entry.symlink != nil {
		return status.Errorf(codes.InvalidArgument, "REAPI output path %q conflicts with another output", strings.Join(components, "/"))
	}
	entry.symlink = &target
	return nil
}

func (d *outputDirectory) addDirectory(components []string, directory *outputDirectory) error {
	if len(components) == 0 {
		return d.mergeDirectory(directory)
	}
	parent, name, err := d.parentFor(components)
	if err != nil {
		return err
	}
	entry, err := parent.entry(name)
	if err != nil {
		return err
	}
	if entry.file != nil || entry.symlink != nil {
		return status.Errorf(codes.InvalidArgument, "REAPI output path %q conflicts with another output", strings.Join(components, "/"))
	}
	if entry.directory != nil {
		return entry.directory.mergeDirectory(directory)
	}
	entry.directory = directory
	return nil
}

func (d *outputDirectory) mergeDirectory(other *outputDirectory) error {
	for name, otherEntry := range other.entries {
		entry, err := d.entry(name)
		if err != nil {
			return err
		}
		switch {
		case otherEntry.directory != nil:
			if entry.file != nil || entry.symlink != nil {
				return status.Errorf(codes.InvalidArgument, "REAPI output directory entry %q conflicts with another output", name)
			}
			if entry.directory == nil {
				entry.directory = otherEntry.directory
			} else if err := entry.directory.mergeDirectory(otherEntry.directory); err != nil {
				return err
			}
		case otherEntry.file != nil:
			if entry.file != nil || entry.directory != nil || entry.symlink != nil {
				return status.Errorf(codes.InvalidArgument, "REAPI output directory entry %q conflicts with another output", name)
			}
			entry.file = otherEntry.file
		case otherEntry.symlink != nil:
			if entry.file != nil || entry.directory != nil || entry.symlink != nil {
				return status.Errorf(codes.InvalidArgument, "REAPI output directory entry %q conflicts with another output", name)
			}
			entry.symlink = otherEntry.symlink
		default:
			return status.Errorf(codes.InvalidArgument, "REAPI output directory entry %q has no type", name)
		}
	}
	return nil
}

func (d *outputDirectory) parentFor(components []string) (*outputDirectory, string, error) {
	if len(components) == 0 {
		return nil, "", status.Error(codes.InvalidArgument, "REAPI output path is empty where a file or symlink is required")
	}
	parent := d
	for _, component := range components[:len(components)-1] {
		entry, err := parent.entry(component)
		if err != nil {
			return nil, "", err
		}
		if entry.file != nil || entry.symlink != nil {
			return nil, "", status.Errorf(codes.InvalidArgument, "REAPI output path %q has a non-directory ancestor", strings.Join(components, "/"))
		}
		if entry.directory == nil {
			entry.directory = newOutputDirectory()
		}
		parent = entry.directory
	}
	return parent, components[len(components)-1], nil
}

func (d *outputDirectory) Close() error {
	return nil
}

func (d *outputDirectory) ReadDir() ([]bb_filesystem.FileInfo, error) {
	names := make([]string, 0, len(d.entries))
	for name := range d.entries {
		names = append(names, name)
	}
	sort.Strings(names)
	entries := make([]bb_filesystem.FileInfo, 0, len(names))
	for _, name := range names {
		entry := d.entries[name]
		component := path.MustNewComponent(name)
		switch {
		case entry.file != nil:
			entries = append(entries, bb_filesystem.NewFileInfo(component, bb_filesystem.FileTypeRegularFile, entry.file.isExecutable))
		case entry.directory != nil:
			entries = append(entries, bb_filesystem.NewFileInfo(component, bb_filesystem.FileTypeDirectory, false))
		case entry.symlink != nil:
			entries = append(entries, bb_filesystem.NewFileInfo(component, bb_filesystem.FileTypeSymlink, false))
		default:
			return nil, status.Errorf(codes.InvalidArgument, "REAPI output directory entry %q has no type", name)
		}
	}
	return entries, nil
}

func (d *outputDirectory) Readlink(name path.Component) (path.Parser, error) {
	entry, ok := d.entries[name.String()]
	if !ok || entry.symlink == nil {
		return nil, status.Errorf(codes.InvalidArgument, "REAPI output %q is not a symbolic link", name.String())
	}
	return path.UNIXFormat.NewParser(*entry.symlink), nil
}

func (d *outputDirectory) EnterCapturableDirectory(name path.Component) (*model_filesystem.CreatedDirectory[dag.ObjectContentsWalker], model_filesystem.CapturableDirectory[dag.ObjectContentsWalker, dag.ObjectContentsWalker], error) {
	entry, ok := d.entries[name.String()]
	if !ok || entry.directory == nil {
		return nil, nil, status.Errorf(codes.InvalidArgument, "REAPI output %q is not a directory", name.String())
	}
	return nil, entry.directory, nil
}

func (d *outputDirectory) OpenForFileMerkleTreeCreation(name path.Component) (model_filesystem.CapturableFile[dag.ObjectContentsWalker], error) {
	entry, ok := d.entries[name.String()]
	if !ok || entry.file == nil {
		return nil, status.Errorf(codes.InvalidArgument, "REAPI output %q is not a regular file", name.String())
	}
	return model_filesystem.NewSimpleCapturableFile(entry.file.contents), nil
}

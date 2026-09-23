package query

import (
	"bytes"
	"context"
	"encoding"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"net/url"
	"os"
	"runtime"
	"slices"
	"strings"

	"bonanza.build/pkg/bazelclient/arguments"
	"bonanza.build/pkg/bazelclient/commands"
	commands_build "bonanza.build/pkg/bazelclient/commands/build"
	"bonanza.build/pkg/bazelclient/formatted"
	"bonanza.build/pkg/bazelclient/logging"
	"bonanza.build/pkg/crypto"
	"bonanza.build/pkg/label"
	model_core "bonanza.build/pkg/model/core"
	"bonanza.build/pkg/model/core/btree"
	model_encoding "bonanza.build/pkg/model/encoding"
	model_evaluation "bonanza.build/pkg/model/evaluation"
	model_executewithstorage "bonanza.build/pkg/model/executewithstorage"
	model_filesystem "bonanza.build/pkg/model/filesystem"
	model_parser "bonanza.build/pkg/model/parser"
	encryptedaction_pb "bonanza.build/pkg/proto/encryptedaction"
	model_analysis_pb "bonanza.build/pkg/proto/model/analysis"
	model_core_pb "bonanza.build/pkg/proto/model/core"
	model_encoding_pb "bonanza.build/pkg/proto/model/encoding"
	model_evaluation_pb "bonanza.build/pkg/proto/model/evaluation"
	model_executewithstorage_pb "bonanza.build/pkg/proto/model/executewithstorage"
	model_filesystem_pb "bonanza.build/pkg/proto/model/filesystem"
	remoteexecution_pb "bonanza.build/pkg/proto/remoteexecution"
	dag_pb "bonanza.build/pkg/proto/storage/dag"
	object_pb "bonanza.build/pkg/proto/storage/object"
	"bonanza.build/pkg/remoteexecution"
	pg_starlark "bonanza.build/pkg/starlark"
	"bonanza.build/pkg/storage/dag"
	dag_grpc "bonanza.build/pkg/storage/dag/grpc"
	"bonanza.build/pkg/storage/object"
	object_grpc "bonanza.build/pkg/storage/object/grpc"
	object_namespacemapping "bonanza.build/pkg/storage/object/namespacemapping"

	"github.com/buildbarn/bb-storage/pkg/eviction"
	"github.com/buildbarn/bb-storage/pkg/filesystem"
	"github.com/buildbarn/bb-storage/pkg/filesystem/path"

	"github.com/buildbarn/bb-storage/pkg/util"
	"google.golang.org/protobuf/proto"

	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/semaphore"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/security/advancedtls"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/emptypb"
)

// newGRPCClient creates a gRPC client for one of the endpoints provided
// on the command line (e.g., --remote_cache or --remote_executor).
//
// This is a copy of the identically named function in the "build"
// command. It is small enough that duplicating it here is preferable
// to exporting it from a package whose primary purpose is implementing
// "bazel build".
func newGRPCClient(endpoint string, commonFlags *arguments.CommonFlags) (*grpc.ClientConn, error) {
	endpointURL, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %w", err)
	}

	var target string
	var clientCredentials credentials.TransportCredentials
	switch scheme := endpointURL.Scheme; scheme {
	case "grpc":
		target = endpointURL.Host
		clientCredentials = insecure.NewCredentials()
	case "grpcs":
		target = endpointURL.Host
		clientCredentials, err = advancedtls.NewClientCreds(&advancedtls.Options{})
		if err != nil {
			return nil, fmt.Errorf("failed to create TLS client credentials: %w", err)
		}
	case "unix":
		target = endpoint
		clientCredentials = insecure.NewCredentials()
	default:
		return nil, errors.New("scheme is not supported")
	}

	return grpc.NewClient(target, grpc.WithTransportCredentials(clientCredentials))
}

type localCapturableDirectoryOptions[TFile model_core.ReferenceMetadata] struct {
	fileParameters *model_filesystem.FileCreationParameters
	capturer       model_filesystem.FileMerkleTreeCapturer[TFile]
}

type localCapturableDirectory[TDirectory, TFile model_core.ReferenceMetadata] struct {
	filesystem.DirectoryCloser
	options *localCapturableDirectoryOptions[TFile]
}

func (d *localCapturableDirectory[TDirectory, TFile]) EnterCapturableDirectory(name path.Component) (*model_filesystem.CreatedDirectory[TDirectory], model_filesystem.CapturableDirectory[TDirectory, TFile], error) {
	child, err := d.DirectoryCloser.EnterDirectory(name)
	if err != nil {
		return nil, nil, err
	}
	return nil, &localCapturableDirectory[TDirectory, TFile]{
		DirectoryCloser: child,
		options:         d.options,
	}, nil
}

func (d *localCapturableDirectory[TDirectory, TFile]) OpenForFileMerkleTreeCreation(name path.Component) (model_filesystem.CapturableFile[TFile], error) {
	f, err := d.OpenRead(name)
	if err != nil {
		return nil, err
	}
	return &localCapturableFile[TFile]{
		file:    f,
		options: d.options,
	}, nil
}

type localCapturedDirectory struct {
	filesystem.DirectoryCloser
}

func (d localCapturedDirectory) EnterCapturedDirectory(name path.Component) (model_filesystem.CapturedDirectory, error) {
	child, err := d.DirectoryCloser.EnterDirectory(name)
	if err != nil {
		return nil, err
	}
	return localCapturedDirectory{
		DirectoryCloser: child,
	}, nil
}

type localCapturableFile[TFile model_core.ReferenceMetadata] struct {
	file    filesystem.FileReader
	options *localCapturableDirectoryOptions[TFile]
}

func (f *localCapturableFile[TFile]) CreateFileMerkleTree(ctx context.Context) (model_core.PatchedMessage[*model_filesystem_pb.FileContents, TFile], error) {
	defer f.Discard()
	return model_filesystem.CreateFileMerkleTree(
		ctx,
		f.options.fileParameters,
		io.NewSectionReader(f.file, 0, math.MaxInt64),
		f.options.capturer,
	)
}

func (f *localCapturableFile[TFile]) Discard() {
	f.file.Close()
	f.file = nil
}

// DoQuery implements the "bazel query" command, which prints the
// labels of targets matched by one or more target patterns.
//
// Unlike "bazel build", "bazel query" only requires the loading phase
// (package parsing and target pattern expansion) to run, so this
// command does not perform any configuration or analysis. It works by
// submitting one TargetPatternExpansion_Key per target pattern
// provided on the command line, and printing the labels contained in
// the resulting values.
// runQuery uploads the workspace, asks the cluster to evaluate one key,
// and returns its value.
//
// query and cquery differ only in the key they request and how they
// print the result, so everything between those two points is shared.
func runQuery(logger logging.Logger, commandName string, commonFlags *arguments.CommonFlags, workspacePath path.Parser, queryKey proto.Message) proto.Message {
	commands.ValidateInsideWorkspace(logger, commandName, workspacePath)

	remoteCacheClient, err := newGRPCClient(commonFlags.RemoteCache, commonFlags)
	if err != nil {
		logger.Fatal(formatted.Textf("Failed to create gRPC client for --remote_cache=%#v: %s", commonFlags.RemoteCache, err))
	}

	// Determine the names and paths of all modules that are present
	// on the local system and need to be uploaded as part of the
	// query. First look for local_path_override() directives in
	// MODULE.bazel.
	workspaceDirectory, err := filesystem.NewLocalDirectory(workspacePath)
	if err != nil {
		logger.Fatal(formatted.Textf("Failed to open workspace directory: %s", err))
	}
	moduleDotBazelFile, err := workspaceDirectory.OpenRead(path.MustNewComponent("MODULE.bazel"))
	workspaceDirectory.Close()
	if err != nil {
		logger.Fatal(formatted.Textf("Failed to open MODULE.bazel: %s", err))
	}
	moduleDotBazelContents, err := io.ReadAll(io.NewSectionReader(moduleDotBazelFile, 0, math.MaxInt64))
	moduleDotBazelFile.Close()
	if err != nil {
		logger.Fatal(formatted.Textf("Failed to read MODULE.bazel: %s", err))
	}
	modulePaths := map[label.Module]path.Parser{}
	moduleDotBazelHandler := commands_build.NewLocalPathExtractingModuleDotBazelHandler(modulePaths, workspacePath)
	if err := pg_starlark.ParseModuleDotBazel(
		string(moduleDotBazelContents),
		util.Must(label.NewCanonicalLabel("@@main+//:MODULE.bazel")),
		path.LocalFormat,
		moduleDotBazelHandler,
	); err != nil {
		logger.Fatal(formatted.Textf("Failed to parse MODULE.bazel: %s", err))
	}
	rootModuleName, err := moduleDotBazelHandler.GetRootModuleName()
	if err != nil {
		logger.Fatal(formatted.Text(err.Error()))
	}

	// Augment results with modules provided to --override_module.
	for _, overrideModule := range commonFlags.OverrideModule {
		fields := strings.SplitN(overrideModule, "=", 2)
		if len(fields) != 2 {
			logger.Fatal(formatted.Text("Module overrides must use the format ${module_name}=${path}"))
		}
		moduleName, err := label.NewModule(fields[0])
		if err != nil {
			logger.Fatal(formatted.Textf("Invalid module name %#v: %s", fields[0], err))
		}
		modulePaths[moduleName] = path.LocalFormat.NewParser(fields[1])
	}

	moduleNames := slices.Collect(maps.Keys(modulePaths))
	slices.SortFunc(moduleNames, func(a, b label.Module) int {
		return strings.Compare(a.String(), b.String())
	})

	// Determine parameters for creating file and directory Merkle
	// trees. Parameters include minimum/maximum sizes of the
	// resulting objects, and whether they are compressed and
	// encrypted.
	referenceFormat := util.Must(object.NewReferenceFormat(object_pb.ReferenceFormat_SHA256_V1))
	encryptionKeyBytes, err := base64.StdEncoding.DecodeString(commonFlags.RemoteEncryptionKey)
	if err != nil {
		logger.Fatal(formatted.Textf("Failed to base64 decode value of --remote_encryption_key: %s", err))
	}
	defaultEncoders := []*model_encoding_pb.BinaryEncoder{{
		Encoder: &model_encoding_pb.BinaryEncoder_Encrypting{
			Encrypting: &model_encoding_pb.EncryptingBinaryEncoder{
				EncryptionKey: encryptionKeyBytes,
			},
		},
	}}
	var chunkEncoders []*model_encoding_pb.BinaryEncoder
	if commonFlags.RemoteCacheCompression {
		chunkEncoders = append(chunkEncoders, &model_encoding_pb.BinaryEncoder{
			Encoder: &model_encoding_pb.BinaryEncoder_LzwCompressing{
				LzwCompressing: &emptypb.Empty{},
			},
		})
	}
	chunkEncoders = append(chunkEncoders, defaultEncoders...)

	directoryParametersMessage := &model_filesystem_pb.DirectoryCreationParameters{
		Access: &model_filesystem_pb.DirectoryAccessParameters{
			Encoders: defaultEncoders,
		},
		DirectoryMaximumSizeBytes: 16 * 1024,
	}
	directoryParameters, err := model_filesystem.NewDirectoryCreationParametersFromProto(directoryParametersMessage, referenceFormat)
	if err != nil {
		logger.Fatal(formatted.Textf("Invalid directory creation parameters: %s", err))
	}
	fileParametersMessage := &model_filesystem_pb.FileCreationParameters{
		Access: &model_filesystem_pb.FileAccessParameters{
			ChunkEncoders:            chunkEncoders,
			FileContentsListEncoders: defaultEncoders,
		},
		ChunkMinimumSizeBytes:            64 * 1024,
		ChunkHorizonSizeBytes:            512 * 1024,
		ChunkGearTableSeed:               encryptionKeyBytes,
		FileContentsListMinimumSizeBytes: 4 * 1024,
		FileContentsListMaximumSizeBytes: 16 * 1024,
	}
	fileParameters, err := model_filesystem.NewFileCreationParametersFromProto(fileParametersMessage, referenceFormat)
	if err != nil {
		logger.Fatal(formatted.Textf("Invalid file creation parameters: %s", err))
	}

	// Construct Merkle trees for all modules that need to be
	// uploaded to storage.
	logger.Info(formatted.Text("Scanning module sources"))
	ctx := context.Background()
	group, groupCtx := errgroup.WithContext(ctx)
	moduleRootDirectories := make([]model_filesystem.CapturedDirectory, 0, len(moduleNames))
	createdModuleRootDirectories := make([]model_filesystem.CreatedDirectory[model_core.CreatedObjectTree], len(moduleNames))
	createMerkleTreesConcurrency := semaphore.NewWeighted(int64(runtime.NumCPU()))
	group.Go(func() error {
		for i, moduleName := range moduleNames {
			modulePath := modulePaths[moduleName]
			moduleRootDirectory, err := filesystem.NewLocalDirectory(modulePath)
			if err != nil {
				return util.StatusWrapf(err, "Failed to open root directory of module %#v", moduleName.String())
			}
			moduleRootDirectories = append(moduleRootDirectories, localCapturedDirectory{
				DirectoryCloser: moduleRootDirectory,
			})
			if err := model_filesystem.CreateDirectoryMerkleTree(
				groupCtx,
				createMerkleTreesConcurrency,
				group,
				directoryParameters,
				&localCapturableDirectory[model_core.CreatedObjectTree, model_core.NoopReferenceMetadata]{
					DirectoryCloser: moduleRootDirectory,
					options: &localCapturableDirectoryOptions[model_core.NoopReferenceMetadata]{
						fileParameters: fileParameters,
						capturer:       model_filesystem.NewSimpleFileMerkleTreeCapturer(model_core.DiscardingCreatedObjectCapturer),
					},
				},
				model_filesystem.FileDiscardingDirectoryMerkleTreeCapturer,
				&createdModuleRootDirectories[i],
			); err != nil {
				return util.StatusWrapf(err, "Failed to create directory Merkle tree for module %#v", moduleName.String())
			}
		}
		return nil
	})
	if err := group.Wait(); err != nil {
		logger.Fatal(formatted.Text(err.Error()))
	}

	fetcherPKIXPublicKey, err := base64.StdEncoding.DecodeString(commonFlags.RemoteExecutorFetcherPkixPublicKey)
	if err != nil {
		logger.Fatal(formatted.Textf("Failed to base64 decode --remote_executor_fetcher_pkix_public_key: %s", err))
	}

	// Resolve every target pattern provided on the command line to
	// a canonical target pattern. Patterns that are relative to the
	// current package, or that are absolute within the root
	// repository (e.g., "//foo:bar" or "//foo/...") can be resolved
	// locally, as they are implicitly rooted at the root module's
	// canonical repo.
	//
	// Patterns prefixed with an apparent repo name (e.g.,
	// "@some_dep//foo:bar") cannot be resolved without consulting
	// the module resolution graph (Bzlmod repo mappings), which is
	// only available as part of evaluation. Supporting these is left
	// as future work.
	//
	// A target pattern that already refers to a single canonical
	// label (e.g., "//foo:bar", as opposed to a wildcard pattern
	// like "//foo:all" or "//foo/...") does not need to be expanded
	// at all: it matches itself. TargetPatternExpansion_Key does not
	// accept such patterns (its computer only handles the
	// single-package and recursive wildcard shapes), so these are
	// collected separately and reported directly.
	// Construct a BuildSpecification message that lists all the
	// modules and contains all of the flags needed to resolve
	// packages.
	buildSpecification := model_analysis_pb.BuildSpecification_Value{
		RootModuleName:                         rootModuleName.String(),
		DirectoryCreationParameters:            directoryParametersMessage,
		FileCreationParameters:                 fileParametersMessage,
		IgnoreRootModuleDevDependencies:        commonFlags.IgnoreDevDependency,
		BuiltinsModuleNames:                    commonFlags.BuiltinsModule,
		RepoPlatform:                           commonFlags.RepoPlatform,
		FetchPlatformPkixPublicKey:             fetcherPKIXPublicKey,
		ActionEncoders:                         defaultEncoders,
		RuleImplementationWrapperIdentifier:    commonFlags.RuleImplementationWrapperIdentifier,
		SubruleImplementationWrapperIdentifier: commonFlags.SubruleImplementationWrapperIdentifier,
	}
	switch commonFlags.LockfileMode {
	case arguments.LockfileMode_Off:
	case arguments.LockfileMode_Update:
		buildSpecification.UseLockfile = &model_analysis_pb.BuildSpecification_Value_UseLockfile{}
	case arguments.LockfileMode_Refresh:
		buildSpecification.UseLockfile = &model_analysis_pb.BuildSpecification_Value_UseLockfile{
			Error: true,
		}
	case arguments.LockfileMode_Error:
		buildSpecification.UseLockfile = &model_analysis_pb.BuildSpecification_Value_UseLockfile{
			MaximumCacheDuration: &durationpb.Duration{Seconds: 3600},
		}
	default:
		panic("unknown lockfile mode")
	}
	if len(commonFlags.Registry) > 0 {
		buildSpecification.ModuleRegistryUrls = commonFlags.Registry
	} else {
		buildSpecification.ModuleRegistryUrls = []string{"https://bcr.bazel.build/"}
	}
	buildSpecificationPatcher := model_core.NewReferenceMessagePatcher[dag.ObjectContentsWalker]()

	for i, moduleName := range moduleNames {
		createdRootDirectory := createdModuleRootDirectories[i]
		if l := createdRootDirectory.MaximumSymlinkEscapementLevels; l == nil || l.Value != 0 {
			logger.Fatal(formatted.Textf("Module %#v contains one or more symbolic links that potentially escape the module's root directory", moduleName.String()))
		}
		createdObject, err := model_core.MarshalAndEncodeDeterministic(
			model_core.ProtoToBinaryMarshaler(createdModuleRootDirectories[i].Message),
			referenceFormat,
			directoryParameters.GetEncoder(),
		)
		if err != nil {
			logger.Fatal(formatted.Textf("Failed to create root directory object for module %#v: %s", moduleName.String(), err))
		}

		createdObjectTree := model_core.CreatedObjectTree(createdObject.Value)
		decodingParameters := createdObject.GetDecodingParameters()
		buildSpecification.Modules = append(
			buildSpecification.Modules,
			&model_analysis_pb.BuildSpecification_Value_Module{
				Name: moduleName.String(),
				RootDirectoryReference: createdRootDirectory.ToDirectoryReference(
					&model_core_pb.DecodableReference{
						Reference: buildSpecificationPatcher.AddReference(
							model_core.MetadataEntry[dag.ObjectContentsWalker]{
								LocalReference: createdObject.Value.GetLocalReference(),
								Metadata: model_filesystem.NewCapturedDirectoryWalker(
									directoryParameters.DirectoryAccessParameters,
									fileParameters,
									moduleRootDirectories[i],
									&createdObjectTree,
									decodingParameters,
								),
							},
						),
						DecodingParameters: decodingParameters,
					},
				),
			},
		)
	}

	actionEncoder, err := model_encoding.NewDeterministicBinaryEncoderFromProto(
		defaultEncoders,
		uint32(referenceFormat.GetMaximumObjectSizeBytes()),
	)
	if err != nil {
		logger.Fatal(formatted.Textf("Failed to create action encoder: %s", err))
	}

	overrides, err := model_core.BuildPatchedMessage(func(patcher *model_core.ReferenceMessagePatcher[dag.ObjectContentsWalker]) (encoding.BinaryMarshaler, error) {
		buildSpecificationKey, err := model_core.MarshalTopLevelAny(
			model_core.NewSimpleTopLevelMessage[object.LocalReference](
				&model_analysis_pb.BuildSpecification_Key{},
			),
		)
		if err != nil {
			return nil, err
		}
		buildSpecificationKeyReference, err := model_core.ComputeTopLevelMessageReference(buildSpecificationKey, referenceFormat)
		if err != nil {
			return nil, err
		}

		buildSpecificationValue, err := model_core.MarshalAny(
			model_core.NewPatchedMessage(&buildSpecification, buildSpecificationPatcher),
		)
		if err != nil {
			return nil, err
		}

		return model_core.NewProtoListBinaryMarshaler([]*model_evaluation_pb.Evaluations{{
			Level: &model_evaluation_pb.Evaluations_Leaf_{
				Leaf: &model_evaluation_pb.Evaluations_Leaf{
					KeyReference: buildSpecificationKeyReference.GetRawReference(),
					Graphlet: &model_evaluation_pb.Graphlet{
						Evaluation: &model_evaluation_pb.Graphlet_EvaluationInline{
							EvaluationInline: &model_evaluation_pb.Evaluation{
								Value: buildSpecificationValue.Merge(patcher),
							},
						},
					},
				},
			},
		}}), nil
	})
	if err != nil {
		logger.Fatal(formatted.Textf("Failed to create overrides list message: %s", err))
	}
	createdOverrides, err := model_core.MarshalAndEncodeDeterministic(overrides, referenceFormat, actionEncoder)
	if err != nil {
		logger.Fatal(formatted.Textf("Failed to create overrides list object: %s", err))
	}

	// One QueryResult key covers the whole expression: the walk it
	// describes happens in the cluster, so the client requests a single
	// value rather than one per target pattern.
	queryResultKey := model_core.NewSimplePatchedMessage[dag.ObjectContentsWalker](queryKey)
	queryResultKeyTopLevel, _ := queryResultKey.SortAndSetReferences()
	queryResultKeyAny, err := model_core.MarshalTopLevelAny(queryResultKeyTopLevel)
	if err != nil {
		logger.Fatal(formatted.Textf("Failed to marshal query result key: %s", err))
	}
	queryResultKeyReference, err := model_core.ComputeTopLevelMessageReference(queryResultKeyAny, referenceFormat)
	if err != nil {
		logger.Fatal(formatted.Textf("Failed to compute query result key reference: %s", err))
	}

	actionMessage, err := model_core.BuildPatchedMessage(func(patcher *model_core.ReferenceMessagePatcher[dag.ObjectContentsWalker]) (encoding.BinaryMarshaler, error) {
		overridesReference, err := patcher.CaptureAndAddDecodableReference(
			ctx,
			createdOverrides,
			model_core.WalkableCreatedObjectCapturer,
		)
		if err != nil {
			return nil, err
		}
		patchedKeyAny, err := model_core.MarshalAny(
			model_core.NewSimplePatchedMessage[dag.ObjectContentsWalker](queryKey),
		)
		if err != nil {
			return nil, err
		}
		return model_core.NewProtoBinaryMarshaler(&model_evaluation_pb.Action{
			OverridesReference: overridesReference,
			RequestedKeys: []*model_evaluation_pb.Keys{{
				Level: &model_evaluation_pb.Keys_Leaf{
					Leaf: patchedKeyAny.Merge(patcher),
				},
			}},
		}), nil
	})
	if err != nil {
		logger.Fatal(formatted.Textf("Failed to create action message: %s", err))
	}
	createdAction, err := model_core.MarshalAndEncodeDeterministic(actionMessage, referenceFormat, actionEncoder)
	if err != nil {
		logger.Fatal(formatted.Textf("Failed to create action object: %s", err))
	}

	logger.Info(formatted.Text("Uploading module sources"))
	instanceName, err := object.NewInstanceName(commonFlags.RemoteInstanceName)
	if err != nil {
		logger.Fatal(formatted.Textf("Invalid --remote_instance_name=%#v: %s", commonFlags.RemoteInstanceName, err))
	}
	actionReference := createdAction.Value.GetLocalReference()
	actionGlobalReference := instanceName.WithLocalReference(actionReference)
	dagUploader := dag_grpc.NewUploader(
		dag_pb.NewUploaderClient(remoteCacheClient),
		semaphore.NewWeighted(10),
		// The effective limit is the minimum of this value and the
		// server's (see dag.uploader_server), so a value too small here
		// cannot be raised by reconfiguring the server. These bounds
		// need to accommodate the whole workspace of the largest repo
		// being built.
		object.NewLimit(&object_pb.Limit{
			Count:     1000000,
			SizeBytes: 1 << 30,
		}),
	)
	if err := dagUploader.UploadDAG(
		ctx,
		actionGlobalReference,
		dag.NewSimpleObjectContentsWalker(
			createdAction.Value.Contents,
			createdAction.Value.Metadata,
		),
	); err != nil {
		logger.Fatal(formatted.Textf("Failed to upload workspace directory: %s", err))
	}

	clientPrivateKeyData, err := os.ReadFile(commonFlags.RemoteExecutorClientPrivateKey)
	if err != nil {
		logger.Fatal(formatted.Textf("Failed to read --remote_executor_client_private_key=%#v: %s", commonFlags.RemoteExecutorClientPrivateKey, err))
	}
	clientPrivateKey, err := crypto.ParsePEMWithPKCS8ECDHPrivateKey(clientPrivateKeyData)
	if err != nil {
		logger.Fatal(formatted.Textf("Failed to parse --remote_executor_client_private_key=%#v: %s", commonFlags.RemoteExecutorClientPrivateKey, err))
	}

	clientCertificateChainData, err := os.ReadFile(commonFlags.RemoteExecutorClientCertificateChain)
	if err != nil {
		logger.Fatal(formatted.Textf("Failed to read --remote_executor_client_certificate_chain=%#v: %s", commonFlags.RemoteExecutorClientCertificateChain, err))
	}
	clientCertificateChain, err := crypto.ParsePEMWithCertificateChain(clientCertificateChainData)
	if err != nil {
		logger.Fatal(formatted.Textf("Failed to parse --remote_executor_client_certificate_chain=%#v: %s", commonFlags.RemoteExecutorClientCertificateChain, err))
	}

	remoteExecutorClient, err := newGRPCClient(commonFlags.RemoteExecutor, commonFlags)
	if err != nil {
		logger.Fatal(formatted.Textf("Failed to create gRPC client for --remote_executor=%#v: %s", commonFlags.RemoteExecutor, err))
	}
	builderClient := model_executewithstorage.NewNamespaceAddingClient(
		model_executewithstorage.NewProtoClient(
			remoteexecution.NewProtoClient[*model_executewithstorage_pb.Action, model_core_pb.WeakDecodableReference, model_core_pb.WeakDecodableReference](
				remoteexecution.NewRemoteClient(
					remoteexecution_pb.NewExecutionClient(remoteExecutorClient),
					clientPrivateKey,
					clientCertificateChain,
				),
			),
		),
		instanceName,
	)

	builderPKIXPublicKey, err := base64.StdEncoding.DecodeString(commonFlags.RemoteExecutorBuilderPkixPublicKey)
	if err != nil {
		logger.Fatal(formatted.Textf("Failed to base64 decode --remote_executor_builder_pkix_public_key: %s", err))
	}
	builderECDHPublicKey, err := crypto.ParsePKIXECDHPublicKey(builderPKIXPublicKey)
	if err != nil {
		logger.Fatal(formatted.Textf("Failed to parse --remote_executor_builder_pkix_public_key: %s", err))
	}

	parsedObjectPool := model_parser.NewParsedObjectPool(
		eviction.NewLRUSet[model_parser.ParsedObjectEvictionKey](),
		/* maximumCount = */ 1e3,
		/* maximumSizeBytes = */ 1e5,
	)
	parsedObjectPoolIngester := model_parser.NewParsedObjectPoolIngester[object.LocalReference](
		parsedObjectPool,
		model_parser.NewDownloadingObjectReader(
			object_namespacemapping.NewNamespaceAddingDownloader(
				object_grpc.NewDownloader(object_pb.NewDownloaderClient(remoteCacheClient)),
				instanceName,
			),
		),
	)

	logger.Info(formatted.Text("Performing query"))
	var resultReference model_core.Decodable[object.LocalReference]
	var errQuery error
	evaluationActionObjectFormat := model_core.NewProtoObjectFormat(&model_evaluation_pb.Action{})
	progressReader := model_parser.LookupParsedObjectReader(
		parsedObjectPoolIngester,
		model_parser.NewChainedObjectParser(
			model_parser.NewEncodedObjectParser[object.LocalReference](actionEncoder),
			model_parser.NewProtoObjectParser[object.LocalReference, model_evaluation_pb.Progress](),
		),
	)
	progressLinesWritten := 0
	for progressReference := range builderClient.RunAction(
		context.Background(),
		builderECDHPublicKey,
		&model_executewithstorage.Action[object.LocalReference]{
			Reference: model_core.CopyDecodable(
				createdAction,
				actionReference,
			),
			Encoders: defaultEncoders,
			Format:   evaluationActionObjectFormat,
		},
		&encryptedaction_pb.Action_AdditionalData{
			ExecutionTimeout: &durationpb.Duration{Seconds: 24 * 60 * 60},
		},
		&resultReference,
		&errQuery,
	) {
		progress, err := progressReader.ReadObject(context.Background(), progressReference)
		if err != nil {
			logger.Fatal(formatted.Textf("Failed to read progress message: %s", err))
		}
		logger.RemovePreviousLines(progressLinesWritten)
		logger.Info(formatted.Textf(
			"🚧 %d   🚦 %d   🏁 %d   📤 %d   🌍 %d",
			progress.Message.BlockedKeysCount,
			progress.Message.EvaluatableKeysCount,
			progress.Message.EvaluatedKeysCount,
			progress.Message.UploadingKeysCount,
			progress.Message.CompletedKeysCount,
		))
		progressLinesWritten = 1
	}
	if errQuery != nil {
		logger.Fatal(formatted.Textf("Failed to perform query: %s", errQuery))
	}
	logger.RemovePreviousLines(progressLinesWritten)

	resultReader := model_parser.LookupParsedObjectReader(
		parsedObjectPoolIngester,
		model_parser.NewChainedObjectParser(
			model_parser.NewEncodedObjectParser[object.LocalReference](actionEncoder),
			model_parser.NewProtoObjectParser[object.LocalReference, model_evaluation_pb.Result](),
		),
	)
	result, err := resultReader.ReadObject(
		context.Background(),
		resultReference,
	)
	if err != nil {
		logger.Fatal(formatted.Textf("Failed to read result message: %s", err))
	}

	if f := result.Message.Failure; f != nil {
		logger.Fatal(formatted.Textf("Failed to perform query: %s", status.FromProto(f.Status)))
	}
	if result.Message.OutcomesReference == nil {
		logger.Fatal(formatted.Text("Query did not yield any outcomes"))
	}
	outcomesReference, err := model_core.FlattenDecodableReference(model_core.Nested(result, result.Message.OutcomesReference))
	if err != nil {
		logger.Fatal(formatted.Textf("Invalid outcomes reference: %s", err))
	}

	// Read the outcomes list produced by the evaluation, and look
	// up the result of every requested TargetPatternExpansion_Key
	// within it.
	evaluationsReader := model_parser.LookupParsedObjectReader(
		parsedObjectPoolIngester,
		model_parser.NewChainedObjectParser(
			model_parser.NewEncodedObjectParser[object.LocalReference](actionEncoder),
			model_parser.NewProtoListObjectParser[object.LocalReference, model_evaluation_pb.Evaluations](),
		),
	)
	evaluationReader := model_parser.LookupParsedObjectReader(
		parsedObjectPoolIngester,
		model_parser.NewChainedObjectParser(
			model_parser.NewEncodedObjectParser[object.LocalReference](actionEncoder),
			model_parser.NewProtoObjectParser[object.LocalReference, model_evaluation_pb.Evaluation](),
		),
	)
	// Objects belonging to analysis values (as opposed to the
	// top-level evaluation bookkeeping objects above) are currently
	// stored without any additional encoding; see
	// baseComputer.getValueObjectEncoder() in pkg/model/analysis.

	evaluationsList, err := evaluationsReader.ReadObject(ctx, outcomesReference)
	if err != nil {
		logger.Fatal(formatted.Textf("Failed to read outcomes object: %s", err))
	}

	evaluations, err := btree.Find(
		ctx,
		evaluationsReader,
		evaluationsList,
		func(entry model_core.Message[*model_evaluation_pb.Evaluations, object.LocalReference]) (int, *model_core_pb.DecodableReference) {
			switch level := entry.Message.Level.(type) {
			case *model_evaluation_pb.Evaluations_Leaf_:
				return bytes.Compare(queryResultKeyReference.GetRawReference(), level.Leaf.KeyReference), nil
			case *model_evaluation_pb.Evaluations_Parent_:
				return bytes.Compare(queryResultKeyReference.GetRawReference(), level.Parent.FirstKeyReference), level.Parent.Reference
			default:
				return 0, nil
			}
		},
	)
	if err != nil {
		logger.Fatal(formatted.Textf("Failed to look up query results: %s", err))
	}
	if !evaluations.IsSet() {
		logger.Fatal(formatted.Text("No results were returned for the query"))
	}
	evaluationsLeaf, ok := evaluations.Message.Level.(*model_evaluation_pb.Evaluations_Leaf_)
	if !ok {
		logger.Fatal(formatted.Text("Outcomes list entry for the query is not a valid leaf"))
	}

	graphlet := model_core.Nested(evaluations, evaluationsLeaf.Leaf.Graphlet)
	evaluation, err := model_evaluation.GraphletGetEvaluation(ctx, evaluationReader, graphlet)
	if err != nil {
		logger.Fatal(formatted.Textf("Failed to obtain evaluation for the query: %s", err))
	}
	if evaluation.Message.Value == nil {
		logger.Fatal(formatted.Text("The query did not yield a value; evaluation may have failed"))
	}
	flattenedValue, err := model_core.FlattenAny(model_core.Nested(evaluation, evaluation.Message.Value))
	if err != nil {
		logger.Fatal(formatted.Textf("Failed to flatten the query value: %s", err))
	}
	unmarshaledValue, err := model_core.UnmarshalTopLevelAnyNew(flattenedValue)
	if err != nil {
		logger.Fatal(formatted.Textf("Failed to unmarshal the query value: %s", err))
	}
	return unmarshaledValue.Message
}

package build

import (
	"bytes"
	"context"
	"encoding"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"bonanza.build/pkg/bazelclient/arguments"
	"bonanza.build/pkg/bazelclient/commands"
	"bonanza.build/pkg/bazelclient/formatted"
	"bonanza.build/pkg/bazelclient/logging"
	"bonanza.build/pkg/crypto"
	"bonanza.build/pkg/label"
	"bonanza.build/pkg/model/core"
	model_core "bonanza.build/pkg/model/core"
	"bonanza.build/pkg/model/core/btree"
	model_encoding "bonanza.build/pkg/model/encoding"
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

	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/semaphore"
	"golang.org/x/term"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/security/advancedtls"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/emptypb"
)

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

// Invocation holds the inputs of a single build that bonanza_bazel
// performs. Every command that needs to build something ("build",
// "test") provides these, regardless of the flags it accepts itself.
type Invocation struct {
	CommonFlags           *arguments.CommonFlags
	BuildFlags            *arguments.BuildFlags
	BuildSettingOverrides []arguments.BuildSettingOverride
	Arguments             []string
}

// Results provides access to the outcomes of a build, so that commands
// can extract the values of the evaluation keys they requested.
type Results struct {
	Context                  context.Context
	Logger                   logging.Logger
	ParsedObjectPoolIngester *model_parser.ParsedObjectPoolIngester[object.LocalReference]
	ActionEncoder            model_encoding.DeterministicBinaryEncoder
	ReferenceFormat          object.ReferenceFormat
	DirectoryParameters      *model_filesystem.DirectoryCreationParameters
	FileParameters           *model_filesystem.FileCreationParameters
	OutcomesReference        model_core.Decodable[object.LocalReference]

	buildResultKeyAny model_core.TopLevelMessage[*anypb.Any, object.LocalReference]
}

// ExtraRequestedKeysFunc returns evaluation keys that a command wants to
// have computed in addition to BuildResult. It is called with the target
// patterns and configurations that were derived from the invocation, as
// those tend to make up the keys in question.
// The keys MUST NOT contain any outgoing references.
type ExtraRequestedKeysFunc func(
	targetPatterns []string,
	configurations []*model_analysis_pb.BuildResult_Key_Configuration,
) ([]proto.Message, error)

// MarshalEvaluationKey converts an evaluation key that contains no
// outgoing references to the form in which LookUpEvaluationValue()
// expects it.
func MarshalEvaluationKey(key proto.Message) (model_core.TopLevelMessage[*anypb.Any, object.LocalReference], error) {
	return model_core.MarshalTopLevelAny(
		model_core.NewSimpleTopLevelMessage[object.LocalReference](key),
	)
}

// DoBuild implements the "bonanza_bazel build" command.
func DoBuild(args *arguments.BuildCommand, workspacePath path.Parser) {
	logger := logging.NewLoggerFromFlags(&args.CommonFlags)
	commands.ValidateInsideWorkspace(logger, "build", workspacePath)
	results := PerformBuild(
		logger,
		&Invocation{
			CommonFlags:           &args.CommonFlags,
			BuildFlags:            &args.BuildFlags,
			BuildSettingOverrides: args.BuildSettingOverrides,
			Arguments:             args.Arguments,
		},
		workspacePath,
		/* extraRequestedKeys = */ nil,
	)
	if results == nil {
		return
	}
	if err := MaterializeBuildResult(results, &args.BuildFlags, workspacePath); err != nil {
		logger.Fatal(formatted.Textf("Failed to materialize build outputs: %s", err))
	}
}

// MaterializeBuildResult writes the output files of the targets that
// were built to the local system.
func MaterializeBuildResult(results *Results, buildFlags *arguments.BuildFlags, workspacePath path.Parser) error {
	buildResultValue, err := LookUpEvaluationValue[model_analysis_pb.BuildResult_Value](results, results.buildResultKeyAny)
	if err != nil {
		return fmt.Errorf("failed to look up build result: %w", err)
	}
	return materializeOutputs(
		results.Context,
		results.Logger,
		buildFlags,
		workspacePath,
		results.ParsedObjectPoolIngester,
		results.DirectoryParameters.DirectoryAccessParameters,
		results.FileParameters.FileAccessParameters,
		model_core.Nested(buildResultValue, buildResultValue.Message.RootDirectory),
	)
}

// PerformBuild builds the targets that the invocation names, and returns
// the outcomes of the evaluation. It returns nil if the build did not
// yield any outcomes.
func PerformBuild(logger logging.Logger, inv *Invocation, workspacePath path.Parser, extraRequestedKeys ExtraRequestedKeysFunc) *Results {
	remoteCacheClient, err := newGRPCClient(inv.CommonFlags.RemoteCache, inv.CommonFlags)
	if err != nil {
		logger.Fatal(formatted.Textf("Failed to create gRPC client for --remote_cache=%#v: %s", inv.CommonFlags.RemoteCache, err))
	}

	// Determine the names and paths of all modules that are present
	// on the local system and need to be uploaded as part of the
	// build. First look for local_path_override() directives in
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
	moduleDotBazelHandler := NewLocalPathExtractingModuleDotBazelHandler(modulePaths, workspacePath)
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
	for _, overrideModule := range inv.CommonFlags.OverrideModule {
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
	encryptionKeyBytes, err := base64.StdEncoding.DecodeString(inv.CommonFlags.RemoteEncryptionKey)
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
	if inv.CommonFlags.RemoteCacheCompression {
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

	fetcherPKIXPublicKey, err := base64.StdEncoding.DecodeString(inv.CommonFlags.RemoteExecutorFetcherPkixPublicKey)
	if err != nil {
		logger.Fatal(formatted.Textf("Failed to base64 decode --remote_executor_fetcher_pkix_public_key: %s", err))
	}

	// TODO: Take the current working directory into account, so
	// that any relative target patterns are resolved correctly.
	currentPackage := rootModuleName.ToModuleInstance(nil).GetBareCanonicalRepo().GetRootPackage()

	// Construct a BuildSpecification message that lists all the
	// modules and contains all of the flags to instruct what needs
	// to be built.
	buildSpecification := model_analysis_pb.BuildSpecification_Value{
		RootModuleName:                         rootModuleName.String(),
		DirectoryCreationParameters:            directoryParametersMessage,
		FileCreationParameters:                 fileParametersMessage,
		IgnoreRootModuleDevDependencies:        inv.CommonFlags.IgnoreDevDependency,
		BuiltinsModuleNames:                    inv.CommonFlags.BuiltinsModule,
		RepoPlatform:                           inv.CommonFlags.RepoPlatform,
		FetchPlatformPkixPublicKey:             fetcherPKIXPublicKey,
		ActionEncoders:                         defaultEncoders,
		RuleImplementationWrapperIdentifier:    inv.CommonFlags.RuleImplementationWrapperIdentifier,
		SubruleImplementationWrapperIdentifier: inv.CommonFlags.SubruleImplementationWrapperIdentifier,
	}
	switch inv.CommonFlags.LockfileMode {
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
	if len(inv.CommonFlags.Registry) > 0 {
		buildSpecification.ModuleRegistryUrls = inv.CommonFlags.Registry
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

	// TODO: Should these be moved into special overrides?
	/*
		var invocationID uuid.UUID
		if v := inv.CommonFlags.InvocationId; v == "" {
			invocationID = util.Must(uuid.NewRandom())
		} else {
			invocationID, err = uuid.Parse(v)
			if err != nil {
				logger.Fatal(formatted.Textf("Invalid --invocation_id=%#v: %s", v, err))
			}
		}
		var buildRequestID uuid.UUID
		if v := inv.CommonFlags.BuildRequestId; v == "" {
			buildRequestID = util.Must(uuid.NewRandom())
		} else {
			buildRequestID, err = uuid.Parse(v)
			if err != nil {
				logger.Fatal(formatted.Textf("Invalid --build_request_id=%#v: %s", v, err))
			}
		}
	*/

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

	targetPatterns := make([]string, 0, len(inv.Arguments))
	for _, targetPattern := range inv.Arguments {
		apparentTargetPattern, err := currentPackage.AppendTargetPattern(targetPattern)
		if err != nil {
			logger.Fatal(formatted.Textf("Invalid target pattern %#v: %s", targetPattern, err))
		}
		targetPatterns = append(targetPatterns, apparentTargetPattern.String())
	}

	// Determine the configurations for which to build. The Bazel
	// CLI only supports specifying build setting overrides and a
	// single list of platforms. However, there is no way to pick
	// different build setting overrides depending on the platform.
	commonBuildSettingOverrides := make([]*model_analysis_pb.BuildResult_Key_BuildSettingOverride, 0, len(inv.BuildSettingOverrides))
	for _, override := range inv.BuildSettingOverrides {
		apparentLabel, err := currentPackage.AppendTargetPattern(override.Label)
		if err != nil {
			logger.Fatal(formatted.Textf("Invalid build setting override --%s=%#v: %s", override.Label, override.Value, err))
		}
		commonBuildSettingOverrides = append(
			commonBuildSettingOverrides,
			&model_analysis_pb.BuildResult_Key_BuildSettingOverride{
				Label: apparentLabel.String(),
				Value: override.Value,
			},
		)
	}
	targetPlatforms := strings.FieldsFunc(inv.BuildFlags.Platforms, func(r rune) bool { return r == ',' })
	if len(targetPlatforms) == 0 {
		targetPlatforms = []string{"@platforms//host"}
	}
	configurations := make([]*model_analysis_pb.BuildResult_Key_Configuration, 0, len(targetPlatforms))
	for _, targetPlatform := range targetPlatforms {
		configurations = append(configurations, &model_analysis_pb.BuildResult_Key_Configuration{
			BuildSettingOverrides: append(
				[]*model_analysis_pb.BuildResult_Key_BuildSettingOverride{{
					Label: "@bazel_tools//command_line_option:platforms",
					Value: targetPlatform,
				}},
				commonBuildSettingOverrides...,
			),
		})
	}

	// The evaluation key that this build requests. Also create a
	// marshaled copy of it, as we need to embed that into the
	// bonanza_browser URL to link to the results.
	patchedBuildResultKey := model_core.NewSimplePatchedMessage[dag.ObjectContentsWalker](
		&model_analysis_pb.BuildResult_Key{
			TargetPatterns: targetPatterns,
			Configurations: configurations,
		},
	)
	buildResultKey, _ := patchedBuildResultKey.SortAndSetReferences()
	buildResultKeyAny, err := model_core.MarshalTopLevelAny(buildResultKey)
	if err != nil {
		logger.Fatal(formatted.Textf("Failed to marshal build result key: %s", err))
	}
	marshaledBuildResultKey, err := model_core.MarshalTopLevelMessage(buildResultKeyAny)
	if err != nil {
		logger.Fatal(formatted.Textf("Failed to marshal build result key: %s", err))
	}

	// Keys that the command wants to have computed on top of
	// BuildResult, such as the test results that "test" requests.
	var extraKeys []proto.Message
	if extraRequestedKeys != nil {
		extraKeys, err = extraRequestedKeys(targetPatterns, configurations)
		if err != nil {
			logger.Fatal(formatted.Textf("Failed to create requested evaluation keys: %s", err))
		}
	}

	// Construct an Action message.
	actionMessage, err := model_core.BuildPatchedMessage(func(patcher *model_core.ReferenceMessagePatcher[dag.ObjectContentsWalker]) (encoding.BinaryMarshaler, error) {
		overridesReference, err := patcher.CaptureAndAddDecodableReference(
			ctx,
			createdOverrides,
			model_core.WalkableCreatedObjectCapturer,
		)
		if err != nil {
			return nil, err
		}

		patchedBuildResultKeyAny, err := model_core.MarshalAny(patchedBuildResultKey)
		if err != nil {
			return nil, err
		}

		requestedKeys := []*model_evaluation_pb.Keys{{
			Level: &model_evaluation_pb.Keys_Leaf{
				Leaf: patchedBuildResultKeyAny.Merge(patcher),
			},
		}}
		for _, extraKey := range extraKeys {
			patchedExtraKeyAny, err := model_core.MarshalAny(
				model_core.NewSimplePatchedMessage[dag.ObjectContentsWalker](extraKey),
			)
			if err != nil {
				return nil, err
			}
			requestedKeys = append(requestedKeys, &model_evaluation_pb.Keys{
				Level: &model_evaluation_pb.Keys_Leaf{
					Leaf: patchedExtraKeyAny.Merge(patcher),
				},
			})
		}

		return model_core.NewProtoBinaryMarshaler(&model_evaluation_pb.Action{
			OverridesReference: overridesReference,
			RequestedKeys:      requestedKeys,
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
	instanceName, err := object.NewInstanceName(inv.CommonFlags.RemoteInstanceName)
	if err != nil {
		logger.Fatal(formatted.Textf("Invalid --remote_instance_name=%#v: %s", inv.CommonFlags.RemoteInstanceName, err))
	}
	actionReference := createdAction.Value.GetLocalReference()
	actionGlobalReference := instanceName.WithLocalReference(actionReference)
	dagUploader := dag_grpc.NewUploader(
		dag_pb.NewUploaderClient(remoteCacheClient),
		semaphore.NewWeighted(10),
		object.NewLimit(&object_pb.Limit{
			Count:     1000,
			SizeBytes: 1 << 20,
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

	clientPrivateKeyData, err := os.ReadFile(inv.CommonFlags.RemoteExecutorClientPrivateKey)
	if err != nil {
		logger.Fatal(formatted.Textf("Failed to read --remote_executor_client_private_key=%#v: %s", inv.CommonFlags.RemoteExecutorClientPrivateKey, err))
	}
	clientPrivateKey, err := crypto.ParsePEMWithPKCS8ECDHPrivateKey(clientPrivateKeyData)
	if err != nil {
		logger.Fatal(formatted.Textf("Failed to parse --remote_executor_client_private_key=%#v: %s", inv.CommonFlags.RemoteExecutorClientPrivateKey, err))
	}

	clientCertificateChainData, err := os.ReadFile(inv.CommonFlags.RemoteExecutorClientCertificateChain)
	if err != nil {
		logger.Fatal(formatted.Textf("Failed to read --remote_executor_client_certificate_chain=%#v: %s", inv.CommonFlags.RemoteExecutorClientCertificateChain, err))
	}
	clientCertificateChain, err := crypto.ParsePEMWithCertificateChain(clientCertificateChainData)
	if err != nil {
		logger.Fatal(formatted.Textf("Failed to parse --remote_executor_client_certificate_chain=%#v: %s", inv.CommonFlags.RemoteExecutorClientCertificateChain, err))
	}

	remoteExecutorClient, err := newGRPCClient(inv.CommonFlags.RemoteExecutor, inv.CommonFlags)
	if err != nil {
		logger.Fatal(formatted.Textf("Failed to create gRPC client for --remote_executor=%#v: %s", inv.CommonFlags.RemoteExecutor, err))
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

	builderPKIXPublicKey, err := base64.StdEncoding.DecodeString(inv.CommonFlags.RemoteExecutorBuilderPkixPublicKey)
	if err != nil {
		logger.Fatal(formatted.Textf("Failed to base64 decode --remote_executor_builder_pkix_public_key: %s", err))
	}
	builderECDHPublicKey, err := crypto.ParsePKIXECDHPublicKey(builderPKIXPublicKey)
	if err != nil {
		logger.Fatal(formatted.Textf("Failed to parse --remote_executor_builder_pkix_public_key: %s", err))
	}

	decodableActionReference := model_core.CopyDecodable(createdAction, actionReference)
	actionReferenceStr := model_core.DecodableLocalReferenceToString(decodableActionReference)
	actionLink := formatted.Text(actionReferenceStr)
	browserURL := inv.CommonFlags.BrowserUrl
	evaluationActionObjectFormat := model_core.NewProtoObjectFormat(&model_evaluation_pb.Action{})
	evaluationActionPathComponents, _ := model_core.ObjectFormatToPath(evaluationActionObjectFormat)
	if browserURL != "" {
		if actionURL, err := url.JoinPath(
			browserURL,
			append(
				[]string{
					"object",
					instanceName.AsURLSafeComponent(),
					referenceFormat.ToProto().String(),
					actionReferenceStr,
				},
				evaluationActionPathComponents...,
			)...,
		); err == nil {
			actionLink = formatted.Link(actionURL, actionLink)
		}
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

	var jsonFormatter messageJSONFormatter
	if browserURL != "" {
		if baseURL, err := url.JoinPath(
			browserURL,
			"object",
			instanceName.AsURLSafeComponent(),
			referenceFormat.ToProto().String(),
		); err == nil {
			jsonFormatter.baseURL = baseURL
		}
	}

	logger.Info(formatted.Join(formatted.Text("Performing build "), actionLink))
	var resultReference model_core.Decodable[object.LocalReference]
	var errBuild error
	namespace := object.Namespace{
		InstanceName:    instanceName,
		ReferenceFormat: referenceFormat,
	}
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
		&errBuild,
	) {
		progress, err := progressReader.ReadObject(context.Background(), progressReference)
		if err != nil {
			logger.Fatal(formatted.Textf("Failed to read progress message: %s", err))
		}
		logger.RemovePreviousLines(progressLinesWritten)
		progressLinesWritten = 0

		logger.Info(formatted.Textf(
			"🚧 %d   🚦 %d   🚗💨 %d   🏁 %d   📤 %d   🌍 %d",
			progress.Message.BlockedKeysCount,
			progress.Message.EvaluatableKeysCount,
			uint64(len(progress.Message.OldestEvaluatingKeys))+progress.Message.AdditionalEvaluatingKeysCount,
			progress.Message.EvaluatedKeysCount,
			progress.Message.UploadingKeysCount,
			progress.Message.CompletedKeysCount,
		))
		progressLinesWritten++

		// Determine how many currently evaluating keys we want
		// to display in the terminal. If we can't display all
		// of them (or if the server wasn't able to fit all of
		// them in the progress event), we'll display the number
		// not shown at the bottom of the screen.
		evaluatingKeysToDisplay := progress.Message.OldestEvaluatingKeys
		additionalEvaluatingKeysCount := progress.Message.AdditionalEvaluatingKeysCount
		_, terminalLines, err := term.GetSize(1)
		if err != nil {
			terminalLines = 24
		}
		maximumEvaluatingKeysToDisplay := terminalLines - progressLinesWritten - 1
		if additionalEvaluatingKeysCount > 0 || len(evaluatingKeysToDisplay) > maximumEvaluatingKeysToDisplay {
			maximumEvaluatingKeysToDisplay--
		}
		if maximumEvaluatingKeysToDisplay < 0 {
			maximumEvaluatingKeysToDisplay = 0
		}
		if len(evaluatingKeysToDisplay) > maximumEvaluatingKeysToDisplay {
			additionalEvaluatingKeysCount += uint64(len(evaluatingKeysToDisplay)) - uint64(maximumEvaluatingKeysToDisplay)
			evaluatingKeysToDisplay = evaluatingKeysToDisplay[:maximumEvaluatingKeysToDisplay]
		}

		longestType := 0
		for _, evaluatingKey := range progress.Message.OldestEvaluatingKeys {
			if l := len(getAbbreviatedTypeURL(evaluatingKey.Key.GetValue().GetTypeUrl())); longestType < l {
				longestType = l
			}
		}

		for _, evaluatingKey := range evaluatingKeysToDisplay {
			logger.Info(
				formatted.NoWrap(
					formatKey(
						namespace,
						model_core.Nested(progress, evaluatingKey.Key),
						&jsonFormatter,
						browserURL,
						/* outcomesReference = */ nil,
						longestType,
					),
				),
			)
			progressLinesWritten++
		}

		if additionalEvaluatingKeysCount > 0 {
			logger.Info(formatted.Textf("  ... and %d more", additionalEvaluatingKeysCount))
			progressLinesWritten++
		}
	}
	if errBuild != nil {
		logger.Fatal(formatted.Textf("Failed to perform build: %s", errBuild))
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

	var outcomesReference *model_core.Decodable[object.LocalReference]
	if rm := result.Message.OutcomesReference; rm != nil {
		r, err := model_core.FlattenDecodableReference(model_core.Nested(result, rm))
		if err != nil {
			logger.Fatal(formatted.Textf("Invalid evaluations reference: %s", err))
		}
		outcomesReference = &r
	}

	if f := result.Message.Failure; f != nil {
		printStackTrace(namespace, model_core.Nested(result, f.StackTraceKeys), logger, &jsonFormatter, browserURL, outcomesReference)
		logger.Fatal(formatted.Textf("Failed to perform build: %s", status.FromProto(f.Status)))
	}
	if outcomesReference == nil {
		return nil
	}

	outcomesReferenceNode := formatted.Text(model_core.DecodableLocalReferenceToString(*outcomesReference))
	if browserURL != "" {
		if evaluationURL, err := url.JoinPath(
			browserURL,
			"evaluation",
			namespace.InstanceName.AsURLSafeComponent(),
			namespace.ReferenceFormat.ToProto().String(),
			model_core.DecodableLocalReferenceToString(*outcomesReference),
			base64.RawURLEncoding.EncodeToString(marshaledBuildResultKey),
		); err == nil {
			outcomesReferenceNode = formatted.Link(evaluationURL, outcomesReferenceNode)
		}
	}
	logger.Info(
		formatted.Join(
			formatted.Text("Got build results "),
			outcomesReferenceNode,
		),
	)

	return &Results{
		Context:                  ctx,
		Logger:                   logger,
		ParsedObjectPoolIngester: parsedObjectPoolIngester,
		ActionEncoder:            actionEncoder,
		ReferenceFormat:          referenceFormat,
		DirectoryParameters:      directoryParameters,
		FileParameters:           fileParameters,
		OutcomesReference:        *outcomesReference,
		buildResultKeyAny:        buildResultKeyAny,
	}
}

// LookUpEvaluationValue extracts the value of a single key from the list
// of outcomes that a build emitted.
func LookUpEvaluationValue[
	TMessage any,
	TMessagePtr interface {
		*TMessage
		proto.Message
	},
](
	results *Results,
	keyAny model_core.TopLevelMessage[*anypb.Any, object.LocalReference],
) (model_core.Message[TMessagePtr, object.LocalReference], error) {
	ctx := results.Context
	parsedObjectPoolIngester := results.ParsedObjectPoolIngester
	actionEncoder := results.ActionEncoder
	outcomesReference := results.OutcomesReference
	referenceFormat := results.ReferenceFormat

	var bad model_core.Message[TMessagePtr, object.LocalReference]
	keyReference, err := model_core.ComputeTopLevelMessageReference(keyAny, referenceFormat)
	if err != nil {
		return bad, err
	}

	evaluationsReader := model_parser.LookupParsedObjectReader(
		parsedObjectPoolIngester,
		model_parser.NewChainedObjectParser(
			model_parser.NewEncodedObjectParser[object.LocalReference](actionEncoder),
			model_parser.NewProtoListObjectParser[object.LocalReference, model_evaluation_pb.Evaluations](),
		),
	)
	evaluationsList, err := evaluationsReader.ReadObject(ctx, outcomesReference)
	if err != nil {
		return bad, fmt.Errorf("failed to read outcomes list: %w", err)
	}

	evaluations, err := btree.Find(
		ctx,
		evaluationsReader,
		evaluationsList,
		func(entry model_core.Message[*model_evaluation_pb.Evaluations, object.LocalReference]) (int, *model_core_pb.DecodableReference) {
			switch level := entry.Message.Level.(type) {
			case *model_evaluation_pb.Evaluations_Leaf_:
				return bytes.Compare(keyReference.GetRawReference(), level.Leaf.KeyReference), nil
			case *model_evaluation_pb.Evaluations_Parent_:
				return bytes.Compare(keyReference.GetRawReference(), level.Parent.FirstKeyReference), level.Parent.Reference
			default:
				return 0, nil
			}
		},
	)
	if err != nil {
		return bad, fmt.Errorf("failed to look up key in outcomes list: %w", err)
	}
	if !evaluations.IsSet() {
		return bad, errors.New("key is not present in the outcomes list")
	}
	leaf, ok := evaluations.Message.Level.(*model_evaluation_pb.Evaluations_Leaf_)
	if !ok {
		return bad, errors.New("outcomes list entry is not a valid leaf")
	}

	var evaluation model_core.Message[*model_evaluation_pb.Evaluation, object.LocalReference]
	switch level := leaf.Leaf.Graphlet.GetEvaluation().(type) {
	case *model_evaluation_pb.Graphlet_EvaluationInline:
		evaluation = model_core.Nested(evaluations, level.EvaluationInline)
	case *model_evaluation_pb.Graphlet_EvaluationExternal:
		evaluationReader := model_parser.LookupParsedObjectReader(
			parsedObjectPoolIngester,
			model_parser.NewChainedObjectParser(
				model_parser.NewEncodedObjectParser[object.LocalReference](actionEncoder),
				model_parser.NewProtoObjectParser[object.LocalReference, model_evaluation_pb.Evaluation](),
			),
		)
		evaluation, err = model_parser.Dereference(ctx, evaluationReader, model_core.Nested(evaluations, level.EvaluationExternal))
		if err != nil {
			return bad, fmt.Errorf("failed to read evaluation: %w", err)
		}
	default:
		return bad, errors.New("evaluation is not present")
	}

	value, err := model_core.UnmarshalAnyNew(model_core.Nested(evaluation, evaluation.Message.Value))
	if err != nil {
		return bad, fmt.Errorf("failed to unmarshal value: %w", err)
	}
	message, ok := value.Message.(TMessagePtr)
	if !ok {
		return bad, fmt.Errorf("value has type %s, while %s was expected", value.Message.ProtoReflect().Descriptor().FullName(), TMessagePtr(new(TMessage)).ProtoReflect().Descriptor().FullName())
	}
	return model_core.Nested(value.Decay(), message), nil
}

// materializeOutputs writes the output files of a build to a directory
// on the local system, and creates convenience symbolic links pointing
// into it.
func materializeOutputs(
	ctx context.Context,
	logger logging.Logger,
	buildFlags *arguments.BuildFlags,
	workspacePath path.Parser,
	parsedObjectPoolIngester *model_parser.ParsedObjectPoolIngester[object.LocalReference],
	directoryAccessParameters *model_filesystem.DirectoryAccessParameters,
	fileAccessParameters *model_filesystem.FileAccessParameters,
	rootDirectory model_core.Message[*model_filesystem_pb.DirectoryContents, object.LocalReference],
) error {
	if rootDirectory.Message == nil {
		// The build did not yield any output files.
		return nil
	}

	outputPathStr, err := getOutputPath(buildFlags, workspacePath)
	if err != nil {
		return err
	}
	outputPath := path.LocalFormat.NewParser(outputPathStr)
	if err := os.MkdirAll(outputPathStr, 0o777); err != nil {
		return fmt.Errorf("failed to create output directory: %w", err)
	}
	outputDirectory, err := filesystem.NewLocalDirectory(outputPath)
	if err != nil {
		return fmt.Errorf("failed to open output directory: %w", err)
	}
	defer outputDirectory.Close()

	directoryEncoderObjectParser := model_parser.NewEncodedObjectParser[object.LocalReference](directoryAccessParameters.GetEncoder())
	m := outputRootMaterializer{
		context: ctx,
		directoryContentsReader: model_parser.LookupParsedObjectReader(
			parsedObjectPoolIngester,
			model_parser.NewChainedObjectParser(
				directoryEncoderObjectParser,
				model_parser.NewProtoObjectParser[object.LocalReference, model_filesystem_pb.DirectoryContents](),
			),
		),
		leavesReader: model_parser.LookupParsedObjectReader(
			parsedObjectPoolIngester,
			model_parser.NewChainedObjectParser(
				directoryEncoderObjectParser,
				model_parser.NewProtoObjectParser[object.LocalReference, model_filesystem_pb.Leaves](),
			),
		),
		fileReader: model_filesystem.NewFileReader(
			model_parser.LookupParsedObjectReader(
				parsedObjectPoolIngester,
				model_parser.NewChainedObjectParser(
					model_parser.NewEncodedObjectParser[object.LocalReference](fileAccessParameters.GetFileContentsListEncoder()),
					model_filesystem.NewFileContentsListObjectParser[object.LocalReference](),
				),
			),
			model_parser.LookupParsedObjectReader(
				parsedObjectPoolIngester,
				model_parser.NewChainedObjectParser(
					model_parser.NewEncodedObjectParser[object.LocalReference](fileAccessParameters.GetChunkEncoder()),
					model_parser.NewRawObjectParser[object.LocalReference](),
				),
			),
			semaphore.NewWeighted(int64(runtime.NumCPU())),
		),
	}
	if err := m.materializeDirectory(rootDirectory, outputDirectory); err != nil {
		return err
	}

	logger.Info(formatted.Textf("Wrote %d output files and %d symbolic links to %s", m.filesWritten, m.symlinksWritten, outputPathStr))
	return createConvenienceSymlinks(buildFlags, workspacePath, outputPathStr, outputDirectory)
}

// localPathString renders a parsed path as a string in the format of the
// local system.
func localPathString(p path.Parser) (string, error) {
	builder, scopeWalker := path.EmptyBuilder.Join(path.NewAbsoluteScopeWalker(path.VoidComponentWalker))
	if err := path.Resolve(p, scopeWalker); err != nil {
		return "", err
	}
	return path.LocalFormat.GetString(builder)
}

// getOutputPath returns the directory into which output files should be
// written.
func getOutputPath(buildFlags *arguments.BuildFlags, workspacePath path.Parser) (string, error) {
	if p := buildFlags.OutputPath; p != "" {
		return p, nil
	}
	workspacePathStr, err := localPathString(workspacePath)
	if err != nil {
		return "", err
	}
	return filepath.Join(workspacePathStr, "bonanza-out"), nil
}

// createConvenienceSymlinks creates symbolic links in the workspace that
// point to the directories inside the output path in which the output
// files of the root repo are placed. This mirrors the "bazel-bin"
// symbolic link that Bazel creates.
func createConvenienceSymlinks(buildFlags *arguments.BuildFlags, workspacePath path.Parser, outputPathStr string, outputDirectory filesystem.Directory) error {
	symlinkPrefix := buildFlags.SymlinkPrefix
	if symlinkPrefix == "/" {
		return nil
	}

	// Only create a "bin" symbolic link if the build used a single
	// configuration, as there is no way to disambiguate otherwise.
	bazelOut, ok := path.NewComponent("bazel-out")
	if !ok {
		panic("invalid component name")
	}
	bazelOutDirectory, err := outputDirectory.EnterDirectory(bazelOut)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	configurations, err := bazelOutDirectory.ReadDir()
	bazelOutDirectory.Close()
	if err != nil {
		return err
	}
	if len(configurations) != 1 {
		return nil
	}

	workspacePathStr, err := localPathString(workspacePath)
	if err != nil {
		return err
	}
	workspaceDirectory, err := filesystem.NewLocalDirectory(workspacePath)
	if err != nil {
		return err
	}
	defer workspaceDirectory.Close()

	binName, ok := path.NewComponent(symlinkPrefix + "bin")
	if !ok {
		return fmt.Errorf("invalid --symlink_prefix=%#v", symlinkPrefix)
	}
	binTarget := filepath.Join(outputPathStr, "bazel-out", configurations[0].Name().String(), "bin")
	if relativeBinTarget, err := filepath.Rel(workspacePathStr, binTarget); err == nil {
		binTarget = relativeBinTarget
	}
	workspaceDirectory.Remove(binName)
	return workspaceDirectory.Symlink(path.LocalFormat.NewParser(binTarget), binName)
}

func formatKey(namespace object.Namespace, keyAny model_core.Message[*model_core_pb.Any, object.LocalReference], jsonFormatter *messageJSONFormatter, browserURL string, outcomesReference *model_core.Decodable[object.LocalReference], longestType int) formatted.Node {
	abbreviatedType := getAbbreviatedTypeURL(keyAny.Message.GetValue().GetTypeUrl())
	abbreviatedTypeNode := formatted.Text(abbreviatedType)
	var body formatted.Node
	if flattenedKey, err := model_core.FlattenAny(keyAny); err != nil {
		body = formatted.Bold(formatted.Text(fmt.Sprintf("Failed to flatten key: %s", err)))
	} else if key, err := model_core.UnmarshalTopLevelAnyNew[object.LocalReference](flattenedKey); err != nil {
		body = formatted.Bold(formatted.Text(fmt.Sprintf("Failed to unmarshal key: %s", err)))
	} else {
		if browserURL != "" && outcomesReference != nil {
			if marshaledKey, err := model_core.MarshalTopLevelMessage(flattenedKey); err == nil {
				if evaluationURL, err := url.JoinPath(
					browserURL,
					"evaluation",
					namespace.InstanceName.AsURLSafeComponent(),
					namespace.ReferenceFormat.ToProto().String(),
					model_core.DecodableLocalReferenceToString(*outcomesReference),
					base64.RawURLEncoding.EncodeToString(marshaledKey),
				); err == nil {
					abbreviatedTypeNode = formatted.Link(evaluationURL, abbreviatedTypeNode)
				}
			}
		}
		body = jsonFormatter.formatJSONMessage(model_core.Nested(key.Decay(), key.Message.ProtoReflect()))
	}
	return formatted.Join(
		formatted.Text("  "),
		abbreviatedTypeNode,
		formatted.Textf("%*s", longestType-len(abbreviatedType), ""),
		formatted.Textf("  "),
		body,
	)
}

func printStackTrace(namespace object.Namespace, stackTraceKeys model_core.Message[[]*model_core_pb.Any, object.LocalReference], logger logging.Logger, jsonFormatter *messageJSONFormatter, browserURL string, outcomesReference *model_core.Decodable[object.LocalReference]) {
	if len(stackTraceKeys.Message) > 0 {
		logger.Error(formatted.Text("Traceback (most recent key last):"))

		longestType := 0
		for _, keyAny := range stackTraceKeys.Message {
			if l := len(getAbbreviatedTypeURL(keyAny.Value.GetTypeUrl())); longestType < l {
				longestType = l
			}
		}

		for _, keyAny := range stackTraceKeys.Message {
			logger.Error(
				formatKey(
					namespace,
					model_core.Nested(stackTraceKeys, keyAny),
					jsonFormatter,
					browserURL,
					outcomesReference,
					longestType,
				),
			)
		}
	}
}

func getAbbreviatedTypeURL(typeURL string) string {
	typeURL = strings.TrimSuffix(typeURL, ".Key")
	if dot := strings.LastIndexByte(typeURL, '.'); dot >= 0 {
		return typeURL[dot+1:]
	}
	return typeURL
}

type messageJSONFormatter struct {
	baseURL string
}

func formatReferenceLink(link, rawReference string) formatted.Node {
	if len(rawReference) > 8+3 {
		rawReference = rawReference[:8] + "..."
	}
	return formatted.Link(link, formatted.Cyan(formatted.Textf("%#v", rawReference)))
}

func (f *messageJSONFormatter) formatJSONField(fieldDescriptor protoreflect.FieldDescriptor, value model_core.Message[protoreflect.Value, object.LocalReference]) formatted.Node {
	var v any
	switch fieldDescriptor.Kind() {
	// Simple scalar types for which we can just call json.Marshal().
	case protoreflect.BoolKind:
		v = value.Message.Bool()
	case protoreflect.Int32Kind, protoreflect.Int64Kind,
		protoreflect.Sint32Kind, protoreflect.Sint64Kind,
		protoreflect.Sfixed32Kind, protoreflect.Sfixed64Kind:
		v = value.Message.Int()
	case protoreflect.Uint32Kind, protoreflect.Uint64Kind,
		protoreflect.Fixed32Kind, protoreflect.Fixed64Kind:
		v = value.Message.Uint()
	case protoreflect.FloatKind, protoreflect.DoubleKind:
		v = value.Message.Float()
	case protoreflect.StringKind:
		v = value.Message.String()
	case protoreflect.BytesKind:
		v = value.Message.Bytes()

	case protoreflect.GroupKind, protoreflect.MessageKind:
		if r, ok := value.Message.Message().Interface().(*model_core_pb.DecodableReference); ok {
			if reference, err := model_core.FlattenDecodableReference(model_core.Nested(value, r)); err == nil {
				rawReference := model_core.DecodableLocalReferenceToString(reference)
				if f.baseURL != "" {
					if fieldOptions, ok := fieldDescriptor.Options().(*descriptorpb.FieldOptions); ok {
						// Field is a valid reference for
						// which we have type information in
						// the field options. Emit a link to
						// the object.
						objectFormat := proto.GetExtension(fieldOptions, model_core_pb.E_ObjectFormat).(*model_core_pb.ObjectFormat)
						segments, ok := core.ObjectFormatToPath(objectFormat)
						if ok {
							if link, err := url.JoinPath(f.baseURL, append([]string{rawReference}, segments...)...); err == nil {
								return formatReferenceLink(link, rawReference)
							}
						}
					}
				}
				return formatted.Cyan(formatted.Textf("%#v", rawReference))
			}
		}

		// Recurse into message.
		return f.formatJSONMessage(model_core.Nested(value, value.Message.Message()))

	case protoreflect.EnumKind:
		// Render an enum value as a string or integer,
		// depending on whether it corresponds to a known value.
		number := value.Message.Enum()
		if enumValueDescriptor := fieldDescriptor.Enum().Values().ByNumber(number); enumValueDescriptor != nil {
			v = string(enumValueDescriptor.Name())
		} else {
			v = number
		}

	default:
		return formatted.Bold(formatted.Red(formatted.Text("[ Unknown field kind ]")))
	}

	return f.formatJSONValue(v)
}

func (messageJSONFormatter) formatJSONValue(v any) formatted.Node {
	jsonValue, err := json.Marshal(v)
	if err != nil {
		return formatted.Bold(formatted.Red(formatted.Textf("[ %s ]", err)))
	}
	return formatted.Magenta(formatted.Text(string(jsonValue)))
}

func (f *messageJSONFormatter) formatJSONMessage(m model_core.Message[protoreflect.Message, object.LocalReference]) formatted.Node {
	switch v := m.Message.Interface().(type) {
	case *durationpb.Duration:
		if jsonValue, err := protojson.Marshal(v); err == nil {
			return formatted.Magenta(formatted.Text(string(jsonValue)))
		}
	}

	fields := map[string]formatted.Node{}
	m.Message.Range(func(fieldDescriptor protoreflect.FieldDescriptor, value protoreflect.Value) bool {
		var valueNode formatted.Node
		if fieldDescriptor.IsList() {
			// Repeated fields should be rendered as JSON lists.
			list := value.List()
			listLength := list.Len()
			if listLength == 0 {
				valueNode = formatted.Text("[]")
			} else {
				listParts := make([]formatted.Node, 0, 2*listLength+1)
				separator := "["
				for i := 0; i < listLength; i++ {
					listParts = append(
						listParts,
						formatted.Text(separator),
						f.formatJSONField(fieldDescriptor, model_core.Nested(m, list.Get(i))),
					)
					separator = ", "
				}
				valueNode = formatted.Join(append(listParts, formatted.Text("]"))...)
			}
		} else {
			valueNode = f.formatJSONField(fieldDescriptor, model_core.Nested(m, value))
		}
		name := fieldDescriptor.JSONName()
		fields[name] = valueNode
		return true
	})

	// Sort fields by name and join them together in a single JSON object.
	if len(fields) == 0 {
		return formatted.Text("{}")
	}

	messageParts := make([]formatted.Node, 0, 4*len(fields)+1)
	separator := "{"
	for _, key := range slices.Sorted(maps.Keys(fields)) {
		messageParts = append(
			messageParts,
			formatted.Text(separator),
			formatted.Yellow(formatted.Textf("%#v", key)),
			formatted.Text(": "),
			fields[key],
		)
		separator = ", "
	}
	return formatted.Join(append(messageParts, formatted.Text("}"))...)
}

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
	exclusions     *commands_build.SourceExclusions
}

type localCapturableDirectory[TDirectory, TFile model_core.ReferenceMetadata] struct {
	filesystem.DirectoryCloser
	options      *localCapturableDirectoryOptions[TFile]
	relativePath []string
}

func (d *localCapturableDirectory[TDirectory, TFile]) ReadDir() ([]filesystem.FileInfo, error) {
	entries, err := d.DirectoryCloser.ReadDir()
	if err != nil {
		return nil, err
	}
	filtered := entries[:0]
	for _, entry := range entries {
		if !d.options.exclusions.ShouldExclude(d.relativePath, entry) {
			filtered = append(filtered, entry)
		}
	}
	return filtered, nil
}

func (d *localCapturableDirectory[TDirectory, TFile]) EnterCapturableDirectory(name path.Component) (*model_filesystem.CreatedDirectory[TDirectory], model_filesystem.CapturableDirectory[TDirectory, TFile], error) {
	child, err := d.DirectoryCloser.EnterDirectory(name)
	if err != nil {
		return nil, nil, err
	}
	childRelativePath := append(slices.Clone(d.relativePath), name.String())
	return nil, &localCapturableDirectory[TDirectory, TFile]{
		DirectoryCloser: child,
		options:         d.options,
		relativePath:    childRelativePath,
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
func DoQuery(args *arguments.QueryCommand, workspacePath path.Parser) {
	logger := logging.NewLoggerFromFlags(&args.CommonFlags)
	commands.ValidateInsideWorkspace(logger, "query", workspacePath)

	if len(args.Arguments) == 0 {
		logger.Fatal(formatted.Text("A query expression must be provided"))
	}
	if args.QueryFlags.Output != arguments.QueryOutput_LabelKind && args.QueryFlags.Output != arguments.QueryOutput_Label {
		logger.Fatal(formatted.Text("Invalid value for --output"))
	}

	remoteCacheClient, err := newGRPCClient(args.CommonFlags.RemoteCache, &args.CommonFlags)
	if err != nil {
		logger.Fatal(formatted.Textf("Failed to create gRPC client for --remote_cache=%#v: %s", args.CommonFlags.RemoteCache, err))
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

	workspacePathStr, err := commands_build.ResolveToAbsoluteString(workspacePath)
	if err != nil {
		logger.Fatal(formatted.Textf("Failed to resolve workspace path: %s", err))
	}
	workspaceBaseName := commands_build.BaseName(workspacePathStr)
	if args.CommonFlags.StrictVendor && args.CommonFlags.VendorDir == "" {
		logger.Fatal(formatted.Text("--strict_vendor requires --vendor_dir"))
	}
	strictVendorMode := args.CommonFlags.StrictVendor ||
		(args.CommonFlags.VendorDir != "" && args.CommonFlags.StrictModuleResolution && len(args.CommonFlags.Registry) == 0)

	registryURLs := append([]string(nil), args.CommonFlags.Registry...)
	if len(registryURLs) == 0 && (args.CommonFlags.VendorDir != "" || !args.CommonFlags.StrictModuleResolution) {
		registryURLs = []string{"https://bcr.bazel.build/"}
	}
	var vendorDirectory *commands_build.VendorDirectory
	if args.CommonFlags.VendorDir != "" {
		if args.CommonFlags.LockfileMode != arguments.LockfileMode_Error {
			logger.Fatal(formatted.Text("--vendor_dir requires --lockfile_mode=error, because update and refresh semantics would require remote registry access"))
		}
		for index, registryURL := range registryURLs {
			normalizedRegistryURL, err := commands_build.NormalizeVendorRegistryURL(registryURL)
			if err != nil {
				logger.Fatal(formatted.Textf("Invalid registry for --vendor_dir: %s", err))
			}
			registryURLs[index] = normalizedRegistryURL
		}
		vendorDirectory, err = commands_build.ScanVendorDirectory(
			workspacePath,
			args.CommonFlags.VendorDir,
			registryURLs,
			/* requireLockfile = */ true,
			/* strictVendorMode = */ strictVendorMode,
		)
		if err != nil {
			logger.Fatal(formatted.Textf("Invalid --vendor_dir=%q: %s", args.CommonFlags.VendorDir, err))
		}
	}

	// Augment results with modules provided to --override_module.
	for _, overrideModule := range args.CommonFlags.OverrideModule {
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
	encryptionKeyBytes, err := base64.StdEncoding.DecodeString(args.CommonFlags.RemoteEncryptionKey)
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

	var vendoredRepos []commands_build.VendoredRepo
	var vendoredRegistries []commands_build.VendoredRegistry
	if vendorDirectory != nil {
		vendoredRepos = vendorDirectory.Repos
		vendoredRegistries = vendorDirectory.Registries
	}
	var chunkEncoders []*model_encoding_pb.BinaryEncoder
	if args.CommonFlags.RemoteCacheCompression {
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

	moduleDepths := make(map[label.Module]int, len(moduleNames))
	for _, moduleName := range moduleNames {
		modulePathStr, err := commands_build.ResolveToAbsoluteString(modulePaths[moduleName])
		if err != nil {
			logger.Fatal(formatted.Textf("Failed to resolve path of module %#v: %s", moduleName.String(), err))
		}
		depth, withinWorkspace := commands_build.RelativeDepth(workspacePathStr, modulePathStr)
		if !withinWorkspace {
			depth = 0
		}
		moduleDepths[moduleName] = depth
	}
	vendoredRepoDepths := make([]int, len(vendoredRepos))
	for i, vendoredRepo := range vendoredRepos {
		depth, withinWorkspace := commands_build.RelativeDepth(workspacePathStr, vendoredRepo.RootPath)
		if !withinWorkspace {
			depth = 0
		}
		vendoredRepoDepths[i] = depth
	}
	vendoredRegistryDepths := make([]int, len(vendoredRegistries))
	for i, vendoredRegistry := range vendoredRegistries {
		depth, withinWorkspace := commands_build.RelativeDepth(workspacePathStr, vendoredRegistry.RootPath)
		if !withinWorkspace {
			depth = 0
		}
		vendoredRegistryDepths[i] = depth
	}

	logger.Info(formatted.Text("Scanning module sources"))
	ctx := context.Background()
	group, groupCtx := errgroup.WithContext(ctx)
	moduleRootDirectories := make([]model_filesystem.CapturedDirectory, len(moduleNames))
	createdModuleRootDirectories := make([]model_filesystem.CreatedDirectory[model_core.CreatedObjectTree], len(moduleNames))
	vendoredRepoRootDirectories := make([]model_filesystem.CapturedDirectory, len(vendoredRepos))
	createdVendoredRepoRootDirectories := make([]model_filesystem.CreatedDirectory[model_core.CreatedObjectTree], len(vendoredRepos))
	vendoredRegistryRootDirectories := make([]model_filesystem.CapturedDirectory, len(vendoredRegistries))
	createdVendoredRegistryRootDirectories := make([]model_filesystem.CreatedDirectory[model_core.CreatedObjectTree], len(vendoredRegistries))
	createMerkleTreesConcurrency := semaphore.NewWeighted(int64(runtime.NumCPU()))
	captureLocalSource := func(
		sourceName string,
		sourcePath path.Parser,
		isRootModule bool,
		excludedRootPath string,
		respectGitignore bool,
		requireGitignore bool,
		capturedDirectory *model_filesystem.CapturedDirectory,
		createdDirectory *model_filesystem.CreatedDirectory[model_core.CreatedObjectTree],
	) error {
		sourceDirectory, err := filesystem.NewLocalDirectory(sourcePath)
		if err != nil {
			return util.StatusWrapf(err, "Failed to open root directory of %s", sourceName)
		}
		sourcePathStr, err := commands_build.ResolveToAbsoluteString(sourcePath)
		if err != nil {
			sourceDirectory.Close()
			return util.StatusWrapf(err, "Failed to resolve path of %s", sourceName)
		}
		exclusions, err := commands_build.NewSourceExclusions(
			logger,
			sourceDirectory,
			sourceName,
			sourcePathStr,
			isRootModule,
			workspaceBaseName,
			respectGitignore,
			requireGitignore,
		)
		if err != nil {
			sourceDirectory.Close()
			return err
		}
		if excludedRootPath != "" {
			if err := exclusions.AddIgnoredRelativePath(excludedRootPath); err != nil {
				sourceDirectory.Close()
				return fmt.Errorf("exclude %q from %s source upload: %w", excludedRootPath, sourceName, err)
			}
		}
		*capturedDirectory = localCapturedDirectory{
			DirectoryCloser: sourceDirectory,
		}
		if err := model_filesystem.CreateDirectoryMerkleTree(
			groupCtx,
			createMerkleTreesConcurrency,
			group,
			directoryParameters,
			&localCapturableDirectory[model_core.CreatedObjectTree, model_core.NoopReferenceMetadata]{
				DirectoryCloser: sourceDirectory,
				options: &localCapturableDirectoryOptions[model_core.NoopReferenceMetadata]{
					fileParameters: fileParameters,
					capturer:       model_filesystem.NewSimpleFileMerkleTreeCapturer(model_core.DiscardingCreatedObjectCapturer),
					exclusions:     exclusions,
				},
			},
			model_filesystem.FileDiscardingDirectoryMerkleTreeCapturer,
			createdDirectory,
		); err != nil {
			return util.StatusWrapf(err, "Failed to create directory Merkle tree for %s", sourceName)
		}
		return nil
	}
	group.Go(func() error {
		for i, moduleName := range moduleNames {
			excludedRootPath := ""
			if moduleName == rootModuleName && vendorDirectory != nil {
				modulePathStr, err := commands_build.ResolveToAbsoluteString(modulePaths[moduleName])
				if err != nil {
					return util.StatusWrapf(err, "Failed to resolve root module path")
				}
				if modulePathStr == workspacePathStr {
					excludedRootPath = vendorDirectory.RootRelativePath
				}
			}
			if err := captureLocalSource(
				fmt.Sprintf("module %#v", moduleName.String()),
				modulePaths[moduleName],
				moduleName == rootModuleName,
				excludedRootPath,
				args.CommonFlags.RespectGitignore,
				moduleName == rootModuleName && args.CommonFlags.RequireGitignore,
				&moduleRootDirectories[i],
				&createdModuleRootDirectories[i],
			); err != nil {
				return err
			}
		}
		for i, vendoredRepo := range vendoredRepos {
			if err := captureLocalSource(
				fmt.Sprintf("vendored repository %q", "@@"+vendoredRepo.CanonicalRepo.String()),
				path.LocalFormat.NewParser(vendoredRepo.RootPath),
				/* isRootModule = */ false,
				/* excludedRootPath = */ "",
				/* respectGitignore = */ false,
				/* requireGitignore = */ false,
				&vendoredRepoRootDirectories[i],
				&createdVendoredRepoRootDirectories[i],
			); err != nil {
				return err
			}
		}
		for i, vendoredRegistry := range vendoredRegistries {
			if err := captureLocalSource(
				fmt.Sprintf("vendored registry %q", vendoredRegistry.URL),
				path.LocalFormat.NewParser(vendoredRegistry.RootPath),
				/* isRootModule = */ false,
				/* excludedRootPath = */ "",
				/* respectGitignore = */ false,
				/* requireGitignore = */ false,
				&vendoredRegistryRootDirectories[i],
				&createdVendoredRegistryRootDirectories[i],
			); err != nil {
				return err
			}
		}
		return nil
	})
	if err := group.Wait(); err != nil {
		logger.Fatal(formatted.Text(err.Error()))
	}

	fetcherPKIXPublicKey, err := base64.StdEncoding.DecodeString(args.CommonFlags.RemoteExecutorFetcherPkixPublicKey)
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
	// Parse the query expression. The client owns the language and
	// the cluster owns the walk, so target patterns cross the boundary
	// unresolved: canonicalizing an apparent pattern needs the repo
	// mapping, which only the cluster has.
	parsedExpression, err := ParseExpression(args.Arguments)
	if err != nil {
		logger.Fatal(formatted.Textf("Invalid query expression: %s", err))
	}
	queryExpression, err := Encode(parsedExpression)
	if err != nil {
		logger.Fatal(formatted.Textf("Invalid query expression: %s", err))
	}

	// Construct a BuildSpecification message that lists all the
	// modules and contains all of the flags needed to resolve
	// packages.
	buildSpecification := model_analysis_pb.BuildSpecification_Value{
		RootModuleName:                         rootModuleName.String(),
		DirectoryCreationParameters:            directoryParametersMessage,
		FileCreationParameters:                 fileParametersMessage,
		IgnoreRootModuleDevDependencies:        args.CommonFlags.IgnoreDevDependency,
		BuiltinsModuleNames:                    args.CommonFlags.BuiltinsModule,
		RepoPlatform:                           args.CommonFlags.RepoPlatform,
		FetchPlatformPkixPublicKey:             fetcherPKIXPublicKey,
		ActionEncoders:                         defaultEncoders,
		RuleImplementationWrapperIdentifier:    args.CommonFlags.RuleImplementationWrapperIdentifier,
		SubruleImplementationWrapperIdentifier: args.CommonFlags.SubruleImplementationWrapperIdentifier,
		ModuleRegistryUrls:                     registryURLs,
		StrictVendorMode:                       strictVendorMode,
	}
	switch args.CommonFlags.LockfileMode {
	case arguments.LockfileMode_Off:
	case arguments.LockfileMode_Update:
		buildSpecification.UseLockfile = &model_analysis_pb.BuildSpecification_Value_UseLockfile{}
	case arguments.LockfileMode_Refresh:
		buildSpecification.UseLockfile = &model_analysis_pb.BuildSpecification_Value_UseLockfile{
			MaximumCacheDuration: &durationpb.Duration{Seconds: 3600},
		}
	case arguments.LockfileMode_Error:
		buildSpecification.UseLockfile = &model_analysis_pb.BuildSpecification_Value_UseLockfile{
			Error: true,
		}
	default:
		panic("unknown lockfile mode")
	}
	buildSpecificationPatcher := model_core.NewReferenceMessagePatcher[dag.ObjectContentsWalker]()

	captureRootDirectory := func(
		sourceName string,
		createdRootDirectory model_filesystem.CreatedDirectory[model_core.CreatedObjectTree],
		capturedRootDirectory model_filesystem.CapturedDirectory,
		sourceDepth int,
	) (*model_filesystem_pb.DirectoryReference, error) {
		switch maximumEscapement := createdRootDirectory.MaximumSymlinkEscapementLevels; {
		case maximumEscapement == nil:
			return nil, fmt.Errorf("%s contains one or more symbolic links whose target cannot be bounded (e.g. an absolute path, or \"..\" following a named path component)", sourceName)
		case maximumEscapement.Value > uint32(sourceDepth):
			return nil, fmt.Errorf("%s contains one or more symbolic links that escape the workspace directory", sourceName)
		case maximumEscapement.Value != 0:
			logger.Warning(formatted.Textf("%s contains one or more symbolic links that escape its own root directory, but remain within the workspace", sourceName))
		}
		createdObject, err := model_core.MarshalAndEncodeDeterministic(
			model_core.ProtoToBinaryMarshaler(createdRootDirectory.Message),
			referenceFormat,
			directoryParameters.GetEncoder(),
		)
		if err != nil {
			return nil, err
		}
		createdObjectTree := model_core.CreatedObjectTree(createdObject.Value)
		decodingParameters := createdObject.GetDecodingParameters()
		return createdRootDirectory.ToDirectoryReference(
			&model_core_pb.DecodableReference{
				Reference: buildSpecificationPatcher.AddReference(
					model_core.MetadataEntry[dag.ObjectContentsWalker]{
						LocalReference: createdObject.Value.GetLocalReference(),
						Metadata: model_filesystem.NewCapturedDirectoryWalker(
							directoryParameters.DirectoryAccessParameters,
							fileParameters,
							capturedRootDirectory,
							&createdObjectTree,
							decodingParameters,
						),
					},
				),
				DecodingParameters: decodingParameters,
			},
		), nil
	}

	for i, moduleName := range moduleNames {
		rootDirectoryReference, err := captureRootDirectory(
			fmt.Sprintf("module %#v", moduleName.String()),
			createdModuleRootDirectories[i],
			moduleRootDirectories[i],
			moduleDepths[moduleName],
		)
		if err != nil {
			logger.Fatal(formatted.Textf("Failed to capture root directory for module %#v: %s", moduleName.String(), err))
		}
		buildSpecification.Modules = append(
			buildSpecification.Modules,
			&model_analysis_pb.BuildSpecification_Value_Module{
				Name:                   moduleName.String(),
				RootDirectoryReference: rootDirectoryReference,
			},
		)
	}
	for i, vendoredRepo := range vendoredRepos {
		rootDirectoryReference, err := captureRootDirectory(
			fmt.Sprintf("vendored repository %q", "@@"+vendoredRepo.CanonicalRepo.String()),
			createdVendoredRepoRootDirectories[i],
			vendoredRepoRootDirectories[i],
			vendoredRepoDepths[i],
		)
		if err != nil {
			logger.Fatal(formatted.Textf("Failed to capture vendored repository %q: %s", "@@"+vendoredRepo.CanonicalRepo.String(), err))
		}
		buildSpecification.VendoredRepos = append(
			buildSpecification.VendoredRepos,
			&model_analysis_pb.BuildSpecification_Value_VendoredRepo{
				CanonicalRepo:          vendoredRepo.CanonicalRepo.String(),
				RootDirectoryReference: rootDirectoryReference,
				Pinned:                 vendoredRepo.Pinned,
			},
		)
	}
	for i, vendoredRegistry := range vendoredRegistries {
		rootDirectoryReference, err := captureRootDirectory(
			fmt.Sprintf("vendored registry %q", vendoredRegistry.URL),
			createdVendoredRegistryRootDirectories[i],
			vendoredRegistryRootDirectories[i],
			vendoredRegistryDepths[i],
		)
		if err != nil {
			logger.Fatal(formatted.Textf("Failed to capture vendored registry %q: %s", vendoredRegistry.URL, err))
		}
		files := make([]*model_analysis_pb.BuildSpecification_Value_VendoredRegistry_File, 0, len(vendoredRegistry.Files))
		for _, file := range vendoredRegistry.Files {
			files = append(files, &model_analysis_pb.BuildSpecification_Value_VendoredRegistry_File{
				Path:   file.Path,
				Sha256: file.SHA256,
			})
		}
		buildSpecification.VendoredRegistries = append(
			buildSpecification.VendoredRegistries,
			&model_analysis_pb.BuildSpecification_Value_VendoredRegistry{
				Url:                    vendoredRegistry.URL,
				RootDirectoryReference: rootDirectoryReference,
				Files:                  files,
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
	queryResultKey := model_core.NewSimplePatchedMessage[dag.ObjectContentsWalker](
		&model_analysis_pb.QueryResult_Key{
			Expression: queryExpression,
		},
	)
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
			model_core.NewSimplePatchedMessage[dag.ObjectContentsWalker](
				&model_analysis_pb.QueryResult_Key{
					Expression: queryExpression,
				},
			),
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
	instanceName, err := object.NewInstanceName(args.CommonFlags.RemoteInstanceName)
	if err != nil {
		logger.Fatal(formatted.Textf("Invalid --remote_instance_name=%#v: %s", args.CommonFlags.RemoteInstanceName, err))
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

	clientPrivateKeyData, err := os.ReadFile(args.CommonFlags.RemoteExecutorClientPrivateKey)
	if err != nil {
		logger.Fatal(formatted.Textf("Failed to read --remote_executor_client_private_key=%#v: %s", args.CommonFlags.RemoteExecutorClientPrivateKey, err))
	}
	clientPrivateKey, err := crypto.ParsePEMWithPKCS8ECDHPrivateKey(clientPrivateKeyData)
	if err != nil {
		logger.Fatal(formatted.Textf("Failed to parse --remote_executor_client_private_key=%#v: %s", args.CommonFlags.RemoteExecutorClientPrivateKey, err))
	}

	clientCertificateChainData, err := os.ReadFile(args.CommonFlags.RemoteExecutorClientCertificateChain)
	if err != nil {
		logger.Fatal(formatted.Textf("Failed to read --remote_executor_client_certificate_chain=%#v: %s", args.CommonFlags.RemoteExecutorClientCertificateChain, err))
	}
	clientCertificateChain, err := crypto.ParsePEMWithCertificateChain(clientCertificateChainData)
	if err != nil {
		logger.Fatal(formatted.Textf("Failed to parse --remote_executor_client_certificate_chain=%#v: %s", args.CommonFlags.RemoteExecutorClientCertificateChain, err))
	}

	remoteExecutorClient, err := newGRPCClient(args.CommonFlags.RemoteExecutor, &args.CommonFlags)
	if err != nil {
		logger.Fatal(formatted.Textf("Failed to create gRPC client for --remote_executor=%#v: %s", args.CommonFlags.RemoteExecutor, err))
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

	builderPKIXPublicKey, err := base64.StdEncoding.DecodeString(args.CommonFlags.RemoteExecutorBuilderPkixPublicKey)
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
	queryResultValue, ok := unmarshaledValue.Message.(*model_analysis_pb.QueryResult_Value)
	if !ok {
		logger.Fatal(formatted.Text("The query yielded a value of an unexpected type"))
	}

	// The cluster returns the targets already sorted by label, and
	// carries each target's kind so that --output=label_kind needs no
	// second round trip.
	for _, target := range queryResultValue.Targets {
		if args.QueryFlags.Output == arguments.QueryOutput_LabelKind {
			fmt.Printf("%s %s\n", target.Kind, target.Label)
		} else {
			fmt.Println(target.Label)
		}
	}
}

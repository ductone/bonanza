package analysis

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	pathpkg "path"
	"sort"
	"strings"

	model_core "bonanza.build/pkg/model/core"
	model_filesystem "bonanza.build/pkg/model/filesystem"
	model_analysis_pb "bonanza.build/pkg/proto/model/analysis"
	model_filesystem_pb "bonanza.build/pkg/proto/model/filesystem"
	"bonanza.build/pkg/storage/object"

	"github.com/buildbarn/bb-storage/pkg/filesystem/path"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func findVendoredRegistry(buildSpecification *model_analysis_pb.BuildSpecification_Value, registryURL string) (*model_analysis_pb.BuildSpecification_Value_VendoredRegistry, bool) {
	registries := buildSpecification.VendoredRegistries
	index, found := sort.Find(len(registries), func(index int) int {
		return strings.Compare(registryURL, registries[index].Url)
	})
	if !found {
		return nil, false
	}
	return registries[index], true
}

func findVendoredRegistryFile(registry *model_analysis_pb.BuildSpecification_Value_VendoredRegistry, relativePath string) (*model_analysis_pb.BuildSpecification_Value_VendoredRegistry_File, bool) {
	files := registry.Files
	index, found := sort.Find(len(files), func(index int) int {
		return strings.Compare(relativePath, files[index].Path)
	})
	if !found {
		return nil, false
	}
	return files[index], true
}

// readVerifiedVendoredRegistryFile reads a registry file only if it was listed
// in the lockfile-derived allowlist uploaded by the client. The boolean result
// reports whether the path was allowlisted; a true result with an unset Message
// means the mirror became incomplete after validation.
func readVerifiedVendoredRegistryFile[TReference object.BasicReference](
	ctx context.Context,
	buildSpecification model_core.Message[*model_analysis_pb.BuildSpecification_Value, TReference],
	registry *model_analysis_pb.BuildSpecification_Value_VendoredRegistry,
	directoryReaders *DirectoryReaders[TReference],
	relativePath string,
) (model_core.Message[*model_filesystem_pb.FileContents, TReference], bool, error) {
	if registry == nil || registry.RootDirectoryReference == nil {
		return model_core.Message[*model_filesystem_pb.FileContents, TReference]{}, false, errors.New("vendored registry does not have a root directory")
	}
	verifiedFile, verified := findVendoredRegistryFile(registry, relativePath)
	if !verified {
		return model_core.Message[*model_filesystem_pb.FileContents, TReference]{}, false, nil
	}
	if len(verifiedFile.Sha256) != sha256.Size {
		return model_core.Message[*model_filesystem_pb.FileContents, TReference]{}, true, fmt.Errorf("vendored registry file %q has an invalid SHA-256", relativePath)
	}
	cleanRelativePath := pathpkg.Clean(relativePath)
	if cleanRelativePath != relativePath || cleanRelativePath == "." || pathpkg.IsAbs(cleanRelativePath) || strings.HasPrefix(cleanRelativePath, "../") {
		return model_core.Message[*model_filesystem_pb.FileContents, TReference]{}, true, fmt.Errorf("vendored registry file %q is not registry-relative", relativePath)
	}

	rootDirectory := model_core.Nested(
		buildSpecification,
		&model_filesystem_pb.Directory{
			Contents: &model_filesystem_pb.Directory_ContentsExternal{
				ContentsExternal: registry.RootDirectoryReference,
			},
		},
	)
	walker := model_filesystem.NewDirectoryComponentWalker(
		ctx,
		directoryReaders.DirectoryContents,
		directoryReaders.Leaves,
		func() (path.ComponentWalker, error) {
			return nil, errors.New("vendored registry path escapes its root directory")
		},
		rootDirectory,
		nil,
	)
	if err := path.Resolve(
		path.UNIXFormat.NewParser(relativePath),
		path.NewLoopDetectingScopeWalker(path.NewRelativeScopeWalker(walker)),
	); err != nil {
		if status.Code(err) == codes.NotFound {
			return model_core.Message[*model_filesystem_pb.FileContents, TReference]{}, true, nil
		}
		return model_core.Message[*model_filesystem_pb.FileContents, TReference]{}, true, fmt.Errorf("resolve vendored registry file %q: %w", relativePath, err)
	}
	properties := walker.GetCurrentFileProperties()
	if !properties.IsSet() {
		return model_core.Message[*model_filesystem_pb.FileContents, TReference]{}, true, fmt.Errorf("vendored registry path %q resolves to a directory", relativePath)
	}
	if properties.Message.Contents == nil {
		return model_core.Message[*model_filesystem_pb.FileContents, TReference]{}, true, fmt.Errorf("vendored registry file %q is empty", relativePath)
	}
	return model_core.Nested(properties, properties.Message.Contents), true, nil
}

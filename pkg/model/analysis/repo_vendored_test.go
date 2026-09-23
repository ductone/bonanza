package analysis

import (
	"context"
	"testing"

	model_core "bonanza.build/pkg/model/core"
	model_analysis_pb "bonanza.build/pkg/proto/model/analysis"
	model_filesystem_pb "bonanza.build/pkg/proto/model/filesystem"
	"bonanza.build/pkg/storage/object"

	"github.com/stretchr/testify/require"
)

// vendoredRepoTestEnvironment embeds the full generated environment contract,
// but only provides the dependency that the vendored fast path is allowed to
// read. If ComputeRepoValue reaches a module extension, registry, or fetch
// dependency, the nil embedded environment panics and this test fails.
type vendoredRepoTestEnvironment struct {
	RepoEnvironment[object.LocalReference, model_core.NoopReferenceMetadata]

	buildSpecification model_core.Message[*model_analysis_pb.BuildSpecification_Value, object.LocalReference]
}

func (e vendoredRepoTestEnvironment) GetBuildSpecificationValue(*model_analysis_pb.BuildSpecification_Key) model_core.Message[*model_analysis_pb.BuildSpecification_Value, object.LocalReference] {
	return e.buildSpecification
}

func (vendoredRepoTestEnvironment) CaptureExistingObject(object.LocalReference) model_core.NoopReferenceMetadata {
	return model_core.NoopReferenceMetadata{}
}

func TestComputeRepoValueUsesVendoredRepoBeforeRemoteResolution(t *testing.T) {
	vendoredRoot := &model_filesystem_pb.DirectoryReference{}
	environment := vendoredRepoTestEnvironment{
		buildSpecification: model_core.NewSimpleMessage[object.LocalReference](
			&model_analysis_pb.BuildSpecification_Value{
				VendoredRepos: []*model_analysis_pb.BuildSpecification_Value_VendoredRepo{{
					CanonicalRepo:          "rules_go++go_sdk+c1_go_sdk",
					RootDirectoryReference: vendoredRoot,
				}},
			},
		),
	}

	computer := baseComputer[object.LocalReference, model_core.NoopReferenceMetadata]{}
	value, err := computer.ComputeRepoValue(
		context.Background(),
		&model_analysis_pb.Repo_Key{CanonicalRepo: "rules_go++go_sdk+c1_go_sdk"},
		environment,
	)
	require.NoError(t, err)
	require.Equal(t, vendoredRoot, value.Message.RootDirectoryReference)
}

func TestComputeRepoValuePrefersLocalModuleOverVendoredModule(t *testing.T) {
	localRoot := &model_filesystem_pb.DirectoryReference{}
	vendoredRoot := &model_filesystem_pb.DirectoryReference{}
	environment := vendoredRepoTestEnvironment{
		buildSpecification: model_core.NewSimpleMessage[object.LocalReference](
			&model_analysis_pb.BuildSpecification_Value{
				Modules: []*model_analysis_pb.BuildSpecification_Value_Module{{
					Name:                   "rules_go",
					RootDirectoryReference: localRoot,
				}},
				VendoredRepos: []*model_analysis_pb.BuildSpecification_Value_VendoredRepo{{
					CanonicalRepo:          "rules_go+",
					RootDirectoryReference: vendoredRoot,
				}},
			},
		),
	}

	computer := baseComputer[object.LocalReference, model_core.NoopReferenceMetadata]{}

	value, err := computer.ComputeRepoValue(
		context.Background(),
		&model_analysis_pb.Repo_Key{CanonicalRepo: "rules_go+"},
		environment,
	)
	require.NoError(t, err)
	require.Equal(t, localRoot, value.Message.RootDirectoryReference)
}

func TestComputeRepoValueRejectsRepositoryMissingFromStrictVendorSnapshot(t *testing.T) {
	environment := vendoredRepoTestEnvironment{
		buildSpecification: model_core.NewSimpleMessage[object.LocalReference](
			&model_analysis_pb.BuildSpecification_Value{
				StrictVendorMode: true,
			},
		),
	}

	computer := baseComputer[object.LocalReference, model_core.NoopReferenceMetadata]{}
	_, err := computer.ComputeRepoValue(
		context.Background(),
		&model_analysis_pb.Repo_Key{CanonicalRepo: "rules_go+"},
		environment,
	)
	require.ErrorContains(t, err, "not present in the validated vendor snapshot")
}

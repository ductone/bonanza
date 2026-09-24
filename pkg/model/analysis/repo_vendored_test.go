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
	localRoot := &model_filesystem_pb.DirectoryReference{DirectoriesCount: 1}
	vendoredRoot := &model_filesystem_pb.DirectoryReference{DirectoriesCount: 2}
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

func TestPinnedRepoOverridesLocalModuleSources(t *testing.T) {
	vendoredRoot := &model_filesystem_pb.DirectoryReference{DirectoriesCount: 2}
	localRoot := &model_filesystem_pb.DirectoryReference{DirectoriesCount: 1}
	for _, canonicalRepo := range []string{
		"rules_go+",
		"rules_go++go_sdk+c1_go_sdk",
	} {
		environment := vendoredRepoTestEnvironment{
			buildSpecification: model_core.NewSimpleMessage[object.LocalReference](
				&model_analysis_pb.BuildSpecification_Value{
					StrictVendorMode: true,
					Modules: []*model_analysis_pb.BuildSpecification_Value_Module{{
						Name:                   "rules_go",
						RootDirectoryReference: localRoot,
					}},
					VendoredRepos: []*model_analysis_pb.BuildSpecification_Value_VendoredRepo{{
						CanonicalRepo:          canonicalRepo,
						RootDirectoryReference: vendoredRoot,
						Pinned:                 true,
					}},
				},
			),
		}
		computer := baseComputer[object.LocalReference, model_core.NoopReferenceMetadata]{}
		value, err := computer.ComputeRepoValue(
			context.Background(),
			&model_analysis_pb.Repo_Key{CanonicalRepo: canonicalRepo},
			environment,
		)
		require.NoError(t, err)
		require.Equal(t, vendoredRoot, value.Message.RootDirectoryReference)
	}
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

func TestStrictVendorRejectsUnvendoredLocalModuleExtensionRepo(t *testing.T) {
	environment := vendoredRepoTestEnvironment{
		buildSpecification: model_core.NewSimpleMessage[object.LocalReference](
			&model_analysis_pb.BuildSpecification_Value{
				StrictVendorMode: true,
				Modules: []*model_analysis_pb.BuildSpecification_Value_Module{{
					Name: "c1",
				}},
			},
		),
	}
	computer := baseComputer[object.LocalReference, model_core.NoopReferenceMetadata]{}
	_, err := computer.ComputeRepoValue(
		context.Background(),
		&model_analysis_pb.Repo_Key{CanonicalRepo: "c1++_repo_rules+c1_squire_bazel_linux_arm64"},
		environment,
	)
	require.ErrorContains(t, err, "strict vendor mode")
}

// A vendored generated repository can load labels from its parent module
// without evaluating the producing extension (which may need network or a
// repository worker that an offline query does not have).
type vendoredRepoMappingTestEnvironment struct {
	CanonicalRepoNameEnvironment[object.LocalReference, model_core.NoopReferenceMetadata]

	buildSpecification model_core.Message[*model_analysis_pb.BuildSpecification_Value, object.LocalReference]
	moduleMapping      model_core.Message[*model_analysis_pb.ModuleRepoMapping_Value, object.LocalReference]
	extensionRepoNames model_core.Message[*model_analysis_pb.ModuleExtensionRepoNames_Value, object.LocalReference]
}

func (e vendoredRepoMappingTestEnvironment) GetBuildSpecificationValue(*model_analysis_pb.BuildSpecification_Key) model_core.Message[*model_analysis_pb.BuildSpecification_Value, object.LocalReference] {
	return e.buildSpecification
}

func (e vendoredRepoMappingTestEnvironment) GetModuleRepoMappingValue(*model_analysis_pb.ModuleRepoMapping_Key) model_core.Message[*model_analysis_pb.ModuleRepoMapping_Value, object.LocalReference] {
	return e.moduleMapping
}

func (e vendoredRepoMappingTestEnvironment) GetModuleExtensionRepoNamesValue(*model_analysis_pb.ModuleExtensionRepoNames_Key) model_core.Message[*model_analysis_pb.ModuleExtensionRepoNames_Value, object.LocalReference] {
	return e.extensionRepoNames
}

func TestVendoredGeneratedRepoResolvesAliasesWithoutEvaluatingExtension(t *testing.T) {
	environment := vendoredRepoMappingTestEnvironment{
		buildSpecification: model_core.NewSimpleMessage[object.LocalReference](
			&model_analysis_pb.BuildSpecification_Value{
				StrictVendorMode: true,
				VendoredRepos: []*model_analysis_pb.BuildSpecification_Value_VendoredRepo{{
					CanonicalRepo: "rules_go++go_sdk+c1_go_sdk",
				}},
			},
		),
		moduleMapping: model_core.NewSimpleMessage[object.LocalReference](
			&model_analysis_pb.ModuleRepoMapping_Value{
				Mappings: []*model_analysis_pb.ModuleRepoMapping_Value_Mapping{{
					FromApparentRepo: "io_bazel_rules_go",
					ToCanonicalRepo:  "rules_go+",
				}},
			},
		),
	}
	computer := baseComputer[object.LocalReference, model_core.NoopReferenceMetadata]{}
	for apparentRepo, expected := range map[string]string{
		"c1_go_sdk":         "rules_go++go_sdk+c1_go_sdk",
		"io_bazel_rules_go": "rules_go+",
		"unvendored_sdk":    "",
	} {
		value, err := computer.ComputeCanonicalRepoNameValue(
			context.Background(),
			&model_analysis_pb.CanonicalRepoName_Key{
				FromCanonicalRepo: "rules_go++go_sdk+c1_go_sdk",
				ToApparentRepo:    apparentRepo,
			},
			environment,
		)
		require.NoError(t, err)
		require.Equal(t, expected, value.Message.ToCanonicalRepo)
	}
}

func TestUnvendoredGeneratedRepoRetainsExtensionFallback(t *testing.T) {
	environment := vendoredRepoMappingTestEnvironment{
		buildSpecification: model_core.NewSimpleMessage[object.LocalReference](
			&model_analysis_pb.BuildSpecification_Value{},
		),
		extensionRepoNames: model_core.NewSimpleMessage[object.LocalReference](
			&model_analysis_pb.ModuleExtensionRepoNames_Value{RepoNames: []string{"fetched_sdk"}},
		),
	}
	computer := baseComputer[object.LocalReference, model_core.NoopReferenceMetadata]{}
	value, err := computer.ComputeCanonicalRepoNameValue(
		context.Background(),
		&model_analysis_pb.CanonicalRepoName_Key{
			FromCanonicalRepo: "rules_go++go_sdk+c1_go_sdk",
			ToApparentRepo:    "fetched_sdk",
		},
		environment,
	)
	require.NoError(t, err)
	require.Equal(t, "rules_go++go_sdk+fetched_sdk", value.Message.ToCanonicalRepo)
}

func TestLocalModuleExtensionDoesNotReuseVendoredAlias(t *testing.T) {
	environment := vendoredRepoMappingTestEnvironment{
		buildSpecification: model_core.NewSimpleMessage[object.LocalReference](
			&model_analysis_pb.BuildSpecification_Value{
				Modules: []*model_analysis_pb.BuildSpecification_Value_Module{{
					Name: "rules_go",
				}},
				VendoredRepos: []*model_analysis_pb.BuildSpecification_Value_VendoredRepo{{
					CanonicalRepo: "rules_go++go_sdk+c1_go_sdk",
				}},
			},
		),
		extensionRepoNames: model_core.NewSimpleMessage[object.LocalReference](
			&model_analysis_pb.ModuleExtensionRepoNames_Value{RepoNames: []string{"local_sdk"}},
		),
		moduleMapping: model_core.NewSimpleMessage[object.LocalReference](
			&model_analysis_pb.ModuleRepoMapping_Value{},
		),
	}
	computer := baseComputer[object.LocalReference, model_core.NoopReferenceMetadata]{}
	value, err := computer.ComputeCanonicalRepoNameValue(
		context.Background(),
		&model_analysis_pb.CanonicalRepoName_Key{
			FromCanonicalRepo: "rules_go++go_sdk+c1_go_sdk",
			ToApparentRepo:    "c1_go_sdk",
		},
		environment,
	)
	require.NoError(t, err)
	require.Empty(t, value.Message.ToCanonicalRepo)
}

func TestPinnedLocalExtensionRepoResolvesItsAliasWithoutEvaluation(t *testing.T) {
	environment := vendoredRepoMappingTestEnvironment{
		buildSpecification: model_core.NewSimpleMessage[object.LocalReference](
			&model_analysis_pb.BuildSpecification_Value{
				StrictVendorMode: true,
				Modules: []*model_analysis_pb.BuildSpecification_Value_Module{{
					Name: "rules_go",
				}},
				VendoredRepos: []*model_analysis_pb.BuildSpecification_Value_VendoredRepo{{
					CanonicalRepo: "rules_go++go_sdk+c1_go_sdk",
					Pinned:        true,
				}},
			},
		),
	}
	computer := baseComputer[object.LocalReference, model_core.NoopReferenceMetadata]{}
	value, err := computer.ComputeCanonicalRepoNameValue(
		context.Background(),
		&model_analysis_pb.CanonicalRepoName_Key{
			FromCanonicalRepo: "rules_go++go_sdk+c1_go_sdk",
			ToApparentRepo:    "c1_go_sdk",
		},
		environment,
	)
	require.NoError(t, err)
	require.Equal(t, "rules_go++go_sdk+c1_go_sdk", value.Message.ToCanonicalRepo)
}

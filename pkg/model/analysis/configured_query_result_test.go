package analysis

import (
	"testing"

	model_analysis_pb "bonanza.build/pkg/proto/model/analysis"
	model_starlark_pb "bonanza.build/pkg/proto/model/starlark"

	"github.com/stretchr/testify/require"
)

func TestConfiguredQueryRejectsLoadingPhaseDependencyWalks(t *testing.T) {
	selection := &model_analysis_pb.QueryExpression{
		Expression: &model_analysis_pb.QueryExpression_Set_{Set: &model_analysis_pb.QueryExpression_Set{
			Patterns: []string{"//cmd/example:example"},
		}},
	}
	require.NoError(t, validateConfiguredQueryExpression(selection))
	require.NoError(t, validateConfiguredQueryExpression(&model_analysis_pb.QueryExpression{
		Expression: &model_analysis_pb.QueryExpression_Kind_{Kind: &model_analysis_pb.QueryExpression_Kind{
			Pattern: "go_binary rule", Targets: selection,
		}},
	}))
	require.ErrorContains(t, validateConfiguredQueryExpression(&model_analysis_pb.QueryExpression{
		Expression: &model_analysis_pb.QueryExpression_Rdeps_{Rdeps: &model_analysis_pb.QueryExpression_Rdeps{
			Universe: selection, Targets: selection,
		}},
	}), "not supported")
}

func TestConfiguredQueryClassifiesC1SourceAndBinary(t *testing.T) {
	source := &model_starlark_pb.File{}
	path, location := classifyConfiguredQueryFile("c1+", "external/c1+/bazel/python/check.py", source)
	require.Equal(t, configuredQueryRootSource, location)
	require.Equal(t, "bazel/python/check.py", path)

	generated := &model_starlark_pb.File{Owner: &model_starlark_pb.File_Owner{}}
	path, location = classifyConfiguredQueryFile("c1+", "bazel-out/config/bin/external/c1+/cmd/example/example", generated)
	require.Equal(t, configuredQueryGenerated, location)
	require.Equal(t, "bazel-out/config/bin/external/c1+/cmd/example/example", path)

	path, location = classifyConfiguredQueryFile("c1+", "external/rules_python+/tool.py", source)
	require.Equal(t, configuredQueryExternalSource, location)
	require.Equal(t, "external/rules_python+/tool.py", path)
}

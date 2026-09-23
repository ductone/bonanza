package analysis

import (
	"testing"

	model_analysis_pb "bonanza.build/pkg/proto/model/analysis"

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

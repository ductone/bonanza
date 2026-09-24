package analysis

import (
	"testing"

	model_analysis_pb "bonanza.build/pkg/proto/model/analysis"
	"github.com/stretchr/testify/require"
)

func TestRepositoryEnvironmentOverridesPlatform(t *testing.T) {
	platform := []*model_analysis_pb.RegisteredRepoPlatform_Value_EnvironmentVariable{
		{Name: "PRESERVED", Value: "platform"},
		{Name: "OVERRIDDEN", Value: "old"},
		{Name: "REMOVED", Value: "old"},
	}
	got := mergeRepositoryEnvironment(platform, []*model_analysis_pb.BuildSpecification_Value_EnvironmentOverride{
		{Name: "OVERRIDDEN", Value: "new"},
		{Name: "REMOVED", Unset: true},
		{Name: "ADDED", Value: ""},
	})
	require.Equal(t, []*model_analysis_pb.RegisteredRepoPlatform_Value_EnvironmentVariable{
		{Name: "ADDED", Value: ""},
		{Name: "OVERRIDDEN", Value: "new"},
		{Name: "PRESERVED", Value: "platform"},
	}, got)
	require.Equal(t, "old", platform[1].Value)
}

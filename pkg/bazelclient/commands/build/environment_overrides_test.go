package build

import (
	"testing"

	model_analysis_pb "bonanza.build/pkg/proto/model/analysis"

	"github.com/stretchr/testify/require"
)

func TestResolveEnvironmentOverridesPrecedence(t *testing.T) {
	client := map[string]string{"DO_NOT_TRACK": "client", "EMPTY": "inherited"}
	lookup := func(name string) (string, bool) {
		value, ok := client[name]
		return value, ok
	}
	got, err := resolveEnvironmentOverrides([]string{
		"DO_NOT_TRACK", "EMPTY=", "UNSET", "DO_NOT_TRACK=1", "EMPTY",
	}, lookup)
	require.NoError(t, err)
	require.Equal(t, []*model_analysis_pb.BuildSpecification_Value_EnvironmentOverride{
		{Name: "DO_NOT_TRACK", Value: "1"},
		{Name: "EMPTY", Value: "inherited"},
		{Name: "UNSET", Unset: true},
	}, got)
}

func TestResolveEnvironmentOverridesRejectsInvalidNames(t *testing.T) {
	for _, option := range []string{"=value", "9BAD=value", "BAD-NAME=value", "GOOD=embedded\x00nul"} {
		_, err := resolveEnvironmentOverrides([]string{option}, func(string) (string, bool) { return "", false })
		require.Error(t, err, option)
	}
}

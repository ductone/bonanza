package cquery

import (
	"path/filepath"
	"testing"

	model_analysis_pb "bonanza.build/pkg/proto/model/analysis"

	"github.com/stretchr/testify/require"
)

func TestOutputFilesUsesMaterializedLayout(t *testing.T) {
	root := t.TempDir()
	file := "bazel-out/config/bin/external/c1+/cmd/example/example"
	files, err := outputFiles(root, []*model_analysis_pb.ConfiguredQueryResult_Value_Target{
		{Label: "@@c1+//cmd/example:example", Files: []string{file}},
		{Label: "@@c1+//cmd/example:alias", Files: []string{file}},
	})
	require.NoError(t, err)
	require.Equal(t, []string{filepath.Join(root, filepath.FromSlash(file))}, files)
}

func TestOutputFilesRejectsPathOutsideBuildRoot(t *testing.T) {
	_, err := outputFiles(t.TempDir(), []*model_analysis_pb.ConfiguredQueryResult_Value_Target{
		{Label: "@@c1+//cmd/example:example", Files: []string{"../secret"}},
	})
	require.ErrorContains(t, err, "invalid output path")
}

package build

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResolveTargetPatterns(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "targets")
	require.NoError(t, os.WriteFile(filename, []byte("//pkg/uid:uid\n\n  -//pkg/uid:uid_test\r\n"), 0o600))

	patterns, err := ResolveTargetPatterns(nil, filename)
	require.NoError(t, err)
	require.Equal(t, []string{"//pkg/uid:uid", "-//pkg/uid:uid_test"}, patterns)

	_, err = ResolveTargetPatterns([]string{"//other:target"}, filename)
	require.ErrorContains(t, err, "cannot be combined")

	_, err = ResolveTargetPatterns(nil, filename+".missing")
	require.ErrorContains(t, err, "read --target_pattern_file")
}

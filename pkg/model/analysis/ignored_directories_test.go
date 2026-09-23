package analysis

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIgnoredDirectoriesHonorRepoRelativeGlobsAndAncestors(t *testing.T) {
	ignored, err := newIgnoredDirectories([]string{
		".git",
		"frontend/node_modules",
		"**/node_modules",
		"local_vendor/**/vendor",
	})
	require.NoError(t, err)
	for _, directory := range []string{
		".git",
		".git/objects",
		"node_modules",
		"pkg/ui/node_modules",
		"pkg/ui/node_modules/dependency",
		"local_vendor/connector/vendor",
		"local_vendor/connector/vendor/module",
	} {
		require.Truef(t, ignored.contains(directory), "%q should be ignored", directory)
	}
	for _, directory := range []string{"", "pkg/uid", "local_vendor/connector", "other/vendor"} {
		require.Falsef(t, ignored.contains(directory), "%q should remain discoverable", directory)
	}
}

package query

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	commands_build "bonanza.build/pkg/bazelclient/commands/build"
	"bonanza.build/pkg/bazelclient/formatted"
	"bonanza.build/pkg/bazelclient/logging"
	model_core "bonanza.build/pkg/model/core"

	"github.com/buildbarn/bb-storage/pkg/filesystem"
	"github.com/buildbarn/bb-storage/pkg/filesystem/path"
	"github.com/stretchr/testify/require"
)

func TestQueryUploadExcludesIgnoredLocalState(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, exec.Command("git", "-C", root, "init", "-q").Run())
	require.NoError(t, os.WriteFile(filepath.Join(root, ".gitignore"), []byte(".dev/\nnested/private\n"), 0o644))
	require.NoError(t, os.Mkdir(filepath.Join(root, ".dev"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".dev", "secret"), []byte("private"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "BUILD.bazel"), []byte("exports_files([])"), 0o644))
	require.NoError(t, os.Mkdir(filepath.Join(root, "nested"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "nested", "private"), []byte("private"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "nested", "public.go"), []byte("package example"), 0o644))
	d, err := filesystem.NewLocalDirectory(path.LocalFormat.NewParser(root))
	require.NoError(t, err)
	defer d.Close()
	exclusions, err := commands_build.NewSourceExclusions(
		logging.NewConsoleLogger(io.Discard, formatted.WritePlainText), d,
		"example", root, true, "example", true, true,
	)
	require.NoError(t, err)
	source := &localCapturableDirectory[model_core.CreatedObjectTree, model_core.NoopReferenceMetadata]{
		DirectoryCloser: d,
		options: &localCapturableDirectoryOptions[model_core.NoopReferenceMetadata]{
			exclusions: exclusions,
		},
	}
	entries, err := source.ReadDir()
	require.NoError(t, err)
	names := make(map[string]bool, len(entries))
	for _, entry := range entries {
		names[entry.Name().String()] = true
	}
	require.True(t, names["BUILD.bazel"])
	require.False(t, names[".dev"])
	require.False(t, names[".git"])
	_, nested, err := source.EnterCapturableDirectory(path.MustNewComponent("nested"))
	require.NoError(t, err)
	defer nested.Close()
	nestedEntries, err := nested.ReadDir()
	require.NoError(t, err)
	require.Len(t, nestedEntries, 1)
	require.Equal(t, "public.go", nestedEntries[0].Name().String())
}

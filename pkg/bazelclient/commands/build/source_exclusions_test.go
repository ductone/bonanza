package build

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"bonanza.build/pkg/bazelclient/formatted"
	"bonanza.build/pkg/bazelclient/logging"
	model_core "bonanza.build/pkg/model/core"

	"github.com/buildbarn/bb-storage/pkg/filesystem"
	"github.com/buildbarn/bb-storage/pkg/filesystem/path"
	"github.com/stretchr/testify/require"
)

func TestSourceUploadExcludesIgnoredStateAndPriorOutputs(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, exec.Command("git", "-C", root, "init", "-q").Run())
	require.NoError(t, os.WriteFile(filepath.Join(root, ".gitignore"), []byte(".dev/\nnested/private\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".bazelignore"), []byte("scratch\n"), 0o644))
	for _, name := range []string{".dev", "scratch", "bonanza-out", "nested"} {
		require.NoError(t, os.Mkdir(filepath.Join(root, name), 0o755))
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, ".dev", "secret"), []byte("private"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "new-source.go"), []byte("package example"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "nested", "private"), []byte("private"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "nested", "public.go"), []byte("package example"), 0o644))
	require.NoError(t, os.Symlink("../external", filepath.Join(root, "bazel-bin")))
	require.NoError(t, os.Symlink("bonanza-out", filepath.Join(root, "bonanza-bin")))

	d, err := filesystem.NewLocalDirectory(path.LocalFormat.NewParser(root))
	require.NoError(t, err)
	defer d.Close()
	logger := logging.NewConsoleLogger(io.Discard, formatted.WritePlainText)
	exclusions, err := NewSourceExclusions(logger, d, "example", root, true, "example", true, true)
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
	require.True(t, names["new-source.go"], "untracked, non-ignored source must remain visible")
	for _, name := range []string{".dev", ".git", "scratch", "bazel-bin", "bonanza-bin", "bonanza-out"} {
		require.Falsef(t, names[name], "%s must not be uploaded", name)
	}
	_, nested, err := source.EnterCapturableDirectory(path.MustNewComponent("nested"))
	require.NoError(t, err)
	defer nested.Close()
	nestedEntries, err := nested.ReadDir()
	require.NoError(t, err)
	require.Len(t, nestedEntries, 1)
	require.Equal(t, "public.go", nestedEntries[0].Name().String())
}

func TestRequiredGitExclusionsFailClosed(t *testing.T) {
	root := t.TempDir()
	d, err := filesystem.NewLocalDirectory(path.LocalFormat.NewParser(root))
	require.NoError(t, err)
	defer d.Close()
	logger := logging.NewConsoleLogger(io.Discard, formatted.WritePlainText)
	_, err = NewSourceExclusions(logger, d, "example", root, true, "example", true, true)
	require.ErrorContains(t, err, "load Git exclusions")
}

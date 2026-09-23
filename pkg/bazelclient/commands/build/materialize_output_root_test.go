package build

import (
	"os"
	"path/filepath"
	"testing"

	model_core "bonanza.build/pkg/model/core"
	model_filesystem_pb "bonanza.build/pkg/proto/model/filesystem"
	"bonanza.build/pkg/storage/object"

	"github.com/buildbarn/bb-storage/pkg/filesystem"
	"github.com/buildbarn/bb-storage/pkg/filesystem/path"
	"github.com/stretchr/testify/require"
)

// newInlineDirectory creates a DirectoryContents message that does not
// contain any references to objects in storage, so that it can be
// materialized without needing to read anything.
func newInlineDirectory(leaves *model_filesystem_pb.Leaves, directories ...*model_filesystem_pb.DirectoryNode) *model_filesystem_pb.DirectoryContents {
	return &model_filesystem_pb.DirectoryContents{
		Leaves: &model_filesystem_pb.DirectoryContents_LeavesInline{
			LeavesInline: leaves,
		},
		Directories: directories,
	}
}

func TestOutputRootMaterializer(t *testing.T) {
	materialize := func(t *testing.T, contents *model_filesystem_pb.DirectoryContents) (string, *outputRootMaterializer, error) {
		outputPath := t.TempDir()
		d, err := filesystem.NewLocalDirectory(path.LocalFormat.NewParser(outputPath))
		require.NoError(t, err)
		defer d.Close()

		m := outputRootMaterializer{context: t.Context()}
		return outputPath, &m, m.materializeDirectory(
			model_core.NewSimpleMessage[object.LocalReference](contents),
			d,
		)
	}

	t.Run("Empty", func(t *testing.T) {
		outputPath, m, err := materialize(t, newInlineDirectory(&model_filesystem_pb.Leaves{}))
		require.NoError(t, err)
		require.Equal(t, 0, m.filesWritten)

		entries, err := os.ReadDir(outputPath)
		require.NoError(t, err)
		require.Empty(t, entries)
	})

	t.Run("NestedDirectories", func(t *testing.T) {
		outputPath, m, err := materialize(t, newInlineDirectory(
			&model_filesystem_pb.Leaves{},
			&model_filesystem_pb.DirectoryNode{
				Name: "bazel-out",
				Directory: &model_filesystem_pb.Directory{
					Contents: &model_filesystem_pb.Directory_ContentsInline{
						ContentsInline: newInlineDirectory(
							&model_filesystem_pb.Leaves{},
							&model_filesystem_pb.DirectoryNode{
								Name: "bin",
								Directory: &model_filesystem_pb.Directory{
									Contents: &model_filesystem_pb.Directory_ContentsInline{
										ContentsInline: newInlineDirectory(&model_filesystem_pb.Leaves{
											Files: []*model_filesystem_pb.FileNode{
												{
													Name:       "empty.txt",
													Properties: &model_filesystem_pb.FileProperties{},
												},
												{
													Name: "tool",
													Properties: &model_filesystem_pb.FileProperties{
														IsExecutable: true,
													},
												},
											},
											Symlinks: []*model_filesystem_pb.SymlinkNode{{
												Name:   "link",
												Target: "../../../external/main+/hello.txt",
											}},
										}),
									},
								},
							},
						),
					},
				},
			},
		))
		require.NoError(t, err)
		require.Equal(t, 2, m.filesWritten)
		require.Equal(t, 1, m.symlinksWritten)

		binPath := filepath.Join(outputPath, "bazel-out", "bin")
		emptyInfo, err := os.Lstat(filepath.Join(binPath, "empty.txt"))
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o644), emptyInfo.Mode().Perm())
		require.Equal(t, int64(0), emptyInfo.Size())

		toolInfo, err := os.Lstat(filepath.Join(binPath, "tool"))
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o755), toolInfo.Mode().Perm())

		target, err := os.Readlink(filepath.Join(binPath, "link"))
		require.NoError(t, err)
		require.Equal(t, "../../../external/main+/hello.txt", target)
	})

	// Names are provided by the server, so they must be validated
	// before being used to create entries on the local system.
	t.Run("InvalidFileName", func(t *testing.T) {
		_, _, err := materialize(t, newInlineDirectory(&model_filesystem_pb.Leaves{
			Files: []*model_filesystem_pb.FileNode{{
				Name:       "../escape",
				Properties: &model_filesystem_pb.FileProperties{},
			}},
		}))
		require.ErrorContains(t, err, "invalid file name")
	})

	t.Run("InvalidDirectoryName", func(t *testing.T) {
		_, _, err := materialize(t, newInlineDirectory(
			&model_filesystem_pb.Leaves{},
			&model_filesystem_pb.DirectoryNode{
				Name: "..",
				Directory: &model_filesystem_pb.Directory{
					Contents: &model_filesystem_pb.Directory_ContentsInline{
						ContentsInline: newInlineDirectory(&model_filesystem_pb.Leaves{}),
					},
				},
			},
		))
		require.ErrorContains(t, err, "invalid directory name")
	})

	// A file that was written by a previous build must be replaced,
	// including its permissions.
	t.Run("ReplaceExistingEntry", func(t *testing.T) {
		outputPath := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(outputPath, "tool"), []byte("stale"), 0o755))

		d, err := filesystem.NewLocalDirectory(path.LocalFormat.NewParser(outputPath))
		require.NoError(t, err)
		defer d.Close()

		m := outputRootMaterializer{context: t.Context()}
		require.NoError(t, m.materializeDirectory(
			model_core.NewSimpleMessage[object.LocalReference](newInlineDirectory(&model_filesystem_pb.Leaves{
				Files: []*model_filesystem_pb.FileNode{{
					Name:       "tool",
					Properties: &model_filesystem_pb.FileProperties{},
				}},
			})),
			d,
		))

		info, err := os.Lstat(filepath.Join(outputPath, "tool"))
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o644), info.Mode().Perm())
		require.Equal(t, int64(0), info.Size())
	})
}

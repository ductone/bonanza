package build

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// readDirectoryNames returns the names that survive exclusion, for the
// directory at directoryPath below a module root.
func readDirectoryNames(t *testing.T, moduleRoot, directoryPath string, isRoot bool) map[string]bool {
	t.Helper()
	logger := logging.NewConsoleLogger(io.Discard, formatted.WritePlainText)

	moduleRootDirectory, err := filesystem.NewLocalDirectory(path.LocalFormat.NewParser(moduleRoot))
	require.NoError(t, err)
	defer moduleRootDirectory.Close()
	exclusions, err := NewSourceExclusions(logger, moduleRootDirectory, "example", moduleRoot, isRoot, "example", true, true)
	require.NoError(t, err)

	directory, err := filesystem.NewLocalDirectory(path.LocalFormat.NewParser(filepath.Join(moduleRoot, directoryPath)))
	require.NoError(t, err)
	defer directory.Close()
	source := &localCapturableDirectory[model_core.CreatedObjectTree, model_core.NoopReferenceMetadata]{
		DirectoryCloser: directory,
		options: &localCapturableDirectoryOptions[model_core.NoopReferenceMetadata]{
			exclusions: exclusions,
		},
	}
	if directoryPath != "" {
		source.relativePath = strings.Split(directoryPath, "/")
	}

	entries, err := source.ReadDir()
	require.NoError(t, err)
	names := make(map[string]bool, len(entries))
	for _, entry := range entries {
		names[entry.Name().String()] = true
	}
	return names
}

func TestSourceUploadExcludesNestedGitMetadata(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, exec.Command("git", "-C", root, "init", "-q").Run())
	for _, directory := range []string{"nested/repo/.git/objects", "nested/repo/src"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, directory), 0o755))
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, "nested", "repo", ".git", "config"), []byte("[core]"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "nested", "repo", "src", "main.go"), []byte("package main"), 0o644))

	require.True(t, readDirectoryNames(t, root, "nested", true)["repo"], "a nested checkout is still source")
	repoNames := readDirectoryNames(t, root, "nested/repo", true)
	require.False(t, repoNames[".git"], ".git is never a build input, at any depth")
	require.True(t, repoNames["src"])
}

func TestSourceUploadExcludesOutputTreesThatAreNotSymlinks(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, exec.Command("git", "-C", root, "init", "-q").Run())
	for _, directory := range []string{"bazel-out", "bazel-bin"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, directory, "k8-fastbuild"), 0o755))
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, "bazel-out", "k8-fastbuild", "stale.txt"), []byte("stale"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "source.go"), []byte("package example"), 0o644))

	names := readDirectoryNames(t, root, "", true)
	require.True(t, names["source.go"])
	require.False(t, names["bazel-out"], "an output tree is not source, symlink or not")
	require.False(t, names["bazel-bin"])
}

func TestSourceUploadFiltersGitIgnoredPathsOfNestedModules(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, exec.Command("git", "-C", root, "init", "-q").Run())
	require.NoError(t, os.WriteFile(filepath.Join(root, ".gitignore"), []byte("sub/.dev/\nsub/nested/private\nunrelated/\n"), 0o644))
	for _, directory := range []string{"sub/.dev", "sub/nested", "unrelated"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, directory), 0o755))
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, "sub", ".dev", "secret"), []byte("private"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "sub", "nested", "private"), []byte("private"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "sub", "nested", "public.go"), []byte("package example"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "unrelated", "other"), []byte("other"), 0o644))

	// The module is a subdirectory of the working tree rather than its
	// top level, and the ignored paths Git reports are rooted at the
	// working tree, so they have to be translated to module-relative
	// paths for the module's own exclusions to line up. An ignored path
	// outside the module (unrelated/) contributes nothing.
	subRoot := filepath.Join(root, "sub")
	subNames := readDirectoryNames(t, subRoot, "", false)
	require.True(t, subNames["nested"])
	require.False(t, subNames[".dev"], "ignored credentials below a nested module must be excluded")
	require.False(t, subNames[".git"])
	require.Equal(t, map[string]bool{"public.go": true}, readDirectoryNames(t, subRoot, "nested", false))
}

func TestNestedModuleCoveredByIgnoreRuleFailsClosed(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, exec.Command("git", "-C", root, "init", "-q").Run())
	require.NoError(t, os.WriteFile(filepath.Join(root, ".gitignore"), []byte("vendor/\n"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "vendor", "module"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "vendor", "module", "source.go"), []byte("package example"), 0o644))

	moduleRoot := filepath.Join(root, "vendor", "module")
	d, err := filesystem.NewLocalDirectory(path.LocalFormat.NewParser(moduleRoot))
	require.NoError(t, err)
	defer d.Close()
	logger := logging.NewConsoleLogger(io.Discard, formatted.WritePlainText)
	_, err = NewSourceExclusions(logger, d, "vendor_module", moduleRoot, false, "example", true, true)
	require.ErrorContains(t, err, "covered by Git ignore rule")
}

func TestTranslateIgnoredPath(t *testing.T) {
	for _, tc := range []struct {
		modulePrefix string
		ignoredPath  string
		expected     string
		withinModule bool
	}{
		{modulePrefix: "", ignoredPath: "a/b", expected: "a/b", withinModule: true},
		{modulePrefix: "sub", ignoredPath: "sub/nested/private", expected: "nested/private", withinModule: true},
		{modulePrefix: "sub/deep", ignoredPath: "sub/deep/x", expected: "x", withinModule: true},
		{modulePrefix: "sub", ignoredPath: "subway/x", withinModule: false},
		{modulePrefix: "sub", ignoredPath: "other/x", withinModule: false},
	} {
		actual, withinModule, err := translateIgnoredPath(tc.modulePrefix, tc.ignoredPath)
		require.NoError(t, err)
		require.Equal(t, tc.withinModule, withinModule, "%q in %q", tc.ignoredPath, tc.modulePrefix)
		require.Equal(t, tc.expected, actual, "%q in %q", tc.ignoredPath, tc.modulePrefix)
	}

	for _, tc := range [][2]string{
		{"sub", "sub"},
		{"sub/deep", "sub"},
	} {
		_, _, err := translateIgnoredPath(tc[0], tc[1])
		require.ErrorContains(t, err, "covered by Git ignore rule")
	}
}

func TestModulePrefixWithinWorkingTree(t *testing.T) {
	prefix, err := modulePrefixWithinWorkingTree("/repo", "/repo")
	require.NoError(t, err)
	require.Equal(t, "", prefix)

	prefix, err = modulePrefixWithinWorkingTree("/repo", "/repo/sub/dir/")
	require.NoError(t, err)
	require.Equal(t, "sub/dir", prefix)

	_, err = modulePrefixWithinWorkingTree("/repo", "/repo2")
	require.ErrorContains(t, err, "not inside Git working tree")
}

package build

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os/exec"
	"strings"

	"bonanza.build/pkg/bazelclient/formatted"
	"bonanza.build/pkg/bazelclient/logging"

	"github.com/buildbarn/bb-storage/pkg/filesystem"
	"github.com/buildbarn/bb-storage/pkg/filesystem/path"
)

// workspaceRootOutputNames returns the names of the entries that "bazel build"
// and "bonanza_bazel build" themselves create in the root of a workspace:
// Bazel's convenience symlinks (bazel-bin, bazel-out, bazel-testlogs, and
// bazel-<workspace base name>), plus Bonanza's own materialization directory
// (bonanza-out) and the bin symlink pointing into it. Every local invocation
// creates them, they always point outside of the workspace (into the output
// base, which typically lives under a cache directory or /tmp, or into
// bonanza-out), and they are never build inputs. Most repositories don't list
// them in .bazelignore or .gitignore, because a lazily reading Bazel client
// never needs to be told to skip them: nothing in a BUILD file ever references
// bazel-out itself. An eager uploader has no such luxury, so it hardcodes the
// well-known names instead.
//
// These names are matched regardless of whether the entry is a symbolic link
// or a real directory. A build tree left behind by different tooling (or a
// stale output base) leaves real directories named bazel-*, and an output tree
// is never a source input either way.
func workspaceRootOutputNames(workspaceBaseName string) map[string]struct{} {
	names := map[string]struct{}{
		"bazel-bin":      {},
		"bazel-out":      {},
		"bazel-testlogs": {},
		"bonanza-bin":    {},
		"bonanza-out":    {},
	}
	if workspaceBaseName != "" {
		names["bazel-"+workspaceBaseName] = struct{}{}
	}
	return names
}

// sourceExclusions decides which entries of a single module's source
// tree should be skipped while constructing the Merkle tree that gets
// uploaded to the remote cache.
//
// Bazel itself never needs a general exclusion mechanism for this
// purpose, because it reads lazily: only files that the build graph
// actually references are ever opened, so a stray node_modules or
// .git directory is simply never touched. bonanza_bazel uploads
// eagerly instead, because analysis runs on the cluster and needs the
// whole module tree in content-addressed storage before it can start.
// That difference means bonanza needs its own opinion about what
// belongs to a module's source, one Bazel was never forced to have.
type SourceExclusions struct {
	// ignoredRelativePaths contains module-root-relative paths
	// (path components joined by "/") that should be skipped
	// wholesale, regardless of whether they are a file or a
	// directory. Populated from .bazelignore and from `git
	// ls-files --others --ignored`. Git's own ".git" metadata is
	// not listed here, because it is skipped at every depth rather
	// than at the module root only; see ShouldExclude.
	ignoredRelativePaths map[string]struct{}
	// workspaceRootExcludedNames is non-nil only for the module
	// that corresponds to the workspace root, since Bazel's
	// convenience symlinks and Bonanza's output directory only
	// ever appear there.
	workspaceRootExcludedNames map[string]struct{}
}

// newSourceExclusions constructs the exclusion set for a single
// module. bazelIgnorePaths and gitIgnorePaths may be nil.
func newSourceExclusions(bazelIgnorePaths, gitIgnorePaths map[string]struct{}, isRootModule bool, workspaceBaseName string) *SourceExclusions {
	ignored := make(map[string]struct{}, len(bazelIgnorePaths)+len(gitIgnorePaths))
	for p := range bazelIgnorePaths {
		ignored[p] = struct{}{}
	}
	for p := range gitIgnorePaths {
		ignored[p] = struct{}{}
	}

	e := &SourceExclusions{
		ignoredRelativePaths: ignored,
	}
	if isRootModule {
		e.workspaceRootExcludedNames = workspaceRootOutputNames(workspaceBaseName)
	}
	return e
}

// NewSourceExclusions uses the same upload policy for build, test, run, and
// query. Modules that live inside a Git working tree but are not its top level
// (local_path_override(), or an --override_module pointing at a subdirectory)
// get their ignored paths translated to module-relative paths.
//
// When requireGitignore is set, a module whose Git ignore rules cannot be read
// is an error, so that a caller can fail before uploading any module source.
func NewSourceExclusions(logger logging.Logger, moduleRoot filesystem.Directory, moduleName, modulePathStr string, isRoot bool, workspaceBaseName string, respectGitignore, requireGitignore bool) (*SourceExclusions, error) {
	bazelIgnorePaths, err := loadBazelIgnorePatterns(moduleRoot)
	if err != nil {
		return nil, fmt.Errorf("load .bazelignore for module %q: %w", moduleName, err)
	}
	var gitIgnorePaths map[string]struct{}
	if respectGitignore || requireGitignore {
		gitIgnorePaths, err = loadGitIgnoredPaths(modulePathStr)
		if err != nil {
			if requireGitignore {
				return nil, fmt.Errorf("load Git exclusions for module %q: %w", moduleName, err)
			}
			logger.Warning(formatted.Textf("Cannot query Git exclusions for module %q: %s; uploading without Git exclusions", moduleName, err))
		}
	}
	return newSourceExclusions(bazelIgnorePaths, gitIgnorePaths, isRoot, workspaceBaseName), nil
}

// AddIgnoredRelativePath excludes one module-root-relative path from an eager
// source upload. It rejects non-local spellings so a caller cannot turn a
// source exclusion into a traversal outside the module tree.
func (e *SourceExclusions) AddIgnoredRelativePath(relativePath string) error {
	relativePath = strings.Trim(relativePath, "/")
	if relativePath == "" {
		return errors.New("path is empty")
	}
	for _, component := range strings.Split(relativePath, "/") {
		if component == "" || component == "." || component == ".." {
			return fmt.Errorf("path %q is not module-relative", relativePath)
		}
	}
	e.ignoredRelativePaths[relativePath] = struct{}{}
	return nil
}

// gitMetadataName is the name of Git's per-working-tree metadata entry. Unlike
// the configured exclusions, it is skipped at every depth and regardless of
// whether it is a directory or a file: a submodule, a nested checkout, or a
// vendored repository inside a module carries its own ".git", and in a linked
// worktree that entry is a file pointing at the real object store elsewhere.
// None of that metadata is ever a build input, and it is the single largest
// thing a module can accidentally upload.
const gitMetadataName = ".git"

// ShouldExclude reports whether a module-relative entry is excluded from upload.
func (e *SourceExclusions) ShouldExclude(relativePath []string, entry filesystem.FileInfo) bool {
	name := entry.Name().String()

	if name == gitMetadataName {
		return true
	}

	if len(relativePath) == 0 {
		// Only the workspace root module can contain the output
		// paths of local builds.
		if _, ok := e.workspaceRootExcludedNames[name]; ok {
			return true
		}
	}

	key := name
	if len(relativePath) > 0 {
		key = strings.Join(relativePath, "/") + "/" + name
	}
	_, ok := e.ignoredRelativePaths[key]
	return ok
}

// loadBazelIgnorePatterns reads the .bazelignore file from the root of
// a module, if any, and returns the set of module-root-relative paths
// it lists. This mirrors the way Bazel itself uses .bazelignore to
// exclude directories from package discovery -- applied here to the
// eager source upload instead, since that is the step that actually
// needs to be told what to skip.
//
// As with Bazel's own .bazelignore handling, entries are plain
// slash-separated relative paths (no globbing), one per line, with
// blank lines and "#"-prefixed comments ignored.
func loadBazelIgnorePatterns(moduleRoot filesystem.Directory) (map[string]struct{}, error) {
	f, err := moduleRoot.OpenRead(path.MustNewComponent(".bazelignore"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to open .bazelignore: %w", err)
	}
	defer f.Close()

	contents, err := io.ReadAll(io.NewSectionReader(f, 0, math.MaxInt64))
	if err != nil {
		return nil, fmt.Errorf("failed to read .bazelignore: %w", err)
	}

	patterns := map[string]struct{}{}
	for _, line := range strings.Split(string(contents), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimRight(line, "/")
		if line != "" {
			patterns[line] = struct{}{}
		}
	}
	return patterns, nil
}

// loadGitIgnoredPaths returns the paths below a module that Git ignores
// (through .gitignore, .git/info/exclude, or the user's global excludes file),
// expressed relative to the module root.
//
// The module root does not have to be the top level of the Git working tree. A
// module supplied through local_path_override() or an explicit
// --override_module may point at a subdirectory of the checkout, in which case
// the paths Git reports relative to the working tree root are translated into
// module-relative paths. That keeps a Git-ignored credentials directory such
// as ".dev/" out of the upload however deeply the module is nested, instead of
// abandoning filtering for that module entirely.
//
// Every failure -- the module is not inside a Git working tree, the "git"
// binary is missing, or the module is itself covered by an ignore rule (so the
// ignored paths below it cannot be enumerated) -- is returned as an error.
// Callers running with --require_gitignore turn that into a fatal error before
// any module source is uploaded; callers that merely default to
// --respect_gitignore log a warning and upload without Git exclusions.
func loadGitIgnoredPaths(modulePathStr string) (map[string]struct{}, error) {
	toplevel, err := gitWorkingTreeRoot(modulePathStr)
	if err != nil {
		return nil, err
	}
	modulePrefix, err := modulePrefixWithinWorkingTree(toplevel, modulePathStr)
	if err != nil {
		return nil, err
	}

	var stdout, stderr bytes.Buffer
	// Run at the working tree root, so that the reported paths are
	// relative to it and can be translated to module-relative paths.
	cmd := exec.Command("git", "-C", toplevel, "ls-files", "--others", "--ignored", "--exclude-standard", "--directory", "-z")
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("list Git-ignored paths: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}

	paths := map[string]struct{}{}
	for _, entry := range strings.Split(stdout.String(), "\x00") {
		entry = strings.TrimRight(entry, "/")
		if entry == "" {
			continue
		}
		relativePath, withinModule, err := translateIgnoredPath(modulePrefix, entry)
		if err != nil {
			return nil, err
		}
		if withinModule {
			paths[relativePath] = struct{}{}
		}
	}
	return paths, nil
}

// gitWorkingTreeRoot returns the top level of the Git working tree that
// contains modulePathStr.
func gitWorkingTreeRoot(modulePathStr string) (string, error) {
	toplevel, err := exec.Command("git", "-C", modulePathStr, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("find Git root: %w", err)
	}
	trimmed := strings.TrimRight(strings.TrimSpace(string(toplevel)), "/")
	if trimmed == "" {
		return "", errors.New("Git working tree root is empty")
	}
	return trimmed, nil
}

// modulePrefixWithinWorkingTree returns modulePathStr relative to the top level
// of its Git working tree, or "" when the module root is the working tree root
// itself.
func modulePrefixWithinWorkingTree(toplevel, modulePathStr string) (string, error) {
	modulePath := strings.TrimRight(modulePathStr, "/")
	if modulePath == toplevel {
		return "", nil
	}
	if modulePrefix, ok := strings.CutPrefix(modulePath, toplevel+"/"); ok {
		return modulePrefix, nil
	}
	return "", fmt.Errorf("module path %q is not inside Git working tree %q", modulePathStr, toplevel)
}

// translateIgnoredPath converts one path reported by `git ls-files` (relative
// to the top level of the working tree) into a module-relative path.
// withinModule is false when the reported path lies outside the module, in
// which case it contributes no exclusion.
//
// An error is returned when the module is itself covered by the reported
// ignore rule. Git collapses such a directory, so the ignored paths below it
// cannot be enumerated; uploading the module on the assumption that it
// contains no ignored state is exactly the outcome this filtering exists to
// prevent.
func translateIgnoredPath(modulePrefix, ignoredPath string) (string, bool, error) {
	if modulePrefix == "" {
		return ignoredPath, true, nil
	}
	if ignoredPath == modulePrefix || strings.HasPrefix(modulePrefix, ignoredPath+"/") {
		return "", false, fmt.Errorf("module path %q is itself covered by Git ignore rule %q", modulePrefix, ignoredPath)
	}
	if relativePath, ok := strings.CutPrefix(ignoredPath, modulePrefix+"/"); ok {
		return relativePath, true, nil
	}
	return "", false, nil
}

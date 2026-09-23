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

// bazelConvenienceSymlinkNames returns the names of the top level
// symlinks that "bazel build" itself creates in the root of a
// workspace to point into the output base (bazel-bin, bazel-out,
// bazel-testlogs, and bazel-<workspace base name>). Every local Bazel
// invocation creates these, they always point outside of the
// workspace (into the output base, which typically lives under a
// cache directory or /tmp), and they are never build inputs. Most
// repositories don't list them in .bazelignore or .gitignore, because
// a lazily reading Bazel client never needs to be told to skip them:
// nothing in a BUILD file ever references bazel-out itself. An eager
// uploader has no such luxury, so it hardcodes the well-known names
// instead.
func bazelConvenienceSymlinkNames(workspaceBaseName string) map[string]struct{} {
	names := map[string]struct{}{
		"bazel-bin":      {},
		"bazel-out":      {},
		"bazel-testlogs": {},
		"bonanza-bin":    {},
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
	// directory. Populated from .bazelignore, from `git
	// ls-files --others --ignored`, and unconditionally from
	// ".git" itself.
	ignoredRelativePaths map[string]struct{}
	// rootConvenienceSymlinkNames is non-nil only for the module
	// that corresponds to the workspace root, since Bazel's
	// convenience symlinks only ever appear there.
	rootConvenienceSymlinkNames map[string]struct{}
}

// newSourceExclusions constructs the exclusion set for a single
// module. bazelIgnorePaths and gitIgnorePaths may be nil.
func newSourceExclusions(bazelIgnorePaths, gitIgnorePaths map[string]struct{}, isRootModule bool, workspaceBaseName string) *SourceExclusions {
	ignored := map[string]struct{}{
		// Version control metadata is never a build input.
		// Unlike the other exclusions below, this one doesn't
		// depend on any configuration file being present, so
		// that a module can't accidentally re-include its own
		// (potentially enormous) VCS history by omission.
		".git": {},
	}
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
		e.rootConvenienceSymlinkNames = bazelConvenienceSymlinkNames(workspaceBaseName)
		// The default materialization directory must never become an input
		// to a subsequent build or query.
		e.ignoredRelativePaths["bonanza-out"] = struct{}{}
	}
	return e
}

// NewSourceExclusions uses the same upload policy for build, test, run, and query.
// When requireGitignore is set, a missing or unusable Git checkout is an error.
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

// ShouldExclude reports whether a module-relative entry is excluded from upload.
func (e *SourceExclusions) ShouldExclude(relativePath []string, entry filesystem.FileInfo) bool {
	name := entry.Name().String()

	if len(relativePath) == 0 && entry.Type() == filesystem.FileTypeSymlink {
		if _, ok := e.rootConvenienceSymlinkNames[name]; ok {
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

// loadGitIgnoredPaths returns Git-ignored untracked paths relative to the module.
// A non-checkout or failed Git query is an error so callers can fail closed.
func loadGitIgnoredPaths(modulePathStr string) (map[string]struct{}, error) {
	toplevel, err := exec.Command("git", "-C", modulePathStr, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return nil, fmt.Errorf("find Git root: %w", err)
	}
	if strings.TrimSpace(string(toplevel)) != strings.TrimRight(modulePathStr, "/") {
		return nil, fmt.Errorf("module path is not the Git working-tree root")
	}

	var stdout, stderr bytes.Buffer
	cmd := exec.Command("git", "-C", modulePathStr, "ls-files", "--others", "--ignored", "--exclude-standard", "--directory", "-z")
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("list Git-ignored paths: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}

	paths := map[string]struct{}{}
	for _, entry := range strings.Split(stdout.String(), "\x00") {
		entry = strings.TrimRight(entry, "/")
		if entry != "" {
			paths[entry] = struct{}{}
		}
	}
	return paths, nil
}

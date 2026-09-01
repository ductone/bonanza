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
type sourceExclusions struct {
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
func newSourceExclusions(bazelIgnorePaths, gitIgnorePaths map[string]struct{}, isRootModule bool, workspaceBaseName string) *sourceExclusions {
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

	e := &sourceExclusions{
		ignoredRelativePaths: ignored,
	}
	if isRootModule {
		e.rootConvenienceSymlinkNames = bazelConvenienceSymlinkNames(workspaceBaseName)
	}
	return e
}

// shouldExclude reports whether a directory entry located at
// relativePath (the path of its parent directory, relative to the
// module root; empty for entries directly inside the module root)
// should be omitted from the uploaded source tree.
func (e *sourceExclusions) shouldExclude(relativePath []string, entry filesystem.FileInfo) bool {
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

// loadGitIgnoredPaths asks Git for the set of untracked files and
// directories that are ignored somewhere below modulePathStr (via
// .gitignore, .git/info/exclude, or the user's global excludes file).
//
// This is deliberately a best-effort helper rather than a hard
// requirement: if the module doesn't turn out to be the top level of
// a Git working tree (the common case for extracted/vendored
// dependencies, which usually aren't Git checkouts at all), or the
// "git" binary can't be found or invoked, it silently returns no
// exclusions instead of failing the build. Respecting .gitignore is
// meant to make uploads smaller and more deterministic by default; it
// should never be the reason a build that used to work stops working.
func loadGitIgnoredPaths(logger logging.Logger, moduleName, modulePathStr string) map[string]struct{} {
	toplevel, err := exec.Command("git", "-C", modulePathStr, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return nil
	}
	if strings.TrimSpace(string(toplevel)) != strings.TrimRight(modulePathStr, "/") {
		// The module lives inside a larger enclosing Git
		// working tree (e.g. it's a subdirectory rather than a
		// checkout of its own). Querying ignore rules from here
		// would report paths relative to that outer tree, not
		// to the module root, so don't risk misinterpreting
		// them.
		return nil
	}

	var stdout, stderr bytes.Buffer
	cmd := exec.Command("git", "-C", modulePathStr, "ls-files", "--others", "--ignored", "--exclude-standard", "--directory", "-z")
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		logger.Warning(formatted.Textf("Failed to query Git ignore rules for module %#v: %s (%s); uploading its full directory tree instead", moduleName, err, strings.TrimSpace(stderr.String())))
		return nil
	}

	paths := map[string]struct{}{}
	for _, entry := range strings.Split(stdout.String(), "\x00") {
		entry = strings.TrimRight(entry, "/")
		if entry != "" {
			paths[entry] = struct{}{}
		}
	}
	return paths
}

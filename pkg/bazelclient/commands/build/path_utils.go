package build

import (
	"strings"

	"github.com/buildbarn/bb-storage/pkg/filesystem/path"
)

// ResolveToAbsoluteString resolves a path.Parser (which may be
// relative, contain "." or ".." components, etc.) to a clean absolute
// path string in the local path format. It is used to compare module
// paths against the workspace path irrespective of how each one was
// originally spelled out (e.g., via a relative --override_module
// value versus the workspace path derived from the current working
// directory).
func ResolveToAbsoluteString(p path.Parser) (string, error) {
	builder, scopeWalker := path.EmptyBuilder.Join(path.NewAbsoluteScopeWalker(path.VoidComponentWalker))
	if err := path.Resolve(p, scopeWalker); err != nil {
		return "", err
	}
	return path.LocalFormat.GetString(builder)
}

// BaseName returns the final component of an absolute path string, as
// produced by ResolveToAbsoluteString. Returns an empty string for the
// root directory.
func BaseName(absolutePathStr string) string {
	trimmed := strings.TrimRight(absolutePathStr, "/")
	if trimmed == "" {
		return ""
	}
	if i := strings.LastIndexByte(trimmed, '/'); i >= 0 {
		return trimmed[i+1:]
	}
	return trimmed
}

// relativeDepth reports how many path components separate descendantPathStr
// from ancestorPathStr, assuming descendantPathStr lies at or below
// ancestorPathStr. The second return value is false if descendantPathStr
// does not lie within ancestorPathStr at all, in which case no
// meaningful depth exists.
func relativeDepth(ancestorPathStr, descendantPathStr string) (int, bool) {
	ancestor := strings.TrimRight(ancestorPathStr, "/")
	descendant := strings.TrimRight(descendantPathStr, "/")
	if descendant == ancestor {
		return 0, true
	}
	if strings.HasPrefix(descendant, ancestor+"/") {
		rest := descendant[len(ancestor)+1:]
		return len(strings.Split(rest, "/")), true
	}
	return 0, false
}

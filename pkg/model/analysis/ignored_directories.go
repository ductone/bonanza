package analysis

import (
	"bonanza.build/pkg/glob"
)

type ignoredDirectories struct {
	nfa     *glob.NFA
	matcher glob.Matcher
}

func newIgnoredDirectories(patterns []string) (*ignoredDirectories, error) {
	if len(patterns) == 0 {
		return nil, nil
	}
	nfa, err := glob.NewNFAFromPatterns(patterns, nil)
	if err != nil {
		return nil, err
	}
	return &ignoredDirectories{nfa: nfa}, nil
}

func (d *ignoredDirectories) matches(path string) bool {
	if d == nil || path == "" {
		return false
	}
	d.matcher.Initialize(d.nfa)
	for _, r := range path {
		if !d.matcher.WriteRune(r) {
			return false
		}
	}
	return d.matcher.IsMatch()
}

// contains also checks ancestors because ignoring a directory hides all its
// packages and files, even when a descendant does not match the glob itself.
func (d *ignoredDirectories) contains(path string) bool {
	for i, r := range path {
		if r == '/' && d.matches(path[:i]) {
			return true
		}
	}
	return d.matches(path)
}

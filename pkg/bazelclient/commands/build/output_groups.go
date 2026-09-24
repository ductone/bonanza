package build

import (
	"fmt"
	"slices"
	"strings"
)

// parseOutputGroups resolves Bazel's replacement and additive output-group
// syntax. An empty result is reserved for the implicit default group in the
// build protocol; explicitly selecting no groups is rejected rather than
// silently building the defaults.
func parseOutputGroups(value string) ([]string, error) {
	parts := strings.Split(value, ",")
	groups := []string(nil)
	if strings.HasPrefix(parts[0], "+") || strings.HasPrefix(parts[0], "-") {
		groups = append(groups, "default")
	}
	for _, part := range parts {
		mode := byte('+')
		if strings.HasPrefix(part, "+") || strings.HasPrefix(part, "-") {
			mode, part = part[0], part[1:]
		}
		if !validOutputGroup(part) {
			return nil, fmt.Errorf("invalid --output_groups entry %q", part)
		}
		if mode == '-' {
			if index := slices.Index(groups, part); index >= 0 {
				groups = slices.Delete(groups, index, index+1)
			}
		} else if !slices.Contains(groups, part) {
			groups = append(groups, part)
		}
	}
	if len(groups) == 0 {
		return nil, fmt.Errorf("--output_groups must select at least one group")
	}
	if len(groups) == 1 && groups[0] == "default" {
		return nil, nil
	}
	return groups, nil
}

func validOutputGroup(name string) bool {
	if name == "" || !(name[0] == '_' || name[0] >= 'A' && name[0] <= 'Z' || name[0] >= 'a' && name[0] <= 'z') {
		return false
	}
	for i := 1; i < len(name); i++ {
		c := name[i]
		if !(c == '_' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

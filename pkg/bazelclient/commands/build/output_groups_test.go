package build

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseOutputGroups(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  []string
	}{
		{"default", nil},
		{"mtree", []string{"mtree"}},
		{"default,mtree,mtree", []string{"default", "mtree"}},
		{"+mtree,-default,+tarball", []string{"mtree", "tarball"}},
	} {
		got, err := parseOutputGroups(tc.input)
		require.NoError(t, err)
		require.Equal(t, tc.want, got)
	}
	for _, input := range []string{"", "-default", "default,invalid/name", "+"} {
		_, err := parseOutputGroups(input)
		require.ErrorContains(t, err, "--output_groups")
	}
}

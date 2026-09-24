package run

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGetCommandLine(t *testing.T) {
	t.Run("NoRunUnder", func(t *testing.T) {
		argv, err := getCommandLine("", "/out/bin/tool", []string{"--help"})
		require.NoError(t, err)
		require.Equal(t, []string{"/out/bin/tool", "--help"}, argv)
	})

	t.Run("RunUnderWithArguments", func(t *testing.T) {
		argv, err := getCommandLine("valgrind --quiet", "/out/bin/tool", nil)
		require.NoError(t, err)
		require.Equal(t, []string{"valgrind", "--quiet", "/out/bin/tool"}, argv)
	})

	t.Run("RunUnderLabel", func(t *testing.T) {
		_, err := getCommandLine("//pkg:target", "/out/bin/tool", nil)
		require.ErrorContains(t, err, "refers to a target")

		_, err = getCommandLine("@repo//pkg:target", "/out/bin/tool", nil)
		require.ErrorContains(t, err, "refers to a target")
	})

	t.Run("RunUnderWhitespaceOnly", func(t *testing.T) {
		_, err := getCommandLine("   ", "/out/bin/tool", nil)
		require.ErrorContains(t, err, "does not contain a command")
	})
}

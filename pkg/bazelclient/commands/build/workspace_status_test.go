package build

import (
	"testing"
	"time"

	"bonanza.build/pkg/bazelclient/arguments"

	"github.com/stretchr/testify/require"
)

func TestWorkspaceStatus(t *testing.T) {
	at := time.Date(2026, time.September, 24, 18, 30, 0, 0, time.UTC)
	stamped := arguments.BuildSettingOverride{Label: stampSetting, Value: "true"}
	unstamped := arguments.BuildSettingOverride{Label: stampSetting, Value: "false"}

	stable, volatile, err := workspaceStatus([]arguments.BuildSettingOverride{unstamped, stamped}, "abc123", at, "builder", "agent")
	require.NoError(t, err)
	require.Equal(t, "BUILD_EMBED_LABEL abc123\nBUILD_HOST builder\nBUILD_USER agent\n", stable)
	require.Equal(t, "BUILD_TIMESTAMP 1790274600\nFORMATTED_DATE 2026 Sep 24 18 30 00 Thu\n", volatile)

	// Last flag wins; an unstamped build must not depend on its time,
	// hostname, user or an otherwise unused embed label.
	stable, volatile, err = workspaceStatus([]arguments.BuildSettingOverride{stamped, unstamped}, "abc123", at, "builder", "agent")
	require.NoError(t, err)
	require.Empty(t, stable)
	require.Empty(t, volatile)

	_, _, err = workspaceStatus([]arguments.BuildSettingOverride{stamped}, "abc\nFORGED_KEY token", at, "builder", "agent")
	require.ErrorContains(t, err, "--embed_label")
	_, _, err = workspaceStatus([]arguments.BuildSettingOverride{{Label: stampSetting, Value: "invalid"}}, "", at, "builder", "agent")
	require.ErrorContains(t, err, "invalid --stamp")
}

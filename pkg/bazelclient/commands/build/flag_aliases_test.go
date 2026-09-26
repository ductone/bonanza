package build

import (
	"os"
	"path/filepath"
	"testing"

	"bonanza.build/pkg/bazelclient/arguments"
	"bonanza.build/pkg/label"

	"github.com/buildbarn/bb-storage/pkg/filesystem/path"
	"github.com/stretchr/testify/require"
)

func TestVendoredFlagAliasResolvesCLIOverrides(t *testing.T) {
	workspace := t.TempDir()
	vendorPath := filepath.Join(workspace, "vendor")
	require.NoError(t, os.MkdirAll(vendorPath, 0o755))
	writeVendoredRepo(t, vendorPath, "rules_python+")
	require.NoError(t, os.WriteFile(filepath.Join(vendorPath, "rules_python+", "MODULE.bazel"), []byte(`module(name = "rules_python")
flag_alias(name = "incompatible_default_to_explicit_init_py", starlark_flag = "//python/config_settings:incompatible_default_to_explicit_init_py")
`), 0o644))

	vendor, err := ScanVendorDirectory(path.LocalFormat.NewParser(workspace), "vendor", nil, false)
	require.NoError(t, err)
	require.Equal(t, map[string]string{
		"incompatible_default_to_explicit_init_py": "@@rules_python+//python/config_settings:incompatible_default_to_explicit_init_py",
	}, vendor.FlagAliases)

	overrides, err := ResolveFlagAliases([]arguments.BuildSettingOverride{
		{Label: "incompatible_default_to_explicit_init_py", Value: "true", IsAlias: true},
		{Label: "noincompatible_default_to_explicit_init_py", Value: "true", IsAlias: true},
		{Label: "//:other_flag", Value: "custom"},
	}, vendor.FlagAliases)
	require.NoError(t, err)
	require.Equal(t, []arguments.BuildSettingOverride{
		{Label: "@@rules_python+//python/config_settings:incompatible_default_to_explicit_init_py", Value: "true", IsAlias: false},
		{Label: "@@rules_python+//python/config_settings:incompatible_default_to_explicit_init_py", Value: "false", IsAlias: false},
		{Label: "//:other_flag", Value: "custom"},
	}, overrides)
}

func TestRootModuleFlagAliasResolvesFromWorkspace(t *testing.T) {
	workspace := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "MODULE.bazel"), []byte(`module(name = "c1")
flag_alias(name = "local_feature", starlark_flag = "//settings:feature")
`), 0o644))
	moduleName, err := label.NewModule("c1")
	require.NoError(t, err)
	aliases := map[string]string{}
	require.NoError(t, scanModuleFlagAliases(aliases, workspace, moduleName.ToModuleInstance(nil).GetBareCanonicalRepo()))
	require.Equal(t, "@@c1+//settings:feature", aliases["local_feature"])
}

func TestUnknownAliasFailsAfterModuleResolution(t *testing.T) {
	_, err := ResolveFlagAliases([]arguments.BuildSettingOverride{{Label: "unknown", Value: "true", IsAlias: true}}, nil)
	require.ErrorContains(t, err, "unrecognized build flag --unknown")
}

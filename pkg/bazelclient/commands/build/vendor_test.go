package build

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/buildbarn/bb-storage/pkg/filesystem/path"
	"github.com/stretchr/testify/require"
)

func TestScanVendorDirectoryMapsCanonicalModuleAndExtensionRepos(t *testing.T) {
	workspace := t.TempDir()
	vendorDirectory := filepath.Join(workspace, "bazel", "vendor")
	require.NoError(t, os.MkdirAll(filepath.Join(vendorDirectory, "_registries", "bcr.bazel.build"), 0o755))

	registryContents := []byte("{}\n")
	registryPath := filepath.Join(vendorDirectory, "_registries", "bcr.bazel.build", "bazel_registry.json")
	require.NoError(t, os.WriteFile(registryPath, registryContents, 0o644))
	registryHash := sha256.Sum256(registryContents)
	lockfile, err := json.Marshal(map[string]any{
		"registryFileHashes": map[string]string{
			"https://bcr.bazel.build/bazel_registry.json": hex.EncodeToString(registryHash[:]),
		},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "MODULE.bazel.lock"), lockfile, 0o644))

	patchPath := filepath.Join(workspace, "bazel", "patches", "rules_go.patch")
	require.NoError(t, os.MkdirAll(filepath.Dir(patchPath), 0o755))
	patchContents := []byte("patch\n")
	require.NoError(t, os.WriteFile(patchPath, patchContents, 0o644))
	patchHash := sha256.Sum256(patchContents)

	writeVendoredRepo(t, vendorDirectory, "rules_go+", "FILE:@@//bazel/patches/rules_go.patch "+hex.EncodeToString(patchHash[:]))
	writeVendoredRepo(t, vendorDirectory, "rules_go++go_sdk+c1_go_sdk")
	writeVendoredRepo(t, vendorDirectory, "c1++_repo_rules+c1_tool")
	require.NoError(t, os.WriteFile(
		filepath.Join(vendorDirectory, "VENDOR.bazel"),
		[]byte(`pin("@@rules_go++go_sdk+c1_go_sdk")`),
		0o644,
	))

	vendor, err := ScanVendorDirectory(
		path.LocalFormat.NewParser(workspace),
		"bazel/vendor",
		[]string{"https://bcr.bazel.build/"},
		/* requireLockfile = */ true,
	)
	require.NoError(t, err)
	require.Equal(t, "bazel/vendor", vendor.RootRelativePath)
	require.Len(t, vendor.Repos, 3)
	require.Equal(t, []string{
		"c1++_repo_rules+c1_tool",
		"rules_go+",
		"rules_go++go_sdk+c1_go_sdk",
	}, []string{
		vendor.Repos[0].CanonicalRepo.String(),
		vendor.Repos[1].CanonicalRepo.String(),
		vendor.Repos[2].CanonicalRepo.String(),
	})
	require.False(t, vendor.Repos[1].Pinned)
	require.True(t, vendor.Repos[2].Pinned)
	require.Len(t, vendor.Registries, 1)
	require.Equal(t, "https://bcr.bazel.build/", vendor.Registries[0].URL)
	require.Len(t, vendor.Registries[0].Files, 1)
	require.Equal(t, "bazel_registry.json", vendor.Registries[0].Files[0].Path)
}

func TestScanVendorDirectoryMapsBuiltinPlatformsRepository(t *testing.T) {
	workspace := t.TempDir()
	vendorDirectory := filepath.Join(workspace, "vendor")
	platformsDirectory := filepath.Join(vendorDirectory, "platforms")
	require.NoError(t, os.MkdirAll(filepath.Join(platformsDirectory, "host"), 0o755))
	buildContents := []byte("platform(name = \"host\")\n")
	require.NoError(t, os.WriteFile(filepath.Join(platformsDirectory, "host", "BUILD"), buildContents, 0o644))
	writeVendoredRepo(t, vendorDirectory, "platforms")
	buildHash := sha256.Sum256(buildContents)
	writeVendoredRepo(t, vendorDirectory, "rules_go+", "FILE:@@platforms//host:BUILD "+hex.EncodeToString(buildHash[:]))

	vendor, err := ScanVendorDirectory(path.LocalFormat.NewParser(workspace), "vendor", nil, false)
	require.NoError(t, err)
	require.Len(t, vendor.Repos, 2)
	require.Equal(t, "platforms+", vendor.Repos[0].CanonicalRepo.String())
	require.Equal(t, platformsDirectory, vendor.Repos[0].RootPath)
	require.False(t, vendor.Repos[0].Pinned)
	require.NoError(t, os.WriteFile(filepath.Join(platformsDirectory, "host", "BUILD"), []byte("changed\n"), 0o644))
	_, err = ScanVendorDirectory(path.LocalFormat.NewParser(workspace), "vendor", nil, false)
	require.ErrorContains(t, err, "FILE input \"@@platforms//host:BUILD\" no longer matches its recorded SHA-256")
}

func TestScanVendorDirectoryMapsBuiltinBazelToolsExtensionRepo(t *testing.T) {
	workspace := t.TempDir()
	vendorDirectory := filepath.Join(workspace, "vendor")
	writeVendoredRepo(t, vendorDirectory, "bazel_tools+winsdk_configure+local_config_winsdk")
	require.NoError(t, os.WriteFile(
		filepath.Join(vendorDirectory, "VENDOR.bazel"),
		[]byte(`pin("@@bazel_tools+winsdk_configure+local_config_winsdk")`),
		0o644,
	))

	vendor, err := ScanVendorDirectory(path.LocalFormat.NewParser(workspace), "vendor", nil, false)
	require.NoError(t, err)
	require.Len(t, vendor.Repos, 1)
	require.Equal(t, "bazel_tools++winsdk_configure+local_config_winsdk", vendor.Repos[0].CanonicalRepo.String())
	require.Equal(t, filepath.Join(vendorDirectory, "bazel_tools+winsdk_configure+local_config_winsdk"), vendor.Repos[0].RootPath)
	require.True(t, vendor.Repos[0].Pinned)
}

func TestScanVendorDirectoryMapsBuiltinPlatformsExtensionRepo(t *testing.T) {
	workspace := t.TempDir()
	vendorDirectory := filepath.Join(workspace, "vendor")
	writeVendoredRepo(t, vendorDirectory, "platforms+host_platform+host_platform")
	require.NoError(t, os.WriteFile(
		filepath.Join(vendorDirectory, "VENDOR.bazel"),
		[]byte(`pin("@@platforms+host_platform+host_platform")`),
		0o644,
	))

	vendor, err := ScanVendorDirectory(path.LocalFormat.NewParser(workspace), "vendor", nil, false)
	require.NoError(t, err)
	require.Len(t, vendor.Repos, 1)
	require.Equal(t, "platforms++host_platform+host_platform", vendor.Repos[0].CanonicalRepo.String())
	require.Equal(t, filepath.Join(vendorDirectory, "platforms+host_platform+host_platform"), vendor.Repos[0].RootPath)
	require.True(t, vendor.Repos[0].Pinned)
}

func TestScanVendorDirectoryValidatesBuiltinPlatformsRepository(t *testing.T) {
	t.Run("MalformedMarker", func(t *testing.T) {
		workspace := t.TempDir()
		vendorDirectory := filepath.Join(workspace, "vendor")
		require.NoError(t, os.MkdirAll(filepath.Join(vendorDirectory, "platforms"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(vendorDirectory, "@platforms.marker"), []byte("invalid fingerprint\n"), 0o644))

		_, err := ScanVendorDirectory(path.LocalFormat.NewParser(workspace), "vendor", nil, false)
		require.ErrorContains(t, err, "invalid repository fingerprint")
	})

	t.Run("MissingDirectory", func(t *testing.T) {
		workspace := t.TempDir()
		vendorDirectory := filepath.Join(workspace, "vendor")
		require.NoError(t, os.MkdirAll(vendorDirectory, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(vendorDirectory, "@platforms.marker"), []byte(vendorMarkerFingerprint+"\n"), 0o644))

		_, err := ScanVendorDirectory(path.LocalFormat.NewParser(workspace), "vendor", nil, false)
		require.ErrorContains(t, err, "does not have matching directory \"platforms\"")
	})

	t.Run("MissingMarker", func(t *testing.T) {
		workspace := t.TempDir()
		vendorDirectory := filepath.Join(workspace, "vendor")
		require.NoError(t, os.MkdirAll(filepath.Join(vendorDirectory, "platforms"), 0o755))

		_, err := ScanVendorDirectory(path.LocalFormat.NewParser(workspace), "vendor", nil, false)
		require.ErrorContains(t, err, "does not have matching marker \"@platforms.marker\"")
	})

	t.Run("NonCanonicalName", func(t *testing.T) {
		workspace := t.TempDir()
		vendorDirectory := filepath.Join(workspace, "vendor")
		require.NoError(t, os.MkdirAll(vendorDirectory, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(vendorDirectory, "@other.marker"), []byte(vendorMarkerFingerprint+"\n"), 0o644))

		_, err := ScanVendorDirectory(path.LocalFormat.NewParser(workspace), "vendor", nil, false)
		require.ErrorContains(t, err, "does not contain a valid canonical repository name")
	})

	t.Run("DuplicateCanonicalRepo", func(t *testing.T) {
		workspace := t.TempDir()
		vendorDirectory := filepath.Join(workspace, "vendor")
		writeVendoredRepo(t, vendorDirectory, "platforms")
		writeVendoredRepo(t, vendorDirectory, "platforms+")

		_, err := ScanVendorDirectory(path.LocalFormat.NewParser(workspace), "vendor", nil, false)
		require.ErrorContains(t, err, "both map to canonical repository \"@@platforms+\"")
	})
}

func TestScanVendorDirectoryHonorsBuiltinPlatformsPinAndIgnore(t *testing.T) {
	workspace := t.TempDir()
	vendorDirectory := filepath.Join(workspace, "vendor")
	writeVendoredRepo(t, vendorDirectory, "platforms")
	require.NoError(t, os.WriteFile(filepath.Join(vendorDirectory, "VENDOR.bazel"), []byte(`pin("@@platforms")`), 0o644))

	vendor, err := ScanVendorDirectory(path.LocalFormat.NewParser(workspace), "vendor", nil, false)
	require.NoError(t, err)
	require.Len(t, vendor.Repos, 1)
	require.Equal(t, "platforms+", vendor.Repos[0].CanonicalRepo.String())
	require.True(t, vendor.Repos[0].Pinned)

	require.NoError(t, os.WriteFile(filepath.Join(vendorDirectory, "VENDOR.bazel"), []byte("pin(\"@@platforms\")\nignore(\"@@platforms+\")"), 0o644))
	_, err = ScanVendorDirectory(path.LocalFormat.NewParser(workspace), "vendor", nil, false)
	require.ErrorContains(t, err, "cannot be both pinned and ignored")
}

func TestScanVendorDirectoryRejectsMissingMarkerOrDirectory(t *testing.T) {
	t.Run("MissingDirectory", func(t *testing.T) {
		workspace := t.TempDir()
		vendorDirectory := filepath.Join(workspace, "vendor")
		require.NoError(t, os.MkdirAll(vendorDirectory, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(vendorDirectory, "@rules_go+.marker"), []byte(vendorMarkerFingerprint+"\n"), 0o644))

		_, err := ScanVendorDirectory(path.LocalFormat.NewParser(workspace), "vendor", nil, false)
		require.ErrorContains(t, err, "does not have matching directory")
	})

	t.Run("MissingMarker", func(t *testing.T) {
		workspace := t.TempDir()
		vendorDirectory := filepath.Join(workspace, "vendor")
		require.NoError(t, os.MkdirAll(filepath.Join(vendorDirectory, "rules_go+"), 0o755))

		_, err := ScanVendorDirectory(path.LocalFormat.NewParser(workspace), "vendor", nil, false)
		require.ErrorContains(t, err, "does not have matching marker")
	})
}

func TestScanVendorDirectoryHonorsIgnoredRepository(t *testing.T) {
	workspace := t.TempDir()
	vendorDirectory := filepath.Join(workspace, "vendor")
	require.NoError(t, os.MkdirAll(vendorDirectory, 0o755))
	writeVendoredRepo(t, vendorDirectory, "rules_go+")
	require.NoError(t, os.WriteFile(filepath.Join(vendorDirectory, "VENDOR.bazel"), []byte(`ignore("@@rules_go+")`), 0o644))

	vendor, err := ScanVendorDirectory(path.LocalFormat.NewParser(workspace), "vendor", nil, false)
	require.NoError(t, err)
	require.Empty(t, vendor.Repos)
}

func TestScanVendorDirectoryRejectsPinIgnoreConflict(t *testing.T) {
	workspace := t.TempDir()
	vendorDirectory := filepath.Join(workspace, "vendor")
	require.NoError(t, os.MkdirAll(vendorDirectory, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(vendorDirectory, "VENDOR.bazel"), []byte("pin(\"@@rules_go+\")\nignore(\"@@rules_go+\")"), 0o644))

	_, err := ScanVendorDirectory(path.LocalFormat.NewParser(workspace), "vendor", nil, false)
	require.ErrorContains(t, err, "cannot be both pinned and ignored")
}

func TestScanVendorDirectoryRejectsStaleMarkerUnlessPinned(t *testing.T) {
	workspace := t.TempDir()
	vendorDirectory := filepath.Join(workspace, "vendor")
	patchPath := filepath.Join(workspace, "patches", "rules_go.patch")
	require.NoError(t, os.MkdirAll(filepath.Dir(patchPath), 0o755))
	require.NoError(t, os.MkdirAll(vendorDirectory, 0o755))
	require.NoError(t, os.WriteFile(patchPath, []byte("original"), 0o644))
	expectedHash := sha256.Sum256([]byte("original"))
	writeVendoredRepo(t, vendorDirectory, "rules_go+", "FILE:@@//patches/rules_go.patch "+hex.EncodeToString(expectedHash[:]))
	require.NoError(t, os.WriteFile(patchPath, []byte("changed"), 0o644))

	_, err := ScanVendorDirectory(path.LocalFormat.NewParser(workspace), "vendor", nil, false)
	require.ErrorContains(t, err, "no longer matches its recorded SHA-256")

	require.NoError(t, os.WriteFile(filepath.Join(vendorDirectory, "VENDOR.bazel"), []byte(`pin("@@rules_go+")`), 0o644))
	vendor, err := ScanVendorDirectory(path.LocalFormat.NewParser(workspace), "vendor", nil, false)
	require.NoError(t, err)
	require.Len(t, vendor.Repos, 1)
	require.True(t, vendor.Repos[0].Pinned)
}

func TestScanVendorDirectoryRejectsCorruptRegistryLockHash(t *testing.T) {
	workspace := t.TempDir()
	registry := filepath.Join(workspace, "vendor", "_registries", "bcr.bazel.build")
	require.NoError(t, os.MkdirAll(registry, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(registry, "bazel_registry.json"), []byte("{}\n"), 0o644))
	lockfile, err := json.Marshal(map[string]any{
		"registryFileHashes": map[string]string{
			"https://bcr.bazel.build/bazel_registry.json": hex.EncodeToString(make([]byte, sha256.Size)),
		},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "MODULE.bazel.lock"), lockfile, 0o644))

	_, err = ScanVendorDirectory(path.LocalFormat.NewParser(workspace), "vendor", []string{"https://bcr.bazel.build/"}, true)
	require.ErrorContains(t, err, "does not match the SHA-256 recorded in MODULE.bazel.lock")
}

const vendorMarkerFingerprint = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func writeVendoredRepo(t *testing.T, vendorDirectory, canonicalRepo string, markerLines ...string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(vendorDirectory, canonicalRepo), 0o755))
	marker := vendorMarkerFingerprint + "\n"
	for _, markerLine := range markerLines {
		marker += markerLine + "\n"
	}
	require.NoError(t, os.WriteFile(filepath.Join(vendorDirectory, "@"+canonicalRepo+".marker"), []byte(marker), 0o644))
}

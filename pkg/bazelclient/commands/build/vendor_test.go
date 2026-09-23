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
	require.Len(t, vendor.Registries, 1)
	require.Equal(t, "https://bcr.bazel.build/", vendor.Registries[0].URL)
	require.Len(t, vendor.Registries[0].Files, 1)
	require.Equal(t, "bazel_registry.json", vendor.Registries[0].Files[0].Path)
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

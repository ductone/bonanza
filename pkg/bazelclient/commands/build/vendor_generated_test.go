package build

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buildbarn/bb-storage/pkg/filesystem/path"
	"github.com/stretchr/testify/require"
)

// writeLockfile writes a MODULE.bazel.lock that records the given
// module extension results, so tests exercise the same lockfile-verified
// path production uses.
func writeLockfile(t *testing.T, workspace string, moduleExtensions map[string]any) {
	t.Helper()
	contents, err := json.Marshal(map[string]any{
		"lockFileVersion":    28,
		"registryFileHashes": map[string]string{},
		"moduleExtensions":   moduleExtensions,
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "MODULE.bazel.lock"), contents, 0o644))
}

// writeGeneratedRepoManifest writes the Bonanza-only import manifest.
func writeGeneratedRepoManifest(t *testing.T, vendorDirectory string, manifest map[string]any) {
	t.Helper()
	if platform, ok := manifest["platform"]; !ok || platform == "" {
		manifest["platform"] = "linux_arm64"
	}
	if _, ok := manifest["version"]; !ok {
		manifest["version"] = 1
	}
	contents, err := json.Marshal(manifest)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(vendorDirectory, generatedRepoManifestFilename), contents, 0o644))
}

// writeModuleSource writes a vendored module with a MODULE.bazel that
// declares the given use_extension()/use_repo() pairs.
func writeModuleSource(t *testing.T, vendorDirectory, canonicalModule, moduleDotBazel string) {
	t.Helper()
	directory := filepath.Join(vendorDirectory, canonicalModule)
	require.NoError(t, os.MkdirAll(directory, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(directory, "MODULE.bazel"), []byte(moduleDotBazel), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(vendorDirectory, "@"+canonicalModule+".marker"), []byte(vendorMarkerFingerprint+"\n"), 0o644))
}

// generatedRepoImportEntry builds a manifest entry, computing the digest
// of the tree that is already in place.
func generatedRepoImportEntry(t *testing.T, vendorDirectory, canonicalRepo, extension string, extra map[string]any) map[string]any {
	t.Helper()
	digest, err := GeneratedRepoTreeSHA256(filepath.Join(vendorDirectory, canonicalRepo))
	require.NoError(t, err)
	entry := map[string]any{
		"canonical_repo": canonicalRepo,
		"extension":      extension,
		"sha256":         hex.EncodeToString(digest),
	}
	for key, value := range extra {
		entry[key] = value
	}
	return entry
}

func TestScanVendorDirectoryRejectsUnjustifiedGeneratedRepoInStrictMode(t *testing.T) {
	workspace := t.TempDir()
	vendorDirectory := filepath.Join(workspace, "vendor")
	writeVendoredRepo(t, vendorDirectory, "gazelle++go_deps+com_github_example_one")
	writeVendoredRepo(t, vendorDirectory, "gazelle++go_deps+com_github_example_two")

	vendor, err := ScanVendorDirectory(path.LocalFormat.NewParser(workspace), "vendor", nil, false, false)
	require.NoError(t, err)
	require.Len(t, vendor.GeneratedRepoClosure, 2)

	// A hand-written vendor marker is not evidence that a generated
	// repository is a reviewed, portable input, and strict mode reports the
	// complete set of unjustified repositories at once rather than the
	// first one an analysis happens to need.
	_, err = ScanVendorDirectory(path.LocalFormat.NewParser(workspace), "vendor", nil, false, true)
	require.ErrorContains(t, err, "strict vendor mode requires every generated repository in the snapshot to be justified")
	require.ErrorContains(t, err, "@@gazelle++go_deps+com_github_example_one")
	require.ErrorContains(t, err, "@@gazelle++go_deps+com_github_example_two")
	require.ErrorContains(t, err, "2 are not")
}

func TestScanVendorDirectoryAcceptsLockfileJustifiedGeneratedRepo(t *testing.T) {
	workspace := t.TempDir()
	vendorDirectory := filepath.Join(workspace, "vendor")
	writeVendoredRepo(t, vendorDirectory, "yq.bzl++yq+yq_linux_arm64")
	writeLockfile(t, workspace, map[string]any{
		"@@yq.bzl+//yq:extensions.bzl%yq": map[string]any{
			"general": map[string]any{
				"generatedRepoSpecs": map[string]any{
					"yq_linux_arm64": map[string]any{
						"repoRuleId": "@@yq.bzl+//yq/toolchain:platforms.bzl%yq_platform_repo",
						"attributes": map[string]any{"platform": "linux_arm64"},
					},
				},
			},
		},
	})

	vendor, err := ScanVendorDirectory(path.LocalFormat.NewParser(workspace), "vendor", nil, true, true)
	require.NoError(t, err)
	require.Len(t, vendor.GeneratedRepoClosure, 1)
	require.Equal(t, "yq.bzl++yq+yq_linux_arm64", vendor.GeneratedRepoClosure[0].CanonicalRepo)
	require.Equal(t, []string{"lockfile", "snapshot"}, vendor.GeneratedRepoClosure[0].Sources)
	require.True(t, vendor.GeneratedRepoClosure[0].Present)
}

func TestScanVendorDirectoryAcceptsUseRepoJustifiedGeneratedRepo(t *testing.T) {
	workspace := t.TempDir()
	vendorDirectory := filepath.Join(workspace, "vendor")
	writeModuleSource(t, vendorDirectory, "rules_go+", `
module(name = "rules_go")
go_sdk = use_extension("//go:extensions.bzl", "go_sdk")
use_repo(go_sdk, "go_toolchains")
`)
	writeVendoredRepo(t, vendorDirectory, "rules_go++go_sdk+go_toolchains")

	vendor, err := ScanVendorDirectory(path.LocalFormat.NewParser(workspace), "vendor", nil, false, true)
	require.NoError(t, err)
	require.Len(t, vendor.GeneratedRepoClosure, 1)
	require.Equal(t, []string{"snapshot", "use_extension:rules_go+"}, vendor.GeneratedRepoClosure[0].Sources)
}

func TestScanVendorDirectoryImportsGeneratedRepo(t *testing.T) {
	workspace := t.TempDir()
	vendorDirectory := filepath.Join(workspace, "vendor")
	writeModuleSource(t, vendorDirectory, "rules_java+", `
module(name = "rules_java")
toolchains = use_extension("//java:extensions.bzl", "toolchains")
use_repo(toolchains, "remote_java_tools")
`)
	// An imported repository is materialized by trusted CI and therefore
	// carries no Bazel vendor marker.
	require.NoError(t, os.MkdirAll(filepath.Join(vendorDirectory, "rules_java++toolchains+remote_java_tools"), 0o755))
	writeGeneratedRepoManifest(t, vendorDirectory, map[string]any{
		"repos": []map[string]any{
			generatedRepoImportEntry(t, vendorDirectory,
				"rules_java++toolchains+remote_java_tools",
				"@@rules_java+//java:extensions.bzl%toolchains",
				nil),
		},
	})

	vendor, err := ScanVendorDirectory(path.LocalFormat.NewParser(workspace), "vendor", nil, false, true)
	require.NoError(t, err)
	require.Len(t, vendor.GeneratedRepoImports, 1)
	require.Equal(t, "rules_java++toolchains+remote_java_tools", vendor.GeneratedRepoImports[0].CanonicalRepo.String())
	require.Equal(t, "linux_arm64", vendor.GeneratedRepoImports[0].Platform)
	// Imported repositories are pinned, so their contents override normal
	// module and extension resolution without evaluating the extension.
	require.Len(t, vendor.Repos, 2)
	require.Equal(t, "rules_java+", vendor.Repos[0].CanonicalRepo.String())
	require.False(t, vendor.Repos[0].Pinned)
	require.Equal(t, "rules_java++toolchains+remote_java_tools", vendor.Repos[1].CanonicalRepo.String())
	require.True(t, vendor.Repos[1].Pinned)
	t.Run("Closure", func(t *testing.T) {
		require.Len(t, vendor.GeneratedRepoClosure, 1)
		// "snapshot" is a Bazel marker/directory pair; an import is
		// materialized without a marker, and says so through its own source.
		require.Equal(t, []string{"import", "use_extension:rules_java+"}, vendor.GeneratedRepoClosure[0].Sources)
		require.True(t, vendor.GeneratedRepoClosure[0].Present)
	})
}

func TestScanVendorDirectoryRejectsUncorroboratedGeneratedRepoImport(t *testing.T) {
	workspace := t.TempDir()
	vendorDirectory := filepath.Join(workspace, "vendor")
	require.NoError(t, os.MkdirAll(filepath.Join(vendorDirectory, "rules_java++toolchains+remote_java_tools"), 0o755))
	writeGeneratedRepoManifest(t, vendorDirectory, map[string]any{
		"repos": []map[string]any{
			generatedRepoImportEntry(t, vendorDirectory,
				"rules_java++toolchains+remote_java_tools",
				"@@rules_java+//java:extensions.bzl%toolchains",
				nil),
		},
	})

	_, err := ScanVendorDirectory(path.LocalFormat.NewParser(workspace), "vendor", nil, false, true)
	require.ErrorContains(t, err, "which neither MODULE.bazel.lock nor a use_extension()/use_repo_rule() declaration in the root or a vendored module corroborates")
}

func TestScanVendorDirectoryRejectsCorruptGeneratedRepoImport(t *testing.T) {
	workspace := t.TempDir()
	vendorDirectory := filepath.Join(workspace, "vendor")
	writeModuleSource(t, vendorDirectory, "rules_java+", `
module(name = "rules_java")
toolchains = use_extension("//java:extensions.bzl", "toolchains")
use_repo(toolchains, "remote_java_tools")
`)
	directory := filepath.Join(vendorDirectory, "rules_java++toolchains+remote_java_tools")
	require.NoError(t, os.MkdirAll(directory, 0o755))
	writeGeneratedRepoManifest(t, vendorDirectory, map[string]any{
		"repos": []map[string]any{
			generatedRepoImportEntry(t, vendorDirectory,
				"rules_java++toolchains+remote_java_tools",
				"@@rules_java+//java:extensions.bzl%toolchains",
				nil),
		},
	})

	// Any change to the tree after it was reviewed invalidates its digest.
	require.NoError(t, os.WriteFile(filepath.Join(directory, "MODULE.bazel"), []byte("module(name = \"rules_java\")\n"), 0o644))
	_, err := ScanVendorDirectory(path.LocalFormat.NewParser(workspace), "vendor", nil, false, true)
	require.ErrorContains(t, err, "does not match its recorded SHA-256")
}

func TestScanVendorDirectoryRejectsIgnoredGeneratedRepoImport(t *testing.T) {
	workspace := t.TempDir()
	vendorDirectory := filepath.Join(workspace, "vendor")
	writeModuleSource(t, vendorDirectory, "rules_java+", `
module(name = "rules_java")
toolchains = use_extension("//java:extensions.bzl", "toolchains")
use_repo(toolchains, "remote_java_tools")
`)
	require.NoError(t, os.MkdirAll(filepath.Join(vendorDirectory, "rules_java++toolchains+remote_java_tools"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(vendorDirectory, "VENDOR.bazel"),
		[]byte("ignore(\"@@rules_java++toolchains+remote_java_tools\")\n"),
		0o644,
	))
	writeGeneratedRepoManifest(t, vendorDirectory, map[string]any{
		"repos": []map[string]any{
			generatedRepoImportEntry(t, vendorDirectory,
				"rules_java++toolchains+remote_java_tools",
				"@@rules_java+//java:extensions.bzl%toolchains",
				nil),
		},
	})

	_, err := ScanVendorDirectory(path.LocalFormat.NewParser(workspace), "vendor", nil, false, true)
	require.ErrorContains(t, err, "is ignored by VENDOR.bazel, so it cannot also be imported")
}

func TestScanVendorDirectoryRejectsMixedPlatformGeneratedRepoImport(t *testing.T) {
	workspace := t.TempDir()
	vendorDirectory := filepath.Join(workspace, "vendor")
	writeModuleSource(t, vendorDirectory, "rules_java+", `
module(name = "rules_java")
toolchains = use_extension("//java:extensions.bzl", "toolchains")
use_repo(toolchains, "remote_java_tools")
`)
	require.NoError(t, os.MkdirAll(filepath.Join(vendorDirectory, "rules_java++toolchains+remote_java_tools"), 0o755))
	writeGeneratedRepoManifest(t, vendorDirectory, map[string]any{
		"platform": "linux_arm64",
		"repos": []map[string]any{
			generatedRepoImportEntry(t, vendorDirectory,
				"rules_java++toolchains+remote_java_tools",
				"@@rules_java+//java:extensions.bzl%toolchains",
				map[string]any{"platform": "linux_amd64"}),
		},
	})

	_, err := ScanVendorDirectory(path.LocalFormat.NewParser(workspace), "vendor", nil, false, true)
	require.ErrorContains(t, err, "a bundle must not mix platforms")
}

func TestScanVendorDirectoryRejectsUnknownGeneratedRepoPlatform(t *testing.T) {
	workspace := t.TempDir()
	vendorDirectory := filepath.Join(workspace, "vendor")
	writeModuleSource(t, vendorDirectory, "rules_java+", `
module(name = "rules_java")
toolchains = use_extension("//java:extensions.bzl", "toolchains")
use_repo(toolchains, "remote_java_tools")
`)
	require.NoError(t, os.MkdirAll(filepath.Join(vendorDirectory, "rules_java++toolchains+remote_java_tools"), 0o755))
	writeGeneratedRepoManifest(t, vendorDirectory, map[string]any{
		"platform": "plan9_z80",
		"repos":    []map[string]any{},
	})

	_, err := ScanVendorDirectory(path.LocalFormat.NewParser(workspace), "vendor", nil, false, true)
	require.ErrorContains(t, err, "has unsupported operating system \"plan9\"")
}

func TestScanVendorDirectoryRejectsAbsoluteSymlinkInGeneratedRepoImport(t *testing.T) {
	workspace := t.TempDir()
	vendorDirectory := filepath.Join(workspace, "vendor")
	writeModuleSource(t, vendorDirectory, "rules_java+", `
module(name = "rules_java")
toolchains = use_extension("//java:extensions.bzl", "toolchains")
use_repo(toolchains, "local_jdk")
`)
	directory := filepath.Join(vendorDirectory, "rules_java++toolchains+local_jdk")
	require.NoError(t, os.MkdirAll(directory, 0o755))
	// A host cache copy symlinks to absolute host paths. That is not a
	// portable production input, so the tree is rejected before its digest
	// can legitimize it.
	require.NoError(t, os.Symlink("/host/jdk/bin/java", filepath.Join(directory, "java")))
	writeGeneratedRepoManifest(t, vendorDirectory, map[string]any{
		"repos": []map[string]any{
			{
				"canonical_repo": "rules_java++toolchains+local_jdk",
				"extension":      "@@rules_java+//java:extensions.bzl%toolchains",
				"sha256":         strings.Repeat("0", 64),
			},
		},
	})

	_, err := ScanVendorDirectory(path.LocalFormat.NewParser(workspace), "vendor", nil, false, true)
	require.ErrorContains(t, err, "absolute symbolic link")
}

func TestScanVendorDirectoryReportsCompleteGeneratedRepoClosure(t *testing.T) {
	workspace := t.TempDir()
	vendorDirectory := filepath.Join(workspace, "vendor")
	// The root module declares two repositories; the snapshot supplies only
	// one. The closure reports both, marking the absent one as missing, so
	// a bundle can be completed without one failed build per repository.
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "MODULE.bazel"), []byte(`
module(name = "example")
python = use_extension("@rules_python//python/extensions:python.bzl", "python")
use_repo(python, "python_3_13", "python_3_12")
`), 0o644))
	writeModuleSource(t, vendorDirectory, "rules_python+", `
module(name = "rules_python")
`)
	writeVendoredRepo(t, vendorDirectory, "rules_python++python+python_3_13")

	vendor, err := ScanVendorDirectory(path.LocalFormat.NewParser(workspace), "vendor", nil, false, false)
	require.NoError(t, err)
	require.Len(t, vendor.GeneratedRepoClosure, 2)
	require.Equal(t, "rules_python++python+python_3_12", vendor.GeneratedRepoClosure[0].CanonicalRepo)
	require.False(t, vendor.GeneratedRepoClosure[0].Present)
	require.Equal(t, []string{"use_extension:example+"}, vendor.GeneratedRepoClosure[0].Sources)
	require.Equal(t, "rules_python++python+python_3_13", vendor.GeneratedRepoClosure[1].CanonicalRepo)
	require.True(t, vendor.GeneratedRepoClosure[1].Present)
}

func TestGeneratedRepoTreeSHA256IsDeterministicAndContentAddressed(t *testing.T) {
	directory := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(directory, "pkg"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(directory, "pkg", "BUILD"), []byte("filegroup()\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(directory, "MODULE.bazel"), []byte("module(name = \"x\")\n"), 0o644))

	first, err := GeneratedRepoTreeSHA256(directory)
	require.NoError(t, err)
	second, err := GeneratedRepoTreeSHA256(directory)
	require.NoError(t, err)
	require.Equal(t, hex.EncodeToString(first), hex.EncodeToString(second))

	// The executable bit is part of the identity, so a tree cannot be
	// smuggled past review by dropping it.
	require.NoError(t, os.Chmod(filepath.Join(directory, "MODULE.bazel"), 0o755))
	third, err := GeneratedRepoTreeSHA256(directory)
	require.NoError(t, err)
	require.NotEqual(t, hex.EncodeToString(first), hex.EncodeToString(third))
}

func TestVendorRepoDirectoryNameRoundTripsThroughScanner(t *testing.T) {
	for _, canonicalRepo := range []string{
		"platforms+",
		"platforms++host_platform+host_platform",
		"bazel_tools++winsdk_configure+local_config_winsdk",
		"rules_java++toolchains+remote_java_tools",
	} {
		parsed, err := parseVendorRepoName(canonicalRepo)
		require.NoError(t, err)
		roundTripped, err := parseVendorRepoName(vendorRepoDirectoryName(parsed))
		require.NoError(t, err)
		require.Equal(t, canonicalRepo, roundTripped.String())
	}
}

package build

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	pathpkg "path"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"bonanza.build/pkg/label"

	"github.com/buildbarn/bb-storage/pkg/filesystem/path"

	"go.starlark.net/syntax"
)

// VendoredRepo is a repository tree found in a Bazel --vendor_dir. CanonicalRepo
// is deliberately a CanonicalRepo rather than a Module: vendor directories also
// contain module-extension and use_repo_rule repositories.
type VendoredRepo struct {
	CanonicalRepo label.CanonicalRepo
	RootPath      string
	Pinned        bool
}

// VendoredRegistryFile is a registry file whose hash was checked against the
// root MODULE.bazel.lock before it was uploaded.
type VendoredRegistryFile struct {
	Path   string
	SHA256 []byte
}

// VendoredRegistry is one registry mirror under _registries. URL is normalized
// to a scheme, host, and trailing slash so it can be matched unambiguously by
// the analysis worker.
type VendoredRegistry struct {
	URL      string
	RootPath string
	Files    []VendoredRegistryFile
}

// VendorDirectory describes the locally scanned portion of a Bazel vendor
// directory. RootRelativePath is set only when the directory is below the
// workspace; callers must exclude it from the root source upload to avoid
// duplicating all vendored sources.
type VendorDirectory struct {
	RootPath         string
	RootRelativePath string
	Repos            []VendoredRepo
	Registries       []VendoredRegistry
	FlagAliases      map[string]string
	// GeneratedRepoImports lists the generated repositories that
	// generated_repos.json declares, verifies against MODULE.bazel.lock or
	// a use_repo()/use_repo_rule() declaration, scopes to a platform and
	// pins.
	GeneratedRepoImports []GeneratedRepoImport
	// GeneratedRepoClosure is the complete set of generated repositories
	// the pinned inputs can produce, together with the sources that require
	// them and whether the snapshot supplies them.
	GeneratedRepoClosure []GeneratedRepoClosureEntry
}

type vendorConfiguration struct {
	pinnedRepos  map[string]struct{}
	ignoredRepos map[string]struct{}
}

type vendorMarkerFile struct {
	label  string
	sha256 []byte
}

type vendorRepoRecord struct {
	repo        VendoredRepo
	markerFiles []vendorMarkerFile
}

type vendorLockfile struct {
	RegistryFileHashes map[string]string `json:"registryFileHashes"`
	// ModuleExtensions records the repositories Bazel saw each module
	// extension produce, together with the attributes it produced them
	// from. Only extensions that MODULE.bazel.lock contains can justify a
	// generated repository as a lockfile record.
	ModuleExtensions map[string]vendorLockfileModuleExtension `json:"moduleExtensions"`
}

type vendorLockfileModuleExtension struct {
	General vendorLockfileModuleExtensionGeneral `json:"general"`
}

type vendorLockfileModuleExtensionGeneral struct {
	GeneratedRepoSpecs map[string]vendorLockfileRepoSpec `json:"generatedRepoSpecs"`
}

type vendorLockfileRepoSpec struct {
	RepoRuleID string         `json:"repoRuleId"`
	Attributes map[string]any `json:"attributes"`
}

// readVendorLockfile loads MODULE.bazel.lock. Registry hashes and the
// module extension records that justify generated repositories are read
// from the same document, so a malformed lockfile can never justify part
// of the snapshot.
func readVendorLockfile(workspacePath string) (*vendorLockfile, error) {
	lockfilePath := filepath.Join(workspacePath, "MODULE.bazel.lock")
	contents, err := os.ReadFile(lockfilePath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, errors.New("--vendor_dir with --lockfile_mode=error requires MODULE.bazel.lock")
		}
		return nil, fmt.Errorf("read MODULE.bazel.lock: %w", err)
	}
	var lockfile vendorLockfile
	if err := json.Unmarshal(contents, &lockfile); err != nil {
		return nil, fmt.Errorf("parse MODULE.bazel.lock: %w", err)
	}
	return &lockfile, nil
}

// ScanVendorDirectory validates a Bazel --vendor_dir before any source is
// uploaded. Bonanza deliberately implements a closed vendor snapshot: ignored
// repositories and missing/stale inputs are errors rather than a silent remote
// fallback. The caller supplies the effective registry order, which is also the
// order the worker uses for module version selection.
//
// When strictVendorMode is set, every generated repository in the snapshot must
// additionally be justified by MODULE.bazel.lock, a use_repo()/use_repo_rule()
// declaration in the root or a vendored module, a VENDOR.bazel pin() or a
// generated_repos.json import. A hand-written Bazel vendor marker alone is not
// evidence that a repository is a reviewed, portable input.
func ScanVendorDirectory(workspacePath path.Parser, vendorDir string, registryURLs []string, requireLockfile bool, strictVendorMode bool) (*VendorDirectory, error) {
	if vendorDir == "" {
		return nil, nil
	}

	workspacePathStr, err := ResolveToAbsoluteString(workspacePath)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace path: %w", err)
	}
	vendorPath := vendorDir
	if !filepath.IsAbs(vendorPath) {
		vendorPath = filepath.Join(workspacePathStr, vendorPath)
	}
	vendorPath = filepath.Clean(vendorPath)
	vendorInfo, err := os.Stat(vendorPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("--vendor_dir=%q does not exist", vendorDir)
		}
		return nil, fmt.Errorf("stat --vendor_dir=%q: %w", vendorDir, err)
	}
	if !vendorInfo.IsDir() {
		return nil, fmt.Errorf("--vendor_dir=%q is not a directory", vendorDir)
	}

	rootRelativePath := ""
	if relativePath, err := filepath.Rel(workspacePathStr, vendorPath); err != nil {
		return nil, fmt.Errorf("determine --vendor_dir path relative to workspace: %w", err)
	} else if relativePath == "." {
		return nil, errors.New("--vendor_dir must not be the workspace root, because its sources cannot be excluded from the root upload")
	} else if filepath.IsLocal(relativePath) {
		rootRelativePath = filepath.ToSlash(relativePath)
	}

	configuration, err := loadVendorConfiguration(filepath.Join(vendorPath, "VENDOR.bazel"))
	if err != nil {
		return nil, err
	}
	manifest, err := loadGeneratedRepoManifest(vendorPath)
	if err != nil {
		return nil, err
	}
	importedDirectoryNames := manifestDirectoryNames(manifest)
	reposByName, err := scanVendoredRepos(vendorPath, configuration, importedDirectoryNames)
	if err != nil {
		return nil, err
	}
	if err := validateVendoredRepoMarkers(workspacePathStr, reposByName, configuration); err != nil {
		return nil, err
	}

	var lockfile *vendorLockfile
	if requireLockfile {
		if lockfile, err = readVendorLockfile(workspacePathStr); err != nil {
			return nil, err
		}
	}
	declarations, err := scanGeneratedRepoDeclarations(workspacePathStr, vendorPath, reposByName, lockfile, path.LocalFormat)
	if err != nil {
		return nil, err
	}
	imports, err := scanGeneratedRepoImports(vendorPath, manifest, reposByName, configuration, declarations)
	if err != nil {
		return nil, err
	}
	if strictVendorMode {
		if err := validateGeneratedRepoProvenance(declarations, imports.closure); err != nil {
			return nil, err
		}
	}
	for _, importedRepo := range imports.imports {
		canonicalRepoName := importedRepo.CanonicalRepo.String()
		reposByName[canonicalRepoName] = vendorRepoRecord{
			repo: VendoredRepo{
				CanonicalRepo: importedRepo.CanonicalRepo,
				RootPath:      filepath.Join(vendorPath, importedRepo.DirectoryName),
				Pinned:        true,
			},
		}
	}

	registries, err := scanVendoredRegistries(vendorPath, registryURLs)
	if err != nil {
		return nil, err
	}
	if requireLockfile {
		if err := validateVendoredRegistryLockfile(workspacePathStr, registries); err != nil {
			return nil, err
		}
	}

	repoNames := mapsKeys(reposByName)
	slices.SortFunc(repoNames, func(a, b string) int {
		return strings.Compare(a, b)
	})
	repos := make([]VendoredRepo, 0, len(repoNames))
	for _, repoName := range repoNames {
		repos = append(repos, reposByName[repoName].repo)
	}
	aliases, err := scanVendoredFlagAliases(repos)
	if err != nil {
		return nil, err
	}

	return &VendorDirectory{
		RootPath:             vendorPath,
		RootRelativePath:     rootRelativePath,
		Repos:                repos,
		Registries:           registries,
		FlagAliases:          aliases,
		GeneratedRepoImports: imports.imports,
		GeneratedRepoClosure: imports.closure,
	}, nil
}

// manifestDirectoryNames returns the vendor directory names the manifest
// declares, so that the marker/directory symmetry check accepts imported
// repositories that trusted CI materialized without a Bazel marker. An
// entry whose name cannot be parsed is skipped here: the import scan
// reports it with its full context.
func manifestDirectoryNames(manifest *generatedRepoManifest) map[string]struct{} {
	directoryNames := map[string]struct{}{}
	if manifest == nil {
		return directoryNames
	}
	for _, entry := range manifest.Repos {
		canonicalRepo, err := parseGeneratedRepoCanonicalName(entry.CanonicalRepo)
		if err != nil {
			continue
		}
		directoryNames[vendorRepoDirectoryName(canonicalRepo)] = struct{}{}
	}
	return directoryNames
}

// mapsKeys is kept local so that scanner code does not depend on a map's
// iteration order before sorting its result.
func mapsKeys[T any](m map[string]T) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	return keys
}

func loadVendorConfiguration(vendorFilePath string) (vendorConfiguration, error) {
	configuration := vendorConfiguration{
		pinnedRepos:  map[string]struct{}{},
		ignoredRepos: map[string]struct{}{},
	}
	contents, err := os.ReadFile(vendorFilePath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return configuration, nil
		}
		return vendorConfiguration{}, fmt.Errorf("read VENDOR.bazel: %w", err)
	}
	if !utf8.Valid(contents) {
		return vendorConfiguration{}, errors.New("VENDOR.bazel is not valid UTF-8")
	}

	file, err := syntax.Parse(vendorFilePath, contents, 0)
	if err != nil {
		return vendorConfiguration{}, fmt.Errorf("parse VENDOR.bazel: %w", err)
	}
	for _, statement := range file.Stmts {
		expressionStatement, ok := statement.(*syntax.ExprStmt)
		if !ok {
			return vendorConfiguration{}, fmt.Errorf("VENDOR.bazel: unsupported statement at %s", syntax.Start(statement))
		}
		call, ok := expressionStatement.X.(*syntax.CallExpr)
		if !ok {
			return vendorConfiguration{}, fmt.Errorf("VENDOR.bazel: unsupported expression at %s", syntax.Start(expressionStatement.X))
		}
		function, ok := call.Fn.(*syntax.Ident)
		if !ok || (function.Name != "ignore" && function.Name != "pin") {
			return vendorConfiguration{}, fmt.Errorf("VENDOR.bazel: unsupported directive at %s; only ignore() and pin() are supported", syntax.Start(call))
		}
		for _, argument := range call.Args {
			literal, ok := argument.(*syntax.Literal)
			if !ok {
				return vendorConfiguration{}, fmt.Errorf("VENDOR.bazel: %s() arguments must be canonical repository string literals", function.Name)
			}
			canonicalRepoName, ok := literal.Value.(string)
			if !ok {
				return vendorConfiguration{}, fmt.Errorf("VENDOR.bazel: %s() arguments must be canonical repository strings", function.Name)
			}
			canonicalRepo, err := parseVendorCanonicalRepo(canonicalRepoName)
			if err != nil {
				return vendorConfiguration{}, fmt.Errorf("VENDOR.bazel: %s(): %w", function.Name, err)
			}
			if function.Name == "ignore" {
				if _, pinned := configuration.pinnedRepos[canonicalRepo.String()]; pinned {
					return vendorConfiguration{}, fmt.Errorf("VENDOR.bazel: repository %q cannot be both pinned and ignored", "@@"+canonicalRepo.String())
				}
				configuration.ignoredRepos[canonicalRepo.String()] = struct{}{}
				continue
			}
			if _, ignored := configuration.ignoredRepos[canonicalRepo.String()]; ignored {
				return vendorConfiguration{}, fmt.Errorf("VENDOR.bazel: repository %q cannot be both ignored and pinned", "@@"+canonicalRepo.String())
			}
			if _, exists := configuration.pinnedRepos[canonicalRepo.String()]; exists {
				return vendorConfiguration{}, fmt.Errorf("VENDOR.bazel: repository %q is pinned more than once", "@@"+canonicalRepo.String())
			}
			configuration.pinnedRepos[canonicalRepo.String()] = struct{}{}
		}
	}
	return configuration, nil
}

// Bazel stores its built-in platforms repository as @platforms.marker and
// extension repositories of its built-in platforms and bazel_tools modules
// without a module-instance separator. Normalize these at the vendor boundary,
// keeping ordinary canonical repository names strict.
func parseVendorRepoName(name string) (label.CanonicalRepo, error) {
	switch {
	case name == "platforms":
		name = "platforms+"
	case strings.HasPrefix(name, "platforms+") && strings.Count(name, "+") == 2:
		name = "platforms+" + name[len("platforms"):]
	case strings.HasPrefix(name, "bazel_tools+") && strings.Count(name, "+") == 2:
		name = "bazel_tools+" + name[len("bazel_tools"):]
	}
	return label.NewCanonicalRepo(name)
}

func parseVendorCanonicalRepo(value string) (label.CanonicalRepo, error) {
	if !strings.HasPrefix(value, "@@") {
		return label.CanonicalRepo{}, fmt.Errorf("repository %q must start with @@", value)
	}
	canonicalRepo, err := parseVendorRepoName(strings.TrimPrefix(value, "@@"))
	if err != nil {
		return label.CanonicalRepo{}, fmt.Errorf("invalid canonical repository %q: %w", value, err)
	}
	return canonicalRepo, nil
}

func scanVendoredRepos(vendorPath string, configuration vendorConfiguration, importedDirectoryNames map[string]struct{}) (map[string]vendorRepoRecord, error) {
	entries, err := os.ReadDir(vendorPath)
	if err != nil {
		return nil, fmt.Errorf("read --vendor_dir: %w", err)
	}

	repos := map[string]vendorRepoRecord{}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "@") || !strings.HasSuffix(name, ".marker") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, fmt.Errorf("stat vendor marker %q: %w", name, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("vendor marker %q is not a regular file", name)
		}
		vendoredRepoName := strings.TrimSuffix(strings.TrimPrefix(name, "@"), ".marker")
		canonicalRepo, err := parseVendorRepoName(vendoredRepoName)
		if err != nil {
			return nil, fmt.Errorf("vendor marker %q does not contain a valid canonical repository name: %w", name, err)
		}
		if _, ignored := configuration.ignoredRepos[canonicalRepo.String()]; ignored {
			continue
		}
		markerFiles, err := parseVendorMarker(filepath.Join(vendorPath, name))
		if err != nil {
			return nil, err
		}
		if existing, exists := repos[canonicalRepo.String()]; exists {
			return nil, fmt.Errorf("vendor markers %q and %q both map to canonical repository %q", "@"+filepath.Base(existing.repo.RootPath)+".marker", name, "@@"+canonicalRepo.String())
		}
		canonicalRepoName := canonicalRepo.String()
		_, pinned := configuration.pinnedRepos[canonicalRepoName]
		repos[canonicalRepoName] = vendorRepoRecord{
			repo: VendoredRepo{
				CanonicalRepo: canonicalRepo,
				RootPath:      filepath.Join(vendorPath, vendoredRepoName),
				Pinned:        pinned,
			},
			markerFiles: markerFiles,
		}
	}

	for canonicalRepoName, record := range repos {
		info, err := os.Lstat(record.repo.RootPath)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil, fmt.Errorf("vendor marker for %q does not have matching directory %q", "@@"+canonicalRepoName, filepath.Base(record.repo.RootPath))
			}
			return nil, fmt.Errorf("stat vendor directory %q: %w", filepath.Base(record.repo.RootPath), err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("vendor marker for %q has non-directory counterpart %q", "@@"+canonicalRepoName, filepath.Base(record.repo.RootPath))
		}
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, imported := importedDirectoryNames[entry.Name()]; imported {
			// An imported generated repository is materialized by trusted
			// CI and validated by its recorded digest, so it need not carry
			// a Bazel vendor marker.
			continue
		}
		canonicalRepo, err := parseVendorRepoName(entry.Name())
		if err != nil {
			continue
		}
		if _, ignored := configuration.ignoredRepos[canonicalRepo.String()]; ignored {
			continue
		}
		if _, ok := repos[canonicalRepo.String()]; !ok {
			return nil, fmt.Errorf("vendored repository directory %q does not have matching marker %q", entry.Name(), "@"+entry.Name()+".marker")
		}
	}
	for pinnedRepo := range configuration.pinnedRepos {
		if _, ok := repos[pinnedRepo]; !ok {
			return nil, fmt.Errorf("VENDOR.bazel pins %q, but the corresponding marker and directory are missing", "@@"+pinnedRepo)
		}
	}
	return repos, nil
}

func parseVendorMarker(markerPath string) ([]vendorMarkerFile, error) {
	contents, err := os.ReadFile(markerPath)
	if err != nil {
		return nil, fmt.Errorf("read vendor marker %q: %w", markerPath, err)
	}
	if !utf8.Valid(contents) {
		return nil, fmt.Errorf("vendor marker %q is not valid UTF-8", markerPath)
	}
	lines := strings.Split(strings.TrimRight(string(contents), "\n"), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) == "" {
		return nil, fmt.Errorf("vendor marker %q is empty", markerPath)
	}
	if _, err := parseSHA256(strings.TrimSpace(lines[0])); err != nil {
		return nil, fmt.Errorf("vendor marker %q has invalid repository fingerprint: %w", markerPath, err)
	}

	var files []vendorMarkerFile
	for lineIndex, line := range lines[1:] {
		if !strings.HasPrefix(line, "FILE:") {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, "FILE:"))
		if len(fields) != 2 {
			return nil, fmt.Errorf("vendor marker %q line %d has malformed FILE input", markerPath, lineIndex+2)
		}
		hash, err := parseSHA256(fields[1])
		if err != nil {
			return nil, fmt.Errorf("vendor marker %q line %d has invalid FILE hash: %w", markerPath, lineIndex+2, err)
		}
		files = append(files, vendorMarkerFile{label: fields[0], sha256: hash})
	}
	return files, nil
}

func parseSHA256(value string) ([]byte, error) {
	if len(value) != sha256.Size*2 {
		return nil, errors.New("want a SHA-256 hexadecimal digest")
	}
	digest, err := hex.DecodeString(value)
	if err != nil || len(digest) != sha256.Size {
		return nil, errors.New("want a SHA-256 hexadecimal digest")
	}
	return digest, nil
}

func validateVendoredRepoMarkers(workspacePath string, repos map[string]vendorRepoRecord, configuration vendorConfiguration) error {
	repoNames := mapsKeys(repos)
	slices.Sort(repoNames)
	for _, canonicalRepoName := range repoNames {
		if _, pinned := configuration.pinnedRepos[canonicalRepoName]; pinned {
			continue
		}
		for _, file := range repos[canonicalRepoName].markerFiles {
			filePath, err := resolveVendorMarkerFile(workspacePath, repos, file.label)
			if err != nil {
				return fmt.Errorf("validate marker for %q: %w", "@@"+canonicalRepoName, err)
			}
			contents, err := os.ReadFile(filePath)
			if err != nil {
				return fmt.Errorf("validate marker for %q: read FILE input %q: %w", "@@"+canonicalRepoName, file.label, err)
			}
			actualHash := sha256.Sum256(contents)
			if !slices.Equal(actualHash[:], file.sha256) {
				return fmt.Errorf("vendor marker for %q is stale: FILE input %q no longer matches its recorded SHA-256", "@@"+canonicalRepoName, file.label)
			}
		}
	}
	return nil
}

func resolveVendorMarkerFile(workspacePath string, repos map[string]vendorRepoRecord, fileLabel string) (string, error) {
	if !strings.HasPrefix(fileLabel, "@@") {
		return "", fmt.Errorf("FILE input %q is not a canonical label", fileLabel)
	}
	parts := strings.SplitN(strings.TrimPrefix(fileLabel, "@@"), "//", 2)
	if len(parts) != 2 {
		return "", fmt.Errorf("FILE input %q is not a canonical label", fileLabel)
	}

	rootPath := workspacePath
	if parts[0] != "" {
		canonicalRepo, err := parseVendorRepoName(parts[0])
		if err != nil {
			return "", fmt.Errorf("FILE input %q has invalid repository: %w", fileLabel, err)
		}
		repo, ok := repos[canonicalRepo.String()]
		if !ok {
			return "", fmt.Errorf("FILE input %q refers to repository that is not vendored", fileLabel)
		}
		rootPath = repo.repo.RootPath
	}

	relativePath, err := labelFilePath(parts[1])
	if err != nil {
		return "", fmt.Errorf("FILE input %q: %w", fileLabel, err)
	}
	filePath := filepath.Join(rootPath, filepath.FromSlash(relativePath))
	contained, err := filepath.Rel(rootPath, filePath)
	if err != nil || !filepath.IsLocal(contained) {
		return "", fmt.Errorf("FILE input %q escapes its repository", fileLabel)
	}
	return filePath, nil
}

func labelFilePath(value string) (string, error) {
	packagePath, targetName, hasTargetName := strings.Cut(value, ":")
	if hasTargetName {
		if targetName == "" {
			return "", errors.New("has an empty target name")
		}
		if packagePath == "" {
			value = targetName
		} else {
			value = packagePath + "/" + targetName
		}
	}
	value = pathpkg.Clean(value)
	if value == "." || pathpkg.IsAbs(value) || strings.HasPrefix(value, "../") {
		return "", errors.New("does not identify a repository-relative file")
	}
	return value, nil
}

func scanVendoredRegistries(vendorPath string, registryURLs []string) ([]VendoredRegistry, error) {
	if len(registryURLs) == 0 {
		return nil, nil
	}

	registries := make([]VendoredRegistry, 0, len(registryURLs))
	seenURLs := map[string]struct{}{}
	for _, registryURL := range registryURLs {
		normalizedURL, directoryName, err := normalizeVendorRegistryURL(registryURL)
		if err != nil {
			return nil, err
		}
		if _, exists := seenURLs[normalizedURL]; exists {
			return nil, fmt.Errorf("registry %q is provided more than once", normalizedURL)
		}
		seenURLs[normalizedURL] = struct{}{}
		rootPath := filepath.Join(vendorPath, "_registries", directoryName)
		info, err := os.Lstat(rootPath)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil, fmt.Errorf("vendor registry mirror for %q is missing at %q", normalizedURL, rootPath)
			}
			return nil, fmt.Errorf("stat vendor registry mirror for %q: %w", normalizedURL, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("vendor registry mirror for %q at %q is not a directory", normalizedURL, rootPath)
		}
		registries = append(registries, VendoredRegistry{
			URL:      normalizedURL,
			RootPath: rootPath,
		})
	}
	slices.SortFunc(registries, func(a, b VendoredRegistry) int {
		return strings.Compare(a.URL, b.URL)
	})
	return registries, nil
}

func normalizeVendorRegistryURL(registryURL string) (string, string, error) {
	parsedURL, err := url.Parse(registryURL)
	if err != nil {
		return "", "", fmt.Errorf("invalid registry URL %q: %w", registryURL, err)
	}
	if parsedURL.Scheme != "http" && parsedURL.Scheme != "https" {
		return "", "", fmt.Errorf("registry URL %q has unsupported scheme %q for --vendor_dir", registryURL, parsedURL.Scheme)
	}
	if parsedURL.User != nil || parsedURL.RawQuery != "" || parsedURL.Fragment != "" || parsedURL.Host == "" {
		return "", "", fmt.Errorf("registry URL %q cannot be mapped to a vendor registry mirror", registryURL)
	}

	if parsedURL.Port() != "" || (parsedURL.Path != "" && parsedURL.Path != "/") {
		return "", "", fmt.Errorf("registry URL %q cannot be mapped to a vendor registry mirror; only origin-root registry URLs are supported", registryURL)
	}
	host := strings.ToLower(parsedURL.Hostname())
	if host == "" {
		return "", "", fmt.Errorf("registry URL %q has no hostname", registryURL)
	}
	return parsedURL.Scheme + "://" + host + "/", host, nil
}

// NormalizeVendorRegistryURL converts a registry URL to the origin-root form
// used by vendor/_registries. It rejects URL forms Bazel's vendor layout cannot
// map unambiguously.
func NormalizeVendorRegistryURL(registryURL string) (string, error) {
	normalizedURL, _, err := normalizeVendorRegistryURL(registryURL)
	return normalizedURL, err
}

func validateVendoredRegistryLockfile(workspacePath string, registries []VendoredRegistry) error {
	lockfile, err := readVendorLockfile(workspacePath)
	if err != nil {
		return err
	}
	if len(registries) > 0 && len(lockfile.RegistryFileHashes) == 0 {
		return errors.New("MODULE.bazel.lock does not contain registryFileHashes required to validate --vendor_dir")
	}

	registriesByURL := make(map[string]*VendoredRegistry, len(registries))
	for index := range registries {
		registriesByURL[registries[index].URL] = &registries[index]
	}
	filesByRegistry := make(map[string]map[string][]byte, len(registries))
	for registryURL := range registriesByURL {
		filesByRegistry[registryURL] = map[string][]byte{}
	}
	for fileURL, expectedHash := range lockfile.RegistryFileHashes {
		registryURL, relativePath, err := registryFileLocation(fileURL)
		if err != nil {
			return fmt.Errorf("invalid registryFileHashes URL %q: %w", fileURL, err)
		}
		registry, ok := registriesByURL[registryURL]
		if !ok {
			return fmt.Errorf("MODULE.bazel.lock requires registry file %q, but --vendor_dir has no mirror for %q", fileURL, registryURL)
		}
		expectedDigest, err := parseSHA256(expectedHash)
		if err != nil {
			return fmt.Errorf("MODULE.bazel.lock registry file %q: %w", fileURL, err)
		}
		if previous, exists := filesByRegistry[registryURL][relativePath]; exists && !slices.Equal(previous, expectedDigest) {
			return fmt.Errorf("MODULE.bazel.lock assigns conflicting hashes to vendor registry file %q", fileURL)
		}
		filePath := filepath.Join(registry.RootPath, filepath.FromSlash(relativePath))
		contained, err := filepath.Rel(registry.RootPath, filePath)
		if err != nil || !filepath.IsLocal(contained) {
			return fmt.Errorf("vendor registry file %q escapes its mirror", fileURL)
		}
		fileContents, err := os.ReadFile(filePath)
		if err != nil {
			return fmt.Errorf("read vendored registry file %q: %w", fileURL, err)
		}
		actualDigest := sha256.Sum256(fileContents)
		if !slices.Equal(actualDigest[:], expectedDigest) {
			return fmt.Errorf("vendored registry file %q does not match the SHA-256 recorded in MODULE.bazel.lock", fileURL)
		}
		filesByRegistry[registryURL][relativePath] = expectedDigest
	}
	for index := range registries {
		files := filesByRegistry[registries[index].URL]
		paths := mapsKeys(files)
		slices.Sort(paths)
		registries[index].Files = make([]VendoredRegistryFile, 0, len(paths))
		for _, filePath := range paths {
			registries[index].Files = append(registries[index].Files, VendoredRegistryFile{
				Path:   filePath,
				SHA256: files[filePath],
			})
		}
	}
	return nil
}

func registryFileLocation(fileURL string) (string, string, error) {
	parsedURL, err := url.Parse(fileURL)
	if err != nil {
		return "", "", err
	}
	if parsedURL.Scheme != "http" && parsedURL.Scheme != "https" || parsedURL.User != nil || parsedURL.RawQuery != "" || parsedURL.Fragment != "" || parsedURL.Host == "" || parsedURL.Port() != "" {
		return "", "", errors.New("not an origin-root HTTP(S) registry URL")
	}
	relativePath := pathpkg.Clean(strings.TrimPrefix(parsedURL.Path, "/"))
	if relativePath == "." || pathpkg.IsAbs(relativePath) || strings.HasPrefix(relativePath, "../") {
		return "", "", errors.New("does not identify a registry-relative file")
	}
	return parsedURL.Scheme + "://" + strings.ToLower(parsedURL.Hostname()) + "/", relativePath, nil
}

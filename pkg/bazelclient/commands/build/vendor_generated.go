package build

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"bonanza.build/pkg/label"
	pg_starlark "bonanza.build/pkg/starlark"

	"github.com/buildbarn/bb-storage/pkg/filesystem/path"

	"go.starlark.net/starlark"
)

// generatedRepoManifestFilename is a Bonanza-only manifest inside
// --vendor_dir. Bazel itself only reads VENDOR.bazel (@*.marker pairs
// and the _registries tree), and VENDOR.bazel's own grammar is fixed to
// ignore() and pin(), so imported generated repositories are declared
// out of band instead of by extending Bazel's directive language.
const generatedRepoManifestFilename = "generated_repos.json"

// generatedRepoManifestVersion is the only manifest version this
// implementation understands. Any other value fails closed, so that a
// future incompatible manifest cannot be silently misread.
const generatedRepoManifestVersion = 1

// generatedRepoPlatformAny marks a repository whose contents do not
// depend on the platform they were materialized on.
const generatedRepoPlatformAny = "*"

// generatedRepoPlatformOSes is the closed set of operating system
// prefixes accepted in a generated repository platform key. The
// vocabulary matches what Bazel records in MODULE.bazel.lock for
// platform-scoped generated repositories (for example "linux_arm64").
var generatedRepoPlatformOSes = []string{
	"android", "darwin", "freebsd", "ios", "linux", "netbsd", "openbsd", "windows",
}

// GeneratedRepoImport is one generated repository declared by
// generated_repos.json. Imported trees are validated against
// MODULE.bazel.lock (or a use_repo()/use_repo_rule() declaration), are
// scoped to a single platform, carry a content digest, and are pinned,
// so their contents override normal module/extension resolution.
type GeneratedRepoImport struct {
	CanonicalRepo label.CanonicalRepo
	// Extension is the canonical module extension (or repository rule)
	// identifier that declares the repository, e.g.
	// "@@rules_java+//java:extensions.bzl%toolchains".
	Extension     string
	Platform      string
	SHA256        []byte
	DirectoryName string
}

// GeneratedRepoClosureEntry is one generated repository that the exact
// pinned inputs can produce, together with where that requirement comes
// from and whether the vendor snapshot supplies it. This is the complete
// closure: repositories that are absent from the snapshot are reported
// rather than discovered one analysis error at a time.
type GeneratedRepoClosureEntry struct {
	CanonicalRepo string
	// Sources is sorted and deduplicated. Values are "lockfile",
	// "use_extension:<module instance>", "use_repo_rule:<module instance>",
	// "import" and "pin".
	Sources []string
	// Present reports whether the vendor snapshot supplies the repository,
	// either as a @*.marker/directory pair or as a generated_repos.json
	// import.
	Present bool
}

type generatedRepoManifest struct {
	Version  int                          `json:"version"`
	Platform string                       `json:"platform"`
	Repos    []generatedRepoManifestEntry `json:"repos"`
}

type generatedRepoManifestEntry struct {
	CanonicalRepo string `json:"canonical_repo"`
	Extension     string `json:"extension"`
	Platform      string `json:"platform"`
	SHA256        string `json:"sha256"`
}

// validateGeneratedRepoPlatform enforces the platform key grammar that
// scopes a generated repository to the platform it was materialized on.
// Platform keys are "os_cpu" (for example "linux_arm64") or "*" for
// platform-independent repositories. An unknown key is rejected instead
// of being treated as an unconstrained platform.
func validateGeneratedRepoPlatform(platform string) error {
	if platform == generatedRepoPlatformAny {
		return nil
	}
	osName, cpu, ok := strings.Cut(platform, "_")
	if !ok {
		return errors.New(`must be "*" or an "os_cpu" key such as "linux_arm64"`)
	}
	if !slices.Contains(generatedRepoPlatformOSes, osName) {
		return fmt.Errorf("has unsupported operating system %q", osName)
	}
	if cpu == "" || strings.HasPrefix(cpu, "_") || strings.HasSuffix(cpu, "_") {
		return fmt.Errorf("has malformed CPU component %q", cpu)
	}
	for _, character := range cpu {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '_' {
			return fmt.Errorf("has malformed CPU component %q", cpu)
		}
	}
	return nil
}

// loadGeneratedRepoManifest reads and validates the shape of
// --vendor_dir/generated_repos.json. A missing manifest is not an error:
// it means the snapshot declares no imports.
func loadGeneratedRepoManifest(vendorPath string) (*generatedRepoManifest, error) {
	manifestPath := filepath.Join(vendorPath, generatedRepoManifestFilename)
	contents, err := os.ReadFile(manifestPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", generatedRepoManifestFilename, err)
	}
	if !utf8.Valid(contents) {
		return nil, fmt.Errorf("%s is not valid UTF-8", generatedRepoManifestFilename)
	}

	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	var manifest generatedRepoManifest
	if err := decoder.Decode(&manifest); err != nil {
		return nil, fmt.Errorf("parse %s: %w", generatedRepoManifestFilename, err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s has trailing data after its JSON document", generatedRepoManifestFilename)
	}

	if manifest.Version != generatedRepoManifestVersion {
		return nil, fmt.Errorf("%s has unsupported version %d, want %d", generatedRepoManifestFilename, manifest.Version, generatedRepoManifestVersion)
	}
	if err := validateGeneratedRepoPlatform(manifest.Platform); err != nil {
		return nil, fmt.Errorf("%s platform %q %w", generatedRepoManifestFilename, manifest.Platform, err)
	}
	if manifest.Platform == generatedRepoPlatformAny {
		return nil, fmt.Errorf("%s platform must name the single platform the bundle was materialized for, not %q", generatedRepoManifestFilename, generatedRepoPlatformAny)
	}
	return &manifest, nil
}

// writeLengthPrefixed feeds an unambiguous length-prefixed string into a
// digest, so that concatenated entries cannot be confused with each
// other.
func writeLengthPrefixed(hasher io.Writer, value string) {
	var size [8]byte
	length := uint64(len(value))
	for index := range size {
		size[index] = byte(length >> (8 * (7 - index)))
	}
	hasher.Write(size[:])
	io.WriteString(hasher, value)
}

// GeneratedRepoTreeSHA256 computes the canonical content digest of an
// imported generated repository. The digest covers, in sorted order,
// every directory, regular file (content and executable bit) and
// symbolic link (target) in the tree, so that a hand-copied host cache
// entry cannot silently stand in for the reviewed bundle. Absolute
// symbolic links, symbolic links that escape the repository and
// irregular files are rejected: they are exactly the host-local shapes
// that are not a portable production input.
func GeneratedRepoTreeSHA256(rootPath string) ([]byte, error) {
	hasher := sha256.New()
	var walk func(relativeDirectory string) error
	walk = func(relativeDirectory string) error {
		directoryPath := rootPath
		if relativeDirectory != "" {
			directoryPath = filepath.Join(rootPath, filepath.FromSlash(relativeDirectory))
		}
		entries, err := os.ReadDir(directoryPath)
		if err != nil {
			return fmt.Errorf("read directory %q: %w", relativeDirectory, err)
		}
		slices.SortFunc(entries, func(a, b fs.DirEntry) int {
			return strings.Compare(a.Name(), b.Name())
		})
		for _, entry := range entries {
			relativePath := entry.Name()
			if relativeDirectory != "" {
				relativePath = relativeDirectory + "/" + entry.Name()
			}
			info, err := entry.Info()
			if err != nil {
				return fmt.Errorf("stat %q: %w", relativePath, err)
			}
			switch {
			case info.IsDir():
				hasher.Write([]byte{'d'})
				writeLengthPrefixed(hasher, relativePath)
				if err := walk(relativePath); err != nil {
					return err
				}
			case info.Mode().IsRegular():
				contents, err := os.ReadFile(filepath.Join(rootPath, filepath.FromSlash(relativePath)))
				if err != nil {
					return fmt.Errorf("read %q: %w", relativePath, err)
				}
				hasher.Write([]byte{'f'})
				writeLengthPrefixed(hasher, relativePath)
				if info.Mode()&0o111 != 0 {
					hasher.Write([]byte{1})
				} else {
					hasher.Write([]byte{0})
				}
				digest := sha256.Sum256(contents)
				hasher.Write(digest[:])
			case info.Mode()&fs.ModeSymlink != 0:
				target, err := os.Readlink(filepath.Join(rootPath, filepath.FromSlash(relativePath)))
				if err != nil {
					return fmt.Errorf("read symbolic link %q: %w", relativePath, err)
				}
				if err := validateGeneratedRepoSymlink(relativePath, target); err != nil {
					return err
				}
				hasher.Write([]byte{'l'})
				writeLengthPrefixed(hasher, relativePath)
				writeLengthPrefixed(hasher, target)
			default:
				return fmt.Errorf("entry %q is neither a directory, a regular file nor a symbolic link (%s)", relativePath, info.Mode())
			}
		}
		return nil
	}
	if err := walk(""); err != nil {
		return nil, err
	}
	return hasher.Sum(nil), nil
}

// validateGeneratedRepoSymlink rejects the symbolic link shapes that
// make a bundle depend on the machine that produced it.
func validateGeneratedRepoSymlink(relativePath string, target string) error {
	if filepath.IsAbs(target) || strings.HasPrefix(target, "/") {
		return fmt.Errorf("entry %q is an absolute symbolic link to %q; a generated repository import must be self-contained", relativePath, target)
	}
	resolved := filepath.ToSlash(filepath.Clean(filepath.Join(filepath.ToSlash(filepath.Dir(relativePath)), filepath.FromSlash(target))))
	if resolved == ".." || strings.HasPrefix(resolved, "../") {
		return fmt.Errorf("entry %q is a symbolic link to %q, which escapes the repository", relativePath, target)
	}
	return nil
}

// generatedRepoDeclaration is one resolved declaration of a generated
// repository by a module's MODULE.bazel.
type generatedRepoDeclaration struct {
	// Extension is the canonical identifier of the module extension or
	// repository rule that declares the repositories, e.g.
	// "@@platforms+//host:extension.bzl%host_platform".
	Extension string
	// Module is the canonical instance of the declaring module.
	Module string
	Name   string
	Repos  []string
	// RepoRule records whether the declaration came from use_repo_rule()
	// instead of use_extension()/use_repo().
	RepoRule bool
}

type moduleDotBazelDeclarations struct {
	Instance   string
	BareName   string
	Extensions []*moduleExtensionUsage
	RepoRules  []*moduleRepoRuleUsage
	ParseError error
}

type moduleExtensionUsage struct {
	BzlFile label.ApparentLabel
	Name    label.StarlarkIdentifier
	Repos   []string
}

type moduleRepoRuleUsage struct {
	BzlFile label.ApparentLabel
	Name    label.StarlarkIdentifier
	Repos   []string
}

// generatedRepoDeclarations is the offline inventory of every generated
// repository that the exact pinned inputs can produce: the repositories
// recorded in MODULE.bazel.lock, plus the ones declared by use_repo()
// and use_repo_rule() in the root module and in every vendored module.
type generatedRepoDeclarations struct {
	declarations []generatedRepoDeclaration
	// lockfile maps a canonical extension identifier to the repositories
	// MODULE.bazel.lock records as produced by it. The value is the
	// platform attribute Bazel recorded for that repository, if any.
	lockfile map[string]map[string]string
	// parseProblems names module sources whose MODULE.bazel could not be
	// parsed. Such a module cannot justify its generated repositories, so
	// strict vendor mode reports it instead of silently accepting them.
	parseProblems []string
}

// scanGeneratedRepoDeclarations parses the root MODULE.bazel and every
// vendored module's MODULE.bazel, then resolves each use_extension()/
// use_repo() and use_repo_rule() declaration to a canonical
// identifier. Registry-sourced modules that the snapshot does not
// contain cannot be read offline; their declarations are only reachable
// through MODULE.bazel.lock, which is recorded separately.
func scanGeneratedRepoDeclarations(workspacePath string, vendorPath string, repos map[string]vendorRepoRecord, lockfile *vendorLockfile, localPathFormat path.Format) (*generatedRepoDeclarations, error) {
	declarations := &generatedRepoDeclarations{
		lockfile: map[string]map[string]string{},
	}

	var modules []*moduleDotBazelDeclarations
	for canonicalRepoName, record := range repos {
		canonicalRepo := record.repo.CanonicalRepo
		if _, _, isModuleExtensionRepo := canonicalRepo.GetModuleExtension(); isModuleExtensionRepo {
			continue
		}
		moduleDotBazelPath := filepath.Join(record.repo.RootPath, "MODULE.bazel")
		module, err := scanModuleDotBazelDeclarations(canonicalRepoName, moduleDotBazelPath, localPathFormat)
		if err != nil {
			return nil, err
		}
		if module != nil {
			modules = append(modules, module)
		}
	}
	rootModule, err := scanModuleDotBazelDeclarations("", filepath.Join(workspacePath, "MODULE.bazel"), localPathFormat)
	if err != nil {
		return nil, err
	}
	if rootModule != nil {
		modules = append(modules, rootModule)
	}

	// Apparent repository labels in use_extension() can only be resolved
	// to a canonical module instance when exactly one module source in the
	// snapshot carries that bare name. Renames and multiple versions are
	// therefore deliberately unresolvable instead of being guessed.
	instancesByBareName := map[string][]string{}
	for _, module := range modules {
		if module.BareName == "" || module.Instance == "" {
			continue
		}
		instancesByBareName[module.BareName] = append(instancesByBareName[module.BareName], module.Instance)
	}

	for _, module := range modules {
		if module.ParseError != nil {
			name := module.Instance
			if name == "" {
				name = module.BareName
			}
			declarations.parseProblems = append(declarations.parseProblems, fmt.Sprintf("%s: %s", name, module.ParseError))
			continue
		}
		for _, extension := range module.Extensions {
			identifier, ok := resolveCanonicalDeclarationIdentifier(module, extension.BzlFile, extension.Name, instancesByBareName)
			if !ok {
				continue
			}
			declarations.declarations = append(declarations.declarations, generatedRepoDeclaration{
				Extension: identifier,
				Module:    module.Instance,
				Name:      extension.Name.String(),
				Repos:     extension.Repos,
			})
		}
		for _, repoRule := range module.RepoRules {
			identifier, ok := resolveCanonicalDeclarationIdentifier(module, repoRule.BzlFile, repoRule.Name, instancesByBareName)
			if !ok {
				continue
			}
			declarations.declarations = append(declarations.declarations, generatedRepoDeclaration{
				Extension: identifier,
				Module:    module.Instance,
				Name:      repoRule.Name.String(),
				Repos:     repoRule.Repos,
				RepoRule:  true,
			})
		}
	}

	if lockfile != nil {
		for extensionIdentifier, extension := range lockfile.ModuleExtensions {
			if _, _, ok := parseLockfileExtensionIdentifier(extensionIdentifier); !ok {
				return nil, fmt.Errorf("MODULE.bazel.lock contains malformed module extension key %q", extensionIdentifier)
			}
			repos := map[string]string{}
			for repoName, spec := range extension.General.GeneratedRepoSpecs {
				platform := ""
				if value, ok := spec.Attributes["platform"].(string); ok {
					platform = value
				}
				repos[repoName] = platform
			}
			declarations.lockfile[extensionIdentifier] = repos
		}
	}

	slices.SortFunc(declarations.declarations, func(a, b generatedRepoDeclaration) int {
		if result := strings.Compare(a.Extension, b.Extension); result != 0 {
			return result
		}
		if result := strings.Compare(a.Module, b.Module); result != 0 {
			return result
		}
		return strings.Compare(a.Name, b.Name)
	})
	slices.Sort(declarations.parseProblems)
	return declarations, nil
}

// resolveCanonicalDeclarationIdentifier builds the canonical identifier
// of a module extension or repository rule from a declaration, or
// reports that the declaration cannot be resolved unambiguously offline.
func resolveCanonicalDeclarationIdentifier(module *moduleDotBazelDeclarations, bzlFile label.ApparentLabel, name label.StarlarkIdentifier, instancesByBareName map[string][]string) (string, bool) {
	raw := bzlFile.String()
	repository, packageAndFile := "", raw
	if index := strings.Index(raw, "//"); index >= 0 {
		repository, packageAndFile = raw[:index], raw[index+2:]
	}
	if packageAndFile == "" || module.Instance == "" {
		return "", false
	}
	moduleInstance := ""
	switch {
	case repository == "":
		moduleInstance = module.Instance
	case strings.HasPrefix(repository, "@@"):
		moduleInstance = strings.TrimPrefix(repository, "@@")
	case strings.HasPrefix(repository, "@"):
		instances := instancesByBareName[strings.TrimPrefix(repository, "@")]
		if len(instances) != 1 {
			return "", false
		}
		moduleInstance = instances[0]
	default:
		return "", false
	}
	return "@@" + moduleInstance + "//" + packageAndFile + "%" + name.String(), true
}

// parseLockfileExtensionIdentifier splits a MODULE.bazel.lock
// moduleExtensions key ("@@rules_java+//java:extensions.bzl%toolchains")
// into its module instance and extension name.
func parseLockfileExtensionIdentifier(identifier string) (string, string, bool) {
	if !strings.HasPrefix(identifier, "@@") {
		return "", "", false
	}
	bzlFile, extensionName, ok := strings.Cut(strings.TrimPrefix(identifier, "@@"), "%")
	if !ok {
		return "", "", false
	}
	moduleInstance, _, ok := strings.Cut(bzlFile, "//")
	if !ok || moduleInstance == "" || extensionName == "" {
		return "", "", false
	}
	return moduleInstance, extensionName, true
}

// generatedRepoProvenance records why a generated repository may be used
// and what platform it was recorded for.
type generatedRepoProvenance struct {
	Sources []string
	// Platform is the platform attribute MODULE.bazel.lock recorded for
	// the repository, if the lockfile justifies it.
	Platform string
	// LockfileExtension is the canonical identifier of the extension the
	// lockfile records the repository under, so a manifest declaration can
	// be checked against it.
	LockfileExtension string
	// DeclaredExtensions are the canonical identifiers of the module
	// extensions and repository rules that declare the repository, as
	// resolved from the root and vendored module sources.
	DeclaredExtensions []string
}

// provenanceForGeneratedRepo resolves a generated repository to the
// declarations and lockfile records that justify it. An extension that
// the lockfile records must also record the repository itself; a
// recorded extension that does not produce the repository is an error
// rather than an absence of evidence.
func (d *generatedRepoDeclarations) provenanceForGeneratedRepo(canonicalRepo label.CanonicalRepo) (generatedRepoProvenance, error) {
	moduleInstance := canonicalRepo.GetModuleInstance().String()
	moduleExtension, apparentRepo, ok := canonicalRepo.GetModuleExtension()
	if !ok {
		return generatedRepoProvenance{}, fmt.Errorf("%q is not a generated repository", "@@"+canonicalRepo.String())
	}
	extensionName := moduleExtension.GetExtensionName().String()
	repoName := apparentRepo.String()

	provenance := generatedRepoProvenance{}
	for _, declaration := range d.declarations {
		if declaration.Instance() != moduleInstance || declaration.Name != extensionName {
			continue
		}
		if !slices.Contains(declaration.Repos, repoName) {
			continue
		}
		kind := "use_extension"
		if declaration.RepoRule {
			kind = "use_repo_rule"
		}
		provenance.Sources = append(provenance.Sources, kind+":"+declaration.Module)
		provenance.DeclaredExtensions = append(provenance.DeclaredExtensions, declaration.Extension)
	}
	for identifier, repos := range d.lockfile {
		lockfileModuleInstance, lockfileExtensionName, ok := parseLockfileExtensionIdentifier(identifier)
		if !ok || lockfileModuleInstance != moduleInstance || lockfileExtensionName != extensionName {
			continue
		}
		platform, ok := repos[repoName]
		if !ok {
			return generatedRepoProvenance{}, fmt.Errorf("MODULE.bazel.lock records module extension %q, but not repository %q; the name is not a repository of that extension", identifier, repoName)
		}
		provenance.Sources = append(provenance.Sources, "lockfile")
		provenance.Platform = platform
		provenance.LockfileExtension = identifier
		provenance.DeclaredExtensions = append(provenance.DeclaredExtensions, identifier)
	}
	slices.Sort(provenance.Sources)
	provenance.Sources = slices.Compact(provenance.Sources)
	slices.Sort(provenance.DeclaredExtensions)
	provenance.DeclaredExtensions = slices.Compact(provenance.DeclaredExtensions)
	return provenance, nil
}

// Instance returns the module instance of the declaration. Repository
// rules declared by use_repo_rule() belong to the declaring module
// instance, exactly like module extensions do.
func (d generatedRepoDeclaration) Instance() string {
	if index := strings.Index(d.Extension, "//"); index > 2 {
		return strings.TrimPrefix(d.Extension[:index], "@@")
	}
	return ""
}

// scanModuleDotBazelDeclarations parses one MODULE.bazel for the
// declarations that make generated repositories usable offline. A
// missing file yields no declarations; a malformed file is reported to
// the caller rather than silently ignored.
func scanModuleDotBazelDeclarations(instance string, moduleDotBazelPath string, localPathFormat path.Format) (*moduleDotBazelDeclarations, error) {
	contents, err := os.ReadFile(moduleDotBazelPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %q: %w", moduleDotBazelPath, err)
	}
	if !utf8.Valid(contents) {
		return nil, fmt.Errorf("%q is not valid UTF-8", moduleDotBazelPath)
	}

	declarations := &moduleDotBazelDeclarations{Instance: instance}
	handler := &moduleDotBazelDeclarationHandler{declarations: declarations}
	filenameInstance := instance
	if filenameInstance == "" {
		// The root module's name is only known after its module()
		// declaration has been parsed. The filename label is metadata for
		// error reporting, so a placeholder is used until then.
		filenameInstance = "root_module+"
	}
	filename, err := label.NewCanonicalLabel("@@" + filenameInstance + "//:MODULE.bazel")
	if err != nil {
		return nil, fmt.Errorf("construct filename for %q: %w", moduleDotBazelPath, err)
	}
	if err := pg_starlark.ParseModuleDotBazel(string(contents), filename, localPathFormat, pg_starlark.NewOverrideIgnoringRootModuleDotBazelHandler(handler)); err != nil {
		declarations.ParseError = err
	}
	if handler.moduleName != nil {
		declarations.Instance = handler.moduleName.String() + "+"
		declarations.BareName = handler.moduleName.String()
	} else if moduleInstance, err := label.NewModuleInstance(instance); err == nil {
		declarations.BareName = moduleInstance.GetModule().String()
	}
	return declarations, nil
}

type moduleDotBazelDeclarationHandler struct {
	declarations *moduleDotBazelDeclarations
	moduleName   *label.Module
}

func (h *moduleDotBazelDeclarationHandler) Module(name label.Module, version *label.ModuleVersion, repoName label.ApparentRepo, bazelCompatibility []string) error {
	h.moduleName = &name
	return nil
}

func (h *moduleDotBazelDeclarationHandler) BazelDep(name label.Module, version *label.ModuleVersion, repoName label.ApparentRepo, devDependency bool) error {
	return nil
}

func (h *moduleDotBazelDeclarationHandler) RegisterExecutionPlatforms(platformTargetPatterns []label.ApparentTargetPattern, devDependency bool) error {
	return nil
}

func (h *moduleDotBazelDeclarationHandler) RegisterToolchains(toolchainTargetPatterns []label.ApparentTargetPattern, devDependency bool) error {
	return nil
}

func (h *moduleDotBazelDeclarationHandler) UseExtension(extensionBzlFile label.ApparentLabel, extensionName label.StarlarkIdentifier, devDependency, isolate bool) (pg_starlark.ModuleExtensionProxy, error) {
	usage := &moduleExtensionUsage{
		BzlFile: extensionBzlFile,
		Name:    extensionName,
	}
	h.declarations.Extensions = append(h.declarations.Extensions, usage)
	return usage, nil
}

func (h *moduleDotBazelDeclarationHandler) UseRepoRule(repoRuleBzlFile label.ApparentLabel, repoRuleName label.StarlarkIdentifier) (pg_starlark.RepoRuleProxy, error) {
	usage := &moduleRepoRuleUsage{
		BzlFile: repoRuleBzlFile,
		Name:    repoRuleName,
	}
	h.declarations.RepoRules = append(h.declarations.RepoRules, usage)
	return func(name label.ApparentRepo, devDependency bool, attrs map[string]starlark.Value) error {
		usage.Repos = append(usage.Repos, name.String())
		slices.Sort(usage.Repos)
		usage.Repos = slices.Compact(usage.Repos)
		return nil
	}, nil
}

func (u *moduleExtensionUsage) Tag(className string, attrs map[string]starlark.Value) error {
	return nil
}

func (u *moduleExtensionUsage) UseRepo(repos map[label.ApparentRepo]label.ApparentRepo) error {
	for _, repo := range repos {
		u.Repos = append(u.Repos, repo.String())
	}
	slices.Sort(u.Repos)
	u.Repos = slices.Compact(u.Repos)
	return nil
}

// vendorRepoDirectoryName returns the directory name Bazel uses inside
// --vendor_dir for a canonical repository. Bazel omits the
// module-instance separator for extension repositories of the built-in
// bazel_tools and platforms modules, so those names are translated back
// here.
func vendorRepoDirectoryName(canonicalRepo label.CanonicalRepo) string {
	name := canonicalRepo.String()
	switch {
	case name == "platforms+":
		return "platforms"
	case strings.HasPrefix(name, "platforms++"):
		return "platforms+" + strings.TrimPrefix(name, "platforms++")
	case strings.HasPrefix(name, "bazel_tools++"):
		return "bazel_tools+" + strings.TrimPrefix(name, "bazel_tools++")
	default:
		return name
	}
}

// generatedRepoImportSet is the validated result of scanning
// generated_repos.json, together with the closure the snapshot implies.
type generatedRepoImportSet struct {
	imports []GeneratedRepoImport
	closure []GeneratedRepoClosureEntry
}

// parseGeneratedRepoCanonicalName parses the canonical repository name
// used by generated_repos.json. Values are canonical names without a
// leading "@@"; Bazel's built-in module spellings ("platforms" for
// "platforms+") are accepted, matching how vendor directory entries are
// named.
func parseGeneratedRepoCanonicalName(value string) (label.CanonicalRepo, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return label.CanonicalRepo{}, errors.New("must name a canonical repository")
	}
	if strings.HasPrefix(trimmed, "@@") {
		return label.CanonicalRepo{}, errors.New(`must be a canonical repository name without a leading "@@"`)
	}
	canonicalRepo, err := parseVendorRepoName(trimmed)
	if err != nil {
		return label.CanonicalRepo{}, fmt.Errorf("invalid canonical repository %q: %w", value, err)
	}
	return canonicalRepo, nil
}

// scanGeneratedRepoImports validates generated_repos.json against the
// vendor snapshot, MODULE.bazel.lock and the vendored module sources,
// and computes the complete generated repository closure. Every
// declaration problem is reported, so an operator sees the whole gap in
// one run instead of resolving one name per build.
func scanGeneratedRepoImports(vendorPath string, manifest *generatedRepoManifest, reposByName map[string]vendorRepoRecord, configuration vendorConfiguration, declarations *generatedRepoDeclarations) (*generatedRepoImportSet, error) {
	result := &generatedRepoImportSet{}
	var problems []error

	importedRepos := map[string]GeneratedRepoImport{}
	if manifest != nil {
		for _, entry := range manifest.Repos {
			canonicalRepo, err := parseGeneratedRepoCanonicalName(entry.CanonicalRepo)
			if err != nil {
				problems = append(problems, fmt.Errorf("%s: repository %q %w", generatedRepoManifestFilename, entry.CanonicalRepo, err))
				continue
			}
			canonicalRepoName := canonicalRepo.String()
			if _, _, isGeneratedRepo := canonicalRepo.GetModuleExtension(); !isGeneratedRepo {
				problems = append(problems, fmt.Errorf("%s: repository %q is not a generated repository; only module extension and use_repo_rule() repositories are imported, because plain module sources already come from the vendor snapshot", generatedRepoManifestFilename, entry.CanonicalRepo))
				continue
			}
			if _, ignored := configuration.ignoredRepos[canonicalRepoName]; ignored {
				problems = append(problems, fmt.Errorf("%s: repository %q is ignored by VENDOR.bazel, so it cannot also be imported", generatedRepoManifestFilename, entry.CanonicalRepo))
				continue
			}
			if previous, exists := importedRepos[canonicalRepoName]; exists {
				problems = append(problems, fmt.Errorf("%s: repository %q is imported more than once (also as %q)", generatedRepoManifestFilename, entry.CanonicalRepo, previous.DirectoryName))
				continue
			}

			platform := entry.Platform
			if platform == "" {
				platform = manifest.Platform
			}
			if err := validateGeneratedRepoPlatform(platform); err != nil {
				problems = append(problems, fmt.Errorf("%s: repository %q platform %q %w", generatedRepoManifestFilename, entry.CanonicalRepo, platform, err))
				continue
			}
			if platform != generatedRepoPlatformAny && platform != manifest.Platform {
				problems = append(problems, fmt.Errorf("%s: repository %q is scoped to platform %q, while the bundle was materialized for %q; a bundle must not mix platforms", generatedRepoManifestFilename, entry.CanonicalRepo, platform, manifest.Platform))
				continue
			}

			provenance, err := declarations.provenanceForGeneratedRepo(canonicalRepo)
			if err != nil {
				problems = append(problems, fmt.Errorf("%s: repository %q: %w", generatedRepoManifestFilename, entry.CanonicalRepo, err))
				continue
			}
			extension := strings.TrimSpace(entry.Extension)
			if extension == "" {
				problems = append(problems, fmt.Errorf("%s: repository %q must name the extension or repository rule that declares it", generatedRepoManifestFilename, entry.CanonicalRepo))
				continue
			}
			if !slices.Contains(provenance.DeclaredExtensions, extension) {
				problems = append(problems, fmt.Errorf("%s: repository %q declares %q, which neither MODULE.bazel.lock nor a use_extension()/use_repo_rule() declaration in the root or a vendored module corroborates for that repository", generatedRepoManifestFilename, entry.CanonicalRepo, extension))
				continue
			}
			if provenance.Platform != "" && platform != generatedRepoPlatformAny && provenance.Platform != platform {
				problems = append(problems, fmt.Errorf("%s: repository %q is scoped to platform %q, while MODULE.bazel.lock records platform %q for it", generatedRepoManifestFilename, entry.CanonicalRepo, platform, provenance.Platform))
				continue
			}

			digestValue := strings.TrimSpace(entry.SHA256)
			digest, err := parseSHA256(digestValue)
			if err != nil {
				problems = append(problems, fmt.Errorf("%s: repository %q: %w", generatedRepoManifestFilename, entry.CanonicalRepo, err))
				continue
			}

			directoryName := vendorRepoDirectoryName(canonicalRepo)
			directoryPath := filepath.Join(vendorPath, directoryName)
			info, err := os.Lstat(directoryPath)
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					problems = append(problems, fmt.Errorf("%s: repository %q has no matching directory %q in the vendor snapshot", generatedRepoManifestFilename, entry.CanonicalRepo, directoryName))
					continue
				}
				problems = append(problems, fmt.Errorf("%s: repository %q: stat %q: %w", generatedRepoManifestFilename, entry.CanonicalRepo, directoryName, err))
				continue
			}
			if !info.IsDir() {
				problems = append(problems, fmt.Errorf("%s: repository %q has non-directory counterpart %q", generatedRepoManifestFilename, entry.CanonicalRepo, directoryName))
				continue
			}
			actualDigest, err := GeneratedRepoTreeSHA256(directoryPath)
			if err != nil {
				problems = append(problems, fmt.Errorf("%s: repository %q: %w", generatedRepoManifestFilename, entry.CanonicalRepo, err))
				continue
			}
			if !slices.Equal(actualDigest, digest) {
				problems = append(problems, fmt.Errorf("%s: repository %q does not match its recorded SHA-256 (recorded %s, actual %s)", generatedRepoManifestFilename, entry.CanonicalRepo, hex.EncodeToString(digest), hex.EncodeToString(actualDigest)))
				continue
			}

			importedRepos[canonicalRepoName] = GeneratedRepoImport{
				CanonicalRepo: canonicalRepo,
				Extension:     extension,
				Platform:      platform,
				SHA256:        digest,
				DirectoryName: directoryName,
			}
		}
	}

	repoNames := make([]string, 0, len(importedRepos))
	for canonicalRepoName := range importedRepos {
		repoNames = append(repoNames, canonicalRepoName)
	}
	slices.Sort(repoNames)
	for _, canonicalRepoName := range repoNames {
		result.imports = append(result.imports, importedRepos[canonicalRepoName])
	}

	if err := errors.Join(problems...); err != nil {
		return nil, err
	}
	result.closure = computeGeneratedRepoClosure(reposByName, configuration, declarations, result.imports)
	return result, nil
}

// computeGeneratedRepoClosure merges every offline source of generated
// repository names into one sorted inventory, so that the repositories
// a pinned set of inputs can produce are known as a whole, together with
// the ones the snapshot does not supply.
func computeGeneratedRepoClosure(reposByName map[string]vendorRepoRecord, configuration vendorConfiguration, declarations *generatedRepoDeclarations, imports []GeneratedRepoImport) []GeneratedRepoClosureEntry {
	type entry struct {
		sources []string
		present bool
	}
	entries := map[string]*entry{}
	add := func(canonicalRepo string, source string, present bool) {
		e, ok := entries[canonicalRepo]
		if !ok {
			e = &entry{}
			entries[canonicalRepo] = e
		}
		e.sources = append(e.sources, source)
		e.present = e.present || present
	}

	for _, declaration := range declarations.declarations {
		for _, repoName := range declaration.Repos {
			// A module instance already ends in "+", so the repository is
			// "<instance>+<extension>+<repo name>".
			canonicalRepo := declaration.Instance() + "+" + declaration.Name + "+" + repoName
			source := "use_extension:" + declaration.Module
			if declaration.RepoRule {
				source = "use_repo_rule:" + declaration.Module
			}
			add(canonicalRepo, source, false)
		}
	}
	for identifier, repos := range declarations.lockfile {
		moduleInstance, extensionName, ok := parseLockfileExtensionIdentifier(identifier)
		if !ok {
			continue
		}
		for repoName := range repos {
			add(moduleInstance+"+"+extensionName+"+"+repoName, "lockfile", false)
		}
	}
	for canonicalRepoName := range reposByName {
		if _, ignored := configuration.ignoredRepos[canonicalRepoName]; ignored {
			continue
		}
		canonicalRepo, err := label.NewCanonicalRepo(canonicalRepoName)
		if err != nil {
			continue
		}
		if _, _, isGeneratedRepo := canonicalRepo.GetModuleExtension(); !isGeneratedRepo {
			continue
		}
		present := true
		add(canonicalRepoName, "snapshot", present)
		if _, pinned := configuration.pinnedRepos[canonicalRepoName]; pinned {
			add(canonicalRepoName, "pin", present)
		}
	}
	for _, importedRepo := range imports {
		add(importedRepo.CanonicalRepo.String(), "import", true)
	}

	closure := make([]GeneratedRepoClosureEntry, 0, len(entries))
	for canonicalRepo, e := range entries {
		slices.Sort(e.sources)
		e.sources = slices.Compact(e.sources)
		closure = append(closure, GeneratedRepoClosureEntry{
			CanonicalRepo: canonicalRepo,
			Sources:       e.sources,
			Present:       e.present,
		})
	}
	slices.SortFunc(closure, func(a, b GeneratedRepoClosureEntry) int {
		return strings.Compare(a.CanonicalRepo, b.CanonicalRepo)
	})
	return closure
}

// generatedRepoJustificationSources are the sources that justify using a
// generated repository from the snapshot in strict vendor mode. A bare
// Bazel vendor marker is deliberately not among them: a marker can be
// written by hand, and the assessment of the C1 canary recorded exactly
// that as a non-portable input.
var generatedRepoJustificationSources = []string{
	"lockfile", "pin", "import",
}

// validateGeneratedRepoProvenance fails closed in strict vendor mode
// when the snapshot contains a generated repository that nothing
// justifies, and reports the complete set of such repositories at once.
func validateGeneratedRepoProvenance(declarations *generatedRepoDeclarations, closure []GeneratedRepoClosureEntry) error {
	var unjustified []string
	for _, entry := range closure {
		if !entry.Present {
			continue
		}
		justified := false
		for _, source := range entry.Sources {
			if slices.Contains(generatedRepoJustificationSources, source) || strings.HasPrefix(source, "use_extension:") || strings.HasPrefix(source, "use_repo_rule:") {
				justified = true
			}
		}
		if !justified {
			unjustified = append(unjustified, entry.CanonicalRepo)
		}
	}
	if len(unjustified) == 0 {
		// A module source that fails to parse can only be the reason a
		// repository looks unjustified. Without an unjustified repository
		// there is nothing to explain, so a parse problem is not itself a
		// failure.
		return nil
	}

	var message strings.Builder
	fmt.Fprintf(&message, "strict vendor mode requires every generated repository in the snapshot to be justified by MODULE.bazel.lock, a use_repo()/use_repo_rule() declaration, a VENDOR.bazel pin() or a %s import, but %d are not", generatedRepoManifestFilename, len(unjustified))
	const maximumListed = 64
	for _, canonicalRepo := range unjustified[:min(len(unjustified), maximumListed)] {
		fmt.Fprintf(&message, "\n  @@%s", canonicalRepo)
	}
	if len(unjustified) > maximumListed {
		fmt.Fprintf(&message, "\n  and %d more", len(unjustified)-maximumListed)
	}
	fmt.Fprintf(&message, "\nRun \"bonanza_bazel info generated_repos\" to print the complete generated repository closure with its sources and the repositories still missing from the snapshot.")
	if len(declarations.parseProblems) > 0 {
		fmt.Fprintf(&message, "\n%d module sources could not be parsed and therefore could not justify them: %s", len(declarations.parseProblems), strings.Join(declarations.parseProblems, "; "))
	}
	return errors.New(message.String())
}

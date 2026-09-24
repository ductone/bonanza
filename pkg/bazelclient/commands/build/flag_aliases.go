package build

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"bonanza.build/pkg/bazelclient/arguments"
	"bonanza.build/pkg/label"
	pg_starlark "bonanza.build/pkg/starlark"

	"github.com/buildbarn/bb-storage/pkg/filesystem/path"
	"github.com/buildbarn/bb-storage/pkg/util"
)

type flagAliasCapturingHandler struct {
	pg_starlark.RootModuleDotBazelHandler
	aliases map[string]string
}

func (h *flagAliasCapturingHandler) FlagAlias(name string, starlarkFlag label.ApparentLabel) error {
	if name == "" || strings.ContainsAny(name, "=/:@ \t\r\n") {
		return fmt.Errorf("invalid MODULE.bazel flag alias name %q", name)
	}
	if previous, exists := h.aliases[name]; exists && previous != starlarkFlag.String() {
		return fmt.Errorf("MODULE.bazel flag alias %q has conflicting targets %q and %q", name, previous, starlarkFlag.String())
	}
	h.aliases[name] = starlarkFlag.String()
	return nil
}

// scanVendoredFlagAliases reads aliases from the selected module snapshots.
// A flag alias is a client-side name: the server receives only its resolved
// Starlark build-setting label, so repository-rule definitions stay unchanged.
func scanVendoredFlagAliases(repos []VendoredRepo) (map[string]string, error) {
	aliases := map[string]string{}
	for _, repo := range repos {
		if err := scanModuleFlagAliases(aliases, repo.RootPath, repo.CanonicalRepo); err != nil {
			return nil, err
		}
	}
	return aliases, nil
}

func scanModuleFlagAliases(aliases map[string]string, rootPath string, canonicalRepo label.CanonicalRepo) error {
	modulePath := filepath.Join(rootPath, "MODULE.bazel")
	contents, err := os.ReadFile(modulePath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read module %q: %w", canonicalRepo, err)
	}
	if !bytes.Contains(contents, []byte("flag_alias")) {
		return nil
	}
	handler := &flagAliasCapturingHandler{
		RootModuleDotBazelHandler: NewLocalPathExtractingModuleDotBazelHandler(map[label.Module]path.Parser{}, path.LocalFormat.NewParser(rootPath)),
		aliases:                   aliases,
	}
	if err := pg_starlark.ParseModuleDotBazel(
		string(contents),
		canonicalRepo.GetRootPackage().AppendTargetName(util.Must(label.NewTargetName("MODULE.bazel"))),
		path.LocalFormat,
		handler,
	); err != nil {
		return fmt.Errorf("parse flag aliases in module %q: %w", canonicalRepo, err)
	}
	return nil
}

// ResolveFlagAliases preserves the flag order, including repeats and negated
// booleans, before configuration analysis applies transitions.
func ResolveFlagAliases(overrides []arguments.BuildSettingOverride, aliases map[string]string) ([]arguments.BuildSettingOverride, error) {
	resolved := make([]arguments.BuildSettingOverride, 0, len(overrides))
	for _, override := range overrides {
		if !override.IsAlias {
			resolved = append(resolved, override)
			continue
		}
		target, ok := aliases[override.Label]
		if !ok && !override.HasExplicitValue && strings.HasPrefix(override.Label, "no") {
			target, ok = aliases[override.Label[2:]]
			if ok {
				override.Value = "false"
			}
		}
		if !ok {
			return nil, fmt.Errorf("unrecognized build flag --%s (no matching MODULE.bazel flag_alias)", override.Label)
		}
		if !strings.HasPrefix(target, "@@") && !strings.HasPrefix(target, "//") {
			return nil, fmt.Errorf("flag alias --%s refers to %s: apparent repository labels are not supported; use a canonical label", override.Label, target)
		}
		override.Label = target
		override.IsAlias = false
		resolved = append(resolved, override)
	}
	return resolved, nil
}

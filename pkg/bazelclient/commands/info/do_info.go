package info

import (
	"fmt"
	"sort"
	"strings"

	"bonanza.build/pkg/bazelclient/arguments"
	"bonanza.build/pkg/bazelclient/commands"
	commands_build "bonanza.build/pkg/bazelclient/commands/build"
	"bonanza.build/pkg/bazelclient/formatted"
	"bonanza.build/pkg/bazelclient/logging"

	"github.com/buildbarn/bb-storage/pkg/filesystem/path"
)

// printGeneratedRepoClosure prints the generated repository closure of
// the configured --vendor_dir. "present" means the snapshot supplies the
// repository; "missing" means the pinned inputs can produce it but the
// snapshot does not contain it, so strict vendor mode would reject a
// build that needs it.
func printGeneratedRepoClosure(logger logging.Logger, args *arguments.InfoCommand, workspacePath path.Parser) {
	if args.CommonFlags.VendorDir == "" {
		logger.Fatal(formatted.Text("generated_repos requires --vendor_dir"))
	}
	registryURLs := append([]string(nil), args.CommonFlags.Registry...)
	if len(registryURLs) == 0 {
		registryURLs = []string{"https://bcr.bazel.build/"}
	}
	for index, registryURL := range registryURLs {
		normalized, err := commands_build.NormalizeVendorRegistryURL(registryURL)
		if err != nil {
			logger.Fatal(formatted.Textf("Invalid registry for --vendor_dir: %s", err))
		}
		registryURLs[index] = normalized
	}
	vendorDirectory, err := commands_build.ScanVendorDirectory(
		workspacePath,
		args.CommonFlags.VendorDir,
		registryURLs,
		/* requireLockfile = */ true,
		/* strictVendorMode = */ false,
	)
	if err != nil {
		logger.Fatal(formatted.Textf("Invalid --vendor_dir=%q: %s", args.CommonFlags.VendorDir, err))
	}
	for _, entry := range vendorDirectory.GeneratedRepoClosure {
		state := "missing"
		if entry.Present {
			state = "present"
		}
		fmt.Printf("%s\t@@%s\t%s\n", state, entry.CanonicalRepo, strings.Join(entry.Sources, ","))
	}
}

// DoInfo implements the "bazel info" command, which prints the
// currently effective values of certain configuration options. Most of
// these pertain to paths on the local system where data is read or
// written.
func DoInfo(args *arguments.InfoCommand, workspacePath path.Parser) {
	logger := logging.NewLoggerFromFlags(&args.CommonFlags)
	commands.ValidateInsideWorkspace(logger, "info", workspacePath)

	workspacePathBuilder, scopeWalker := path.EmptyBuilder.Join(path.NewAbsoluteScopeWalker(path.VoidComponentWalker))
	if err := path.Resolve(workspacePath, scopeWalker); err != nil {
		logger.Fatal(formatted.Textf("Failed to obtain workspace path: %s", err))
	}
	workspacePathStr, err := path.LocalFormat.GetString(workspacePathBuilder)
	if err != nil {
		logger.Fatal(formatted.Textf("Failed to obtain workspace path: %s", err))
	}

	keys := map[string]string{
		"workspace": workspacePathStr,
	}

	var keysToPrint []string
	switch len(args.Arguments) {
	case 0:
		keysToPrint = make([]string, 0, len(keys))
		for key := range keys {
			keysToPrint = append(keysToPrint, key)
		}
		sort.Strings(keysToPrint)
	case 1:
		key := args.Arguments[0]
		if key == "generated_repos" {
			// Print the complete generated repository closure of the
			// pinned inputs: which repositories the exact snapshot and
			// lockfile can produce, what requires them, and which ones the
			// snapshot does not supply. This is the inventory an operator
			// needs to complete a portable bundle, instead of discovering
			// one missing repository per build.
			printGeneratedRepoClosure(logger, args, workspacePath)
			return
		}
		value, ok := keys[key]
		if !ok {
			logger.Fatal(formatted.Textf("Unknown key: %#v", key))
		}
		fmt.Println(value)
	default:
		keysToPrint = args.Arguments
	}

	unknownKeysSet := map[string]struct{}{}
	var unknownKeysList []string
	for _, key := range keysToPrint {
		if value, ok := keys[key]; ok {
			fmt.Printf("%s: %s\n", key, value)
		} else if _, ok := unknownKeysSet[key]; !ok {
			unknownKeysSet[key] = struct{}{}
			unknownKeysList = append(unknownKeysList, fmt.Sprintf("%#v", key))
		}
	}

	if len(unknownKeysList) > 0 {
		logger.Fatal(formatted.Textf("Unknown key(s): %s", strings.Join(unknownKeysList, ", ")))
	}
}

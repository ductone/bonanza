package arguments

import (
	"fmt"
	"os"

	"github.com/buildbarn/bb-storage/pkg/filesystem"
	"github.com/buildbarn/bb-storage/pkg/filesystem/path"
)

// Parse command line arguments in a way that is consistent with how
// Bazel parses them. In the process, load bazelrc files and apply any
// relevant options listed in those files as well.
func Parse(args []string, rootDirectory filesystem.Directory, pathFormat path.Format, workspacePath, homeDirectoryPath, workingDirectoryPath path.Parser) (Command, error) {
	startupFlags, argsParsed, err := ParseStartupFlags(args)
	if err != nil {
		return nil, err
	}
	args = args[argsParsed:]

	bazelRCPaths, err := GetBazelRCPaths(startupFlags, pathFormat, workspacePath, homeDirectoryPath, workingDirectoryPath)
	if err != nil {
		return nil, err
	}

	configurationDirectives, err := ParseBazelRCFiles(bazelRCPaths, rootDirectory, pathFormat, workspacePath, workingDirectoryPath)
	if err != nil {
		return nil, err
	}
	// Bazel accepts startup directives from rc files. Ignoring one can change
	// cache, workspace, or daemon behavior without telling the caller; until
	// Bonanza supports applying them during rc discovery, reject them.
	for _, directive := range configurationDirectives["startup"] {
		if len(directive) != 0 {
			return nil, fmt.Errorf("bazelrc startup option %q is not supported by the one-shot Bonanza client", directive[0])
		}
	}

	cmd, err := ParseCommandAndArguments(configurationDirectives, args)
	if err != nil {
		return nil, err
	}
	if parsed := cmd.(assignableCommand); parsed.getCommonFlags().AnnounceRc {
		for _, option := range parsed.getRCAnnouncements() {
			fmt.Fprintln(os.Stderr, "rc option:", option)
		}
	}
	return cmd, nil
}

package arguments

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"

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
			option, _, _ := strings.Cut(directive[0], "=")
			return nil, fmt.Errorf("bazelrc startup option %q is unsupported by the one-shot Bonanza client; opt in with --ignore_all_rc_files and an audited Bonanza-only rc/argv (Bazel --host_jvm_args and daemon options cannot be applied)", option)
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

// ParseBonanzaEndpoint distinguishes Bonanza storage/scheduler RPCs from Bazel's
// REAPI cache/executor, which may use the same host:port and grpc transport.
// The explicit scheme is a client assertion, not a protocol translation.
func ParseBonanzaEndpoint(endpoint string) (target string, tls bool, err error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", false, fmt.Errorf("invalid Bonanza endpoint URL")
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return "", false, fmt.Errorf("Bonanza endpoint must not contain credentials, query, fragment, or opaque path")
	}
	switch u.Scheme {
	case "bonanza+grpc", "bonanza+grpcs":
		if u.Path != "" {
			return "", false, fmt.Errorf("Bonanza endpoint must be a host:port without a path")
		}
		host, port, err := net.SplitHostPort(u.Host)
		if err != nil || host == "" {
			return "", false, fmt.Errorf("Bonanza endpoint must be a host:port without a path")
		}
		portNumber, err := strconv.Atoi(port)
		if err != nil || portNumber < 1 || portNumber > 65535 {
			return "", false, fmt.Errorf("Bonanza endpoint port must be in 1..65535")
		}
		return u.Host, u.Scheme == "bonanza+grpcs", nil
	case "bonanza+unix":
		if u.Host != "" || !strings.HasPrefix(u.Path, "/") || u.Path == "/" {
			return "", false, fmt.Errorf("Bonanza UNIX endpoint must use bonanza+unix:///absolute/path")
		}
		return "unix://" + u.Path, false, nil
	default:
		return "", false, fmt.Errorf("endpoint is not a Bonanza endpoint: use bonanza+grpc://, bonanza+grpcs:// or bonanza+unix:/// with Bonanza services; Bazel REAPI/HTTP cache and executor addresses are incompatible")
	}
}

package arguments

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// BuildSettingOverride contains the label of a user-defined build
// setting for which an override was provided on the command line or in
// a bazelrc file, and the value that is assigned to it.
type BuildSettingOverride struct {
	Label            string
	Value            string
	IsAlias          bool
	HasExplicitValue bool
}

// parseBuildSettingOverrideFlag interprets command line options that
// refer to user-defined build settings by label, such as
// --@rules_foo//:my_flag=value or --//my/pkg:my_flag. Providing no
// value causes boolean build settings to be enabled, while the "no"
// prefix (e.g., --no//my/pkg:my_flag) causes them to be disabled.
func parseBuildSettingOverrideFlag(longOptionName string, hasValue bool, value string) (string, string, bool) {
	label := longOptionName[len("--"):]
	if rest, ok := strings.CutPrefix(label, "no"); ok &&
		(strings.HasPrefix(rest, "@") || strings.HasPrefix(rest, "//")) {
		if hasValue {
			// Negated boolean options cannot carry a value.
			return "", "", false
		}
		return rest, "false", true
	}
	if !strings.HasPrefix(label, "@") && !strings.HasPrefix(label, "//") {
		return "", "", false
	}
	if !hasValue {
		value = "true"
	}
	return label, value, true
}

// Command denotes a specific subcommand of the Bazel command line tool
// for which arguments have been parsed.
type Command interface {
	Reset()
}

type commandAncestor struct {
	name      string
	mustApply bool
}

// formatRCAnnouncement avoids echoing environment values and encryption keys
// into logs. Bare NAME overrides are still reported by name.
func formatRCAnnouncement(directive, option string) string {
	for _, flag := range []string{"--action_env=", "--repo_env="} {
		if value, ok := strings.CutPrefix(option, flag); ok {
			if name, _, explicit := strings.Cut(value, "="); explicit {
				option = flag + name + "=<redacted>"
			}
			break
		}
	}
	if strings.HasPrefix(option, "--remote_encryption_key=") {
		option = "--remote_encryption_key=<redacted>"
	}
	return directive + ": " + option
}

// ParseCommandAndArguments parses the name of a command like "build" or
// "test", and any of the arguments that follow that are specific to
// that command.
func ParseCommandAndArguments(configurationDirectives ConfigurationDirectives, args []string) (Command, error) {
	var cmd assignableCommand
	var ancestors []commandAncestor
	if len(args) == 0 {
		cmd = &HelpCommand{}
		ancestors = helpAncestors
	} else {
		var ok bool
		cmd, ancestors, ok = newCommandByName(args[0])
		if !ok {
			return nil, CommandNotRecognizedError{
				Command: args[0],
			}
		}
		args = args[1:]
	}
	cmd.Reset()

	if err := parseArguments(cmd, ancestors, configurationDirectives, args); err != nil {
		return nil, err
	}
	common := cmd.getCommonFlags()
	for _, endpoint := range []struct{ name, value string }{
		{"--remote_cache", common.RemoteCache},
		{"--remote_executor", common.RemoteExecutor},
	} {
		if endpoint.value != "" {
			if _, _, err := ParseBonanzaEndpoint(endpoint.value); err != nil {
				return nil, fmt.Errorf("%s: %w", endpoint.name, err)
			}
		}
	}
	seconds, err := strconv.ParseFloat(common.ShowProgressRateLimit, 64)
	if err != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds < 0 || seconds > float64(math.MaxInt64)/1e9 {
		return nil, fmt.Errorf("--show_progress_rate_limit must be a finite nonnegative number of seconds, got %q", common.ShowProgressRateLimit)
	}
	return cmd, nil
}

// Bazel command options without matching Bonanza semantics must not be
// mistaken for MODULE.bazel flag_alias declarations.
func unsupportedBazelFlag(name string) string {
	switch name {
	case "--verbose_failures", "--noverbose_failures", "--experimental_ui_max_stdouterr_bytes":
		return "Bonanza cannot reproduce Bazel's action command/stderr presentation"
	case "--show_result":
		return "Bonanza does not yet report Bazel's top-level target result paths"
	case "--test_summary":
		return "Bonanza does not yet provide Bazel's detailed test summary"
	case "--incompatible_default_to_explicit_init_py", "--noincompatible_default_to_explicit_init_py":
		return "Bonanza does not implement Bazel's Python implicit __init__.py toggle"
	case "--remote_upload_local_results", "--noremote_upload_local_results",
		"--remote_timeout", "--remote_retries", "--remote_default_exec_properties",
		"--remote_local_fallback", "--noremote_local_fallback":
		return "Bazel REAPI cache/executor controls cannot configure Bonanza storage/scheduler"
	case "--remote_download_minimal", "--remote_download_outputs", "--jobs":
		return "Bonanza's execution and output-materialization policies differ from Bazel"
	case "--check_visibility", "--nocheck_visibility":
		return "Bonanza does not implement Bazel's visibility override"
	default:
		return ""
	}
}

func unsupportedBazelStartupFlag(name string) string {
	switch name {
	case "--max_idle_secs", "--host_jvm_args":
		return "Bonanza is a one-shot Go client, not Bazel's persistent JVM server"
	case "--output_base", "--experimental_remote_repo_contents_cache":
		return "Bonanza does not use Bazel's local output base or REAPI repository cache"
	default:
		return ""
	}
}

var boolExpectedValues = []string{
	"true",
	"false",
	"yes",
	"no",
	"1",
	"0",
}

type stackEntry struct {
	remainingArgs []string
	mustApply     bool
	allowFlags    bool
	directiveName string
}

func parseBool(hasValue bool, value string, out *bool, flagName string) error {
	v := true
	if hasValue {
		switch value {
		case "0", "false", "no":
			v = false
		case "1", "true", "yes":
			v = true
		default:
			return FlagInvalidEnumValueError{
				Flag:           flagName,
				Value:          value,
				ExpectedValues: boolExpectedValues,
			}
		}
	}
	if out != nil {
		*out = v
	}
	return nil
}

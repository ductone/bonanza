package arguments

import (
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

	return cmd, parseArguments(cmd, ancestors, configurationDirectives, args)
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

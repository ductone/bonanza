package help

import (
	"fmt"
	"maps"
	"os"
	"slices"

	"bonanza.build/pkg/bazelclient/arguments"
)

// commandDescriptions contains a single line description of every
// command that bonanza_bazel implements.
var commandDescriptions = map[string]string{
	"build":   "Build the specified targets.",
	"clean":   "Remove build outputs. Bonanza stores all build state remotely, so this does nothing.",
	"help":    "Print help for commands or options.",
	"info":    "Display runtime info about the build tool.",
	"license": "Print the license of this software.",
	"run":     "Build a single target and run it.",
	"version": "Print version information.",
}

// DoHelp implements the "bazel help" command, which prints the list of
// commands that are supported.
func DoHelp(args *arguments.HelpCommand) {
	if len(args.Arguments) > 0 {
		if _, ok := commandDescriptions[args.Arguments[0]]; !ok {
			fmt.Fprintf(os.Stderr, "Unknown command: %s\n", args.Arguments[0])
			os.Exit(1)
		}
	}

	fmt.Println("Usage: bonanza_bazel <command> <options> ...")
	fmt.Println()
	fmt.Println("Available commands:")
	for _, name := range slices.Sorted(maps.Keys(commandDescriptions)) {
		fmt.Printf("  %-10s %s\n", name, commandDescriptions[name])
	}
	fmt.Println()
	fmt.Println("Use \"bonanza_bazel help <command>\" to check whether a command exists.")
}

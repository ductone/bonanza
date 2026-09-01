package clean

import (
	"bonanza.build/pkg/bazelclient/arguments"
	"bonanza.build/pkg/bazelclient/formatted"
	"bonanza.build/pkg/bazelclient/logging"
)

// DoClean implements the "bazel clean" command. As Bonanza performs all
// caching remotely, and output files are written to a directory that the
// user controls, there is nothing that needs to be discarded.
func DoClean(args *arguments.CleanCommand) {
	logging.NewLoggerFromFlags(&args.CommonFlags).
		Info(formatted.Text("Nothing to clean: all build state is stored remotely"))
}

package run

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"bonanza.build/pkg/bazelclient/arguments"
	"bonanza.build/pkg/bazelclient/commands"
	commands_build "bonanza.build/pkg/bazelclient/commands/build"
	"bonanza.build/pkg/bazelclient/formatted"
	"bonanza.build/pkg/bazelclient/logging"
	model_core "bonanza.build/pkg/model/core"
	model_analysis_pb "bonanza.build/pkg/proto/model/analysis"

	"github.com/buildbarn/bb-storage/pkg/filesystem/path"

	"google.golang.org/protobuf/proto"
)

// runfilesEnvironmentVariables are the environment variables that
// describe the location of the runfiles directory. They are recomputed
// for the target that is launched, so any values that the invocation of
// bonanza_bazel inherited need to be discarded.
var runfilesEnvironmentVariables = []string{
	"JAVA_RUNFILES",
	"RUNFILES_DIR",
	"RUNFILES_MANIFEST_FILE",
	"RUNFILES_MANIFEST_ONLY",
}

// DoRun implements the "bazel run" command, which builds a single
// target in the current workspace and launches the executable that it
// provides.
func DoRun(args *arguments.RunCommand, workspacePath path.Parser) {
	logger := logging.NewLoggerFromFlags(&args.CommonFlags)
	commands.ValidateInsideWorkspace(logger, "run", workspacePath)

	targetPatternArguments := args.Arguments
	executableArguments := []string(nil)
	buildFlags := args.BuildFlags
	if buildFlags.TargetPatternFile != "" {
		var err error
		targetPatternArguments, err = commands_build.ResolveTargetPatterns(args.Arguments, buildFlags.TargetPatternFile)
		if err != nil {
			logger.Fatal(formatted.Text(err.Error()))
		}
		buildFlags.TargetPatternFile = ""
	} else if len(targetPatternArguments) > 0 {
		targetPatternArguments = args.Arguments[:1]
		executableArguments = args.Arguments[1:]
	}
	if len(targetPatternArguments) != 1 {
		logger.Fatal(formatted.Text("The \"run\" command expects exactly one target"))
	}

	// Build the target. In addition to the outputs of the build, we
	// need to know which executable to launch and which runfiles
	// belong to it, which RunTarget provides.
	var runTargetKey *model_analysis_pb.RunTarget_Key
	o := commands_build.PerformBuild(
		"run",
		&args.CommonFlags,
		&buildFlags,
		args.BuildSettingOverrides,
		targetPatternArguments,
		func(targetPatterns []string, configurations []*model_analysis_pb.BuildResult_Key_Configuration) []proto.Message {
			if len(configurations) != 1 {
				logger.Fatal(formatted.Text("The \"run\" command cannot be used with multiple target platforms"))
			}
			runTargetKey = &model_analysis_pb.RunTarget_Key{
				TargetPattern: targetPatterns[0],
				Configuration: configurations[0],
			}
			return []proto.Message{runTargetKey}
		},
		workspacePath,
	)
	if o == nil {
		logger.Fatal(formatted.Text("Build did not yield any results"))
	}

	buildResultValue, err := commands_build.LookUpValue[model_analysis_pb.BuildResult_Value](o, &model_analysis_pb.BuildResult_Key{
		TargetPatterns: o.TargetPatterns,
		Configurations: o.Configurations,
	})
	if err != nil {
		logger.Fatal(formatted.Textf("Failed to look up build result: %s", err))
	}
	if err := o.MaterializeBuildOutputs(model_core.Nested(buildResultValue, buildResultValue.Message.RootDirectory)); err != nil {
		logger.Fatal(formatted.Textf("Failed to materialize build outputs: %s", err))
	}

	runTargetValue, err := commands_build.LookUpValue[model_analysis_pb.RunTarget_Value](o, runTargetKey)
	if err != nil {
		logger.Fatal(formatted.Textf("Failed to look up the executable of the target: %s", err))
	}

	outputPathStr, err := o.GetOutputPath()
	if err != nil {
		logger.Fatal(formatted.Textf("Failed to determine output path: %s", err))
	}
	executablePathStr := filepath.Join(outputPathStr, filepath.FromSlash(runTargetValue.Message.ExecutablePath))

	// Source files and predeclared output files report themselves as
	// the executable of their own DefaultInfo provider, so whether a
	// target can actually be launched is only known once its output
	// files have been materialized.
	if fileInfo, err := os.Stat(executablePathStr); err != nil {
		logger.Fatal(formatted.Textf("Failed to inspect executable: %s", err))
	} else if !fileInfo.Mode().IsRegular() || fileInfo.Mode().Perm()&0o111 == 0 {
		logger.Fatal(formatted.Textf("Target %#v does not provide an executable", targetPatternArguments[0]))
	}

	// Materialize the runfiles directory next to the executable, in
	// the same location in which actions that use the target as a
	// tool expect it to be.
	workingDirectoryStr := outputPathStr
	runfilesDirectoryPathStr := ""
	if runfilesDirectory := runTargetValue.Message.RunfilesDirectory; runfilesDirectory != nil {
		runfilesDirectoryPathStr = executablePathStr + ".runfiles"
		filesWritten, symlinksWritten, err := o.MaterializeDirectory(
			model_core.Nested(runTargetValue, runfilesDirectory),
			runfilesDirectoryPathStr,
		)
		if err != nil {
			logger.Fatal(formatted.Textf("Failed to materialize runfiles: %s", err))
		}
		logger.Info(formatted.Textf("Wrote %d runfiles and %d symbolic links to %s", filesWritten, symlinksWritten, runfilesDirectoryPathStr))

		// Just like Bazel, run the executable from the directory
		// inside the runfiles directory that corresponds to
		// ctx.workspace_name, so that it can access its runfiles
		// through relative paths.
		workingDirectoryStr = runfilesDirectoryPathStr
		if workspaceName := runTargetValue.Message.WorkspaceName; workspaceName != "" {
			workspaceDirectoryStr := filepath.Join(runfilesDirectoryPathStr, workspaceName)
			if _, err := os.Stat(workspaceDirectoryStr); err == nil {
				workingDirectoryStr = workspaceDirectoryStr
			}
		}
	}

	argv, err := getCommandLine(args.RunFlags.RunUnder, executablePathStr, executableArguments)
	if err != nil {
		logger.Fatal(formatted.Text(err.Error()))
	}

	workspacePathStr, err := o.GetWorkspacePath()
	if err != nil {
		logger.Fatal(formatted.Textf("Failed to determine workspace path: %s", err))
	}
	invocationDirectoryStr, err := os.Getwd()
	if err != nil {
		logger.Fatal(formatted.Textf("Failed to obtain working directory: %s", err))
	}

	environment := make([]string, 0, len(os.Environ())+4)
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if !slices.Contains(runfilesEnvironmentVariables, name) {
			environment = append(environment, entry)
		}
	}
	if runfilesDirectoryPathStr != "" {
		environment = append(
			environment,
			"JAVA_RUNFILES="+runfilesDirectoryPathStr,
			"RUNFILES_DIR="+runfilesDirectoryPathStr,
		)
	}
	// Bazel exposes these to allow executables to operate on the
	// workspace, even though they are launched from the runfiles
	// directory.
	environment = append(
		environment,
		"BUILD_WORKING_DIRECTORY="+invocationDirectoryStr,
		"BUILD_WORKSPACE_DIRECTORY="+workspacePathStr,
	)

	logger.Info(formatted.Textf("Running %s", strings.Join(argv, " ")))
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = workingDirectoryStr
	cmd.Env = environment
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			os.Exit(exitError.ExitCode())
		}
		logger.Fatal(formatted.Textf("Failed to run %#v: %s", argv[0], err))
	}
}

// getCommandLine returns the command line of the executable that needs
// to be launched, taking the value of --run_under into account.
func getCommandLine(runUnder, executablePathStr string, executableArguments []string) ([]string, error) {
	var argv []string
	if runUnder != "" {
		if strings.HasPrefix(runUnder, "//") || strings.HasPrefix(runUnder, "@") {
			return nil, fmt.Errorf("--run_under=%#v refers to a target, which is not supported yet", runUnder)
		}
		argv = strings.Fields(runUnder)
		if len(argv) == 0 {
			return nil, fmt.Errorf("--run_under=%#v does not contain a command", runUnder)
		}
	}
	argv = append(argv, executablePathStr)
	return append(argv, executableArguments...), nil
}

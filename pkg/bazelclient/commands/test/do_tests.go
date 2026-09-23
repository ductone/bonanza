package test

import (
	"fmt"
	"os"
	"runtime"

	"bonanza.build/pkg/bazelclient/arguments"
	commands_build "bonanza.build/pkg/bazelclient/commands/build"
	"bonanza.build/pkg/bazelclient/formatted"
	"bonanza.build/pkg/bazelclient/logging"
	model_core "bonanza.build/pkg/model/core"
	model_filesystem "bonanza.build/pkg/model/filesystem"
	model_parser "bonanza.build/pkg/model/parser"
	model_analysis_pb "bonanza.build/pkg/proto/model/analysis"
	model_command_pb "bonanza.build/pkg/proto/model/command"
	model_core_pb "bonanza.build/pkg/proto/model/core"
	model_filesystem_pb "bonanza.build/pkg/proto/model/filesystem"
	"bonanza.build/pkg/storage/object"

	"github.com/buildbarn/bb-storage/pkg/filesystem/path"

	"golang.org/x/sync/semaphore"
	"google.golang.org/protobuf/proto"
)

// testsFailedExitCode is the exit status that Bazel uses to report that
// everything built, but that one or more tests did not pass. Scripts
// rely on it being distinct from the status used for build failures.
const testsFailedExitCode = 3

// DoTest implements the "bazel test" command, which builds a specified
// set of targets in the current workspace and runs the ones that are
// tests.
func DoTest(args *arguments.TestCommand, workspacePath path.Parser) {
	logger := logging.NewLoggerFromFlags(&args.CommonFlags)

	// The target patterns and configurations that the tests need to
	// be run for are only known once the invocation has been
	// processed, so capture the key that gets requested.
	var testResultKey *model_analysis_pb.TestResult_Key
	o := commands_build.PerformBuild(
		"test",
		&args.CommonFlags,
		&args.BuildFlags,
		args.BuildSettingOverrides,
		args.Arguments,
		func(targetPatterns []string, configurations []*model_analysis_pb.BuildResult_Key_Configuration) []proto.Message {
			testResultKey = &model_analysis_pb.TestResult_Key{
				Configurations: configurations,
				TargetPatterns: targetPatterns,
				TestFilter:     args.TestFlags.TestFilter,
			}
			return []proto.Message{testResultKey}
		},
		workspacePath,
	)
	if o == nil {
		return
	}

	// Tests only run for targets that built successfully, so leave
	// the outputs of the build behind just like "build" would have.
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

	testResult, err := commands_build.LookUpValue[model_analysis_pb.TestResult_Value](o, testResultKey)
	if err != nil {
		logger.Fatal(formatted.Textf("Failed to look up test results: %s", err))
	}

	tests := testResult.Message.Tests
	if len(tests) == 0 {
		logger.Info(formatted.Text("No test targets were found, yet testing was requested"))
		return
	}

	logPrinter := newTestLogPrinter(o)
	failed := 0
	for _, test := range tests {
		passed := test.Status == model_analysis_pb.TestStatus_TEST_STATUS_PASSED
		if !passed {
			failed++
		}

		if args.TestFlags.TestOutput == arguments.TestOutput_All ||
			(args.TestFlags.TestOutput == arguments.TestOutput_Errors && !passed) {
			if err := logPrinter.print(model_core.Nested(testResult, test.OutputsReference)); err != nil {
				logger.Error(formatted.Textf("Failed to print log of test %#v: %s", test.Label, err))
			}
		}

		if passed {
			logger.Info(
				formatted.Join(
					formatted.Textf("%s  ", test.Label),
					formatted.Green(formatted.Text("PASSED")),
				),
			)
		} else {
			logger.Info(
				formatted.Join(
					formatted.Textf("%s  ", test.Label),
					formatted.Red(formatted.Textf("FAILED (exit code %d)", test.ExitCode)),
				),
			)
		}
	}

	logger.Info(formatted.Textf("Ran %d tests, %d passed, %d failed", len(tests), len(tests)-failed, failed))
	if failed > 0 {
		os.Exit(testsFailedExitCode)
	}
}

// testLogPrinter writes the data that test binaries wrote to standard
// output and standard error to the console of the client.
type testLogPrinter struct {
	outcome       *commands_build.Outcome
	outputsReader model_parser.MessageObjectReader[object.LocalReference, *model_command_pb.Outputs]
	fileReader    *model_filesystem.FileReader[object.LocalReference]
}

func newTestLogPrinter(o *commands_build.Outcome) *testLogPrinter {
	parsedObjectPoolIngester := o.ParsedObjectPoolIngester
	directoryAccessParameters := o.DirectoryAccessParameters
	fileAccessParameters := o.FileAccessParameters
	return &testLogPrinter{
		outcome: o,
		outputsReader: model_parser.LookupParsedObjectReader(
			parsedObjectPoolIngester,
			model_parser.NewChainedObjectParser(
				model_parser.NewEncodedObjectParser[object.LocalReference](directoryAccessParameters.GetEncoder()),
				model_parser.NewProtoObjectParser[object.LocalReference, model_command_pb.Outputs](),
			),
		),
		fileReader: model_filesystem.NewFileReader(
			model_parser.LookupParsedObjectReader(
				parsedObjectPoolIngester,
				model_parser.NewChainedObjectParser(
					model_parser.NewEncodedObjectParser[object.LocalReference](fileAccessParameters.GetFileContentsListEncoder()),
					model_filesystem.NewFileContentsListObjectParser[object.LocalReference](),
				),
			),
			model_parser.LookupParsedObjectReader(
				parsedObjectPoolIngester,
				model_parser.NewChainedObjectParser(
					model_parser.NewEncodedObjectParser[object.LocalReference](fileAccessParameters.GetChunkEncoder()),
					model_parser.NewRawObjectParser[object.LocalReference](),
				),
			),
			semaphore.NewWeighted(int64(runtime.NumCPU())),
		),
	}
}

// print writes the standard output and standard error of a single test
// action, which is what Bazel would have stored in its test.log.
func (p *testLogPrinter) print(outputsReference model_core.Message[*model_core_pb.DecodableReference, object.LocalReference]) error {
	outputs, err := model_parser.MaybeDereference(p.outcome.Context, p.outputsReader, outputsReference)
	if err != nil {
		return fmt.Errorf("failed to obtain outputs of test action: %w", err)
	}
	for _, log := range []model_core.Message[*model_filesystem_pb.FileContents, object.LocalReference]{
		model_core.Nested(outputs, outputs.Message.GetStdout()),
		model_core.Nested(outputs, outputs.Message.GetStderr()),
	} {
		if log.Message == nil {
			continue
		}
		entry, err := model_filesystem.NewFileContentsEntryFromProto(log)
		if err != nil {
			return err
		}
		if err := p.fileReader.FileWriteTo(p.outcome.Context, entry, os.Stdout); err != nil {
			return err
		}
	}
	return nil
}

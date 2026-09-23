package cquery

import (
	"fmt"
	"maps"
	"path/filepath"
	"slices"

	"bonanza.build/pkg/bazelclient/arguments"
	commands_build "bonanza.build/pkg/bazelclient/commands/build"
	"bonanza.build/pkg/bazelclient/commands/query"
	"bonanza.build/pkg/bazelclient/formatted"
	"bonanza.build/pkg/bazelclient/logging"
	model_analysis_pb "bonanza.build/pkg/proto/model/analysis"

	"github.com/buildbarn/bb-storage/pkg/filesystem/path"
	"google.golang.org/protobuf/proto"
)

func outputFiles(outputRoot string, targets []*model_analysis_pb.ConfiguredQueryResult_Value_Target) ([]string, error) {
	files := map[string]struct{}{}
	for _, target := range targets {
		for _, file := range target.Files {
			localPath := filepath.FromSlash(file)
			if !filepath.IsLocal(localPath) {
				return nil, fmt.Errorf("invalid output path %#v for target %#v", file, target.Label)
			}
			files[filepath.Join(outputRoot, localPath)] = struct{}{}
		}
	}
	return slices.Sorted(maps.Keys(files)), nil
}

// DoCquery analyzes the configured default outputs without running their actions.
func DoCquery(args *arguments.CqueryCommand, workspacePath path.Parser) {
	logger := logging.NewLoggerFromFlags(&args.CommonFlags)
	if len(args.Arguments) == 0 {
		logger.Fatal(formatted.Text("A cquery expression must be provided"))
	}
	if args.BuildFlags.TargetPatternFile != "" {
		logger.Fatal(formatted.Text("--target_pattern_file is not a cquery expression"))
	}
	expression, err := query.ParseExpression(args.Arguments)
	if err != nil {
		logger.Fatal(formatted.Textf("Invalid cquery expression: %s", err))
	}
	encodedExpression, err := query.Encode(expression)
	if err != nil {
		logger.Fatal(formatted.Textf("Cannot encode cquery expression: %s", err))
	}
	var resultKey *model_analysis_pb.ConfiguredQueryResult_Key
	outcome := commands_build.PerformBuild(
		"cquery", &args.CommonFlags, &args.BuildFlags,
		args.BuildSettingOverrides, nil,
		func(_ []string, configurations []*model_analysis_pb.BuildResult_Key_Configuration) []proto.Message {
			resultKey = &model_analysis_pb.ConfiguredQueryResult_Key{
				Expression:     encodedExpression,
				Configurations: configurations,
			}
			return []proto.Message{resultKey}
		},
		workspacePath,
	)
	if outcome == nil {
		logger.Fatal(formatted.Text("Configured query did not yield results"))
	}
	result, err := commands_build.LookUpValue[model_analysis_pb.ConfiguredQueryResult_Value](outcome, resultKey)
	if err != nil {
		logger.Fatal(formatted.Textf("Failed to read configured query result: %s", err))
	}

	switch args.CqueryFlags.Output {
	case arguments.QueryOutput_Label:
		for _, target := range result.Message.Targets {
			fmt.Println(target.Label)
		}
	case arguments.QueryOutput_LabelKind:
		for _, target := range result.Message.Targets {
			fmt.Printf("%s %s\n", target.Kind, target.Label)
		}
	case arguments.QueryOutput_Files:
		outputRoot, err := outcome.GetOutputPath()
		if err != nil {
			logger.Fatal(formatted.Textf("Failed to determine output path: %s", err))
		}
		files, err := outputFiles(outputRoot, result.Message.Targets)
		if err != nil {
			logger.Fatal(formatted.Text(err.Error()))
		}
		for _, file := range files {
			fmt.Println(file)
		}
	default:
		logger.Fatal(formatted.Text("Unsupported cquery output format"))
	}
}

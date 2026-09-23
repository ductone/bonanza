package query

import (
	"fmt"
	"strings"

	"bonanza.build/pkg/bazelclient/arguments"
	"bonanza.build/pkg/bazelclient/formatted"
	"bonanza.build/pkg/bazelclient/logging"
	model_analysis_pb "bonanza.build/pkg/proto/model/analysis"

	"github.com/buildbarn/bb-storage/pkg/filesystem/path"
)

// DoQuery implements "bazel query", which evaluates a query expression
// over the loading-phase graph and prints the targets it matches.
func DoQuery(args *arguments.QueryCommand, workspacePath path.Parser) {
	logger := logging.NewLoggerFromFlags(&args.CommonFlags)
	if len(args.Arguments) == 0 {
		logger.Fatal(formatted.Text("A query expression must be provided"))
	}
	expression := parseOrFatal(logger, args.Arguments)

	value := runQuery(logger, "query", &args.CommonFlags, workspacePath, &model_analysis_pb.QueryResult_Key{
		Expression: expression,
	})
	result, ok := value.(*model_analysis_pb.QueryResult_Value)
	if !ok {
		logger.Fatal(formatted.Text("The query yielded a value of an unexpected type"))
	}

	// The cluster returns targets already sorted, and carries each
	// target's kind, so --output=label_kind needs no second round trip.
	for _, target := range result.Targets {
		if args.QueryFlags.Output == arguments.QueryOutput_LabelKind {
			fmt.Printf("%s %s\n", target.Kind, target.Label)
		} else {
			fmt.Println(target.Label)
		}
	}
}

// DoCQuery implements "bazel cquery", which evaluates the same
// expression language over the graph of a configuration.
//
// The difference that matters is in the edges, not the syntax: a
// select() contributes only the branch the configuration selects, and an
// alias reports what it expands to. A target reached in more than one
// configuration is printed once per configuration, which is why the
// label is followed by a configuration in parentheses.
func DoCQuery(args *arguments.CqueryCommand, workspacePath path.Parser) {
	logger := logging.NewLoggerFromFlags(&args.CommonFlags)
	if len(args.Arguments) == 0 {
		logger.Fatal(formatted.Text("A query expression must be provided"))
	}
	expression := parseOrFatal(logger, args.Arguments)

	// The configurations to evaluate in are built the same way a build
	// builds them, so that "cquery --platforms=X" answers for the same
	// configuration "build --platforms=X" would use.
	buildSettingOverrides := make([]*model_analysis_pb.BuildResult_Key_BuildSettingOverride, 0, len(args.BuildSettingOverrides))
	for _, override := range args.BuildSettingOverrides {
		buildSettingOverrides = append(buildSettingOverrides, &model_analysis_pb.BuildResult_Key_BuildSettingOverride{
			Label: override.Label,
			Value: override.Value,
		})
	}
	// Split and defaulted exactly as PerformBuild does, so that
	// "cquery --platforms=X" answers for the configuration
	// "build --platforms=X" would use. Diverging here would make cquery
	// describe a build nobody runs.
	targetPlatforms := strings.FieldsFunc(args.BuildFlags.Platforms, func(r rune) bool { return r == ',' })
	if len(targetPlatforms) == 0 {
		targetPlatforms = []string{"@platforms//host"}
	}
	configurations := make([]*model_analysis_pb.BuildResult_Key_Configuration, 0, len(targetPlatforms))
	for _, targetPlatform := range targetPlatforms {
		configurations = append(configurations, &model_analysis_pb.BuildResult_Key_Configuration{
			BuildSettingOverrides: append(
				[]*model_analysis_pb.BuildResult_Key_BuildSettingOverride{{
					Label: "@bazel_tools//command_line_option:platforms",
					Value: targetPlatform,
				}},
				buildSettingOverrides...,
			),
		})
	}

	value := runQuery(logger, "cquery", &args.CommonFlags, workspacePath, &model_analysis_pb.ConfiguredQueryResult_Key{
		Expression:     expression,
		Configurations: configurations,
	})
	result, ok := value.(*model_analysis_pb.ConfiguredQueryResult_Value)
	if !ok {
		logger.Fatal(formatted.Text("The query yielded a value of an unexpected type"))
	}

	for _, target := range result.Targets {
		if args.CqueryFlags.Output == arguments.QueryOutput_LabelKind {
			fmt.Printf("%s %s (%s)\n", target.Kind, target.Label, target.Configuration)
		} else {
			fmt.Printf("%s (%s)\n", target.Label, target.Configuration)
		}
	}
}

func parseOrFatal(logger logging.Logger, expressionArguments []string) *model_analysis_pb.QueryExpression {
	parsed, err := ParseExpression(expressionArguments)
	if err != nil {
		logger.Fatal(formatted.Textf("Invalid query expression: %s", err))
	}
	encoded, err := Encode(parsed)
	if err != nil {
		logger.Fatal(formatted.Textf("Invalid query expression: %s", err))
	}
	return encoded
}

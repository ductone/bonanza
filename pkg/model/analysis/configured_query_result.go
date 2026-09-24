package analysis

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"bonanza.build/pkg/label"
	model_core "bonanza.build/pkg/model/core"
	"bonanza.build/pkg/model/evaluation"
	model_starlark "bonanza.build/pkg/model/starlark"
	model_analysis_pb "bonanza.build/pkg/proto/model/analysis"
	model_starlark_pb "bonanza.build/pkg/proto/model/starlark"
)

// A loading-phase query may supply the target set only when its membership
// cannot depend on which branch of a select() the configuration chooses.
func validateConfiguredQueryExpression(expression *model_analysis_pb.QueryExpression) error {
	if expression == nil {
		return errors.New("empty cquery expression")
	}
	switch x := expression.Expression.(type) {
	case *model_analysis_pb.QueryExpression_Pattern, *model_analysis_pb.QueryExpression_Set_:
		return nil
	case *model_analysis_pb.QueryExpression_Binary_:
		if err := validateConfiguredQueryExpression(x.Binary.Left); err != nil {
			return err
		}
		return validateConfiguredQueryExpression(x.Binary.Right)
	case *model_analysis_pb.QueryExpression_Filter_:
		return validateConfiguredQueryExpression(x.Filter.Targets)
	case *model_analysis_pb.QueryExpression_Kind_:
		return validateConfiguredQueryExpression(x.Kind.Targets)
	default:
		return errors.New("configured deps(), rdeps(), and attr() queries are not supported")
	}
}

type configuredQueryFileLocation uint8

const (
	configuredQueryGenerated configuredQueryFileLocation = iota
	configuredQueryRootSource
	configuredQueryExternalSource
)

// classifyConfiguredQueryFile distinguishes source paths in the workspace
// from generated outputs and external sources with no local materialization.
func classifyConfiguredQueryFile(rootRepo, inputPath string, file *model_starlark_pb.File) (string, configuredQueryFileLocation) {
	if file.Owner != nil {
		return inputPath, configuredQueryGenerated
	}
	if source, ok := strings.CutPrefix(inputPath, "external/"+rootRepo+"/"); ok {
		return source, configuredQueryRootSource
	}
	return inputPath, configuredQueryExternalSource
}

// ComputeConfiguredQueryResultValue analyzes default outputs without completing
// any actions. File paths are in the same input-root layout as build outputs.
func (c *baseComputer[TReference, TMetadata]) ComputeConfiguredQueryResultValue(ctx context.Context, key *model_analysis_pb.ConfiguredQueryResult_Key, e ConfiguredQueryResultEnvironment[TReference, TMetadata]) (PatchedConfiguredQueryResultValue[TMetadata], error) {
	if err := validateConfiguredQueryExpression(key.Expression); err != nil {
		return PatchedConfiguredQueryResultValue[TMetadata]{}, err
	}
	buildSpecification := e.GetBuildSpecificationValue(&model_analysis_pb.BuildSpecification_Key{})
	queryResult := e.GetQueryResultValue(&model_analysis_pb.QueryResult_Key{Expression: key.Expression})
	if !buildSpecification.IsSet() || !queryResult.IsSet() {
		return PatchedConfiguredQueryResultValue[TMetadata]{}, evaluation.ErrMissingDependency
	}
	rootModule, err := label.NewModule(buildSpecification.Message.RootModuleName)
	if err != nil {
		return PatchedConfiguredQueryResultValue[TMetadata]{}, fmt.Errorf("invalid root module: %w", err)
	}
	rootPackage := rootModule.ToModuleInstance(nil).GetBareCanonicalRepo().GetRootPackage()
	rootRepo := rootPackage.GetCanonicalRepo().String()
	thread := c.newStarlarkThread(ctx, e, buildSpecification.Message.BuiltinsModuleNames)
	result := &model_analysis_pb.ConfiguredQueryResult_Value{}
	missingDependencies := false

	for _, configuration := range key.Configurations {
		configurationReference, err := c.createInitialConfiguration(ctx, e, thread, rootPackage, configuration)
		if err != nil {
			if errors.Is(err, evaluation.ErrMissingDependency) {
				missingDependencies = true
				continue
			}
			return PatchedConfiguredQueryResultValue[TMetadata]{}, fmt.Errorf("initialize cquery configuration: %w", err)
		}
		clonedReference := model_core.Unpatch(e, configurationReference).Decay()
		for _, target := range queryResult.Message.Targets {
			targetLabel, err := label.NewCanonicalLabel(target.Label)
			if err != nil {
				return PatchedConfiguredQueryResultValue[TMetadata]{}, fmt.Errorf("invalid queried target %#v: %w", target.Label, err)
			}
			visibleTarget := e.GetVisibleTargetValue(model_core.MustBuildPatchedMessage(func(patcher *model_core.ReferenceMessagePatcher[TMetadata]) *model_analysis_pb.VisibleTarget_Key {
				return &model_analysis_pb.VisibleTarget_Key{
					FromPackage:            targetLabel.GetCanonicalPackage().String(),
					ToLabel:                target.Label,
					ConfigurationReference: model_core.Patch(e, clonedReference).Merge(patcher),
				}
			}))
			if !visibleTarget.IsSet() {
				missingDependencies = true
				continue
			}
			if visibleTarget.Message.Label == "" {
				continue
			}
			defaultInfo, err := getProviderFromConfiguredTarget(e, visibleTarget.Message.Label, model_core.Patch(e, clonedReference), defaultInfoProviderIdentifier)
			if err != nil {
				if errors.Is(err, evaluation.ErrMissingDependency) {
					missingDependencies = true
					continue
				}
				return PatchedConfiguredQueryResultValue[TMetadata]{}, fmt.Errorf("analyze target %#v: %w", target.Label, err)
			}
			files, err := model_starlark.GetStructFieldValue(ctx, c.valueReaders.List, defaultInfo, "files")
			if err != nil {
				return PatchedConfiguredQueryResultValue[TMetadata]{}, fmt.Errorf("read outputs of %#v: %w", target.Label, err)
			}
			filesDepset, ok := files.Message.Kind.(*model_starlark_pb.Value_Depset)
			if !ok {
				return PatchedConfiguredQueryResultValue[TMetadata]{}, fmt.Errorf("files of %#v is not a depset", target.Label)
			}
			paths := map[string]struct{}{}
			sourcePaths := map[string]struct{}{}
			externalSourcePaths := map[string]struct{}{}
			var iterationError error
			for entry := range model_starlark.AllListLeafElements(ctx, c.valueReaders.List, model_core.Nested(files, filesDepset.Depset.Elements), &iterationError) {
				file, ok := entry.Message.Kind.(*model_starlark_pb.Value_File)
				if !ok {
					return PatchedConfiguredQueryResultValue[TMetadata]{}, fmt.Errorf("output of %#v is not a file", target.Label)
				}
				outputPath, err := model_starlark.FileGetInputRootPath(model_core.Nested(entry, file.File), nil)
				if err != nil {
					return PatchedConfiguredQueryResultValue[TMetadata]{}, fmt.Errorf("get output path of %#v: %w", target.Label, err)
				}
				queryPath, location := classifyConfiguredQueryFile(rootRepo, outputPath, file.File)
				switch location {
				case configuredQueryRootSource:
					sourcePaths[queryPath] = struct{}{}
				case configuredQueryExternalSource:
					externalSourcePaths[queryPath] = struct{}{}
				default:
					paths[queryPath] = struct{}{}
				}
			}
			if iterationError != nil {
				return PatchedConfiguredQueryResultValue[TMetadata]{}, fmt.Errorf("iterate outputs of %#v: %w", target.Label, iterationError)
			}
			result.Targets = append(result.Targets, &model_analysis_pb.ConfiguredQueryResult_Value_Target{
				Label:               target.Label,
				Kind:                target.Kind,
				Files:               slices.Sorted(maps.Keys(paths)),
				SourceFiles:         slices.Sorted(maps.Keys(sourcePaths)),
				ExternalSourceFiles: slices.Sorted(maps.Keys(externalSourcePaths)),
			})
		}
	}
	if missingDependencies {
		return PatchedConfiguredQueryResultValue[TMetadata]{}, evaluation.ErrMissingDependency
	}
	return model_core.NewSimplePatchedMessage[TMetadata](result), nil
}

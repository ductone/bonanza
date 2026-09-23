package analysis

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"

	"bonanza.build/pkg/label"
	model_core "bonanza.build/pkg/model/core"
	"bonanza.build/pkg/model/evaluation"
	model_analysis_pb "bonanza.build/pkg/proto/model/analysis"
)

// ComputeConfiguredQueryResultValue evaluates a "bazel cquery"
// expression.
//
// It is the same language and the same evaluator as query; only the
// edges differ. query walks the loading-phase graph, where a select()
// contributes every branch because there is no configuration to choose
// with. cquery walks one configuration's graph, where the select() has
// been resolved and an alias has been expanded.
//
// A target reached in more than one configuration is reported once per
// configuration, which is why the result carries a configuration
// alongside each label.
func (c *baseComputer[TReference, TMetadata]) ComputeConfiguredQueryResultValue(ctx context.Context, key *model_analysis_pb.ConfiguredQueryResult_Key, e ConfiguredQueryResultEnvironment[TReference, TMetadata]) (PatchedConfiguredQueryResultValue[TMetadata], error) {
	buildSpecification := e.GetBuildSpecificationValue(&model_analysis_pb.BuildSpecification_Key{})
	if !buildSpecification.IsSet() {
		return PatchedConfiguredQueryResultValue[TMetadata]{}, evaluation.ErrMissingDependency
	}
	rootModule, err := label.NewModule(buildSpecification.Message.RootModuleName)
	if err != nil {
		return PatchedConfiguredQueryResultValue[TMetadata]{}, fmt.Errorf("invalid root module name %#v: %w", buildSpecification.Message.RootModuleName, err)
	}
	rootRepo := rootModule.ToModuleInstance(nil).GetBareCanonicalRepo()
	rootPackage := rootRepo.GetRootPackage()

	configurations := key.Configurations
	if len(configurations) == 0 {
		// cquery with no configuration named still has one: the
		// target configuration with no overrides applied.
		configurations = []*model_analysis_pb.BuildResult_Key_Configuration{{}}
	}

	thread := c.newStarlarkThread(ctx, e, buildSpecification.Message.BuiltinsModuleNames)
	labelResolver := newLabelResolver(e)

	type configuredTarget struct {
		label         string
		configuration string
	}
	var results []configuredTarget
	missingDependencies := false
	kinds := map[string]string{}

	for i, configuration := range configurations {
		configurationReference, err := c.createInitialConfiguration(ctx, e, thread, rootPackage, configuration)
		if err != nil {
			if !errors.Is(err, evaluation.ErrMissingDependency) {
				return PatchedConfiguredQueryResultValue[TMetadata]{}, fmt.Errorf("failed to create initial configuration at index %d: %w", i, err)
			}
			missingDependencies = true
			continue
		}
		clonedConfigurationReference := model_core.Unpatch(e, configurationReference).Decay()

		ev := &queryEvaluator[TReference, TMetadata]{
			context:       ctx,
			computer:      c,
			environment:   e,
			rootRepo:      rootRepo,
			labelResolver: labelResolver,
			dependenciesOfTarget: func(targetLabel string) ([]string, bool) {
				patchedConfigurationReference := model_core.Patch(e, clonedConfigurationReference)
				value := e.GetConfiguredTargetDependenciesValue(
					model_core.NewPatchedMessage(
						&model_analysis_pb.ConfiguredTargetDependencies_Key{
							Label:                  targetLabel,
							ConfigurationReference: patchedConfigurationReference.Message,
						},
						patchedConfigurationReference.Patcher,
					),
				)
				if !value.IsSet() {
					return nil, false
				}
				return value.Message.Labels, true
			},
		}

		labels, err := ev.evaluate(key.Expression)
		if err != nil {
			return PatchedConfiguredQueryResultValue[TMetadata]{}, err
		}
		if ev.missingDependencies {
			missingDependencies = true
			continue
		}

		configurationID := strconv.Itoa(i)
		sortedLabels := make([]string, 0, len(labels))
		for l := range labels {
			sortedLabels = append(sortedLabels, l)
		}
		sort.Strings(sortedLabels)
		for _, l := range sortedLabels {
			if _, ok := kinds[l]; !ok {
				kind, ok := ev.targetKind(l)
				if !ok {
					missingDependencies = missingDependencies || ev.missingDependencies
					continue
				}
				kinds[l] = kind
			}
			results = append(results, configuredTarget{label: l, configuration: configurationID})
		}
		if ev.missingDependencies {
			missingDependencies = true
		}
	}
	if missingDependencies {
		return PatchedConfiguredQueryResultValue[TMetadata]{}, evaluation.ErrMissingDependency
	}

	sort.Slice(results, func(i, j int) bool {
		if results[i].label != results[j].label {
			return results[i].label < results[j].label
		}
		return results[i].configuration < results[j].configuration
	})
	targets := make([]*model_analysis_pb.ConfiguredQueryResult_Value_Target, 0, len(results))
	for _, r := range results {
		targets = append(targets, &model_analysis_pb.ConfiguredQueryResult_Value_Target{
			Label:         r.label,
			Kind:          kinds[r.label],
			Configuration: r.configuration,
		})
	}
	return model_core.NewSimplePatchedMessage[TMetadata](&model_analysis_pb.ConfiguredQueryResult_Value{
		Targets: targets,
	}), nil
}
